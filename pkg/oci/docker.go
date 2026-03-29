package oci

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/fsnotify/fsnotify"
	"github.com/opencontainers/go-digest"
)

// DockerStore implements the Store interface for Docker's overlay2 storage driver.
// It reads from the Docker image store in read-only mode.
type DockerStore struct {
	root string // e.g., /var/lib/docker

	mu               sync.RWMutex
	images           []Image                          // cached image list
	tags             map[string]digest.Digest         // repo:tag → config digest
	blobs            map[digest.Digest]string         // digest → file path (for configs)
	layers           map[digest.Digest]layerEntry     // diffID → layer info
	layerBlobs       map[digest.Digest]layerBlob      // tar blob digest → layer blob info
	manifests        map[digest.Digest]manifestEntry  // config digest → generated distribution manifest
	rewrittenConfigs map[digest.Digest][]byte         // rewritten config digest → config JSON
}

type layerEntry struct {
	chainID  digest.Digest
	cacheID  string
	diffPath string // overlay2 diff directory path
	size     int64
}

// layerBlob maps a tar blob digest to the overlay2 diff path it was generated from.
type layerBlob struct {
	diffPath string
	tarSize  int64
	diffID   digest.Digest // original diffID
}

// manifestEntry caches a generated distribution manifest for an image.
type manifestEntry struct {
	data      []byte
	mediaType string
}

// imagedbPath returns the path to Docker's image database.
func (ds *DockerStore) imagedbPath() string {
	return filepath.Join(ds.root, "image", "overlay2", "imagedb", "content", "sha256")
}

// layerdbPath returns the path to Docker's layer database.
func (ds *DockerStore) layerdbPath() string {
	return filepath.Join(ds.root, "image", "overlay2", "layerdb", "sha256")
}

// repositoriesPath returns the path to Docker's repositories.json.
func (ds *DockerStore) repositoriesPath() string {
	return filepath.Join(ds.root, "image", "overlay2", "repositories.json")
}

// NewDockerStore creates a new DockerStore rooted at the given Docker storage path.
func NewDockerStore(root string) *DockerStore {
	return &DockerStore{
		root:             root,
		tags:             make(map[string]digest.Digest),
		blobs:            make(map[digest.Digest]string),
		layers:           make(map[digest.Digest]layerEntry),
		layerBlobs:       make(map[digest.Digest]layerBlob),
		manifests:        make(map[digest.Digest]manifestEntry),
		rewrittenConfigs: make(map[digest.Digest][]byte),
	}
}

// Name returns "docker".
func (ds *DockerStore) Name() string {
	return "docker"
}

// Init scans the Docker storage and builds the in-memory index.
func (ds *DockerStore) Init(ctx context.Context) error {
	if err := ds.loadRepositories(); err != nil {
		return fmt.Errorf("load repositories: %w", err)
	}
	if err := ds.scanImageDB(); err != nil {
		return fmt.Errorf("scan imagedb: %w", err)
	}
	if err := ds.scanLayerDB(); err != nil {
		return fmt.Errorf("scan layerdb: %w", err)
	}
	if err := ds.buildLayerBlobs(); err != nil {
		return fmt.Errorf("build layer blobs: %w", err)
	}
	if err := ds.buildManifests(); err != nil {
		return fmt.Errorf("build manifests: %w", err)
	}
	slog.Info("docker store initialized",
		"images", len(ds.images),
		"tags", len(ds.tags),
		"blobs", len(ds.blobs),
		"layers", len(ds.layers),
		"layer_blobs", len(ds.layerBlobs),
	)
	return nil
}

// ListImages returns all images in the Docker store.
func (ds *DockerStore) ListImages(_ context.Context) ([]Image, error) {
	ds.mu.RLock()
	defer ds.mu.RUnlock()
	out := make([]Image, len(ds.images))
	copy(out, ds.images)
	return out, nil
}

// Resolve resolves a repo:tag reference to a config digest.
func (ds *DockerStore) Resolve(_ context.Context, ref string) (digest.Digest, error) {
	ds.mu.RLock()
	defer ds.mu.RUnlock()
	d, ok := ds.tags[ref]
	if !ok {
		return "", fmt.Errorf("reference not found: %s", ref)
	}
	return d, nil
}

// GetManifest returns a distribution manifest for the given config digest.
// It builds an OCI distribution manifest from Docker's local storage,
// mapping layer diffIDs to tar blob digests for serving.
func (ds *DockerStore) GetManifest(_ context.Context, dgst digest.Digest) ([]byte, string, error) {
	ds.mu.RLock()
	entry, ok := ds.manifests[dgst]
	ds.mu.RUnlock()
	if ok {
		return entry.data, entry.mediaType, nil
	}
	return nil, "", fmt.Errorf("manifest not found: %s", dgst)
}

// GetBlob returns the content of a blob identified by its digest.
// Supports image config blobs from imagedb and layer tar blobs from overlay2.
func (ds *DockerStore) GetBlob(_ context.Context, dgst digest.Digest) (io.ReadCloser, int64, error) {
	ds.mu.RLock()
	path, ok := ds.blobs[dgst]
	lb, isLayer := ds.layerBlobs[dgst]
	rwConfig, isRewritten := ds.rewrittenConfigs[dgst]
	ds.mu.RUnlock()

	// Rewritten config blob (served from memory)
	if isRewritten {
		return io.NopCloser(bytes.NewReader(rwConfig)), int64(len(rwConfig)), nil
	}

	// Config blob from file
	if ok && path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, 0, err
		}
		return io.NopCloser(bytes.NewReader(data)), int64(len(data)), nil
	}

	// Layer blob: tar the overlay2 diff directory on demand
	if isLayer {
		r, w := io.Pipe()
		go func() {
			tw := tar.NewWriter(w)
			err := filepath.Walk(lb.diffPath, func(path string, info os.FileInfo, err error) error {
				if err != nil {
					return err
				}
				rel, err := filepath.Rel(lb.diffPath, path)
				if err != nil {
					return err
				}
				if rel == "." {
					return nil
				}

				header, err := tar.FileInfoHeader(info, "")
				if err != nil {
					return err
				}
				header.Name = rel
				if info.Mode()&os.ModeSymlink != 0 {
					link, err := os.Readlink(path)
					if err != nil {
						return err
					}
					header.Linkname = link
				}
				if err := tw.WriteHeader(header); err != nil {
					return err
				}
				if info.Mode().IsRegular() {
					f, err := os.Open(path)
					if err != nil {
						return err
					}
					defer f.Close()
					if _, err := io.Copy(tw, f); err != nil {
						return err
					}
				}
				return nil
			})
			tw.Close()
			w.CloseWithError(err)
		}()
		return r, lb.tarSize, nil
	}

	return nil, 0, fmt.Errorf("blob not found: %s", dgst)
}

// HasBlob reports whether a blob with the given digest exists.
func (ds *DockerStore) HasBlob(_ context.Context, dgst digest.Digest) bool {
	ds.mu.RLock()
	defer ds.mu.RUnlock()
	if _, ok := ds.rewrittenConfigs[dgst]; ok {
		return true
	}
	if _, ok := ds.blobs[dgst]; ok {
		return true
	}
	if _, ok := ds.layerBlobs[dgst]; ok {
		return true
	}
	return false
}

// Subscribe watches for changes in the Docker image store and emits events.
func (ds *DockerStore) Subscribe(ctx context.Context) (<-chan Event, error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("create watcher: %w", err)
	}

	imagedbDir := ds.imagedbPath()
	if err := watcher.Add(imagedbDir); err != nil {
		watcher.Close()
		return nil, fmt.Errorf("watch imagedb: %w", err)
	}

	ch := make(chan Event, 16)
	go func() {
		defer watcher.Close()
		defer close(ch)
		for {
			select {
			case <-ctx.Done():
				return
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				if event.Has(fsnotify.Create) {
					name := filepath.Base(event.Name)
					dgst, err := digest.Parse("sha256:" + name)
					if err != nil {
						continue
					}
					// Re-scan to update index
					ds.mu.Lock()
					ds.blobs[dgst] = event.Name
					ds.mu.Unlock()

					select {
					case ch <- Event{Type: EventCreate, Digest: dgst}:
					case <-ctx.Done():
						return
					}
				}
				if event.Has(fsnotify.Remove) {
					name := filepath.Base(event.Name)
					dgst, err := digest.Parse("sha256:" + name)
					if err != nil {
						continue
					}
					ds.mu.Lock()
					delete(ds.blobs, dgst)
					ds.mu.Unlock()

					select {
					case ch <- Event{Type: EventDelete, Digest: dgst}:
					case <-ctx.Done():
						return
					}
				}
			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				slog.Error("docker store watcher error", "error", err)
			}
		}
	}()

	return ch, nil
}

// buildLayerBlobs computes tar digests for each layer's overlay2 diff directory.
// This maps the tar blob digest to the overlay2 path so we can serve layer blobs.
func (ds *DockerStore) buildLayerBlobs() error {
	ds.mu.Lock()
	defer ds.mu.Unlock()

	for diffID, entry := range ds.layers {
		if entry.cacheID == "" {
			continue
		}
		diffPath := filepath.Join(ds.root, "overlay2", entry.cacheID, "diff")
		if _, err := os.Stat(diffPath); err != nil {
			slog.Debug("overlay2 diff not found", "diffID", diffID, "cacheID", entry.cacheID)
			continue
		}

		// Compute tar digest and size by streaming through a hash
		h := sha256.New()
		var size int64
		tw := tar.NewWriter(io.MultiWriter(h, countWriter{&size}))
		err := filepath.Walk(diffPath, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(diffPath, path)
			if err != nil {
				return err
			}
			if rel == "." {
				return nil
			}
			header, err := tar.FileInfoHeader(info, "")
			if err != nil {
				return err
			}
			header.Name = rel
			if info.Mode()&os.ModeSymlink != 0 {
				link, lErr := os.Readlink(path)
				if lErr != nil {
					return lErr
				}
				header.Linkname = link
			}
			if err := tw.WriteHeader(header); err != nil {
				return err
			}
			if info.Mode().IsRegular() {
				f, fErr := os.Open(path)
				if fErr != nil {
					return fErr
				}
				defer f.Close()
				if _, err := io.Copy(tw, f); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			slog.Warn("failed to compute layer tar digest", "diffID", diffID, "error", err)
			continue
		}
		tw.Close()

		tarDigest := digest.NewDigestFromBytes(digest.SHA256, h.Sum(nil))
		entry.diffPath = diffPath
		ds.layers[diffID] = entry
		ds.layerBlobs[tarDigest] = layerBlob{
			diffPath: diffPath,
			tarSize:  size,
			diffID:   diffID,
		}
		slog.Debug("layer blob indexed", "diffID", diffID, "tarDigest", tarDigest, "size", size)
	}
	return nil
}

// countWriter is an io.Writer that counts bytes written.
type countWriter struct {
	n *int64
}

func (cw countWriter) Write(p []byte) (int, error) {
	*cw.n += int64(len(p))
	return len(p), nil
}

// distributionManifest represents an OCI Distribution manifest v2.
type distributionManifest struct {
	SchemaVersion int                    `json:"schemaVersion"`
	MediaType     string                 `json:"mediaType"`
	Config        distributionDescriptor `json:"config"`
	Layers        []distributionDescriptor `json:"layers"`
}

type distributionDescriptor struct {
	MediaType string        `json:"mediaType"`
	Digest    digest.Digest `json:"digest"`
	Size      int64         `json:"size"`
}

// buildManifests generates distribution manifests for all tagged images.
// It rewrites the image config's diff_ids to match our generated tar digests,
// since the overlay2 tar produces different metadata than the original layer tar.
func (ds *DockerStore) buildManifests() error {
	ds.mu.Lock()
	defer ds.mu.Unlock()

	for origConfigDigest, configPath := range ds.blobs {
		data, err := os.ReadFile(configPath)
		if err != nil {
			continue
		}

		// Parse config to get diffIDs
		var rawConfig map[string]json.RawMessage
		if err := json.Unmarshal(data, &rawConfig); err != nil {
			continue
		}
		var rootFS struct {
			Type    string   `json:"type"`
			DiffIDs []string `json:"diff_ids"`
		}
		if err := json.Unmarshal(rawConfig["rootfs"], &rootFS); err != nil {
			continue
		}

		// Build layer descriptors and collect new diffIDs
		var layerDescs []distributionDescriptor
		var newDiffIDs []string
		allLayersAvailable := true
		for _, diffIDStr := range rootFS.DiffIDs {
			did, err := digest.Parse(diffIDStr)
			if err != nil {
				allLayersAvailable = false
				break
			}
			// Find the tar blob digest for this diffID
			found := false
			for tarDigest, lb := range ds.layerBlobs {
				if lb.diffID == did {
					layerDescs = append(layerDescs, distributionDescriptor{
						MediaType: "application/vnd.docker.image.rootfs.diff.tar",
						Digest:    tarDigest,
						Size:      lb.tarSize,
					})
					// The tar digest IS the new diffID (sha256 of uncompressed tar)
					newDiffIDs = append(newDiffIDs, tarDigest.String())
					found = true
					break
				}
			}
			if !found {
				allLayersAvailable = false
				break
			}
		}

		if !allLayersAvailable {
			slog.Debug("skipping manifest for image with missing layers", "config", origConfigDigest)
			continue
		}

		// Rewrite the config with new diff_ids
		newRootFS := struct {
			Type    string   `json:"type"`
			DiffIDs []string `json:"diff_ids"`
		}{
			Type:    rootFS.Type,
			DiffIDs: newDiffIDs,
		}
		newRootFSBytes, err := json.Marshal(newRootFS)
		if err != nil {
			continue
		}
		rawConfig["rootfs"] = newRootFSBytes

		newConfigData, err := json.Marshal(rawConfig)
		if err != nil {
			continue
		}

		// Compute new config digest
		newConfigDigest := digest.FromBytes(newConfigData)

		// Store the rewritten config as a blob
		ds.blobs[newConfigDigest] = "" // sentinel; we'll serve from memory
		// Store rewritten config data for serving
		ds.rewrittenConfigs[newConfigDigest] = newConfigData

		manifest := distributionManifest{
			SchemaVersion: 2,
			MediaType:     "application/vnd.docker.distribution.manifest.v2+json",
			Config: distributionDescriptor{
				MediaType: "application/vnd.docker.container.image.v1+json",
				Digest:    newConfigDigest,
				Size:      int64(len(newConfigData)),
			},
			Layers: layerDescs,
		}

		manifestBytes, err := json.Marshal(manifest)
		if err != nil {
			continue
		}

		// Map both original and new config digest to the manifest
		entry := manifestEntry{
			data:      manifestBytes,
			mediaType: "application/vnd.docker.distribution.manifest.v2+json",
		}
		ds.manifests[origConfigDigest] = entry
		ds.manifests[newConfigDigest] = entry

		// Update tags to also resolve to the manifest
		slog.Debug("distribution manifest built",
			"origConfig", origConfigDigest,
			"newConfig", newConfigDigest,
			"layers", len(layerDescs))
	}
	return nil
}

// loadRepositories parses Docker's repositories.json to build tag → digest mapping.
func (ds *DockerStore) loadRepositories() error {
	data, err := os.ReadFile(ds.repositoriesPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil // No repositories yet
		}
		return err
	}

	// Docker repositories.json format:
	// {"Repositories": {"nginx": {"nginx:latest": "sha256:abc...", "nginx@sha256:abc...": "sha256:abc..."}}}
	var repos struct {
		Repositories map[string]map[string]string `json:"Repositories"`
	}
	if err := json.Unmarshal(data, &repos); err != nil {
		return fmt.Errorf("parse repositories.json: %w", err)
	}

	ds.mu.Lock()
	defer ds.mu.Unlock()
	for _, tags := range repos.Repositories {
		for ref, dgstStr := range tags {
			// Skip digest references (nginx@sha256:...), only keep tag references
			if strings.Contains(ref, "@") {
				continue
			}
			dgst, err := digest.Parse(dgstStr)
			if err != nil {
				slog.Warn("invalid digest in repositories.json", "ref", ref, "digest", dgstStr)
				continue
			}
			ds.tags[ref] = dgst
		}
	}
	return nil
}

// scanImageDB scans the imagedb directory for image configs.
func (ds *DockerStore) scanImageDB() error {
	dir := ds.imagedbPath()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	ds.mu.Lock()
	defer ds.mu.Unlock()

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		dgst, err := digest.Parse("sha256:" + entry.Name())
		if err != nil {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		ds.blobs[dgst] = path

		// Parse image config to extract repo tags
		data, err := os.ReadFile(path)
		if err != nil {
			slog.Warn("failed to read image config", "digest", dgst, "error", err)
			continue
		}

		info, err := entry.Info()
		if err != nil {
			continue
		}

		// Find tags for this image
		var tags []string
		for ref, d := range ds.tags {
			if d == dgst {
				tags = append(tags, ref)
			}
		}

		ds.images = append(ds.images, Image{
			Digest:   dgst,
			RepoTags: tags,
			Size:     info.Size(),
		})

		// Parse config to get diff_ids for layer mapping
		var config struct {
			RootFS struct {
				DiffIDs []string `json:"diff_ids"`
			} `json:"rootfs"`
		}
		if err := json.Unmarshal(data, &config); err == nil {
			for _, diffIDStr := range config.RootFS.DiffIDs {
				d, err := digest.Parse(diffIDStr)
				if err != nil {
					continue
				}
				if _, exists := ds.layers[d]; !exists {
					ds.layers[d] = layerEntry{}
				}
			}
		}
	}
	return nil
}

// scanLayerDB scans the layerdb to build the diffID → chainID → cacheID mapping.
func (ds *DockerStore) scanLayerDB() error {
	dir := ds.layerdbPath()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	ds.mu.Lock()
	defer ds.mu.Unlock()

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		chainID := "sha256:" + entry.Name()
		layerDir := filepath.Join(dir, entry.Name())

		// Read diffID
		diffData, err := os.ReadFile(filepath.Join(layerDir, "diff"))
		if err != nil {
			continue
		}
		diffID, err := digest.Parse(strings.TrimSpace(string(diffData)))
		if err != nil {
			continue
		}

		// Read cacheID
		cacheData, err := os.ReadFile(filepath.Join(layerDir, "cache-id"))
		if err != nil {
			continue
		}
		cacheID := strings.TrimSpace(string(cacheData))

		// Read size
		var size int64
		sizeData, err := os.ReadFile(filepath.Join(layerDir, "size"))
		if err == nil {
			fmt.Sscanf(strings.TrimSpace(string(sizeData)), "%d", &size)
		}

		chainDgst, _ := digest.Parse(chainID)
		diffPath := filepath.Join(ds.root, "overlay2", cacheID, "diff")
		ds.layers[diffID] = layerEntry{
			chainID:  chainDgst,
			cacheID:  cacheID,
			diffPath: diffPath,
			size:     size,
		}
	}
	return nil
}

// ComputeChainID calculates the chainID from an ordered list of diffIDs.
// The first chainID equals the first diffID.
// Subsequent chainIDs are: sha256(parentChainID + " " + diffID)
func ComputeChainID(diffIDs []digest.Digest) []digest.Digest {
	if len(diffIDs) == 0 {
		return nil
	}
	chainIDs := make([]digest.Digest, len(diffIDs))
	chainIDs[0] = diffIDs[0]
	for i := 1; i < len(diffIDs); i++ {
		h := sha256.New()
		h.Write([]byte(chainIDs[i-1].String() + " " + diffIDs[i].String()))
		chainIDs[i] = digest.NewDigestFromBytes(digest.SHA256, h.Sum(nil))
	}
	return chainIDs
}

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
// Layer blob digests and distribution manifests are computed lazily on first access.
type DockerStore struct {
	root string // e.g., /var/lib/docker

	mu               sync.RWMutex
	images           []Image                      // cached image list
	tags             map[string]digest.Digest     // repo:tag → config digest
	blobs            map[digest.Digest]string     // digest → file path (for configs)
	layers           map[digest.Digest]layerEntry // diffID → layer info
	layerBlobs       map[digest.Digest]layerBlob  // tar blob digest → layer blob info
	manifests        map[digest.Digest]manifestEntry
	rewrittenConfigs map[digest.Digest][]byte

	// Lazy initialization: one sync.Once per image config digest
	imageOnce sync.Map // digest.Digest → *sync.Once
}

type layerEntry struct {
	chainID  digest.Digest
	cacheID  string
	diffPath string // overlay2 diff directory path
	size     int64
}

type layerBlob struct {
	diffPath string
	tarSize  int64
	diffID   digest.Digest
}

type manifestEntry struct {
	data      []byte
	mediaType string
}

func (ds *DockerStore) imagedbPath() string {
	return filepath.Join(ds.root, "image", "overlay2", "imagedb", "content", "sha256")
}

func (ds *DockerStore) layerdbPath() string {
	return filepath.Join(ds.root, "image", "overlay2", "layerdb", "sha256")
}

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
// Layer blob digests and manifests are computed lazily on first access.
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
	slog.Info("docker store initialized",
		"images", len(ds.images),
		"tags", len(ds.tags),
		"blobs", len(ds.blobs),
		"layers", len(ds.layers),
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
// It tries several normalizations for Docker Hub official images:
//   - exact match (e.g., "docker.io/library/redis:7-alpine")
//   - with docker.io/ prefix (e.g., "library/redis:7-alpine" → "docker.io/library/redis:7-alpine")
//   - without library/ prefix (e.g., "library/redis:7-alpine" → "redis:7-alpine")
//   - short form for docker.io/library (e.g., "redis:7-alpine" → "library/redis:7-alpine")
func (ds *DockerStore) Resolve(_ context.Context, ref string) (digest.Digest, error) {
	ds.mu.RLock()
	defer ds.mu.RUnlock()

	// Exact match
	if d, ok := ds.tags[ref]; ok {
		return d, nil
	}

	// Split into name and tag for normalization
	parts := strings.SplitN(ref, ":", 2)
	if len(parts) != 2 {
		return "", fmt.Errorf("reference not found: %s", ref)
	}
	name, tag := parts[0], parts[1]

	// Try with docker.io/ prefix: library/redis → docker.io/library/redis
	if !strings.Contains(name, ".") {
		candidate := "docker.io/" + name + ":" + tag
		if d, ok := ds.tags[candidate]; ok {
			return d, nil
		}
	}

	// Strip library/ prefix: library/redis → redis
	if strings.HasPrefix(name, "library/") {
		short := strings.TrimPrefix(name, "library/") + ":" + tag
		if d, ok := ds.tags[short]; ok {
			return d, nil
		}
	}

	// Add library/ prefix: redis → library/redis
	if !strings.Contains(name, "/") {
		candidate := "library/" + name + ":" + tag
		if d, ok := ds.tags[candidate]; ok {
			return d, nil
		}
		// Also try docker.io/library/
		candidate = "docker.io/library/" + name + ":" + tag
		if d, ok := ds.tags[candidate]; ok {
			return d, nil
		}
	}

	return "", fmt.Errorf("reference not found: %s", ref)
}

// GetManifest returns a distribution manifest for the given config digest.
// Manifests are generated lazily on first access.
func (ds *DockerStore) GetManifest(_ context.Context, dgst digest.Digest) ([]byte, string, error) {
	// Ensure this image's manifest is built
	ds.ensureImageReady(dgst)

	ds.mu.RLock()
	entry, ok := ds.manifests[dgst]
	ds.mu.RUnlock()
	if ok {
		return entry.data, entry.mediaType, nil
	}
	return nil, "", fmt.Errorf("manifest not found: %s", dgst)
}

// GetBlob returns the content of a blob identified by its digest.
func (ds *DockerStore) GetBlob(_ context.Context, dgst digest.Digest) (io.ReadCloser, int64, error) {
	ds.mu.RLock()
	path, ok := ds.blobs[dgst]
	lb, isLayer := ds.layerBlobs[dgst]
	rwConfig, isRewritten := ds.rewrittenConfigs[dgst]
	ds.mu.RUnlock()

	if isRewritten {
		return io.NopCloser(bytes.NewReader(rwConfig)), int64(len(rwConfig)), nil
	}

	if ok && path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, 0, err
		}
		return io.NopCloser(bytes.NewReader(data)), int64(len(data)), nil
	}

	if isLayer {
		return ds.streamLayerTar(lb)
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

// streamLayerTar creates a tar stream from an overlay2 diff directory.
func (ds *DockerStore) streamLayerTar(lb layerBlob) (io.ReadCloser, int64, error) {
	r, w := io.Pipe()
	go func() {
		err := tarDirectory(w, lb.diffPath)
		w.CloseWithError(err)
	}()
	return r, lb.tarSize, nil
}

// tarDirectory writes a tar archive of dir's contents to w.
func tarDirectory(w io.Writer, dir string) error {
	tw := tar.NewWriter(w)
	defer tw.Close()
	return filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
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
			_, err = io.Copy(tw, f)
			return err
		}
		return nil
	})
}

// computeLayerTarDigest tars a diff directory and returns the digest and size.
func computeLayerTarDigest(diffPath string) (digest.Digest, int64, error) {
	h := sha256.New()
	var size int64
	w := io.MultiWriter(h, countWriter{&size})
	if err := tarDirectory(w, diffPath); err != nil {
		return "", 0, err
	}
	return digest.NewDigestFromBytes(digest.SHA256, h.Sum(nil)), size, nil
}

type countWriter struct{ n *int64 }

func (cw countWriter) Write(p []byte) (int, error) {
	*cw.n += int64(len(p))
	return len(p), nil
}

// ensureImageReady lazily computes layer tar digests and builds the
// distribution manifest for the image identified by configDigest.
// Uses sync.Once to guarantee each image is processed exactly once.
func (ds *DockerStore) ensureImageReady(configDigest digest.Digest) {
	// Fast path: already built
	ds.mu.RLock()
	_, done := ds.manifests[configDigest]
	ds.mu.RUnlock()
	if done {
		return
	}

	once, _ := ds.imageOnce.LoadOrStore(configDigest, &sync.Once{})
	once.(*sync.Once).Do(func() {
		ds.buildImageManifest(configDigest)
	})
}

func (ds *DockerStore) buildImageManifest(configDigest digest.Digest) {
	ds.mu.RLock()
	configPath, ok := ds.blobs[configDigest]
	ds.mu.RUnlock()
	if !ok || configPath == "" {
		return
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		return
	}

	var rawConfig map[string]json.RawMessage
	if err := json.Unmarshal(data, &rawConfig); err != nil {
		return
	}
	var rootFS struct {
		Type    string   `json:"type"`
		DiffIDs []string `json:"diff_ids"`
	}
	if err := json.Unmarshal(rawConfig["rootfs"], &rootFS); err != nil {
		return
	}

	var layerDescs []distributionDescriptor
	var newDiffIDs []string

	for _, diffIDStr := range rootFS.DiffIDs {
		did, err := digest.Parse(diffIDStr)
		if err != nil {
			return
		}

		ds.mu.RLock()
		entry, ok := ds.layers[did]
		ds.mu.RUnlock()
		if !ok || entry.diffPath == "" {
			slog.Debug("layer missing for image", "config", configDigest, "diffID", did)
			return
		}

		// Check if already computed (shared layer across images)
		var tarDigest digest.Digest
		var tarSize int64
		found := false
		ds.mu.RLock()
		for td, lb := range ds.layerBlobs {
			if lb.diffID == did {
				tarDigest = td
				tarSize = lb.tarSize
				found = true
				break
			}
		}
		ds.mu.RUnlock()

		if !found {
			slog.Debug("computing layer tar digest", "diffID", did)
			var err error
			tarDigest, tarSize, err = computeLayerTarDigest(entry.diffPath)
			if err != nil {
				slog.Warn("failed to compute layer tar digest", "diffID", did, "error", err)
				return
			}
			ds.mu.Lock()
			ds.layerBlobs[tarDigest] = layerBlob{
				diffPath: entry.diffPath,
				tarSize:  tarSize,
				diffID:   did,
			}
			ds.mu.Unlock()
			slog.Debug("layer tar digest computed", "diffID", did, "tarDigest", tarDigest, "size", tarSize)
		}

		layerDescs = append(layerDescs, distributionDescriptor{
			MediaType: "application/vnd.docker.image.rootfs.diff.tar",
			Digest:    tarDigest,
			Size:      tarSize,
		})
		newDiffIDs = append(newDiffIDs, tarDigest.String())
	}

	// Rewrite config with new diff_ids
	newRootFS, _ := json.Marshal(struct {
		Type    string   `json:"type"`
		DiffIDs []string `json:"diff_ids"`
	}{Type: rootFS.Type, DiffIDs: newDiffIDs})
	rawConfig["rootfs"] = newRootFS
	newConfigData, err := json.Marshal(rawConfig)
	if err != nil {
		return
	}
	newConfigDigest := digest.FromBytes(newConfigData)

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
		return
	}

	entry := manifestEntry{
		data:      manifestBytes,
		mediaType: "application/vnd.docker.distribution.manifest.v2+json",
	}

	ds.mu.Lock()
	ds.blobs[newConfigDigest] = ""
	ds.rewrittenConfigs[newConfigDigest] = newConfigData
	ds.manifests[configDigest] = entry
	ds.manifests[newConfigDigest] = entry
	ds.mu.Unlock()

	slog.Info("manifest built (lazy)", "config", configDigest, "layers", len(layerDescs))
}

type distributionManifest struct {
	SchemaVersion int                      `json:"schemaVersion"`
	MediaType     string                   `json:"mediaType"`
	Config        distributionDescriptor   `json:"config"`
	Layers        []distributionDescriptor `json:"layers"`
}

type distributionDescriptor struct {
	MediaType string        `json:"mediaType"`
	Digest    digest.Digest `json:"digest"`
	Size      int64         `json:"size"`
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
			case event := <-watcher.Events:
				if event.Has(fsnotify.Create) {
					name := filepath.Base(event.Name)
					dgst, err := digest.Parse("sha256:" + name)
					if err != nil {
						continue
					}
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
			case err := <-watcher.Errors:
				slog.Error("docker store watcher error", "error", err)
			}
		}
	}()

	return ch, nil
}

// loadRepositories parses Docker's repositories.json to build tag → digest mapping.
func (ds *DockerStore) loadRepositories() error {
	data, err := os.ReadFile(ds.repositoriesPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

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

		data, err := os.ReadFile(path)
		if err != nil {
			slog.Warn("failed to read image config", "digest", dgst, "error", err)
			continue
		}

		info, err := entry.Info()
		if err != nil {
			continue
		}

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

		diffData, err := os.ReadFile(filepath.Join(layerDir, "diff"))
		if err != nil {
			continue
		}
		diffID, err := digest.Parse(strings.TrimSpace(string(diffData)))
		if err != nil {
			continue
		}

		cacheData, err := os.ReadFile(filepath.Join(layerDir, "cache-id"))
		if err != nil {
			continue
		}
		cacheID := strings.TrimSpace(string(cacheData))

		var size int64
		sizeData, err := os.ReadFile(filepath.Join(layerDir, "size"))
		if err == nil {
			_, _ = fmt.Sscanf(strings.TrimSpace(string(sizeData)), "%d", &size)
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

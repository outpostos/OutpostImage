package oci

import (
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

	mu     sync.RWMutex
	images []Image                      // cached image list
	tags   map[string]digest.Digest     // repo:tag → config digest
	blobs  map[digest.Digest]string     // digest → file path (for configs)
	layers map[digest.Digest]layerEntry // diffID → layer info
}

type layerEntry struct {
	chainID digest.Digest
	cacheID string
	size    int64
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
		root:   root,
		tags:   make(map[string]digest.Digest),
		blobs:  make(map[digest.Digest]string),
		layers: make(map[digest.Digest]layerEntry),
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
func (ds *DockerStore) Resolve(_ context.Context, ref string) (digest.Digest, error) {
	ds.mu.RLock()
	defer ds.mu.RUnlock()
	d, ok := ds.tags[ref]
	if !ok {
		return "", fmt.Errorf("reference not found: %s", ref)
	}
	return d, nil
}

// GetManifest returns the image config as the manifest content.
// For Docker overlay2, we serve the image config directly since Docker
// doesn't store OCI distribution manifests natively.
func (ds *DockerStore) GetManifest(_ context.Context, dgst digest.Digest) ([]byte, string, error) {
	ds.mu.RLock()
	path, ok := ds.blobs[dgst]
	ds.mu.RUnlock()
	if !ok {
		return nil, "", fmt.Errorf("manifest not found: %s", dgst)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, "", fmt.Errorf("read manifest: %w", err)
	}
	return data, "application/vnd.docker.container.image.v1+json", nil
}

// GetBlob returns the content of a blob identified by its digest.
// Supports image config blobs from imagedb.
func (ds *DockerStore) GetBlob(_ context.Context, dgst digest.Digest) (io.ReadCloser, int64, error) {
	ds.mu.RLock()
	path, ok := ds.blobs[dgst]
	ds.mu.RUnlock()
	if ok {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, 0, err
		}
		return io.NopCloser(bytes.NewReader(data)), int64(len(data)), nil
	}
	return nil, 0, fmt.Errorf("blob not found: %s", dgst)
}

// HasBlob reports whether a blob with the given digest exists.
func (ds *DockerStore) HasBlob(_ context.Context, dgst digest.Digest) bool {
	ds.mu.RLock()
	defer ds.mu.RUnlock()
	if _, ok := ds.blobs[dgst]; ok {
		return true
	}
	if _, ok := ds.layers[dgst]; ok {
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
		ds.layers[diffID] = layerEntry{
			chainID: chainDgst,
			cacheID: cacheID,
			size:    size,
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

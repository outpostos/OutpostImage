package oci

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/opencontainers/go-digest"
	bolt "go.etcd.io/bbolt"
)

// osStatFile is the (*os.File).Stat method; injectable for testing.
var osStatFile = (*os.File).Stat

// ContainerdStore implements the Store interface for containerd's content store.
// It reads from /var/lib/containerd/io.containerd.content.v1.content/ for blobs
// and uses the bolt metadata DB for image/tag resolution.
type ContainerdStore struct {
	root string // e.g., /var/lib/containerd

	mu     sync.RWMutex
	images []Image
	tags   map[string]digest.Digest // repo:tag → manifest digest
	blobs  map[digest.Digest]string // digest → file path
}

// contentStorePath returns the path to containerd's content store blobs.
func (cs *ContainerdStore) contentStorePath() string {
	return filepath.Join(cs.root, "io.containerd.content.v1.content", "blobs", "sha256")
}

// metaDBPath returns the path to containerd's metadata bolt DB.
func (cs *ContainerdStore) metaDBPath() string {
	return filepath.Join(cs.root, "io.containerd.metadata.v1.bolt", "meta.db")
}

// NewContainerdStore creates a new ContainerdStore rooted at the given containerd storage path.
func NewContainerdStore(root string) *ContainerdStore {
	return &ContainerdStore{
		root:  root,
		tags:  make(map[string]digest.Digest),
		blobs: make(map[digest.Digest]string),
	}
}

// Name returns "containerd".
func (cs *ContainerdStore) Name() string {
	return "containerd"
}

// Init scans the containerd content store and metadata DB to build in-memory indices.
func (cs *ContainerdStore) Init(ctx context.Context) error {
	if err := cs.scanContentStore(); err != nil {
		return fmt.Errorf("scan content store: %w", err)
	}
	if err := cs.loadMetadataDB(); err != nil {
		// Metadata DB may not exist or be locked — warn but continue
		slog.Warn("containerd metadata DB load failed (tag resolution unavailable)", "error", err)
	}
	cs.buildImageIndex()
	slog.Info("containerd store initialized",
		"images", len(cs.images),
		"tags", len(cs.tags),
		"blobs", len(cs.blobs),
	)
	return nil
}

// ListImages returns all images in the containerd store.
func (cs *ContainerdStore) ListImages(_ context.Context) ([]Image, error) {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	out := make([]Image, len(cs.images))
	copy(out, cs.images)
	return out, nil
}

// Resolve resolves a repo:tag reference to a manifest digest.
// containerd stores full references like "docker.io/library/nginx:latest",
// so we try multiple variants to handle Docker-style short names.
func (cs *ContainerdStore) Resolve(_ context.Context, ref string) (digest.Digest, error) {
	cs.mu.RLock()
	defer cs.mu.RUnlock()

	candidates, err := containerdTagCandidates(ref)
	if err != nil {
		return "", fmt.Errorf("reference not found: %s", ref)
	}
	for _, candidate := range candidates {
		if d, ok := cs.tags[candidate]; ok {
			return d, nil
		}
	}

	return "", fmt.Errorf("reference not found: %s", ref)
}

func containerdTagCandidates(ref string) ([]string, error) {
	parts := strings.SplitN(ref, ":", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return nil, fmt.Errorf("invalid reference")
	}
	name, tag := parts[0], parts[1]

	candidates := make([]string, 0, 6)
	add := func(name string) {
		candidates = append(candidates, name+":"+tag)
	}

	add(name)

	if strings.HasPrefix(name, "docker.io/") {
		withoutRegistry := strings.TrimPrefix(name, "docker.io/")
		add(withoutRegistry)
		if strings.HasPrefix(withoutRegistry, "library/") {
			add(strings.TrimPrefix(withoutRegistry, "library/"))
		}
		return candidates, nil
	}

	if !hasRegistryHost(name) {
		add("docker.io/" + name)
		if !strings.Contains(name, "/") {
			add("docker.io/library/" + name)
		}
		if strings.HasPrefix(name, "library/") {
			add(strings.TrimPrefix(name, "library/"))
		}
	}

	return candidates, nil
}

func hasRegistryHost(name string) bool {
	first, _, hasSlash := strings.Cut(name, "/")
	if !hasSlash {
		return false
	}
	return strings.Contains(first, ".") || strings.Contains(first, ":") || first == "localhost"
}

// GetManifest returns the manifest bytes and media type for the given digest.
func (cs *ContainerdStore) GetManifest(_ context.Context, dgst digest.Digest) ([]byte, string, error) {
	cs.mu.RLock()
	path, ok := cs.blobs[dgst]
	cs.mu.RUnlock()
	if !ok {
		return nil, "", fmt.Errorf("manifest not found: %s", dgst)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, "", fmt.Errorf("read manifest: %w", err)
	}
	mediaType := detectMediaType(data)
	return data, mediaType, nil
}

// GetBlob returns the content of a blob identified by its digest.
func (cs *ContainerdStore) GetBlob(_ context.Context, dgst digest.Digest) (io.ReadCloser, int64, error) {
	cs.mu.RLock()
	path, ok := cs.blobs[dgst]
	cs.mu.RUnlock()
	if !ok {
		return nil, 0, fmt.Errorf("blob not found: %s", dgst)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	info, err := osStatFile(f)
	if err != nil {
		f.Close()
		return nil, 0, err
	}
	return f, info.Size(), nil
}

// HasBlob reports whether a blob with the given digest exists.
func (cs *ContainerdStore) HasBlob(_ context.Context, dgst digest.Digest) bool {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	_, ok := cs.blobs[dgst]
	return ok
}

// Subscribe watches for changes in the containerd content store and emits events.
// It monitors the blobs/sha256/ directory for file creation and deletion with debouncing.
func (cs *ContainerdStore) Subscribe(ctx context.Context) (<-chan Event, error) {
	return cs.subscribe(ctx, newOCIWatcher)
}

func (cs *ContainerdStore) subscribe(ctx context.Context, newWatcher func() (ociWatcher, error)) (<-chan Event, error) {
	watcher, err := newWatcher()
	if err != nil {
		return nil, fmt.Errorf("create watcher: %w", err)
	}

	blobDir := cs.contentStorePath()
	if err := watcher.Add(blobDir); err != nil {
		watcher.Close()
		return nil, fmt.Errorf("watch content store: %w", err)
	}

	ch := make(chan Event, 16)
	go func() {
		defer watcher.Close()
		defer close(ch)

		// Debounce: track pending events and flush after quiet period
		const debounceDelay = 500 * time.Millisecond
		pending := make(map[string]EventType) // filename → event type
		var timer *time.Timer
		var timerC <-chan time.Time

		flush := func() {
			for name, evType := range pending {
				dgst, err := digest.Parse("sha256:" + name)
				if err != nil {
					continue
				}
				blobPath := filepath.Join(blobDir, name)

				cs.mu.Lock()
				if evType == EventCreate {
					cs.blobs[dgst] = blobPath
				} else {
					delete(cs.blobs, dgst)
				}
				cs.mu.Unlock()

				select {
				case ch <- Event{Type: evType, Digest: dgst}:
				case <-ctx.Done():
					return
				}
			}
			pending = make(map[string]EventType)
		}

		for {
			select {
			case <-ctx.Done():
				return
			case event := <-watcher.Events():
				name := filepath.Base(event.Name)
				if event.Has(fsnotify.Create) {
					pending[name] = EventCreate
				} else if event.Has(fsnotify.Remove) {
					pending[name] = EventDelete
				} else {
					continue
				}
				// Reset debounce timer
				if timer != nil {
					timer.Stop()
				}
				timer = time.NewTimer(debounceDelay)
				timerC = timer.C
			case <-timerC:
				flush()
				timerC = nil
			case err := <-watcher.Errors():
				slog.Error("containerd store watcher error", "error", err)
			}
		}
	}()

	return ch, nil
}

// scanContentStore scans the blobs/sha256/ directory for all content-addressable blobs.
func (cs *ContainerdStore) scanContentStore() error {
	dir := cs.contentStorePath()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	cs.mu.Lock()
	defer cs.mu.Unlock()

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		dgst, err := digest.Parse("sha256:" + entry.Name())
		if err != nil {
			continue
		}
		cs.blobs[dgst] = filepath.Join(dir, entry.Name())
	}
	return nil
}

// loadMetadataDB reads the containerd bolt metadata DB to extract image and tag information.
// Since the containerd daemon holds an exclusive lock on meta.db, we copy it to a
// temporary file first so we can open it without contention.
func (cs *ContainerdStore) loadMetadataDB() error {
	dbPath := cs.metaDBPath()
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return fmt.Errorf("metadata DB not found: %s", dbPath)
	}

	// Copy DB to temp file to avoid lock contention with containerd daemon.
	tmpFile, err := os.CreateTemp("", "outpostimage-meta-*.db")
	if err != nil {
		return fmt.Errorf("create temp DB: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	src, err := os.Open(dbPath)
	if err != nil {
		tmpFile.Close()
		return fmt.Errorf("open source DB: %w", err)
	}
	if _, err := io.Copy(tmpFile, src); err != nil {
		src.Close()
		tmpFile.Close()
		return fmt.Errorf("copy DB: %w", err)
	}
	src.Close()
	tmpFile.Close()

	db, err := bolt.Open(tmpPath, 0400, &bolt.Options{
		ReadOnly: true,
		Timeout:  2 * time.Second,
	})
	if err != nil {
		return fmt.Errorf("open metadata DB: %w", err)
	}
	defer db.Close()

	cs.mu.Lock()
	defer cs.mu.Unlock()

	return db.View(func(tx *bolt.Tx) error {
		// containerd metadata DB structure:
		// v1 → <namespace> → images → <image name> → target (JSON with digest + mediaType)
		v1 := tx.Bucket([]byte("v1"))
		if v1 == nil {
			return nil
		}

		return v1.ForEach(func(nsKey, _ []byte) error {
			nsBucket := v1.Bucket(nsKey)
			if nsBucket == nil {
				return nil
			}

			imagesBucket := nsBucket.Bucket([]byte("images"))
			if imagesBucket == nil {
				return nil
			}

			return imagesBucket.ForEach(func(imageKey, _ []byte) error {
				imageBucket := imagesBucket.Bucket(imageKey)
				if imageBucket == nil {
					return nil
				}

				// containerd stores target as a sub-bucket with
				// keys: digest, mediatype, size
				targetBucket := imageBucket.Bucket([]byte("target"))
				if targetBucket == nil {
					return nil
				}

				digestBytes := targetBucket.Get([]byte("digest"))
				if digestBytes == nil {
					return nil
				}

				dgst, err := digest.Parse(string(digestBytes))
				if err != nil {
					return nil
				}

				imageName := string(imageKey)
				ref := normalizeImageRef(imageName)
				cs.tags[ref] = dgst

				return nil
			})
		})
	})
}

// buildImageIndex builds the image list from tags and blobs.
func (cs *ContainerdStore) buildImageIndex() {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	// Group tags by digest
	digestTags := make(map[digest.Digest][]string)
	for ref, dgst := range cs.tags {
		digestTags[dgst] = append(digestTags[dgst], ref)
	}

	cs.images = nil
	for dgst, tags := range digestTags {
		var size int64
		if path, ok := cs.blobs[dgst]; ok {
			if info, err := os.Stat(path); err == nil {
				size = info.Size()
			}
		}
		cs.images = append(cs.images, Image{
			Digest:   dgst,
			RepoTags: tags,
			Size:     size,
		})
	}
}

// normalizeImageRef converts a containerd image reference to a simple repo:tag format.
// e.g., "docker.io/library/nginx:latest" stays as is,
// "docker.io/library/nginx@sha256:abc" is skipped (handled separately).
func normalizeImageRef(ref string) string {
	// If it contains @, extract the part before @ as the name
	if idx := strings.Index(ref, "@"); idx >= 0 {
		return ref[:idx]
	}
	return ref
}

// detectMediaType attempts to determine the media type of content by examining its structure.
func detectMediaType(data []byte) string {
	var probe struct {
		SchemaVersion int    `json:"schemaVersion"`
		MediaType     string `json:"mediaType"`
		Config        *struct {
			MediaType string `json:"mediaType"`
		} `json:"config"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return "application/octet-stream"
	}

	// If the content has an explicit mediaType field, use it
	if probe.MediaType != "" {
		return probe.MediaType
	}

	// Check if it looks like an OCI manifest
	if probe.Config != nil {
		if strings.Contains(probe.Config.MediaType, "oci") {
			return "application/vnd.oci.image.manifest.v1+json"
		}
		return "application/vnd.docker.distribution.manifest.v2+json"
	}

	// Check if it looks like an image config
	if bytes.Contains(data, []byte(`"rootfs"`)) {
		return "application/vnd.docker.container.image.v1+json"
	}

	return "application/octet-stream"
}

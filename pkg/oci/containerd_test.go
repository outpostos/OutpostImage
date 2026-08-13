package oci

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	bolt "go.etcd.io/bbolt"
)

func TestContainerdStore_Name(t *testing.T) {
	store := NewContainerdStore("/tmp/test-containerd")
	if got := store.Name(); got != "containerd" {
		t.Errorf("Name() = %q, want %q", got, "containerd")
	}
}

func setupContainerdTestStore(t *testing.T) (*ContainerdStore, string) {
	t.Helper()
	root := t.TempDir()

	// Create content store directory structure
	blobDir := filepath.Join(root, "io.containerd.content.v1.content", "blobs", "sha256")
	if err := os.MkdirAll(blobDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Create a test blob (image config)
	configContent := `{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":["sha256:aaaa"]}}`
	configDigest := digest.FromBytes([]byte(configContent))
	configHash := configDigest.Encoded()
	if err := os.WriteFile(filepath.Join(blobDir, configHash), []byte(configContent), 0644); err != nil {
		t.Fatal(err)
	}

	// Create a test manifest
	manifest := map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.docker.distribution.manifest.v2+json",
		"config": map[string]any{
			"mediaType": "application/vnd.docker.container.image.v1+json",
			"digest":    configDigest.String(),
			"size":      len(configContent),
		},
		"layers": []map[string]any{},
	}
	manifestBytes, _ := json.Marshal(manifest)
	manifestDigest := digest.FromBytes(manifestBytes)
	manifestHash := manifestDigest.Encoded()
	if err := os.WriteFile(filepath.Join(blobDir, manifestHash), manifestBytes, 0644); err != nil {
		t.Fatal(err)
	}

	store := NewContainerdStore(root)
	return store, root
}

func TestContainerdStore_Init_ScanContentStore(t *testing.T) {
	store, _ := setupContainerdTestStore(t)
	ctx := context.Background()

	if err := store.Init(ctx); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	// Should have found the blobs
	if len(store.blobs) < 2 {
		t.Errorf("expected at least 2 blobs, got %d", len(store.blobs))
	}
}

func TestContainerdStore_HasBlob(t *testing.T) {
	store, root := setupContainerdTestStore(t)
	ctx := context.Background()

	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}

	// Find a blob that exists
	blobDir := filepath.Join(root, "io.containerd.content.v1.content", "blobs", "sha256")
	entries, _ := os.ReadDir(blobDir)
	if len(entries) == 0 {
		t.Fatal("no blobs found")
	}

	dgst, _ := digest.Parse("sha256:" + entries[0].Name())
	if !store.HasBlob(ctx, dgst) {
		t.Errorf("HasBlob(%s) = false, want true", dgst)
	}

	// Non-existent blob
	fakeDgst, _ := digest.Parse("sha256:0000000000000000000000000000000000000000000000000000000000000000")
	if store.HasBlob(ctx, fakeDgst) {
		t.Errorf("HasBlob(fake) = true, want false")
	}
}

func TestContainerdStore_GetBlob(t *testing.T) {
	store, _ := setupContainerdTestStore(t)
	ctx := context.Background()

	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}

	// Get a known blob
	configContent := `{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":["sha256:aaaa"]}}`
	configDigest := digest.FromBytes([]byte(configContent))

	reader, size, err := store.GetBlob(ctx, configDigest)
	if err != nil {
		t.Fatalf("GetBlob() error = %v", err)
	}
	defer reader.Close()

	if size != int64(len(configContent)) {
		t.Errorf("GetBlob() size = %d, want %d", size, len(configContent))
	}
}

func TestContainerdStore_GetManifest(t *testing.T) {
	store, _ := setupContainerdTestStore(t)
	ctx := context.Background()

	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}

	// Build the manifest to get its digest
	configContent := `{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":["sha256:aaaa"]}}`
	configDigest := digest.FromBytes([]byte(configContent))
	manifest := map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.docker.distribution.manifest.v2+json",
		"config": map[string]any{
			"mediaType": "application/vnd.docker.container.image.v1+json",
			"digest":    configDigest.String(),
			"size":      len(configContent),
		},
		"layers": []map[string]any{},
	}
	manifestBytes, _ := json.Marshal(manifest)
	manifestDigest := digest.FromBytes(manifestBytes)

	data, mediaType, err := store.GetManifest(ctx, manifestDigest)
	if err != nil {
		t.Fatalf("GetManifest() error = %v", err)
	}
	if len(data) == 0 {
		t.Error("GetManifest() returned empty data")
	}
	if mediaType == "" {
		t.Error("GetManifest() returned empty mediaType")
	}
}

func TestContainerdStore_ResolveDockerIONamespaces(t *testing.T) {
	pauseDigest := digest.FromBytes([]byte("pause manifest"))
	multiLevelDigest := digest.FromBytes([]byte("multi-level manifest"))

	store := NewContainerdStore(t.TempDir())
	store.tags["docker.io/rancher/mirrored-pause:3.6"] = pauseDigest
	store.tags["docker.io/acme/team/image:1.0"] = multiLevelDigest

	tests := []struct {
		ref  string
		want digest.Digest
	}{
		{
			ref:  "rancher/mirrored-pause:3.6",
			want: pauseDigest,
		},
		{
			ref:  "docker.io/rancher/mirrored-pause:3.6",
			want: pauseDigest,
		},
		{
			ref:  "acme/team/image:1.0",
			want: multiLevelDigest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.ref, func(t *testing.T) {
			got, err := store.Resolve(context.Background(), tt.ref)
			if err != nil {
				t.Fatalf("Resolve(%q) error = %v", tt.ref, err)
			}
			if got != tt.want {
				t.Fatalf("Resolve(%q) = %s, want %s", tt.ref, got, tt.want)
			}
		})
	}
}

func TestContainerdStore_Subscribe(t *testing.T) {
	store, root := setupContainerdTestStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}

	ch, err := store.Subscribe(ctx)
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}

	// Create a new blob
	blobDir := filepath.Join(root, "io.containerd.content.v1.content", "blobs", "sha256")
	newContent := []byte("new blob content for testing")
	newDigest := digest.FromBytes(newContent)
	newHash := newDigest.Encoded()

	// Small delay to let watcher start
	time.Sleep(100 * time.Millisecond)

	if err := os.WriteFile(filepath.Join(blobDir, newHash), newContent, 0644); err != nil {
		t.Fatal(err)
	}

	// Wait for event (with timeout)
	select {
	case event := <-ch:
		if event.Type != EventCreate {
			t.Errorf("expected EventCreate, got %d", event.Type)
		}
		if event.Digest != newDigest {
			t.Errorf("expected digest %s, got %s", newDigest, event.Digest)
		}
	case <-time.After(3 * time.Second):
		t.Error("timeout waiting for event")
	}
}

// TestContainerdStore_Subscribe_WatchAddError exercises the watcher.Add error path
// in Subscribe when the blob directory does not exist.
func TestContainerdStore_Subscribe_WatchAddError(t *testing.T) {
	// Root with no blob dir — Subscribe should fail on watcher.Add.
	root := t.TempDir()
	store := NewContainerdStore(root)

	_, err := store.Subscribe(context.Background())
	if err == nil {
		t.Fatal("expected error when blob dir does not exist")
	}
}

// TestContainerdStore_Subscribe_EventDelete exercises the EventDelete path in the
// Subscribe debounce goroutine by removing a blob file after subscribing.
func TestContainerdStore_Subscribe_EventDelete(t *testing.T) {
	store, root := setupContainerdTestStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}

	blobDir := filepath.Join(root, "io.containerd.content.v1.content", "blobs", "sha256")

	// Find a blob file to delete.
	entries, err := os.ReadDir(blobDir)
	if err != nil || len(entries) == 0 {
		t.Skip("no blobs to delete")
	}
	blobFile := filepath.Join(blobDir, entries[0].Name())

	ch, err := store.Subscribe(ctx)
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}

	// Short delay to let watcher start.
	time.Sleep(100 * time.Millisecond)

	if err := os.Remove(blobFile); err != nil {
		t.Fatal(err)
	}

	// Wait for a Delete event.
	select {
	case event, ok := <-ch:
		if !ok {
			t.Error("channel closed unexpectedly")
		}
		if event.Type != EventDelete {
			t.Errorf("expected EventDelete, got %v", event.Type)
		}
	case <-time.After(3 * time.Second):
		t.Error("timeout waiting for EventDelete")
	}
}

// TestContainerdStore_Subscribe_ContextCancel verifies that the Subscribe goroutine
// exits cleanly when the context is cancelled (exercises the <-ctx.Done() return).
func TestContainerdStore_Subscribe_ContextCancel(t *testing.T) {
	store, _ := setupContainerdTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())

	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}

	ch, err := store.Subscribe(ctx)
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}

	// Cancel immediately.
	cancel()

	// The channel should be closed after the goroutine exits.
	select {
	case _, ok := <-ch:
		if ok {
			// Might have received a buffered event; drain and wait for close.
			select {
			case <-ch:
			case <-time.After(2 * time.Second):
				t.Error("channel not closed after context cancel")
			}
		}
	case <-time.After(2 * time.Second):
		t.Error("channel not closed after context cancel within 2s")
	}
}

func TestContainerdStore_ListImages_Empty(t *testing.T) {
	root := t.TempDir()
	blobDir := filepath.Join(root, "io.containerd.content.v1.content", "blobs", "sha256")
	if err := os.MkdirAll(blobDir, 0755); err != nil {
		t.Fatal(err)
	}

	store := NewContainerdStore(root)
	ctx := context.Background()

	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}

	images, err := store.ListImages(ctx)
	if err != nil {
		t.Fatalf("ListImages() error = %v", err)
	}
	if len(images) != 0 {
		t.Errorf("ListImages() returned %d images, want 0", len(images))
	}
}

func TestNormalizeImageRef(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"docker.io/library/nginx:latest", "docker.io/library/nginx:latest"},
		{"docker.io/library/nginx@sha256:abc123", "docker.io/library/nginx"},
		{"registry.k8s.io/pause:3.9", "registry.k8s.io/pause:3.9"},
	}
	for _, tt := range tests {
		if got := normalizeImageRef(tt.input); got != tt.want {
			t.Errorf("normalizeImageRef(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestDetectMediaType(t *testing.T) {
	tests := []struct {
		name string
		data []byte
		want string
	}{
		{
			"explicit mediaType",
			[]byte(`{"schemaVersion":2,"mediaType":"application/vnd.docker.distribution.manifest.v2+json"}`),
			"application/vnd.docker.distribution.manifest.v2+json",
		},
		{
			"OCI manifest config",
			[]byte(`{"schemaVersion":2,"config":{"mediaType":"application/vnd.oci.image.config.v1+json"}}`),
			"application/vnd.oci.image.manifest.v1+json",
		},
		{
			"Docker manifest config",
			[]byte(`{"schemaVersion":2,"config":{"mediaType":"application/vnd.docker.container.image.v1+json"}}`),
			"application/vnd.docker.distribution.manifest.v2+json",
		},
		{
			"image config with rootfs",
			[]byte(`{"architecture":"amd64","rootfs":{"type":"layers"}}`),
			"application/vnd.docker.container.image.v1+json",
		},
		{
			"unknown content",
			[]byte(`not json`),
			"application/octet-stream",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := detectMediaType(tt.data); got != tt.want {
				t.Errorf("detectMediaType() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestContainerdStore_Init_ScanContentStoreError exercises the Init error path when
// scanContentStore fails. Creating the sha256 blob dir as a file makes os.ReadDir return
// ENOTDIR (not IsNotExist), so scanContentStore propagates the error to Init.
func TestContainerdStore_Init_ScanContentStoreError(t *testing.T) {
	root := t.TempDir()
	blobDirParent := filepath.Join(root, "io.containerd.content.v1.content", "blobs")
	if err := os.MkdirAll(blobDirParent, 0755); err != nil {
		t.Fatal(err)
	}
	// sha256 is a file, not a directory → os.ReadDir returns "not a directory"
	if err := os.WriteFile(filepath.Join(blobDirParent, "sha256"), []byte("not-a-dir"), 0644); err != nil {
		t.Fatal(err)
	}

	store := NewContainerdStore(root)
	if err := store.Init(context.Background()); err == nil {
		t.Fatal("expected error when blob sha256 dir is a regular file")
	}
}

// TestContainerdStore_BuildImageIndex_WithTags exercises the inner loop body of
// buildImageIndex by pre-populating cs.tags and cs.blobs before calling it.
func TestContainerdStore_BuildImageIndex_WithTags(t *testing.T) {
	store, _ := setupContainerdTestStore(t)
	ctx := context.Background()
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}

	// Pick a blob from the initialized store.
	store.mu.RLock()
	var blobDgst digest.Digest
	var blobPath string
	for d, p := range store.blobs {
		blobDgst = d
		blobPath = p
		break
	}
	store.mu.RUnlock()

	if blobDgst == "" {
		t.Skip("no blobs found in test store")
	}

	// Directly set a tag and a blob so buildImageIndex processes both inner branches.
	store.mu.Lock()
	store.tags["myimage:v1"] = blobDgst
	store.blobs[blobDgst] = blobPath
	store.mu.Unlock()

	store.buildImageIndex()

	images, _ := store.ListImages(ctx)
	var found bool
	for _, img := range images {
		if img.Digest == blobDgst {
			found = true
			if img.Size <= 0 {
				t.Errorf("expected positive size for image, got %d", img.Size)
			}
			break
		}
	}
	if !found {
		t.Errorf("expected image with digest %s in results", blobDgst)
	}
}

// TestContainerdTagCandidates_InvalidRef verifies the error path when the reference
// has no colon (invalid).
func TestContainerdTagCandidates_InvalidRef(t *testing.T) {
	_, err := containerdTagCandidates("no-colon-here")
	if err == nil {
		t.Fatal("expected error for reference with no colon")
	}
}

// TestContainerdTagCandidates_DockerIOLibrary verifies the docker.io/library/ prefix
// strip path in containerdTagCandidates.
func TestContainerdTagCandidates_DockerIOLibrary(t *testing.T) {
	candidates, err := containerdTagCandidates("docker.io/library/nginx:latest")
	if err != nil {
		t.Fatal(err)
	}
	// Should include "docker.io/library/nginx:latest", "library/nginx:latest", "nginx:latest"
	found := false
	for _, c := range candidates {
		if c == "nginx:latest" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected 'nginx:latest' in candidates, got %v", candidates)
	}
}

// TestHasRegistryHost_Localhost verifies that "localhost" in the first component of
// a name is treated as a registry host.
func TestHasRegistryHost_Localhost(t *testing.T) {
	if !hasRegistryHost("localhost/myimage") {
		t.Error("localhost/myimage should be treated as having a registry host")
	}
	if hasRegistryHost("myimage") {
		t.Error("bare name should not be treated as having a registry host")
	}
}

// TestContainerdStore_GetManifest_ReadError exercises the os.ReadFile error path in
// GetManifest by injecting a path into cs.blobs that points to a non-existent file.
func TestContainerdStore_GetManifest_ReadError(t *testing.T) {
	store, _ := setupContainerdTestStore(t)
	if err := store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Inject a blob path that doesn't exist on disk.
	ghost := digest.FromString("nonexistent-blob")
	store.mu.Lock()
	store.blobs[ghost] = "/nonexistent/path/to/blob"
	store.mu.Unlock()

	_, _, err := store.GetManifest(context.Background(), ghost)
	if err == nil {
		t.Fatal("expected error when manifest file does not exist")
	}
}

// TestContainerdStore_Resolve_InvalidRef exercises the error path in Resolve when the
// reference has no colon (containerdTagCandidates returns an error).
func TestContainerdStore_Resolve_InvalidRef(t *testing.T) {
	store, _ := setupContainerdTestStore(t)
	if err := store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}

	_, err := store.Resolve(context.Background(), "no-colon-here")
	if err == nil {
		t.Fatal("expected error for reference with no colon")
	}
}

// TestContainerdStore_ScanContentStore_NotExist covers the os.IsNotExist → return nil
// path in scanContentStore when the blob directory has never been created.
func TestContainerdStore_ScanContentStore_NotExist(t *testing.T) {
	root := t.TempDir() // no content-store subdirectory created
	store := NewContainerdStore(root)
	if err := store.scanContentStore(); err != nil {
		t.Fatalf("expected nil when blob dir does not exist, got: %v", err)
	}
}

// TestContainerdStore_ScanContentStore_SkipsDir covers the `entry.IsDir() → continue`
// path in scanContentStore when a subdirectory exists inside the blob dir.
func TestContainerdStore_ScanContentStore_SkipsDir(t *testing.T) {
	root := t.TempDir()
	blobDir := filepath.Join(root, "io.containerd.content.v1.content", "blobs", "sha256")
	if err := os.MkdirAll(blobDir, 0755); err != nil {
		t.Fatal(err)
	}
	// A subdirectory inside blobDir — scanContentStore must skip it.
	if err := os.Mkdir(filepath.Join(blobDir, "a_subdir"), 0755); err != nil {
		t.Fatal(err)
	}
	// One valid blob file.
	validContent := []byte("scan-skip-dir-test")
	dgst := digest.FromBytes(validContent)
	if err := os.WriteFile(filepath.Join(blobDir, dgst.Encoded()), validContent, 0644); err != nil {
		t.Fatal(err)
	}

	store := NewContainerdStore(root)
	if err := store.scanContentStore(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(store.blobs) != 1 {
		t.Errorf("expected 1 blob after skipping dir, got %d", len(store.blobs))
	}
}

// TestContainerdStore_ScanContentStore_SkipsInvalidName covers the
// `digest.Parse error → continue` path in scanContentStore when a blob file's
// name is not a valid sha256 hex hash.
func TestContainerdStore_ScanContentStore_SkipsInvalidName(t *testing.T) {
	root := t.TempDir()
	blobDir := filepath.Join(root, "io.containerd.content.v1.content", "blobs", "sha256")
	if err := os.MkdirAll(blobDir, 0755); err != nil {
		t.Fatal(err)
	}
	// A file with an invalid name (not a sha256 hex hash).
	if err := os.WriteFile(filepath.Join(blobDir, "not-a-valid-sha256-name"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	// One valid blob file.
	validContent := []byte("scan-skip-invalid-name-test")
	dgst := digest.FromBytes(validContent)
	if err := os.WriteFile(filepath.Join(blobDir, dgst.Encoded()), validContent, 0644); err != nil {
		t.Fatal(err)
	}

	store := NewContainerdStore(root)
	if err := store.scanContentStore(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(store.blobs) != 1 {
		t.Errorf("expected 1 blob (invalid name skipped), got %d", len(store.blobs))
	}
}

// TestContainerdStore_ScanContentStore_FileAsDir exercises the non-IsNotExist error
// path in scanContentStore by making the sha256 blob directory path a regular file.
func TestContainerdStore_ScanContentStore_FileAsDir(t *testing.T) {
	root := t.TempDir()
	blobDirParent := filepath.Join(root, "io.containerd.content.v1.content", "blobs")
	if err := os.MkdirAll(blobDirParent, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blobDirParent, "sha256"), []byte("not-a-dir"), 0644); err != nil {
		t.Fatal(err)
	}

	store := NewContainerdStore(root)
	err := store.scanContentStore()
	if err == nil {
		t.Fatal("expected error when sha256 path is a regular file")
	}
}

// TestContainerdStore_GetBlob_OpenError exercises the os.Open error path in GetBlob
// by injecting a non-existent path into cs.blobs.
func TestContainerdStore_GetBlob_OpenError(t *testing.T) {
	store, _ := setupContainerdTestStore(t)
	if err := store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Inject a blob entry pointing to a file that does not exist.
	ghost := digest.FromString("ghost-blob-open-error")
	store.mu.Lock()
	store.blobs[ghost] = "/nonexistent/path/to/blob-file"
	store.mu.Unlock()

	_, _, err := store.GetBlob(context.Background(), ghost)
	if err == nil {
		t.Fatal("expected error when blob file does not exist")
	}
}

// TestContainerdTagCandidates_LibraryPrefix exercises the "library/" prefix strip path
// in containerdTagCandidates when the reference has no registry host but starts with
// "library/" (e.g. "library/nginx:latest").
func TestContainerdTagCandidates_LibraryPrefix(t *testing.T) {
	candidates, err := containerdTagCandidates("library/nginx:latest")
	if err != nil {
		t.Fatal(err)
	}
	// Should include "library/nginx:latest", "docker.io/library/nginx:latest", "nginx:latest"
	wantSuffix := "nginx:latest"
	found := false
	for _, c := range candidates {
		if c == wantSuffix {
			found = true
		}
	}
	if !found {
		t.Errorf("expected %q in candidates, got %v", wantSuffix, candidates)
	}
}

// TestContainerdStore_Resolve_LibraryPrefix exercises the Resolve path for a reference
// like "library/nginx:latest" that should map to a stored "docker.io/library/nginx:latest" tag.
func TestContainerdStore_Resolve_LibraryPrefix(t *testing.T) {
	store, _ := setupContainerdTestStore(t)
	if err := store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Inject a tag under the docker.io/library/ namespace.
	testDgst := digest.FromString("library-test")
	store.mu.Lock()
	store.tags["docker.io/library/busybox:latest"] = testDgst
	store.mu.Unlock()

	// Resolve via "library/busybox:latest" — should find the docker.io/library/ tag.
	got, err := store.Resolve(context.Background(), "library/busybox:latest")
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if got != testDgst {
		t.Errorf("Resolve() = %s, want %s", got, testDgst)
	}
}

// buildFakeMetaDB creates a containerd-style bolt metadata DB at dbPath with
// the given image entries under namespace "default".
func buildFakeMetaDB(t *testing.T, dbPath string, images map[string]digest.Digest) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(dbPath), 0755); err != nil {
		t.Fatal(err)
	}
	db, err := bolt.Open(dbPath, 0600, &bolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	err = db.Update(func(tx *bolt.Tx) error {
		v1, err := tx.CreateBucketIfNotExists([]byte("v1"))
		if err != nil {
			return err
		}
		ns, err := v1.CreateBucketIfNotExists([]byte("default"))
		if err != nil {
			return err
		}
		imgsBucket, err := ns.CreateBucketIfNotExists([]byte("images"))
		if err != nil {
			return err
		}
		for name, dgst := range images {
			imgBucket, err := imgsBucket.CreateBucketIfNotExists([]byte(name))
			if err != nil {
				return err
			}
			targetBucket, err := imgBucket.CreateBucketIfNotExists([]byte("target"))
			if err != nil {
				return err
			}
			if err := targetBucket.Put([]byte("digest"), []byte(dgst.String())); err != nil {
				return err
			}
			if err := targetBucket.Put([]byte("mediatype"), []byte("application/vnd.docker.distribution.manifest.v2+json")); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestContainerdStore_LoadMetadataDB_Success exercises the main success path of
// loadMetadataDB by building a bolt DB with the containerd metadata structure.
func TestContainerdStore_LoadMetadataDB_Success(t *testing.T) {
	root := t.TempDir()

	// Create the content store blob dir (required for Init).
	blobDir := filepath.Join(root, "io.containerd.content.v1.content", "blobs", "sha256")
	if err := os.MkdirAll(blobDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Create the bolt metadata DB.
	dgst := digest.FromString("test-image-data")
	dbPath := filepath.Join(root, "io.containerd.metadata.v1.bolt", "meta.db")
	buildFakeMetaDB(t, dbPath, map[string]digest.Digest{
		"docker.io/library/nginx:latest": dgst,
		"docker.io/library/alpine:3.18":  digest.FromString("alpine-data"),
	})

	store := NewContainerdStore(root)
	if err := store.Init(context.Background()); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	store.mu.RLock()
	tagCount := len(store.tags)
	store.mu.RUnlock()

	if tagCount < 2 {
		t.Errorf("expected at least 2 tags loaded from metadata DB, got %d", tagCount)
	}

	// Verify a known tag is resolvable.
	got, err := store.Resolve(context.Background(), "docker.io/library/nginx:latest")
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if got != dgst {
		t.Errorf("Resolve() = %s, want %s", got, dgst)
	}
}

// TestContainerdStore_LoadMetadataDB_EmptyV1 exercises the `if v1 == nil { return nil }`
// branch by creating a bolt DB with no "v1" bucket.
func TestContainerdStore_LoadMetadataDB_EmptyV1(t *testing.T) {
	root := t.TempDir()

	blobDir := filepath.Join(root, "io.containerd.content.v1.content", "blobs", "sha256")
	if err := os.MkdirAll(blobDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Create an empty bolt DB (no v1 bucket).
	dbPath := filepath.Join(root, "io.containerd.metadata.v1.bolt", "meta.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0755); err != nil {
		t.Fatal(err)
	}
	emptyDB, err := bolt.Open(dbPath, 0600, &bolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	emptyDB.Close()

	store := NewContainerdStore(root)
	if err := store.Init(context.Background()); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	store.mu.RLock()
	tagCount := len(store.tags)
	store.mu.RUnlock()

	if tagCount != 0 {
		t.Errorf("expected 0 tags for empty DB, got %d", tagCount)
	}
}

// TestContainerdStore_LoadMetadataDB_NoImagesOrTargetBuckets exercises the nil-bucket
// guard branches (nsBucket, imagesBucket, imageBucket, targetBucket checks).
func TestContainerdStore_LoadMetadataDB_NoImagesOrTargetBuckets(t *testing.T) {
	root := t.TempDir()

	blobDir := filepath.Join(root, "io.containerd.content.v1.content", "blobs", "sha256")
	if err := os.MkdirAll(blobDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Build a DB that has v1/default but NO "images" bucket.
	dbPath := filepath.Join(root, "io.containerd.metadata.v1.bolt", "meta.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0755); err != nil {
		t.Fatal(err)
	}
	db, err := bolt.Open(dbPath, 0600, &bolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		v1, _ := tx.CreateBucketIfNotExists([]byte("v1"))
		// Create namespace but no "images" bucket inside it.
		v1.CreateBucketIfNotExists([]byte("default"))
		return nil
	}); err != nil {
		db.Close()
		t.Fatal(err)
	}
	db.Close()

	store := NewContainerdStore(root)
	if err := store.Init(context.Background()); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	store.mu.RLock()
	tagCount := len(store.tags)
	store.mu.RUnlock()

	if tagCount != 0 {
		t.Errorf("expected 0 tags when no images bucket, got %d", tagCount)
	}
}

// TestContainerdStore_LoadMetadataDB_BadDigest exercises the `digest.Parse` error
// branch by inserting a malformed digest into the target bucket.
func TestContainerdStore_LoadMetadataDB_BadDigest(t *testing.T) {
	root := t.TempDir()

	blobDir := filepath.Join(root, "io.containerd.content.v1.content", "blobs", "sha256")
	if err := os.MkdirAll(blobDir, 0755); err != nil {
		t.Fatal(err)
	}

	dbPath := filepath.Join(root, "io.containerd.metadata.v1.bolt", "meta.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0755); err != nil {
		t.Fatal(err)
	}
	db, err := bolt.Open(dbPath, 0600, &bolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		v1, _ := tx.CreateBucketIfNotExists([]byte("v1"))
		ns, _ := v1.CreateBucketIfNotExists([]byte("default"))
		imgs, _ := ns.CreateBucketIfNotExists([]byte("images"))
		imgBucket, _ := imgs.CreateBucketIfNotExists([]byte("docker.io/library/nginx:latest"))
		target, _ := imgBucket.CreateBucketIfNotExists([]byte("target"))
		// Insert a malformed digest.
		target.Put([]byte("digest"), []byte("not-a-valid-digest"))
		return nil
	}); err != nil {
		db.Close()
		t.Fatal(err)
	}
	db.Close()

	store := NewContainerdStore(root)
	if err := store.Init(context.Background()); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	// With a bad digest, the image should be skipped (tags = 0).
	store.mu.RLock()
	tagCount := len(store.tags)
	store.mu.RUnlock()

	if tagCount != 0 {
		t.Errorf("expected 0 tags with bad digest, got %d", tagCount)
	}
}

// TestContainerdStore_LoadMetadataDB_NoTargetBucket exercises the `targetBucket == nil`
// branch by creating an image bucket with no "target" sub-bucket.
func TestContainerdStore_LoadMetadataDB_NoTargetBucket(t *testing.T) {
	root := t.TempDir()

	blobDir := filepath.Join(root, "io.containerd.content.v1.content", "blobs", "sha256")
	if err := os.MkdirAll(blobDir, 0755); err != nil {
		t.Fatal(err)
	}

	dbPath := filepath.Join(root, "io.containerd.metadata.v1.bolt", "meta.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0755); err != nil {
		t.Fatal(err)
	}
	db, err := bolt.Open(dbPath, 0600, &bolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		v1, _ := tx.CreateBucketIfNotExists([]byte("v1"))
		ns, _ := v1.CreateBucketIfNotExists([]byte("default"))
		imgs, _ := ns.CreateBucketIfNotExists([]byte("images"))
		// Create an image bucket but do NOT create a "target" sub-bucket.
		imgs.CreateBucketIfNotExists([]byte("docker.io/library/nginx:latest"))
		return nil
	}); err != nil {
		db.Close()
		t.Fatal(err)
	}
	db.Close()

	store := NewContainerdStore(root)
	if err := store.Init(context.Background()); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	store.mu.RLock()
	tagCount := len(store.tags)
	store.mu.RUnlock()

	if tagCount != 0 {
		t.Errorf("expected 0 tags when no target bucket, got %d", tagCount)
	}
}

// TestContainerdStore_LoadMetadataDB_NoDigestKey exercises the `digestBytes == nil`
// branch by creating a target bucket without a "digest" key.
func TestContainerdStore_LoadMetadataDB_NoDigestKey(t *testing.T) {
	root := t.TempDir()

	blobDir := filepath.Join(root, "io.containerd.content.v1.content", "blobs", "sha256")
	if err := os.MkdirAll(blobDir, 0755); err != nil {
		t.Fatal(err)
	}

	dbPath := filepath.Join(root, "io.containerd.metadata.v1.bolt", "meta.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0755); err != nil {
		t.Fatal(err)
	}
	db, err := bolt.Open(dbPath, 0600, &bolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		v1, _ := tx.CreateBucketIfNotExists([]byte("v1"))
		ns, _ := v1.CreateBucketIfNotExists([]byte("default"))
		imgs, _ := ns.CreateBucketIfNotExists([]byte("images"))
		imgBucket, _ := imgs.CreateBucketIfNotExists([]byte("docker.io/library/nginx:latest"))
		target, _ := imgBucket.CreateBucketIfNotExists([]byte("target"))
		// Put mediatype but NOT digest.
		target.Put([]byte("mediatype"), []byte("application/vnd.docker.distribution.manifest.v2+json"))
		return nil
	}); err != nil {
		db.Close()
		t.Fatal(err)
	}
	db.Close()

	store := NewContainerdStore(root)
	if err := store.Init(context.Background()); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	store.mu.RLock()
	tagCount := len(store.tags)
	store.mu.RUnlock()

	if tagCount != 0 {
		t.Errorf("expected 0 tags when no digest key, got %d", tagCount)
	}
}

// TestContainerdStore_LoadMetadataDB_PlainImageKey exercises the `imageBucket == nil`
// guard by inserting a plain key-value pair in imagesBucket (not a nested bucket).
func TestContainerdStore_LoadMetadataDB_PlainImageKey(t *testing.T) {
	root := t.TempDir()

	blobDir := filepath.Join(root, "io.containerd.content.v1.content", "blobs", "sha256")
	if err := os.MkdirAll(blobDir, 0755); err != nil {
		t.Fatal(err)
	}

	dbPath := filepath.Join(root, "io.containerd.metadata.v1.bolt", "meta.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0755); err != nil {
		t.Fatal(err)
	}
	db, err := bolt.Open(dbPath, 0600, &bolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		v1, _ := tx.CreateBucketIfNotExists([]byte("v1"))
		ns, _ := v1.CreateBucketIfNotExists([]byte("default"))
		imgs, _ := ns.CreateBucketIfNotExists([]byte("images"))
		// Insert a plain key-value pair instead of a nested bucket.
		// Bucket(key) returns nil for plain values so imageBucket == nil.
		imgs.Put([]byte("plain-entry"), []byte("not-a-bucket"))
		return nil
	}); err != nil {
		db.Close()
		t.Fatal(err)
	}
	db.Close()

	store := NewContainerdStore(root)
	if err := store.Init(context.Background()); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	store.mu.RLock()
	tagCount := len(store.tags)
	store.mu.RUnlock()

	if tagCount != 0 {
		t.Errorf("expected 0 tags for plain-key entry, got %d", tagCount)
	}
}

// TestContainerdStore_LoadMetadataDB_CorruptDB exercises the bolt.Open error path
// by providing a file that is not a valid bolt database.
func TestContainerdStore_LoadMetadataDB_CorruptDB(t *testing.T) {
	root := t.TempDir()

	blobDir := filepath.Join(root, "io.containerd.content.v1.content", "blobs", "sha256")
	if err := os.MkdirAll(blobDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Write random bytes to the DB path — bolt should fail to open it.
	dbPath := filepath.Join(root, "io.containerd.metadata.v1.bolt", "meta.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dbPath, []byte("this is not a valid bolt database"), 0600); err != nil {
		t.Fatal(err)
	}

	store := NewContainerdStore(root)
	// Init should not fail even though loadMetadataDB fails (it only logs a warning).
	if err := store.Init(context.Background()); err != nil {
		t.Fatalf("Init() should not propagate loadMetadataDB error, got: %v", err)
	}
}

// TestContainerdStore_LoadMetadataDB_CreateTempFails exercises the os.CreateTemp error path
// in loadMetadataDB. Setting TMPDIR to a non-existent directory causes os.CreateTemp to fail.
func TestContainerdStore_LoadMetadataDB_CreateTempFails(t *testing.T) {
	root := t.TempDir()
	dbDir := filepath.Join(root, "io.containerd.metadata.v1.bolt")
	if err := os.MkdirAll(dbDir, 0755); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dbDir, "meta.db")
	if err := os.WriteFile(dbPath, []byte("dummy"), 0644); err != nil {
		t.Fatal(err)
	}

	// Point TMPDIR to a path that doesn't exist so os.CreateTemp("", ...) fails.
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "nonexistent-tmp"))

	store := NewContainerdStore(root)
	if err := store.loadMetadataDB(); err == nil {
		t.Fatal("expected error when os.CreateTemp fails due to bad TMPDIR")
	}
}

// TestContainerdStore_LoadMetadataDB_CopyError exercises the io.Copy error path in
// loadMetadataDB by making meta.db a directory: os.Stat succeeds, os.Open succeeds
// (returns a dir fd on Linux), but io.Copy fails with EISDIR on the first Read.
func TestContainerdStore_LoadMetadataDB_CopyError(t *testing.T) {
	root := t.TempDir()
	blobDir := filepath.Join(root, "io.containerd.content.v1.content", "blobs", "sha256")
	if err := os.MkdirAll(blobDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Create the meta.db PATH as a directory so io.Copy(tmp, dirFd) fails with EISDIR.
	dbDir := filepath.Join(root, "io.containerd.metadata.v1.bolt", "meta.db")
	if err := os.MkdirAll(dbDir, 0755); err != nil {
		t.Fatal(err)
	}

	store := NewContainerdStore(root)
	if err := store.loadMetadataDB(); err == nil {
		t.Fatal("expected error when meta.db is a directory (io.Copy should fail)")
	}
}

// TestContainerdStore_LoadMetadataDB_V1KeyValue exercises the `nsBucket == nil → return nil`
// path in loadMetadataDB when the v1 bucket contains a regular key-value pair (not a
// sub-bucket). Bolt's ForEach iterates over all entries including non-bucket keys; calling
// Bucket() on such a key returns nil.
func TestContainerdStore_LoadMetadataDB_V1KeyValue(t *testing.T) {
	root := t.TempDir()
	blobDir := filepath.Join(root, "io.containerd.content.v1.content", "blobs", "sha256")
	if err := os.MkdirAll(blobDir, 0755); err != nil {
		t.Fatal(err)
	}

	dbPath := filepath.Join(root, "io.containerd.metadata.v1.bolt", "meta.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0755); err != nil {
		t.Fatal(err)
	}
	db, err := bolt.Open(dbPath, 0600, &bolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	err = db.Update(func(tx *bolt.Tx) error {
		v1, err := tx.CreateBucketIfNotExists([]byte("v1"))
		if err != nil {
			return err
		}
		// A regular KV entry in v1 (not a sub-bucket) → v1.Bucket("not-ns") == nil.
		return v1.Put([]byte("not-a-namespace"), []byte("some-value"))
	})
	if err != nil {
		t.Fatal(err)
	}
	db.Close()

	store := NewContainerdStore(root)
	if err := store.Init(context.Background()); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
}

// TestDetectMediaType_OctetStream exercises the fallback "application/octet-stream"
// path when the content is valid JSON but has no recognizable structure.
func TestDetectMediaType_OctetStream(t *testing.T) {
	// Valid JSON object with no schemaVersion, mediaType, config, or rootfs fields.
	data := []byte(`{"foo":"bar","baz":42}`)
	got := detectMediaType(data)
	if got != "application/octet-stream" {
		t.Errorf("detectMediaType() = %q, want %q", got, "application/octet-stream")
	}
}

// TestContainerdStore_GetManifest_NotFound covers the "not in blobs map" error path
// in GetManifest when the requested digest has never been indexed.
func TestContainerdStore_GetManifest_NotFound(t *testing.T) {
	store := NewContainerdStore(t.TempDir())
	unknownDigest := digest.FromString("not-in-blobs")
	_, _, err := store.GetManifest(context.Background(), unknownDigest)
	if err == nil {
		t.Fatal("expected error for unknown manifest digest")
	}
}

// TestContainerdStore_GetBlob_NotFound covers the "not in blobs map" error path
// in GetBlob when the requested digest has never been indexed.
func TestContainerdStore_GetBlob_NotFound(t *testing.T) {
	store := NewContainerdStore(t.TempDir())
	unknownDigest := digest.FromString("not-in-blobs")
	_, _, err := store.GetBlob(context.Background(), unknownDigest)
	if err == nil {
		t.Fatal("expected error for unknown blob digest")
	}
}

// TestContainerdStore_Resolve_NotFound covers the "all candidates miss" error path
// in Resolve when the reference has valid format but doesn't exist in the tags map.
func TestContainerdStore_Resolve_NotFound(t *testing.T) {
	store := NewContainerdStore(t.TempDir())
	// "missing:image" is a valid ref (has colon) but won't be in any tags.
	_, err := store.Resolve(context.Background(), "missing:image")
	if err == nil {
		t.Fatal("expected error when tag not found in store")
	}
}

// TestContainerdTagCandidates_BareImage covers the "docker.io/library/" candidate
// generation path in containerdTagCandidates for a bare image name (no slash,
// no registry host). Example: "nginx:latest" → adds "docker.io/library/nginx:latest".
func TestContainerdTagCandidates_BareImage(t *testing.T) {
	candidates, err := containerdTagCandidates("nginx:latest")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Expect "docker.io/library/nginx:latest" to be generated.
	wantCandidate := "docker.io/library/nginx:latest"
	found := false
	for _, c := range candidates {
		if c == wantCandidate {
			found = true
		}
	}
	if !found {
		t.Errorf("expected %q in candidates for bare image name, got %v", wantCandidate, candidates)
	}
}

// TestContainerdStore_Subscribe_WriteEvent exercises the "else { continue }" path in the
// Subscribe debounce goroutine for non-Create/Remove fsnotify events (e.g., Write).
func TestContainerdStore_Subscribe_WriteEvent(t *testing.T) {
	store, root := setupContainerdTestStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}

	blobDir := filepath.Join(root, "io.containerd.content.v1.content", "blobs", "sha256")
	entries, err := os.ReadDir(blobDir)
	if err != nil || len(entries) == 0 {
		t.Skip("no blobs to modify")
	}
	blobFile := filepath.Join(blobDir, entries[0].Name())

	ch, err := store.Subscribe(ctx)
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}

	// Let the watcher start.
	time.Sleep(100 * time.Millisecond)

	// Write to an existing blob file — triggers a Write event (not Create/Remove).
	// The goroutine should hit the "else { continue }" path and not emit an event.
	// Then create a new blob to confirm the channel is still alive.
	if err := os.WriteFile(blobFile, []byte("updated content"), 0644); err != nil {
		t.Fatal(err)
	}

	// Wait a short time — Write events should be ignored (no channel send).
	time.Sleep(100 * time.Millisecond)

	// Now create a new blob to verify the goroutine is still running.
	newContent := []byte("new blob after write event")
	newDigest := digest.FromBytes(newContent)
	newHash := newDigest.Encoded()
	if err := os.WriteFile(filepath.Join(blobDir, newHash), newContent, 0644); err != nil {
		t.Fatal(err)
	}

	select {
	case event, ok := <-ch:
		if !ok {
			t.Fatal("channel closed unexpectedly")
		}
		if event.Type != EventCreate {
			t.Errorf("expected EventCreate after Write, got %v", event.Type)
		}
	case <-time.After(3 * time.Second):
		t.Error("timeout waiting for EventCreate after Write event")
	}
}

// TestContainerdStore_Subscribe_FlushInvalidDigest exercises the `continue` path inside
// the flush() closure in Subscribe. When a file with a name that is NOT a valid sha256 hex
// digest is created in the blob dir, it is queued in the pending map. When the debounce
// timer fires and flush() iterates over pending, digest.Parse("sha256:"+name) fails →
// the entry is skipped (continue). A subsequent valid blob creation confirms the goroutine
// is still alive after the skip.
func TestContainerdStore_Subscribe_FlushInvalidDigest(t *testing.T) {
	store, root := setupContainerdTestStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}

	blobDir := filepath.Join(root, "io.containerd.content.v1.content", "blobs", "sha256")

	ch, err := store.Subscribe(ctx)
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}

	// Let the watcher start.
	time.Sleep(100 * time.Millisecond)

	// Create a file whose name is not a valid sha256 hex string. The event handler will
	// queue it in pending; flush() will call digest.Parse("sha256:not-valid") → fail → continue.
	invalidPath := filepath.Join(blobDir, "not-a-valid-digest")
	if err := os.WriteFile(invalidPath, []byte("invalid"), 0644); err != nil {
		t.Fatal(err)
	}

	// Wait for the debounce delay (500ms) + buffer so flush() has been called.
	time.Sleep(700 * time.Millisecond)

	// No event should have been emitted for the invalid file.
	select {
	case ev, ok := <-ch:
		if ok {
			t.Errorf("unexpected event for invalid digest file: %v", ev)
		}
	default:
		// Good — no event emitted.
	}

	// Verify the goroutine is still alive by creating a valid blob.
	validContent := []byte("valid blob after flush invalid digest test")
	validDigest := digest.FromBytes(validContent)
	if err := os.WriteFile(filepath.Join(blobDir, validDigest.Encoded()), validContent, 0644); err != nil {
		t.Fatal(err)
	}

	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatal("channel closed unexpectedly")
		}
		if ev.Type != EventCreate {
			t.Errorf("expected EventCreate, got %v", ev.Type)
		}
		if ev.Digest != validDigest {
			t.Errorf("expected digest %s, got %s", validDigest, ev.Digest)
		}
	case <-time.After(5 * time.Second):
		t.Error("timeout: goroutine did not emit event for valid blob after invalid-digest flush")
	}
}

// TestContainerdStore_Subscribe_Debounce exercises the timer.Stop() path in the
// Subscribe debounce goroutine by sending two file events in rapid succession.
// The second event arrives before the debounce timer fires, causing timer.Stop().
func TestContainerdStore_Subscribe_Debounce(t *testing.T) {
	store, root := setupContainerdTestStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}

	blobDir := filepath.Join(root, "io.containerd.content.v1.content", "blobs", "sha256")

	ch, err := store.Subscribe(ctx)
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}

	// Let the watcher start.
	time.Sleep(100 * time.Millisecond)

	// Write two blobs in rapid succession — the second triggers timer.Stop().
	blob1 := []byte("debounce blob one")
	dgst1 := digest.FromBytes(blob1)
	if err := os.WriteFile(filepath.Join(blobDir, dgst1.Encoded()), blob1, 0644); err != nil {
		t.Fatal(err)
	}

	// Minimal sleep to ensure the first event is in-flight but timer hasn't fired.
	time.Sleep(10 * time.Millisecond)

	blob2 := []byte("debounce blob two")
	dgst2 := digest.FromBytes(blob2)
	if err := os.WriteFile(filepath.Join(blobDir, dgst2.Encoded()), blob2, 0644); err != nil {
		t.Fatal(err)
	}

	// Wait for both events to arrive (after debounce window).
	received := 0
	deadline := time.After(5 * time.Second)
	for received < 2 {
		select {
		case _, ok := <-ch:
			if !ok {
				t.Fatal("channel closed unexpectedly")
			}
			received++
		case <-deadline:
			t.Errorf("timeout waiting for debounced events; got %d/2", received)
			return
		}
	}
}

// TestContainerdStore_Subscribe_FlushCtxDone covers the ctx.Done() path inside the
// flush() closure in Subscribe. The output channel ch has capacity 16. By creating
// 17 valid blob files before the debounce timer fires (all land in pending), flush()
// fills ch with the first 16 events and then blocks on the 17th emit. Cancelling ctx
// causes the `case <-ctx.Done(): return` inside flush() to execute.
func TestContainerdStore_Subscribe_FlushCtxDone(t *testing.T) {
	store, root := setupContainerdTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}

	blobDir := filepath.Join(root, "io.containerd.content.v1.content", "blobs", "sha256")

	ch, err := store.Subscribe(ctx)
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}

	// Let the watcher start before creating files.
	time.Sleep(100 * time.Millisecond)

	// Create 17 files with valid sha256 hex names in rapid succession so that all
	// 17 land in the pending map before the 500ms debounce fires. ch has capacity
	// 16, so flush() will successfully emit items 1–16 into the buffer and then
	// block when trying to emit item 17.
	for i := 1; i <= 17; i++ {
		name := fmt.Sprintf("%064x", i)
		if err := os.WriteFile(filepath.Join(blobDir, name), []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	// Wait for the debounce timer to fire (500ms) plus some processing time.
	time.Sleep(700 * time.Millisecond)

	// Cancel context while the goroutine is blocked on the 17th emit. This exercises
	// the `case <-ctx.Done(): return` inside flush().
	cancel()

	// The goroutine should exit and close ch.
	select {
	case _, ok := <-ch:
		if ok {
			for range ch {
			}
		}
	case <-time.After(3 * time.Second):
		t.Error("channel not closed after context cancel in flush()")
	}
}

// TestContainerdStore_LoadMetadataDB_OpenError covers the os.Open(dbPath) error path
// in loadMetadataDB. When the DB file exists (so os.Stat passes) but is not readable
// (mode 000), os.Open returns EACCES and loadMetadataDB returns a wrapped error.
func TestContainerdStore_LoadMetadataDB_OpenError(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("skipped when running as root (root bypasses file permissions)")
	}

	root := t.TempDir()
	dbDir := filepath.Join(root, "io.containerd.metadata.v1.bolt")
	if err := os.MkdirAll(dbDir, 0755); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dbDir, "meta.db")
	if err := os.WriteFile(dbPath, []byte("dummy bolt db content"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dbPath, 0000); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dbPath, 0644) //nolint:errcheck // best-effort restore for TempDir cleanup

	store := NewContainerdStore(root)
	err := store.loadMetadataDB()
	if err == nil {
		t.Error("expected error when DB file is not readable")
	}
}

// TestContainerdStore_LoadMetadataDB_OpenError_Socket covers the os.Open(dbPath) error
// path in loadMetadataDB even when running as root. A Unix domain socket passes
// os.Stat but os.Open returns ENXIO, triggering the "open source DB" error branch.
// A short path is used to stay within the 108-character Unix socket path limit.
func TestContainerdStore_LoadMetadataDB_OpenError_Socket(t *testing.T) {
	// Use a short root so the socket path stays within the 108-char kernel limit.
	root, err := os.MkdirTemp("", "oi-cdb-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })

	dbDir := filepath.Join(root, "io.containerd.metadata.v1.bolt")
	if err := os.MkdirAll(dbDir, 0755); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dbDir, "meta.db")

	// A Unix socket passes os.Stat but os.Open returns ENXIO ("no such device or
	// address"), so this exercises the error path on all privilege levels.
	l, err := net.Listen("unix", dbPath)
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	defer l.Close()

	store := NewContainerdStore(root)
	if err := store.loadMetadataDB(); err == nil {
		t.Error("expected error when meta.db is a Unix socket (os.Open should fail)")
	}
}

// TestContainerdStore_Subscribe_NewWatcherError covers the newWatcher() error path
// in subscribe when the watcher constructor itself returns an error.
func TestContainerdStore_Subscribe_NewWatcherError(t *testing.T) {
	store, _ := setupContainerdTestStore(t)
	if err := store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}

	wantErr := &fakeWatcherError{}
	_, err := store.subscribe(context.Background(), func() (ociWatcher, error) {
		return nil, wantErr
	})
	if err == nil {
		t.Fatal("expected error when watcher constructor fails")
	}
}

// TestContainerdStore_Subscribe_WatcherError covers the watcher.Errors() channel path
// in subscribe. A fake watcher pushes an error and the goroutine logs and continues;
// context cancellation then causes a clean exit.
func TestContainerdStore_Subscribe_WatcherError(t *testing.T) {
	store, _ := setupContainerdTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}

	fw := newFakeOCIWatcher()
	ch, err := store.subscribe(ctx, func() (ociWatcher, error) { return fw, nil })
	if err != nil {
		t.Fatalf("subscribe error: %v", err)
	}

	// Push a watcher error to exercise the slog.Error path.
	fw.errs <- &fakeWatcherError{}

	// Give the goroutine time to process the error, then cancel.
	time.Sleep(50 * time.Millisecond)
	cancel()

	// Channel must be closed after goroutine exits.
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Error("timed out waiting for goroutine to exit after context cancel")
	}
}

// TestContainerdStore_GetBlob_StatError exercises the f.Stat() error path in GetBlob
// by injecting a fake osStatFile that returns an error for the opened file.
func TestContainerdStore_GetBlob_StatError(t *testing.T) {
	store, _ := setupContainerdTestStore(t)
	if err := store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Create a real file so os.Open succeeds, then inject an osStatFile that fails.
	f, err := os.CreateTemp(t.TempDir(), "blob-stat-error-*")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()

	ghost := digest.FromString("ghost-blob-stat-error")
	store.mu.Lock()
	store.blobs[ghost] = f.Name()
	store.mu.Unlock()

	wantErr := fmt.Errorf("injected stat error")
	orig := osStatFile
	osStatFile = func(*os.File) (os.FileInfo, error) { return nil, wantErr }
	defer func() { osStatFile = orig }()

	_, _, err = store.GetBlob(context.Background(), ghost)
	if err == nil {
		t.Fatal("expected error from f.Stat(), got nil")
	}
}

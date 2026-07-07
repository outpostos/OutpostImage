package oci

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
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

package oci

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
)

// setupDockerFixture creates a minimal Docker overlay2 directory structure for testing.
func setupDockerFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()

	// Create directory structure
	imagedbDir := filepath.Join(root, "image", "overlay2", "imagedb", "content", "sha256")
	layerdbDir := filepath.Join(root, "image", "overlay2", "layerdb", "sha256")
	if err := os.MkdirAll(imagedbDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(layerdbDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Create a fake image config
	imageConfig := map[string]any{
		"architecture": "amd64",
		"os":           "linux",
		"rootfs": map[string]any{
			"type":     "layers",
			"diff_ids": []string{"sha256:aaaa000000000000000000000000000000000000000000000000000000000000"},
		},
	}
	configData, _ := json.Marshal(imageConfig)
	configDigest := digest.FromBytes(configData)
	configHash := configDigest.Encoded()

	if err := os.WriteFile(filepath.Join(imagedbDir, configHash), configData, 0644); err != nil {
		t.Fatal(err)
	}

	// Create repositories.json
	repos := map[string]any{
		"Repositories": map[string]any{
			"nginx": map[string]string{
				"nginx:latest":                   configDigest.String(),
				"nginx@" + configDigest.String(): configDigest.String(),
			},
		},
	}
	reposData, _ := json.Marshal(repos)
	if err := os.WriteFile(filepath.Join(root, "image", "overlay2", "repositories.json"), reposData, 0644); err != nil {
		t.Fatal(err)
	}

	// Create a layer entry in layerdb
	diffID := "aaaa000000000000000000000000000000000000000000000000000000000000"
	// First layer: chainID == diffID
	chainDir := filepath.Join(layerdbDir, diffID)
	if err := os.MkdirAll(chainDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(chainDir, "diff"), []byte("sha256:"+diffID), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(chainDir, "cache-id"), []byte("cache-abc123"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(chainDir, "size"), []byte("1024"), 0644); err != nil {
		t.Fatal(err)
	}

	// Create the overlay2 diff directory so layer tar computation works
	diffDir := filepath.Join(root, "overlay2", "cache-abc123", "diff")
	if err := os.MkdirAll(diffDir, 0755); err != nil {
		t.Fatal(err)
	}
	// Add a small file so the tar is non-empty
	if err := os.WriteFile(filepath.Join(diffDir, "hello"), []byte("world"), 0644); err != nil {
		t.Fatal(err)
	}

	return root
}

func TestDockerStore_Init(t *testing.T) {
	root := setupDockerFixture(t)
	store := NewDockerStore(root)

	if err := store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}

	if store.Name() != "docker" {
		t.Fatalf("Name() = %q, want %q", store.Name(), "docker")
	}
}

func TestDockerStore_ListImages(t *testing.T) {
	root := setupDockerFixture(t)
	store := NewDockerStore(root)

	if err := store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}

	images, err := store.ListImages(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if len(images) != 1 {
		t.Fatalf("got %d images, want 1", len(images))
	}

	img := images[0]
	found := false
	for _, tag := range img.RepoTags {
		if tag == "nginx:latest" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected nginx:latest in tags, got %v", img.RepoTags)
	}
}

func TestDockerStore_Resolve(t *testing.T) {
	root := setupDockerFixture(t)
	store := NewDockerStore(root)
	if err := store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}

	dgst, err := store.Resolve(context.Background(), "nginx:latest")
	if err != nil {
		t.Fatal(err)
	}
	if dgst == "" {
		t.Fatal("expected non-empty digest")
	}

	_, err = store.Resolve(context.Background(), "nonexistent:latest")
	if err == nil {
		t.Fatal("expected error for nonexistent ref")
	}
}

func TestDockerStore_GetManifest(t *testing.T) {
	root := setupDockerFixture(t)
	store := NewDockerStore(root)
	if err := store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}

	dgst, _ := store.Resolve(context.Background(), "nginx:latest")
	data, mediaType, err := store.GetManifest(context.Background(), dgst)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 {
		t.Fatal("expected non-empty manifest data")
	}
	if mediaType == "" {
		t.Fatal("expected non-empty media type")
	}

	// Verify it's valid JSON
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("manifest is not valid JSON: %v", err)
	}
}

func TestDockerStore_GetBlob(t *testing.T) {
	root := setupDockerFixture(t)
	store := NewDockerStore(root)
	if err := store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}

	dgst, _ := store.Resolve(context.Background(), "nginx:latest")
	reader, size, err := store.GetBlob(context.Background(), dgst)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	if size <= 0 {
		t.Fatalf("expected positive size, got %d", size)
	}
}

func TestDockerStore_HasBlob(t *testing.T) {
	root := setupDockerFixture(t)
	store := NewDockerStore(root)
	if err := store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}

	dgst, _ := store.Resolve(context.Background(), "nginx:latest")
	if !store.HasBlob(context.Background(), dgst) {
		t.Fatal("expected HasBlob to return true for existing config digest")
	}

	fakeDgst, _ := digest.Parse("sha256:0000000000000000000000000000000000000000000000000000000000000000")
	if store.HasBlob(context.Background(), fakeDgst) {
		t.Fatal("expected HasBlob to return false for nonexistent digest")
	}
}

func TestDockerStore_HasBlob_Layer(t *testing.T) {
	root := setupDockerFixture(t)
	store := NewDockerStore(root)
	if err := store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Trigger lazy manifest building which computes layer tar digests
	configDgst, _ := store.Resolve(context.Background(), "nginx:latest")
	_, _, err := store.GetManifest(context.Background(), configDgst)
	if err != nil {
		t.Fatalf("GetManifest failed: %v", err)
	}

	// After manifest building, layerBlobs should contain the computed tar digest
	store.mu.RLock()
	hasLayers := len(store.layerBlobs) > 0
	store.mu.RUnlock()
	if !hasLayers {
		t.Fatal("expected layerBlobs to be populated after manifest building")
	}

	// Verify that HasBlob finds the computed tar digest
	store.mu.RLock()
	var tarDgst digest.Digest
	for d := range store.layerBlobs {
		tarDgst = d
		break
	}
	store.mu.RUnlock()
	if !store.HasBlob(context.Background(), tarDgst) {
		t.Fatal("expected HasBlob to return true for computed layer tar digest")
	}
}

func TestComputeChainID(t *testing.T) {
	tests := []struct {
		name    string
		diffIDs []digest.Digest
		want    int
	}{
		{
			name:    "empty",
			diffIDs: nil,
			want:    0,
		},
		{
			name: "single layer",
			diffIDs: []digest.Digest{
				digest.Digest("sha256:aaaa000000000000000000000000000000000000000000000000000000000000"),
			},
			want: 1,
		},
		{
			name: "two layers",
			diffIDs: []digest.Digest{
				digest.Digest("sha256:aaaa000000000000000000000000000000000000000000000000000000000000"),
				digest.Digest("sha256:bbbb000000000000000000000000000000000000000000000000000000000000"),
			},
			want: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			chainIDs := ComputeChainID(tt.diffIDs)
			if len(chainIDs) != tt.want {
				t.Fatalf("got %d chainIDs, want %d", len(chainIDs), tt.want)
			}
			if tt.want > 0 {
				// First chainID should equal first diffID
				if chainIDs[0] != tt.diffIDs[0] {
					t.Errorf("first chainID %s != first diffID %s", chainIDs[0], tt.diffIDs[0])
				}
			}
			if tt.want > 1 {
				// Second chainID should differ from second diffID
				if chainIDs[1] == tt.diffIDs[1] {
					t.Error("second chainID should differ from second diffID")
				}
			}
		})
	}
}

func TestDockerStore_EmptyRoot(t *testing.T) {
	root := t.TempDir()
	store := NewDockerStore(root)

	// Should not error on missing directories
	if err := store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}

	images, err := store.ListImages(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(images) != 0 {
		t.Fatalf("expected 0 images, got %d", len(images))
	}
}

// TestDockerStore_GetBlob_Layer exercises streamLayerTar by fetching a layer blob.
func TestDockerStore_GetBlob_Layer(t *testing.T) {
	root := setupDockerFixture(t)
	store := NewDockerStore(root)
	if err := store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Build manifest so layerBlobs is populated with tar digest → layerBlob.
	configDgst, _ := store.Resolve(context.Background(), "nginx:latest")
	_, _, err := store.GetManifest(context.Background(), configDgst)
	if err != nil {
		t.Fatalf("GetManifest failed: %v", err)
	}

	store.mu.RLock()
	var tarDgst digest.Digest
	for d := range store.layerBlobs {
		tarDgst = d
		break
	}
	store.mu.RUnlock()

	if tarDgst == "" {
		t.Fatal("expected at least one layer blob after manifest build")
	}

	// GetBlob on a layer digest calls streamLayerTar internally.
	rc, size, err := store.GetBlob(context.Background(), tarDgst)
	if err != nil {
		t.Fatalf("GetBlob(layer) failed: %v", err)
	}
	defer rc.Close()
	if size < 0 {
		t.Errorf("expected non-negative size, got %d", size)
	}
}

// TestDockerStore_Subscribe verifies that file creation in imagedb emits events.
func TestDockerStore_Subscribe(t *testing.T) {
	root := setupDockerFixture(t)
	store := NewDockerStore(root)
	if err := store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ch, err := store.Subscribe(ctx)
	if err != nil {
		t.Fatalf("Subscribe failed: %v", err)
	}

	// Write a new file to the imagedb directory to trigger a Create event.
	imagedbDir := filepath.Join(root, "image", "overlay2", "imagedb", "content", "sha256")
	newDigest := "bbbb000000000000000000000000000000000000000000000000000000000000"
	newFile := filepath.Join(imagedbDir, newDigest)
	if err := os.WriteFile(newFile, []byte("{}"), 0644); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	select {
	case ev := <-ch:
		if ev.Type != EventCreate {
			t.Errorf("event type = %v, want EventCreate", ev.Type)
		}
		wantDgst := digest.Digest("sha256:" + newDigest)
		if ev.Digest != wantDgst {
			t.Errorf("event digest = %v, want %v", ev.Digest, wantDgst)
		}
	case <-ctx.Done():
		t.Fatal("timeout waiting for Subscribe event")
	}
}

// TestDockerStore_Subscribe_RemoveEvent verifies that removing an imagedb file emits EventDelete.
func TestDockerStore_Subscribe_RemoveEvent(t *testing.T) {
	root := setupDockerFixture(t)
	store := NewDockerStore(root)
	if err := store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ch, err := store.Subscribe(ctx)
	if err != nil {
		t.Fatalf("Subscribe failed: %v", err)
	}

	imagedbDir := filepath.Join(root, "image", "overlay2", "imagedb", "content", "sha256")
	newDigest := "cccc000000000000000000000000000000000000000000000000000000000000"
	newFile := filepath.Join(imagedbDir, newDigest)
	if err := os.WriteFile(newFile, []byte("{}"), 0644); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	// Drain the Create event.
	select {
	case ev := <-ch:
		if ev.Type != EventCreate {
			t.Errorf("expected Create event, got %v", ev.Type)
		}
	case <-ctx.Done():
		t.Fatal("timeout waiting for Create event")
	}

	// Remove the file to trigger EventDelete.
	if err := os.Remove(newFile); err != nil {
		t.Fatalf("Remove failed: %v", err)
	}

	select {
	case ev := <-ch:
		if ev.Type != EventDelete {
			t.Errorf("expected Delete event, got %v", ev.Type)
		}
		wantDgst := digest.Digest("sha256:" + newDigest)
		if ev.Digest != wantDgst {
			t.Errorf("event digest = %v, want %v", ev.Digest, wantDgst)
		}
	case <-ctx.Done():
		t.Fatal("timeout waiting for Delete event")
	}
}

// TestDockerStore_Subscribe_ContextCancel verifies that cancelling the context closes the event channel.
func TestDockerStore_Subscribe_ContextCancel(t *testing.T) {
	root := setupDockerFixture(t)
	store := NewDockerStore(root)
	if err := store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	ch, err := store.Subscribe(ctx)
	if err != nil {
		t.Fatalf("Subscribe failed: %v", err)
	}

	cancel()

	// After context cancel the goroutine should exit and close ch.
	select {
	case _, ok := <-ch:
		if ok {
			// Drain any buffered events then verify channel closes.
			select {
			case _, ok2 := <-ch:
				if ok2 {
					t.Error("channel still open after context cancel")
				}
			case <-time.After(3 * time.Second):
				t.Error("channel not closed after context cancel")
			}
		}
	case <-time.After(3 * time.Second):
		t.Error("channel not closed after context cancel")
	}
}

// TestDockerStore_Subscribe_AddError verifies that Subscribe returns an error when the
// imagedb directory does not exist (empty root).
func TestDockerStore_Subscribe_AddError(t *testing.T) {
	root := t.TempDir() // empty root, no imagedb directory
	store := NewDockerStore(root)
	_ = store.Init(context.Background())

	_, err := store.Subscribe(context.Background())
	if err == nil {
		t.Fatal("expected error when imagedb directory does not exist")
	}
}

// TestTarDirectory_WithSymlink verifies that tarDirectory handles symlinks correctly.
func TestTarDirectory_WithSymlink(t *testing.T) {
	dir := t.TempDir()

	// Create a regular file and a symlink to it.
	if err := os.WriteFile(filepath.Join(dir, "regular"), []byte("content"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "regular"), filepath.Join(dir, "link")); err != nil {
		t.Skip("symlinks not supported on this filesystem")
	}

	var buf bytes.Buffer
	if err := tarDirectory(&buf, dir); err != nil {
		t.Fatalf("tarDirectory failed: %v", err)
	}

	tr := tar.NewReader(&buf)
	var seenSymlink bool
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("reading tar: %v", err)
		}
		if hdr.Typeflag == tar.TypeSymlink {
			seenSymlink = true
		}
	}
	if !seenSymlink {
		t.Error("expected symlink entry in tar archive")
	}
}

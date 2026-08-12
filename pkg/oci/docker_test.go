package oci

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
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

// TestDockerStore_Subscribe_InvalidDigestFilename verifies that files whose names are not
// valid sha256 hex digests are silently skipped (both Create and Remove events).
func TestDockerStore_Subscribe_InvalidDigestFilename(t *testing.T) {
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

	// Create a file with a non-hex name → digest.Parse("sha256:not-valid") fails → continue
	invalidPath := filepath.Join(imagedbDir, "not-a-valid-digest-name")
	if err := os.WriteFile(invalidPath, []byte("data"), 0644); err != nil {
		t.Fatal(err)
	}

	// Then remove it → Remove event with invalid name → continue
	if err := os.Remove(invalidPath); err != nil {
		t.Fatal(err)
	}

	// Create a valid file so we can confirm the goroutine is still alive after skipping.
	validHash := "dddd000000000000000000000000000000000000000000000000000000000001"
	validPath := filepath.Join(imagedbDir, validHash)
	if err := os.WriteFile(validPath, []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}

	validDgst := digest.Digest("sha256:" + validHash)
	select {
	case ev := <-ch:
		if ev.Digest != validDgst {
			t.Errorf("got event digest %s, want %s", ev.Digest, validDgst)
		}
	case <-ctx.Done():
		t.Fatal("timeout: goroutine did not survive invalid digest filename")
	}
}

// TestDockerStore_Subscribe_ContextDoneWhileEmitting covers the ctx.Done() path
// inside the EventCreate select in Subscribe. By not reading from ch (capacity 16)
// and creating 17 valid sha256-named files, the goroutine fills the buffer with the
// first 16 events and then blocks on the 17th emit. Cancelling ctx causes the
// `case <-ctx.Done(): return` branch to fire.
func TestDockerStore_Subscribe_ContextDoneWhileEmitting(t *testing.T) {
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

	imagedbDir := filepath.Join(root, "image", "overlay2", "imagedb", "content", "sha256")

	// Create 17 files with valid sha256 hex names. ch has capacity 16, so the
	// goroutine fills the buffer on the first 16 and blocks on the 17th.
	for i := 1; i <= 17; i++ {
		name := fmt.Sprintf("%064x", i)
		path := filepath.Join(imagedbDir, name)
		if err := os.WriteFile(path, []byte("{}"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	// Give fsnotify time to deliver all events and let the goroutine fill ch.
	time.Sleep(200 * time.Millisecond)

	// Cancel context while the goroutine is blocked trying to emit the 17th event.
	cancel()

	// The goroutine should exit (via ctx.Done() path) and close ch.
	select {
	case _, ok := <-ch:
		if ok {
			// Drain any buffered events until close.
			for range ch {
			}
		}
	case <-time.After(3 * time.Second):
		t.Error("channel not closed after context cancel")
	}
}

// TestDockerStore_Subscribe_ContextDoneWhileEmittingDelete covers the ctx.Done() path
// inside the EventDelete select in Subscribe. We fill ch (capacity 16) with 16 Create
// events and then delete a file. The goroutine blocks trying to emit the EventDelete
// because ch is full; cancelling ctx triggers `case <-ctx.Done(): return`.
func TestDockerStore_Subscribe_ContextDoneWhileEmittingDelete(t *testing.T) {
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

	imagedbDir := filepath.Join(root, "image", "overlay2", "imagedb", "content", "sha256")

	// Create 16 files to fill ch completely.
	for i := 1; i <= 16; i++ {
		name := fmt.Sprintf("%064x", i)
		if err := os.WriteFile(filepath.Join(imagedbDir, name), []byte("{}"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	// Give fsnotify time to deliver all 16 Create events so ch is full.
	time.Sleep(200 * time.Millisecond)

	// Remove file 1 — generates a Remove event. The goroutine will try to send
	// EventDelete to ch (which is full) and block in the select.
	if err := os.Remove(filepath.Join(imagedbDir, fmt.Sprintf("%064x", 1))); err != nil {
		t.Fatal(err)
	}

	// Give the goroutine time to pick up the Remove event and block on emit.
	time.Sleep(100 * time.Millisecond)

	// Cancel context while the goroutine is blocked on the Delete emit.
	cancel()

	// Goroutine should exit and close ch.
	select {
	case _, ok := <-ch:
		if ok {
			for range ch {
			}
		}
	case <-time.After(3 * time.Second):
		t.Error("channel not closed after context cancel (delete path)")
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
// TestDockerStore_GetBlob_NotFound exercises the "blob not found" error path in GetBlob —
// a digest that is not in blobs, layerBlobs, or rewrittenConfigs returns an error.
func TestDockerStore_GetBlob_NotFound(t *testing.T) {
	root := setupDockerFixture(t)
	store := NewDockerStore(root)
	if err := store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}

	unknown := digest.FromString("not-a-blob")
	_, _, err := store.GetBlob(context.Background(), unknown)
	if err == nil {
		t.Fatal("expected error for unknown blob")
	}
}

// TestComputeLayerTarDigest_Error exercises the error return in computeLayerTarDigest when
// tarDirectory fails — passing a nonexistent path propagates the error.
func TestComputeLayerTarDigest_Error(t *testing.T) {
	_, _, err := computeLayerTarDigest("/nonexistent/path/that/does/not/exist")
	if err == nil {
		t.Fatal("expected error for nonexistent diff path")
	}
}

// TestBuildImageManifest_SharedLayer covers the "found=true" path in buildImageManifest —
// two images sharing the same layer cause the second manifest build to reuse the cached
// tar digest rather than recomputing it.
func TestBuildImageManifest_SharedLayer(t *testing.T) {
	root := t.TempDir()

	imagedbDir := filepath.Join(root, "image", "overlay2", "imagedb", "content", "sha256")
	layerdbDir := filepath.Join(root, "image", "overlay2", "layerdb", "sha256")
	if err := os.MkdirAll(imagedbDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(layerdbDir, 0755); err != nil {
		t.Fatal(err)
	}

	diffID := "aaaa000000000000000000000000000000000000000000000000000000000000"
	chainDir := filepath.Join(layerdbDir, diffID)
	if err := os.MkdirAll(chainDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(chainDir, "diff"), []byte("sha256:"+diffID), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(chainDir, "cache-id"), []byte("shared-cache"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(chainDir, "size"), []byte("512"), 0644); err != nil {
		t.Fatal(err)
	}

	diffDir := filepath.Join(root, "overlay2", "shared-cache", "diff")
	if err := os.MkdirAll(diffDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(diffDir, "data"), []byte("shared layer"), 0644); err != nil {
		t.Fatal(err)
	}

	// Two configs that share the same layer diffID (different arch → different config digest)
	config1Data, _ := json.Marshal(map[string]any{
		"architecture": "amd64", "os": "linux",
		"rootfs": map[string]any{"type": "layers", "diff_ids": []string{"sha256:" + diffID}},
	})
	config2Data, _ := json.Marshal(map[string]any{
		"architecture": "arm64", "os": "linux",
		"rootfs": map[string]any{"type": "layers", "diff_ids": []string{"sha256:" + diffID}},
	})

	dgst1 := digest.FromBytes(config1Data)
	dgst2 := digest.FromBytes(config2Data)

	if err := os.WriteFile(filepath.Join(imagedbDir, dgst1.Encoded()), config1Data, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(imagedbDir, dgst2.Encoded()), config2Data, 0644); err != nil {
		t.Fatal(err)
	}

	repos := map[string]any{"Repositories": map[string]any{
		"image1": map[string]string{"image1:v1": dgst1.String()},
		"image2": map[string]string{"image2:v1": dgst2.String()},
	}}
	reposData, _ := json.Marshal(repos)
	if err := os.WriteFile(filepath.Join(root, "image", "overlay2", "repositories.json"), reposData, 0644); err != nil {
		t.Fatal(err)
	}

	store := NewDockerStore(root)
	if err := store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Build first image manifest — computes and caches layer tar digest.
	m1, _, err := store.GetManifest(context.Background(), dgst1)
	if err != nil {
		t.Fatalf("GetManifest(image1): %v", err)
	}

	// Build second image manifest — must reuse the cached tar digest ("found=true" path).
	m2, _, err := store.GetManifest(context.Background(), dgst2)
	if err != nil {
		t.Fatalf("GetManifest(image2): %v", err)
	}

	if len(m1) == 0 || len(m2) == 0 {
		t.Error("expected non-empty manifest data for both images")
	}
}

// TestDockerStore_Init_ScanImageDBError exercises the scanImageDB error path in Init when
// the imagedb directory exists but is unreadable.
func TestDockerStore_Init_ScanImageDBError(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("cannot test permission errors as root")
	}
	root := t.TempDir()
	imagedbDir := filepath.Join(root, "image", "overlay2", "imagedb", "content", "sha256")
	if err := os.MkdirAll(imagedbDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "image", "overlay2", "layerdb", "sha256"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(imagedbDir, 0000); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(imagedbDir, 0755)

	store := NewDockerStore(root)
	if err := store.Init(context.Background()); err == nil {
		t.Fatal("expected error when imagedb dir is unreadable")
	}
}

// TestDockerStore_Init_ScanLayerDBError exercises the scanLayerDB error path in Init when
// the layerdb directory exists but is unreadable.
func TestDockerStore_Init_ScanLayerDBError(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("cannot test permission errors as root")
	}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "image", "overlay2", "imagedb", "content", "sha256"), 0755); err != nil {
		t.Fatal(err)
	}
	layerdbDir := filepath.Join(root, "image", "overlay2", "layerdb", "sha256")
	if err := os.MkdirAll(layerdbDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(layerdbDir, 0000); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(layerdbDir, 0755)

	store := NewDockerStore(root)
	if err := store.Init(context.Background()); err == nil {
		t.Fatal("expected error when layerdb dir is unreadable")
	}
}

// TestDockerStore_GetManifest_NotFound exercises the "manifest not found" error path in
// GetManifest — a digest that is not in the store returns an error.
func TestDockerStore_GetManifest_NotFound(t *testing.T) {
	root := setupDockerFixture(t)
	store := NewDockerStore(root)
	if err := store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}

	unknown := digest.FromString("unknown-digest")
	_, _, err := store.GetManifest(context.Background(), unknown)
	if err == nil {
		t.Fatal("expected error for unknown manifest")
	}
}

// TestDockerStore_EnsureImageReady_AlreadyBuilt exercises the fast-path in ensureImageReady —
// calling GetManifest twice for the same digest returns cached result without rebuilding.
func TestDockerStore_EnsureImageReady_AlreadyBuilt(t *testing.T) {
	root := setupDockerFixture(t)
	store := NewDockerStore(root)
	if err := store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}

	dgst, _ := store.Resolve(context.Background(), "nginx:latest")

	// First call builds the manifest.
	data1, _, err := store.GetManifest(context.Background(), dgst)
	if err != nil {
		t.Fatal(err)
	}
	// Second call should hit the fast-path (alreadyBuilt) in ensureImageReady.
	data2, _, err := store.GetManifest(context.Background(), dgst)
	if err != nil {
		t.Fatal(err)
	}
	if string(data1) != string(data2) {
		t.Error("expected identical manifest data on second call")
	}
}

// TestDockerStore_GetBlob_RewrittenConfig exercises the rewrittenConfigs path in GetBlob —
// after building a manifest, the rewritten config blob is accessible via GetBlob.
func TestDockerStore_GetBlob_RewrittenConfig(t *testing.T) {
	root := setupDockerFixture(t)
	store := NewDockerStore(root)
	if err := store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Build manifest to populate rewrittenConfigs.
	configDgst, _ := store.Resolve(context.Background(), "nginx:latest")
	_, _, err := store.GetManifest(context.Background(), configDgst)
	if err != nil {
		t.Fatal(err)
	}

	// Find a rewritten config digest.
	store.mu.RLock()
	var rwDgst digest.Digest
	for d := range store.rewrittenConfigs {
		rwDgst = d
		break
	}
	store.mu.RUnlock()
	if rwDgst == "" {
		t.Skip("no rewritten configs after manifest build")
	}

	// GetBlob on a rewritten config digest should succeed.
	rc, size, err := store.GetBlob(context.Background(), rwDgst)
	if err != nil {
		t.Fatalf("GetBlob(rewrittenConfig) failed: %v", err)
	}
	defer rc.Close()
	if size <= 0 {
		t.Errorf("expected positive size, got %d", size)
	}

	// HasBlob should also report true for the rewritten config.
	if !store.HasBlob(context.Background(), rwDgst) {
		t.Error("HasBlob(rewrittenConfig) should return true")
	}
}

// TestDockerStore_LoadRepositories_InvalidJSON exercises the json.Unmarshal error path
// in loadRepositories — repositories.json contains malformed JSON → error returned.
func TestDockerStore_LoadRepositories_InvalidJSON(t *testing.T) {
	root := t.TempDir()
	repoDir := filepath.Join(root, "image", "overlay2")
	if err := os.MkdirAll(repoDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repoDir, "imagedb", "content", "sha256"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repoDir, "layerdb", "sha256"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "repositories.json"), []byte("not-json{"), 0644); err != nil {
		t.Fatal(err)
	}

	store := NewDockerStore(root)
	if err := store.Init(context.Background()); err == nil {
		t.Fatal("expected error for invalid repositories.json JSON")
	}
}

// TestDockerStore_LoadRepositories_InvalidDigest exercises the digest.Parse error path
// in loadRepositories — an invalid digest value is skipped with a warning.
func TestDockerStore_LoadRepositories_InvalidDigest(t *testing.T) {
	root := t.TempDir()
	repoDir := filepath.Join(root, "image", "overlay2")
	if err := os.MkdirAll(repoDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repoDir, "imagedb", "content", "sha256"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repoDir, "layerdb", "sha256"), 0755); err != nil {
		t.Fatal(err)
	}
	repos := `{"Repositories":{"nginx":{"nginx:latest":"not-a-valid-digest"}}}`
	if err := os.WriteFile(filepath.Join(repoDir, "repositories.json"), []byte(repos), 0644); err != nil {
		t.Fatal(err)
	}

	store := NewDockerStore(root)
	if err := store.Init(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	store.mu.RLock()
	numTags := len(store.tags)
	store.mu.RUnlock()
	if numTags != 0 {
		t.Fatalf("expected 0 tags (invalid digest skipped), got %d", numTags)
	}
}

// TestDockerStore_ScanImageDB_InvalidEntry exercises the skip paths in scanImageDB — a
// directory entry and a file with a non-hex name are both silently skipped.
func TestDockerStore_ScanImageDB_InvalidEntry(t *testing.T) {
	root := t.TempDir()
	imagedbDir := filepath.Join(root, "image", "overlay2", "imagedb", "content", "sha256")
	if err := os.MkdirAll(imagedbDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "image", "overlay2", "layerdb", "sha256"), 0755); err != nil {
		t.Fatal(err)
	}

	// File with invalid name (not a sha256 hex)
	if err := os.WriteFile(filepath.Join(imagedbDir, "not-a-valid-hash"), []byte("data"), 0644); err != nil {
		t.Fatal(err)
	}
	// Subdirectory inside imagedb — should be skipped by entry.IsDir() guard
	if err := os.MkdirAll(filepath.Join(imagedbDir, "subdir"), 0755); err != nil {
		t.Fatal(err)
	}

	repoPath := filepath.Join(root, "image", "overlay2", "repositories.json")
	if err := os.WriteFile(repoPath, []byte(`{"Repositories":{}}`), 0644); err != nil {
		t.Fatal(err)
	}

	store := NewDockerStore(root)
	if err := store.Init(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	images, _ := store.ListImages(context.Background())
	if len(images) != 0 {
		t.Fatalf("expected 0 images from invalid/dir entries, got %d", len(images))
	}
}

// TestDockerStore_ScanLayerDB_SkipPaths exercises skip paths in scanLayerDB:
// a regular file (not a dir) is skipped, a dir without "diff" file is skipped,
// a dir with invalid "diff" content is skipped, a dir without "cache-id" is skipped.
func TestDockerStore_ScanLayerDB_SkipPaths(t *testing.T) {
	root := t.TempDir()
	layerdbDir := filepath.Join(root, "image", "overlay2", "layerdb", "sha256")
	if err := os.MkdirAll(layerdbDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "image", "overlay2", "imagedb", "content", "sha256"), 0755); err != nil {
		t.Fatal(err)
	}

	// A regular file in layerdb — should be skipped by !entry.IsDir() guard
	if err := os.WriteFile(filepath.Join(layerdbDir, "somefile"), []byte("data"), 0644); err != nil {
		t.Fatal(err)
	}
	// Dir without "diff" file — os.ReadFile("diff") fails → continue
	noDiffDir := filepath.Join(layerdbDir, "aaaa000000000000000000000000000000000000000000000000000000000000")
	if err := os.MkdirAll(noDiffDir, 0755); err != nil {
		t.Fatal(err)
	}
	// Dir with invalid "diff" content — digest.Parse fails → continue
	badDiffDir := filepath.Join(layerdbDir, "bbbb000000000000000000000000000000000000000000000000000000000000")
	if err := os.MkdirAll(badDiffDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(badDiffDir, "diff"), []byte("not-a-digest"), 0644); err != nil {
		t.Fatal(err)
	}
	// Dir with valid "diff" but missing "cache-id" — os.ReadFile("cache-id") fails → continue
	noCacheDir := filepath.Join(layerdbDir, "cccc000000000000000000000000000000000000000000000000000000000000")
	if err := os.MkdirAll(noCacheDir, 0755); err != nil {
		t.Fatal(err)
	}
	validDiffID := "sha256:aaaa000000000000000000000000000000000000000000000000000000000000"
	if err := os.WriteFile(filepath.Join(noCacheDir, "diff"), []byte(validDiffID), 0644); err != nil {
		t.Fatal(err)
	}

	repoPath := filepath.Join(root, "image", "overlay2", "repositories.json")
	if err := os.WriteFile(repoPath, []byte(`{"Repositories":{}}`), 0644); err != nil {
		t.Fatal(err)
	}

	store := NewDockerStore(root)
	if err := store.Init(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	store.mu.RLock()
	numLayers := len(store.layers)
	store.mu.RUnlock()
	// All entries should have been skipped
	if numLayers != 0 {
		t.Fatalf("expected 0 layers from skip paths, got %d", numLayers)
	}
}

// TestDockerStore_Resolve_Normalizations exercises all four tag-normalization paths in
// Resolve: docker.io/ prefix, library/ prefix strip, library/ prefix add, and
// docker.io/library/ prefix add.
func TestDockerStore_Resolve_Normalizations(t *testing.T) {
	root := t.TempDir()

	imagedbDir := filepath.Join(root, "image", "overlay2", "imagedb", "content", "sha256")
	if err := os.MkdirAll(imagedbDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "image", "overlay2", "layerdb", "sha256"), 0755); err != nil {
		t.Fatal(err)
	}

	configData := []byte(`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":[]}}`)
	configDigest := digest.FromBytes(configData)
	if err := os.WriteFile(filepath.Join(imagedbDir, configDigest.Encoded()), configData, 0644); err != nil {
		t.Fatal(err)
	}

	// Store tags under every expected form so each normalization path has a match.
	repos := map[string]any{
		"Repositories": map[string]any{
			"redis": map[string]string{
				// docker.io/redis:7 → exercises docker.io prefix lookup
				"docker.io/redis:7": configDigest.String(),
			},
			"alpine": map[string]string{
				// alpine:3 → exercises strip-library/ lookup (resolve "library/alpine:3")
				"alpine:3": configDigest.String(),
			},
			"debian": map[string]string{
				// library/debian:12 → exercises add-library/ lookup (resolve "debian:12")
				"library/debian:12": configDigest.String(),
			},
			"ubuntu": map[string]string{
				// docker.io/library/ubuntu:22.04 → exercises docker.io/library/ lookup
				"docker.io/library/ubuntu:22.04": configDigest.String(),
			},
		},
	}
	reposData, _ := json.Marshal(repos)
	if err := os.WriteFile(filepath.Join(root, "image", "overlay2", "repositories.json"), reposData, 0644); err != nil {
		t.Fatal(err)
	}

	store := NewDockerStore(root)
	if err := store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		ref     string
		wantErr bool
	}{
		{"redis:7", false},           // → docker.io/redis:7 (docker.io prefix add)
		{"library/alpine:3", false},  // → alpine:3 (library/ strip)
		{"debian:12", false},         // → library/debian:12 (library/ add)
		{"ubuntu:22.04", false},      // → docker.io/library/ubuntu:22.04 (docker.io/library/ add)
		{"nope:1.0", true},           // all normalizations fail
	}

	for _, tt := range tests {
		dgst, err := store.Resolve(context.Background(), tt.ref)
		if tt.wantErr {
			if err == nil {
				t.Errorf("Resolve(%q): expected error, got none (dgst=%v)", tt.ref, dgst)
			}
			continue
		}
		if err != nil {
			t.Errorf("Resolve(%q): unexpected error: %v", tt.ref, err)
			continue
		}
		if dgst == "" {
			t.Errorf("Resolve(%q): got empty digest", tt.ref)
		}
	}
}

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

// TestDockerStore_Init_LoadRepositoriesError exercises the Init error path when
// loadRepositories fails. Creating repositories.json as a directory makes os.ReadFile
// return EISDIR (not IsNotExist), so loadRepositories propagates the error.
func TestDockerStore_Init_LoadRepositoriesError(t *testing.T) {
	root := t.TempDir()
	// Create "repositories.json" as a subdirectory so os.ReadFile fails with EISDIR.
	repoDir := filepath.Join(root, "image", "overlay2", "repositories.json")
	if err := os.MkdirAll(repoDir, 0755); err != nil {
		t.Fatal(err)
	}

	store := NewDockerStore(root)
	if err := store.Init(context.Background()); err == nil {
		t.Fatal("expected error when repositories.json is a directory")
	}
}

// TestDockerStore_Init_ScanImageDBError_FileAsDir exercises the Init error path when
// scanImageDB fails. Creating the imagedb sha256 path as a file makes os.ReadDir return
// ENOTDIR (not IsNotExist), so scanImageDB propagates the error.
func TestDockerStore_Init_ScanImageDBError_FileAsDir(t *testing.T) {
	root := t.TempDir()
	imagedbContent := filepath.Join(root, "image", "overlay2", "imagedb", "content")
	if err := os.MkdirAll(imagedbContent, 0755); err != nil {
		t.Fatal(err)
	}
	// sha256 is a file, not a directory → os.ReadDir returns "not a directory"
	if err := os.WriteFile(filepath.Join(imagedbContent, "sha256"), []byte("not-a-dir"), 0644); err != nil {
		t.Fatal(err)
	}

	store := NewDockerStore(root)
	if err := store.Init(context.Background()); err == nil {
		t.Fatal("expected error when imagedb sha256 path is a regular file")
	}
}

// TestDockerStore_Init_ScanLayerDBError_FileAsDir exercises the Init error path when
// scanLayerDB fails. Creating the layerdb sha256 path as a file makes os.ReadDir return
// ENOTDIR (not IsNotExist), so scanLayerDB propagates the error.
func TestDockerStore_Init_ScanLayerDBError_FileAsDir(t *testing.T) {
	root := t.TempDir()
	// Create valid imagedb so loadRepositories and scanImageDB both succeed.
	if err := os.MkdirAll(filepath.Join(root, "image", "overlay2", "imagedb", "content", "sha256"), 0755); err != nil {
		t.Fatal(err)
	}
	layerdbParent := filepath.Join(root, "image", "overlay2", "layerdb")
	if err := os.MkdirAll(layerdbParent, 0755); err != nil {
		t.Fatal(err)
	}
	// sha256 is a file, not a directory → os.ReadDir returns "not a directory"
	if err := os.WriteFile(filepath.Join(layerdbParent, "sha256"), []byte("not-a-dir"), 0644); err != nil {
		t.Fatal(err)
	}

	store := NewDockerStore(root)
	if err := store.Init(context.Background()); err == nil {
		t.Fatal("expected error when layerdb sha256 path is a regular file")
	}
}

// TestDockerStore_ScanImageDB_ReadFileFails exercises the slog.Warn path in scanImageDB
// when os.ReadFile fails after a valid-named entry is found. A symlink whose name is a
// valid sha256 hex but that points to a directory causes os.ReadFile to return EISDIR.
func TestDockerStore_ScanImageDB_ReadFileFails(t *testing.T) {
	root := t.TempDir()
	imagedbDir := filepath.Join(root, "image", "overlay2", "imagedb", "content", "sha256")
	if err := os.MkdirAll(imagedbDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "image", "overlay2", "layerdb", "sha256"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "image", "overlay2", "repositories.json"),
		[]byte(`{"Repositories":{}}`), 0644); err != nil {
		t.Fatal(err)
	}

	// Create a symlink named with a valid 64-char hex pointing to a directory.
	// entry.IsDir() is false (symlink), digest.Parse succeeds, os.ReadFile fails (EISDIR).
	subdir := filepath.Join(root, "subdir")
	if err := os.MkdirAll(subdir, 0755); err != nil {
		t.Fatal(err)
	}
	validHex := "bbbb000000000000000000000000000000000000000000000000000000000000"
	if err := os.Symlink(subdir, filepath.Join(imagedbDir, validHex)); err != nil {
		t.Skipf("symlinks not supported on this filesystem: %v", err)
	}

	store := NewDockerStore(root)
	// Init should succeed: the warning is logged but not returned as an error.
	if err := store.Init(context.Background()); err != nil {
		t.Fatalf("Init should succeed despite ReadFile warning: %v", err)
	}
}

// TestDockerStore_BuildImageManifest_InvalidRootFS exercises the early-return in
// buildImageManifest when json.Unmarshal of rawConfig["rootfs"] fails. A config with no
// "rootfs" key causes rawConfig["rootfs"] to be nil, making Unmarshal return an error.
func TestDockerStore_BuildImageManifest_InvalidRootFS(t *testing.T) {
	root := t.TempDir()
	imagedbDir := filepath.Join(root, "image", "overlay2", "imagedb", "content", "sha256")
	if err := os.MkdirAll(imagedbDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "image", "overlay2", "layerdb", "sha256"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "image", "overlay2", "repositories.json"),
		[]byte(`{"Repositories":{}}`), 0644); err != nil {
		t.Fatal(err)
	}

	// Config without "rootfs" key → rawConfig["rootfs"] is nil → Unmarshal returns error.
	configData := []byte(`{"architecture":"amd64","os":"linux"}`)
	dgst := digest.FromBytes(configData)
	if err := os.WriteFile(filepath.Join(imagedbDir, dgst.Encoded()), configData, 0644); err != nil {
		t.Fatal(err)
	}

	store := NewDockerStore(root)
	if err := store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}

	// buildImageManifest hits the rootfs Unmarshal failure and returns early,
	// so GetManifest returns "manifest not found".
	_, _, err := store.GetManifest(context.Background(), dgst)
	if err == nil {
		t.Fatal("expected error: manifest not found when rootfs is missing")
	}
}

// TestDockerStore_BuildImageManifest_InvalidDiffID exercises the early-return in
// buildImageManifest when digest.Parse fails for a diffID string. The config has a
// diff_ids entry that is not a valid digest string.
func TestDockerStore_BuildImageManifest_InvalidDiffID(t *testing.T) {
	root := t.TempDir()
	imagedbDir := filepath.Join(root, "image", "overlay2", "imagedb", "content", "sha256")
	if err := os.MkdirAll(imagedbDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "image", "overlay2", "layerdb", "sha256"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "image", "overlay2", "repositories.json"),
		[]byte(`{"Repositories":{}}`), 0644); err != nil {
		t.Fatal(err)
	}

	// Config with an invalid diff_id value → digest.Parse in buildImageManifest returns error.
	configData := []byte(`{"rootfs":{"type":"layers","diff_ids":["not-a-valid-digest"]}}`)
	dgst := digest.FromBytes(configData)
	if err := os.WriteFile(filepath.Join(imagedbDir, dgst.Encoded()), configData, 0644); err != nil {
		t.Fatal(err)
	}

	store := NewDockerStore(root)
	if err := store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}

	_, _, err := store.GetManifest(context.Background(), dgst)
	if err == nil {
		t.Fatal("expected error: manifest not found when diffID is invalid")
	}
}

// TestDockerStore_BuildImageManifest_MissingLayerPath exercises the
// "entry.diffPath == ''" early-return in buildImageManifest. When scanImageDB adds a
// layerEntry{} for a diffID but scanLayerDB finds no matching layerdb entry, diffPath
// remains empty and buildImageManifest returns before computing the tar digest.
func TestDockerStore_BuildImageManifest_MissingLayerPath(t *testing.T) {
	root := t.TempDir()
	imagedbDir := filepath.Join(root, "image", "overlay2", "imagedb", "content", "sha256")
	if err := os.MkdirAll(imagedbDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "image", "overlay2", "layerdb", "sha256"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "image", "overlay2", "repositories.json"),
		[]byte(`{"Repositories":{}}`), 0644); err != nil {
		t.Fatal(err)
	}

	// Config references a valid diffID but there is no layerdb entry for it.
	// scanImageDB inserts layerEntry{} (empty diffPath); buildImageManifest hits diffPath=="" branch.
	validDiffID := "sha256:aaaa000000000000000000000000000000000000000000000000000000000000"
	configData := []byte(`{"rootfs":{"type":"layers","diff_ids":["` + validDiffID + `"]}}`)
	dgst := digest.FromBytes(configData)
	if err := os.WriteFile(filepath.Join(imagedbDir, dgst.Encoded()), configData, 0644); err != nil {
		t.Fatal(err)
	}

	store := NewDockerStore(root)
	if err := store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}

	_, _, err := store.GetManifest(context.Background(), dgst)
	if err == nil {
		t.Fatal("expected error: manifest not found when layer diffPath is empty")
	}
}

// TestDockerStore_GetBlob_ReadError exercises the os.ReadFile error path in GetBlob when
// a blob path is in the index but the file no longer exists on disk.
func TestDockerStore_GetBlob_ReadError(t *testing.T) {
	root := setupDockerFixture(t)
	store := NewDockerStore(root)
	if err := store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Inject a blob path that points to a non-existent file.
	ghostDigest := digest.FromString("ghost-read-error")
	store.mu.Lock()
	store.blobs[ghostDigest] = "/nonexistent/path/to/blob-file"
	store.mu.Unlock()

	_, _, err := store.GetBlob(context.Background(), ghostDigest)
	if err == nil {
		t.Fatal("expected error when blob file cannot be read")
	}
}

// TestTarDirectory_WithSubdirectory verifies that tarDirectory archives subdirectory
// entries without error, exercising the `return nil` path for directory entries
// inside the Walk callback.
func TestTarDirectory_WithSubdirectory(t *testing.T) {
	dir := t.TempDir()

	// Create a subdirectory.
	subDir := filepath.Join(dir, "subdir")
	if err := os.Mkdir(subDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(subDir, "file.txt"), []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := tarDirectory(&buf, dir); err != nil {
		t.Fatalf("tarDirectory failed: %v", err)
	}

	tr := tar.NewReader(&buf)
	var seenDir, seenFile bool
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("reading tar: %v", err)
		}
		if hdr.Typeflag == tar.TypeDir {
			seenDir = true
		}
		if hdr.Name == "subdir/file.txt" {
			seenFile = true
		}
	}
	if !seenDir {
		t.Error("expected directory entry in tar archive")
	}
	if !seenFile {
		t.Error("expected subdir/file.txt entry in tar archive")
	}
}

// TestBuildImageManifest_RootfsUnmarshalError exercises the early return in buildImageManifest
// when the "rootfs" key is present but not a valid JSON object that can unmarshal into the struct.
func TestBuildImageManifest_RootfsUnmarshalError(t *testing.T) {
	root := t.TempDir()

	imagedbDir := filepath.Join(root, "image", "overlay2", "imagedb", "content", "sha256")
	if err := os.MkdirAll(imagedbDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Write a config where "rootfs" is a string (not an object) — will fail json.Unmarshal.
	configData := []byte(`{"rootfs":"not-an-object"}`)
	dgst := digest.FromBytes(configData)
	if err := os.WriteFile(filepath.Join(imagedbDir, dgst.Encoded()), configData, 0644); err != nil {
		t.Fatal(err)
	}

	store := NewDockerStore(root)
	if err := store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}

	// buildImageManifest should return early; GetManifest should report not found.
	_, _, err := store.GetManifest(context.Background(), dgst)
	if err == nil {
		t.Fatal("expected error when rootfs cannot be unmarshalled")
	}
}

// TestBuildImageManifest_LayerComputeError exercises the slog.Warn + return path in
// buildImageManifest when computeLayerTarDigest fails for a valid layer diffPath that
// doesn't exist on disk.
func TestBuildImageManifest_LayerComputeError(t *testing.T) {
	root := t.TempDir()

	imagedbDir := filepath.Join(root, "image", "overlay2", "imagedb", "content", "sha256")
	layerdbDir := filepath.Join(root, "image", "overlay2", "layerdb", "sha256")
	if err := os.MkdirAll(imagedbDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(layerdbDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Create a diffID and the matching layer DB entry pointing to a non-existent diff dir.
	diffID := "sha256:bbbb000000000000000000000000000000000000000000000000000000000001"
	diffIDDigest, _ := digest.Parse(diffID)
	chainID := diffIDDigest // For a single layer, chainID == diffID

	layerDir := filepath.Join(layerdbDir, chainID.Encoded())
	if err := os.MkdirAll(layerDir, 0755); err != nil {
		t.Fatal(err)
	}
	// cache-id file tells the store where the overlay2 diff dir is.
	cacheID := "nonexistent-cache-id"
	if err := os.WriteFile(filepath.Join(layerDir, "cache-id"), []byte(cacheID), 0644); err != nil {
		t.Fatal(err)
	}
	// diff file holds the diffID.
	if err := os.WriteFile(filepath.Join(layerDir, "diff"), []byte(diffID), 0644); err != nil {
		t.Fatal(err)
	}

	// The diff directory (overlay2/<cache-id>/diff) does NOT exist — tarDirectory will fail.
	// We intentionally don't create filepath.Join(root, "overlay2", cacheID, "diff").

	// Write a config referencing this diffID.
	configData := []byte(`{"rootfs":{"type":"layers","diff_ids":["` + diffID + `"]}}`)
	dgst := digest.FromBytes(configData)
	if err := os.WriteFile(filepath.Join(imagedbDir, dgst.Encoded()), configData, 0644); err != nil {
		t.Fatal(err)
	}

	store := NewDockerStore(root)
	if err := store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}

	// buildImageManifest hits computeLayerTarDigest, which fails → slog.Warn + return.
	_, _, err := store.GetManifest(context.Background(), dgst)
	if err == nil {
		t.Fatal("expected error when layer tar digest computation fails")
	}
}

// TestBuildImageManifest_ReadFileError covers the os.ReadFile error path in
// buildImageManifest when the config blob is in the index but the file has been removed.
func TestBuildImageManifest_ReadFileError(t *testing.T) {
	root := setupDockerFixture(t)
	store := NewDockerStore(root)
	if err := store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Inject a blob path that points to a non-existent file.
	ghostDigest := digest.FromString("ghost-manifest-read-error")
	store.mu.Lock()
	store.blobs[ghostDigest] = "/nonexistent/path/config.json"
	store.mu.Unlock()

	_, _, err := store.GetManifest(context.Background(), ghostDigest)
	if err == nil {
		t.Fatal("expected error when config file cannot be read")
	}
}

// TestBuildImageManifest_InvalidJSON covers the json.Unmarshal(data, &rawConfig) error
// path in buildImageManifest when the config file contains non-JSON content.
func TestBuildImageManifest_InvalidJSON(t *testing.T) {
	root := t.TempDir()
	imagedbDir := filepath.Join(root, "image", "overlay2", "imagedb", "content", "sha256")
	if err := os.MkdirAll(imagedbDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Write a file with invalid JSON — scanImageDB silently ignores the unmarshal error
	// but still registers the digest in ds.blobs.
	configData := []byte("not valid json {{{")
	dgst := digest.FromBytes(configData)
	if err := os.WriteFile(filepath.Join(imagedbDir, dgst.Encoded()), configData, 0644); err != nil {
		t.Fatal(err)
	}

	store := NewDockerStore(root)
	if err := store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}

	// buildImageManifest fails at json.Unmarshal(data, &rawConfig) → early return.
	_, _, err := store.GetManifest(context.Background(), dgst)
	if err == nil {
		t.Fatal("expected error when config JSON is invalid")
	}
}

// TestDockerStore_ScanImageDB_InvalidDiffID exercises the `continue` path in the
// DiffID parse loop inside scanImageDB: when a config's rootfs.diff_ids contains
// a non-digest string, digest.Parse fails and the entry is silently skipped.
func TestDockerStore_ScanImageDB_InvalidDiffID(t *testing.T) {
	root := t.TempDir()
	imagedbDir := filepath.Join(root, "image", "overlay2", "imagedb", "content", "sha256")
	if err := os.MkdirAll(imagedbDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "image", "overlay2", "layerdb", "sha256"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "image", "overlay2", "repositories.json"),
		[]byte(`{"Repositories":{}}`), 0644); err != nil {
		t.Fatal(err)
	}

	// Config with a valid JSON structure but invalid DiffID string — causes digest.Parse
	// to fail inside the DiffID loop, hitting the `continue` branch.
	configData := []byte(`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":["not-a-valid-digest"]}}`)
	dgst := digest.FromBytes(configData)
	if err := os.WriteFile(filepath.Join(imagedbDir, dgst.Encoded()), configData, 0644); err != nil {
		t.Fatal(err)
	}

	store := NewDockerStore(root)
	// Init should succeed; the invalid DiffID is silently skipped.
	if err := store.Init(context.Background()); err != nil {
		t.Fatalf("Init should succeed with invalid diff_id: %v", err)
	}
	images, _ := store.ListImages(context.Background())
	if len(images) != 1 {
		t.Fatalf("expected 1 image (config scanned), got %d", len(images))
	}
}

// TestBuildImageManifest_EmptyConfigPath exercises the `configPath == ""` branch in
// buildImageManifest: when a blob is registered in ds.blobs with an empty path,
// buildImageManifest returns immediately without building a manifest.
func TestBuildImageManifest_EmptyConfigPath(t *testing.T) {
	root := setupDockerFixture(t)
	store := NewDockerStore(root)
	if err := store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Inject a blob entry with an empty path — simulates a rewritten config registered
	// by a previous buildImageManifest run (ds.blobs[newConfigDigest] = "").
	ghostDigest := digest.FromString("empty-path-blob")
	store.mu.Lock()
	store.blobs[ghostDigest] = ""
	store.mu.Unlock()

	// buildImageManifest should return immediately (no manifest, no panic).
	store.buildImageManifest(ghostDigest)

	store.mu.RLock()
	_, hasManifest := store.manifests[ghostDigest]
	store.mu.RUnlock()
	if hasManifest {
		t.Error("expected no manifest for blob with empty config path")
	}
}

// TestDockerStore_Resolve_NoColon covers the len(parts) != 2 early-return path in
// Resolve when the reference contains no colon (no tag separator).
func TestDockerStore_Resolve_NoColon(t *testing.T) {
	root := setupDockerFixture(t)
	store := NewDockerStore(root)
	if err := store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}

	_, err := store.Resolve(context.Background(), "nginx")
	if err == nil {
		t.Fatal("expected error for ref with no colon")
	}
}

// errWriter is an io.Writer that always fails, used to exercise error paths in
// functions that write to an io.Writer (e.g. tarDirectory).
type errWriter struct{ err error }

func (e errWriter) Write(_ []byte) (int, error) { return 0, e.err }

// TestTarDirectory_WriteHeaderError exercises the tw.WriteHeader error path in
// tarDirectory. When the underlying writer fails on every write, the first
// non-root entry causes tar.Writer.WriteHeader to return an error.
func TestTarDirectory_WriteHeaderError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}

	err := tarDirectory(errWriter{err: io.ErrClosedPipe}, dir)
	if err == nil {
		t.Fatal("expected error when underlying writer fails")
	}
}

// TestTarDirectory_WalkError exercises the `if err != nil { return err }` path at the
// top of the filepath.Walk callback. When the root directory does not exist, Walk calls
// the callback with err != nil for the root itself before any entry is visited.
func TestTarDirectory_WalkError(t *testing.T) {
	err := tarDirectory(io.Discard, filepath.Join(t.TempDir(), "nonexistent-subdir"))
	if err == nil {
		t.Fatal("expected error when walk root does not exist")
	}
}

// TestTarDirectory_SocketFile exercises the tar.FileInfoHeader error path in tarDirectory.
// Unix sockets are not supported by archive/tar, so FileInfoHeader returns
// "archive/tar: sockets not supported" when it encounters a socket file.
// The listener must remain open because net.Listen on "unix" removes the socket
// file when the listener is closed on Linux.
func TestTarDirectory_SocketFile(t *testing.T) {
	dir := t.TempDir()

	// Create a regular file so the directory is not empty.
	if err := os.WriteFile(filepath.Join(dir, "regular.txt"), []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}

	// Create a Unix domain socket inside the directory and keep it alive.
	sockPath := filepath.Join(dir, "test.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Skipf("cannot create Unix socket: %v", err)
	}
	defer ln.Close()

	err = tarDirectory(io.Discard, dir)
	if err == nil {
		t.Error("expected error when directory contains a Unix socket")
	}
}

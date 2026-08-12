package oci

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
)

// mockStore is a simple in-memory Store for testing.
type mockStore struct {
	name      string
	images    []Image
	tags      map[string]digest.Digest
	manifests map[digest.Digest][]byte
	blobs     map[digest.Digest][]byte
	events    chan Event
}

func newMockStore(name string) *mockStore {
	return &mockStore{
		name:      name,
		tags:      make(map[string]digest.Digest),
		manifests: make(map[digest.Digest][]byte),
		blobs:     make(map[digest.Digest][]byte),
		events:    make(chan Event, 16),
	}
}

func (m *mockStore) Name() string { return m.name }

func (m *mockStore) ListImages(_ context.Context) ([]Image, error) {
	return m.images, nil
}

func (m *mockStore) Resolve(_ context.Context, ref string) (digest.Digest, error) {
	d, ok := m.tags[ref]
	if !ok {
		return "", fmt.Errorf("not found: %s", ref)
	}
	return d, nil
}

func (m *mockStore) GetManifest(_ context.Context, dgst digest.Digest) ([]byte, string, error) {
	data, ok := m.manifests[dgst]
	if !ok {
		return nil, "", fmt.Errorf("not found: %s", dgst)
	}
	return data, "application/vnd.docker.distribution.manifest.v2+json", nil
}

func (m *mockStore) GetBlob(_ context.Context, dgst digest.Digest) (io.ReadCloser, int64, error) {
	data, ok := m.blobs[dgst]
	if !ok {
		return nil, 0, fmt.Errorf("not found: %s", dgst)
	}
	return io.NopCloser(strings.NewReader(string(data))), int64(len(data)), nil
}

func (m *mockStore) HasBlob(_ context.Context, dgst digest.Digest) bool {
	_, ok := m.blobs[dgst]
	return ok
}

func (m *mockStore) Subscribe(_ context.Context) (<-chan Event, error) {
	return m.events, nil
}

func TestMultiStore_Name(t *testing.T) {
	ms := NewMultiStore()
	if got := ms.Name(); got != "multi" {
		t.Errorf("Name() = %q, want %q", got, "multi")
	}
}

func TestMultiStore_ListImages_MergeDedup(t *testing.T) {
	ctx := context.Background()
	dgst1 := digest.FromBytes([]byte("image1"))
	dgst2 := digest.FromBytes([]byte("image2"))

	s1 := newMockStore("docker")
	s1.images = []Image{
		{Digest: dgst1, RepoTags: []string{"nginx:latest"}, Size: 100},
		{Digest: dgst2, RepoTags: []string{"alpine:3.18"}, Size: 50},
	}

	s2 := newMockStore("containerd")
	s2.images = []Image{
		{Digest: dgst1, RepoTags: []string{"docker.io/library/nginx:latest"}, Size: 100},
	}

	ms := NewMultiStore(s1, s2)
	images, err := ms.ListImages(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if len(images) != 2 {
		t.Errorf("ListImages() returned %d images, want 2", len(images))
	}

	// dgst1 should have merged tags
	for _, img := range images {
		if img.Digest == dgst1 {
			if len(img.RepoTags) != 2 {
				t.Errorf("dgst1 has %d tags, want 2: %v", len(img.RepoTags), img.RepoTags)
			}
		}
	}
}

func TestMultiStore_Resolve_PriorityOrder(t *testing.T) {
	ctx := context.Background()
	dgst1 := digest.FromBytes([]byte("d1"))
	dgst2 := digest.FromBytes([]byte("d2"))

	s1 := newMockStore("docker")
	s1.tags["nginx:latest"] = dgst1

	s2 := newMockStore("containerd")
	s2.tags["nginx:latest"] = dgst2
	s2.tags["alpine:3.18"] = dgst2

	ms := NewMultiStore(s1, s2)

	// First store wins for same ref
	d, err := ms.Resolve(ctx, "nginx:latest")
	if err != nil {
		t.Fatal(err)
	}
	if d != dgst1 {
		t.Errorf("Resolve(nginx:latest) = %s, want %s (from first store)", d, dgst1)
	}

	// Second store provides unique ref
	d, err = ms.Resolve(ctx, "alpine:3.18")
	if err != nil {
		t.Fatal(err)
	}
	if d != dgst2 {
		t.Errorf("Resolve(alpine:3.18) = %s, want %s", d, dgst2)
	}

	// Not found in any store
	_, err = ms.Resolve(ctx, "nonexistent:tag")
	if err == nil {
		t.Error("Resolve(nonexistent:tag) should fail")
	}
}

func TestMultiStore_GetBlob_PriorityOrder(t *testing.T) {
	ctx := context.Background()
	dgst := digest.FromBytes([]byte("blob-content"))

	s1 := newMockStore("docker")
	s1.blobs[dgst] = []byte("from-docker")

	s2 := newMockStore("containerd")
	s2.blobs[dgst] = []byte("from-containerd")

	ms := NewMultiStore(s1, s2)

	rc, _, err := ms.GetBlob(ctx, dgst)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	data, _ := io.ReadAll(rc)
	if string(data) != "from-docker" {
		t.Errorf("GetBlob() returned %q, want %q (first store)", string(data), "from-docker")
	}
}

func TestMultiStore_HasBlob_AnyStore(t *testing.T) {
	ctx := context.Background()
	dgst := digest.FromBytes([]byte("test"))

	s1 := newMockStore("docker")
	s2 := newMockStore("containerd")
	s2.blobs[dgst] = []byte("test")

	ms := NewMultiStore(s1, s2)

	if !ms.HasBlob(ctx, dgst) {
		t.Error("HasBlob() = false, want true (blob in second store)")
	}

	fakeDgst := digest.FromBytes([]byte("fake"))
	if ms.HasBlob(ctx, fakeDgst) {
		t.Error("HasBlob(fake) = true, want false")
	}
}

func TestMultiStore_Subscribe_FanIn(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	dgst1 := digest.FromBytes([]byte("ev1"))
	dgst2 := digest.FromBytes([]byte("ev2"))

	s1 := newMockStore("docker")
	s2 := newMockStore("containerd")

	ms := NewMultiStore(s1, s2)

	ch, err := ms.Subscribe(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Send events from both stores
	s1.events <- Event{Type: EventCreate, Digest: dgst1}
	s2.events <- Event{Type: EventCreate, Digest: dgst2}

	received := make(map[digest.Digest]bool)
	for i := 0; i < 2; i++ {
		select {
		case ev := <-ch:
			received[ev.Digest] = true
		case <-time.After(2 * time.Second):
			t.Fatal("timeout waiting for events")
		}
	}

	if !received[dgst1] || !received[dgst2] {
		t.Errorf("expected events from both stores, got: %v", received)
	}
}

func TestMultiStore_GetManifest(t *testing.T) {
	ctx := context.Background()
	dgst := digest.FromBytes([]byte("manifest"))

	s1 := newMockStore("docker")
	s2 := newMockStore("containerd")
	s2.manifests[dgst] = []byte(`{"schemaVersion":2}`)

	ms := NewMultiStore(s1, s2)

	data, mediaType, err := ms.GetManifest(ctx, dgst)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 {
		t.Error("GetManifest() returned empty data")
	}
	if mediaType == "" {
		t.Error("GetManifest() returned empty mediaType")
	}
}

func TestMultiStore_Empty(t *testing.T) {
	ctx := context.Background()
	ms := NewMultiStore()

	images, err := ms.ListImages(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(images) != 0 {
		t.Errorf("expected 0 images, got %d", len(images))
	}

	_, err = ms.Resolve(ctx, "anything")
	if err == nil {
		t.Error("Resolve should fail on empty MultiStore")
	}

	if ms.HasBlob(ctx, digest.FromBytes([]byte("x"))) {
		t.Error("HasBlob should return false on empty MultiStore")
	}
}

// TestDetectStores_NoStores verifies that DetectStores returns empty when no
// Docker or containerd storage directories are present.
func TestDetectStores_NoStores(t *testing.T) {
	root := t.TempDir()
	stores := DetectStores(context.Background(), root, root)
	if len(stores) != 0 {
		t.Errorf("expected 0 stores, got %d", len(stores))
	}
}

// TestDetectStores_DockerDirPresent verifies that DetectStores detects a Docker
// imagedb directory even when Init fails (no real data) — the detection code
// path itself is exercised.
func TestDetectStores_DockerDirPresent(t *testing.T) {
	dockerRoot := t.TempDir()
	// Create the directory that DockerStore detection looks for.
	imagedbDir := filepath.Join(dockerRoot, "image", "overlay2", "imagedb")
	if err := os.MkdirAll(imagedbDir, 0755); err != nil {
		t.Fatal(err)
	}

	containerdRoot := t.TempDir() // no containerd dirs → skipped

	// Init will fail because there's no real imagedb data, so stores is empty,
	// but the detection (os.Stat) branch is exercised.
	stores := DetectStores(context.Background(), dockerRoot, containerdRoot)
	// The store is not added when Init fails, so we only check for no panic.
	_ = stores
}

// TestDetectStores_ContainerdDirPresent verifies that DetectStores detects a
// containerd content directory even when Init fails.
func TestDetectStores_ContainerdDirPresent(t *testing.T) {
	dockerRoot := t.TempDir() // no Docker dirs → skipped

	containerdRoot := t.TempDir()
	blobsDir := filepath.Join(containerdRoot, "io.containerd.content.v1.content", "blobs")
	if err := os.MkdirAll(blobsDir, 0755); err != nil {
		t.Fatal(err)
	}

	stores := DetectStores(context.Background(), dockerRoot, containerdRoot)
	_ = stores
}

// TestDetectStores_FullDockerFixture verifies that DetectStores successfully
// initialises a DockerStore when a valid fixture directory is present.
func TestDetectStores_FullDockerFixture(t *testing.T) {
	root := setupDockerFixture(t)
	containerdRoot := t.TempDir()

	stores := DetectStores(context.Background(), root, containerdRoot)
	if len(stores) != 1 {
		t.Fatalf("expected 1 store (docker), got %d", len(stores))
	}
	if stores[0].Name() != "docker" {
		t.Errorf("store name = %q, want %q", stores[0].Name(), "docker")
	}
}

// TestDetectStores_DockerInitFails covers the slog.Warn("docker store init failed")
// path in DetectStores when the imagedb directory exists but Init returns an error.
// We force Init to fail by writing invalid JSON to repositories.json.
func TestDetectStores_DockerInitFails(t *testing.T) {
	dockerRoot := t.TempDir()
	// Create the imagedb directory so DetectStores' os.Stat check passes.
	imagedbDir := filepath.Join(dockerRoot, "image", "overlay2", "imagedb")
	if err := os.MkdirAll(imagedbDir, 0755); err != nil {
		t.Fatal(err)
	}
	// Write invalid JSON to repositories.json so loadRepositories (and thus Init) fails.
	reposPath := filepath.Join(dockerRoot, "image", "overlay2", "repositories.json")
	if err := os.WriteFile(reposPath, []byte("{invalid json content"), 0644); err != nil {
		t.Fatal(err)
	}

	containerdRoot := t.TempDir() // no containerd dirs
	stores := DetectStores(context.Background(), dockerRoot, containerdRoot)
	// Init failed, so the store should NOT be added.
	if len(stores) != 0 {
		t.Errorf("expected 0 stores when docker init fails, got %d", len(stores))
	}
}

// TestDetectStores_ContainerdInitFails covers the slog.Warn("containerd store init failed")
// path in DetectStores when the blobs directory exists but Init returns an error.
// We force Init to fail by making the sha256 content dir a regular file so ReadDir fails.
func TestDetectStores_ContainerdInitFails(t *testing.T) {
	dockerRoot := t.TempDir() // no Docker dirs

	containerdRoot := t.TempDir()
	// Create the blobs directory so DetectStores' os.Stat check passes.
	blobsDir := filepath.Join(containerdRoot, "io.containerd.content.v1.content", "blobs")
	if err := os.MkdirAll(blobsDir, 0755); err != nil {
		t.Fatal(err)
	}
	// Create sha256 as a regular file so os.ReadDir(sha256) in scanContentStore fails.
	sha256Path := filepath.Join(blobsDir, "sha256")
	if err := os.WriteFile(sha256Path, []byte("not a directory"), 0644); err != nil {
		t.Fatal(err)
	}

	stores := DetectStores(context.Background(), dockerRoot, containerdRoot)
	// Init failed (scanContentStore returns ENOTDIR), so the store should NOT be added.
	if len(stores) != 0 {
		t.Errorf("expected 0 stores when containerd init fails, got %d", len(stores))
	}
}

// errStore is a Store that returns errors from ListImages and Subscribe.
type errStore struct {
	*mockStore
}

func (e *errStore) ListImages(_ context.Context) ([]Image, error) {
	return nil, fmt.Errorf("store unavailable")
}

func (e *errStore) Subscribe(_ context.Context) (<-chan Event, error) {
	return nil, fmt.Errorf("subscribe unavailable")
}

// TestMultiStore_GetManifest_AllMiss covers the "manifest not found in any store" error return.
func TestMultiStore_GetManifest_AllMiss(t *testing.T) {
	ctx := context.Background()
	dgst := digest.FromBytes([]byte("unknown-manifest"))

	s1 := newMockStore("docker")
	s2 := newMockStore("containerd")
	ms := NewMultiStore(s1, s2)

	_, _, err := ms.GetManifest(ctx, dgst)
	if err == nil {
		t.Fatal("expected error when manifest not in any store")
	}
}

// TestMultiStore_GetBlob_AllMiss covers the "blob not found in any store" error return.
func TestMultiStore_GetBlob_AllMiss(t *testing.T) {
	ctx := context.Background()
	dgst := digest.FromBytes([]byte("unknown-blob"))

	s1 := newMockStore("docker")
	s2 := newMockStore("containerd")
	ms := NewMultiStore(s1, s2)

	_, _, err := ms.GetBlob(ctx, dgst)
	if err == nil {
		t.Fatal("expected error when blob not in any store")
	}
}

// TestMultiStore_ListImages_StoreError covers the slog.Warn path when a store's
// ListImages returns an error — it should be skipped and the other store's images returned.
func TestMultiStore_ListImages_StoreError(t *testing.T) {
	ctx := context.Background()
	dgst := digest.FromBytes([]byte("img1"))

	good := newMockStore("good")
	good.images = []Image{{Digest: dgst, RepoTags: []string{"nginx:latest"}}}

	bad := &errStore{newMockStore("bad")}

	ms := NewMultiStore(bad, good) // bad first — it errors, good should still be queried
	images, err := ms.ListImages(ctx)
	if err != nil {
		t.Fatalf("ListImages() unexpected error: %v", err)
	}
	if len(images) != 1 {
		t.Errorf("ListImages() returned %d images, want 1", len(images))
	}
}

// TestMultiStore_Subscribe_StoreError covers the slog.Warn path when a store's
// Subscribe returns an error — it should be skipped and the other store subscribed.
func TestMultiStore_Subscribe_StoreError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	dgst := digest.FromBytes([]byte("ev1"))

	good := newMockStore("good")
	bad := &errStore{newMockStore("bad")}

	ms := NewMultiStore(bad, good)
	ch, err := ms.Subscribe(ctx)
	if err != nil {
		t.Fatalf("Subscribe() unexpected error: %v", err)
	}

	good.events <- Event{Type: EventCreate, Digest: dgst}

	select {
	case ev := <-ch:
		if ev.Digest != dgst {
			t.Errorf("received event digest %s, want %s", ev.Digest, dgst)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for event from good store")
	}
}

// TestMultiStore_Subscribe_CtxDone covers the ctx.Done() branch in the fan-in goroutine.
// It fills the merged channel buffer so the goroutine blocks, then cancels the context
// to force the ctx.Done() select case.
func TestMultiStore_Subscribe_CtxDone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	s := newMockStore("docker")
	ms := NewMultiStore(s)

	ch, err := ms.Subscribe(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Fill the merged channel buffer (capacity 16) without draining it.
	for i := 0; i < 16; i++ {
		dgst := digest.FromBytes([]byte(fmt.Sprintf("fill-%d", i)))
		s.events <- Event{Type: EventCreate, Digest: dgst}
	}

	// Cancel context so the goroutine picks ctx.Done() when it tries to send the 17th event.
	cancel()

	// Send one more event — goroutine should exit via ctx.Done() since merged is full.
	s.events <- Event{Type: EventCreate, Digest: digest.FromBytes([]byte("overflow"))}

	// Close the store channel to unblock range and let the wg.Wait goroutine close merged.
	close(s.events)

	// Drain merged so the test can finish cleanly.
	for range ch {
	}
}

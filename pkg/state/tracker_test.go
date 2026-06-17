package state

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"

	"github.com/outpostos/outpostimage/pkg/oci"
)

// mockStore implements oci.Store for testing.
type mockStore struct {
	images []oci.Image
	events chan oci.Event
}

func (m *mockStore) Name() string { return "mock" }

func (m *mockStore) ListImages(_ context.Context) ([]oci.Image, error) {
	return m.images, nil
}

func (m *mockStore) Resolve(_ context.Context, _ string) (digest.Digest, error) {
	return "", nil
}

func (m *mockStore) GetManifest(_ context.Context, _ digest.Digest) ([]byte, string, error) {
	return nil, "", nil
}

func (m *mockStore) GetBlob(_ context.Context, _ digest.Digest) (io.ReadCloser, int64, error) {
	return nil, 0, nil
}

func (m *mockStore) HasBlob(_ context.Context, _ digest.Digest) bool {
	return false
}

func (m *mockStore) Subscribe(_ context.Context) (<-chan oci.Event, error) {
	return m.events, nil
}

func TestTracker_InitialScan(t *testing.T) {
	d1, _ := digest.Parse("sha256:aaaa000000000000000000000000000000000000000000000000000000000000")
	d2, _ := digest.Parse("sha256:bbbb000000000000000000000000000000000000000000000000000000000000")

	store := &mockStore{
		images: []oci.Image{
			{Digest: d1, RepoTags: []string{"nginx:latest"}},
			{Digest: d2, RepoTags: []string{"redis:latest"}},
		},
		events: make(chan oci.Event),
	}

	tracker := NewTracker(store)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- tracker.Track(ctx) }()

	// Wait for initial scan
	time.Sleep(100 * time.Millisecond)

	if !tracker.HasDigest(d1) {
		t.Error("expected d1 to be in index")
	}
	if !tracker.HasDigest(d2) {
		t.Error("expected d2 to be in index")
	}
	if tracker.Count() != 2 {
		t.Errorf("count = %d, want 2", tracker.Count())
	}

	cancel()
}

func TestTracker_IncrementalUpdate(t *testing.T) {
	d1, _ := digest.Parse("sha256:aaaa000000000000000000000000000000000000000000000000000000000000")
	d2, _ := digest.Parse("sha256:bbbb000000000000000000000000000000000000000000000000000000000000")

	events := make(chan oci.Event, 10)
	store := &mockStore{
		images: []oci.Image{
			{Digest: d1},
		},
		events: events,
	}

	tracker := NewTracker(store)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- tracker.Track(ctx) }()

	time.Sleep(100 * time.Millisecond)

	// Add d2 via event
	events <- oci.Event{Type: oci.EventCreate, Digest: d2}
	time.Sleep(100 * time.Millisecond)

	if !tracker.HasDigest(d2) {
		t.Error("expected d2 to be added via event")
	}
	if tracker.Count() != 2 {
		t.Errorf("count = %d, want 2", tracker.Count())
	}

	// Remove d1 via event
	events <- oci.Event{Type: oci.EventDelete, Digest: d1}
	time.Sleep(100 * time.Millisecond)

	if tracker.HasDigest(d1) {
		t.Error("expected d1 to be removed via event")
	}
	if tracker.Count() != 1 {
		t.Errorf("count = %d, want 1", tracker.Count())
	}

	cancel()
}

func TestTracker_AllDigests(t *testing.T) {
	d1, _ := digest.Parse("sha256:aaaa000000000000000000000000000000000000000000000000000000000000")

	store := &mockStore{
		images: []oci.Image{{Digest: d1}},
		events: make(chan oci.Event),
	}

	tracker := NewTracker(store)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() { _ = tracker.Track(ctx) }()
	time.Sleep(100 * time.Millisecond)

	all := tracker.AllDigests()
	if len(all) != 1 || all[0] != d1 {
		t.Errorf("AllDigests = %v, want [%s]", all, d1)
	}
}

func TestTracker_NonexistentDigest(t *testing.T) {
	store := &mockStore{
		images: nil,
		events: make(chan oci.Event),
	}

	tracker := NewTracker(store)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() { _ = tracker.Track(ctx) }()
	time.Sleep(100 * time.Millisecond)

	fake, _ := digest.Parse("sha256:0000000000000000000000000000000000000000000000000000000000000000")
	if tracker.HasDigest(fake) {
		t.Error("expected false for nonexistent digest")
	}
}

package state

import (
	"context"
	"log/slog"
	"sync"

	"github.com/opencontainers/go-digest"

	"github.com/outpostos/outpostimage/pkg/oci"
)

// Tracker maintains an in-memory index of all digests available in the local store.
// It performs an initial full scan and then subscribes to store events for incremental updates.
type Tracker struct {
	store oci.Store

	mu      sync.RWMutex
	digests map[digest.Digest]struct{}
}

// NewTracker creates a new Tracker backed by the given store.
func NewTracker(store oci.Store) *Tracker {
	return &Tracker{
		store:   store,
		digests: make(map[digest.Digest]struct{}),
	}
}

// Track performs an initial full scan and then subscribes to store events.
// It blocks until the context is cancelled. Intended to run in an errgroup.
func (t *Tracker) Track(ctx context.Context) error {
	// Phase 1: Initial full scan
	images, err := t.store.ListImages(ctx)
	if err != nil {
		return err
	}

	t.mu.Lock()
	for _, img := range images {
		t.digests[img.Digest] = struct{}{}
	}
	t.mu.Unlock()

	slog.Info("state tracker initial scan complete",
		"store", t.store.Name(),
		"digests", len(images),
	)

	// Phase 2: Subscribe to store events for incremental updates
	ch, err := t.store.Subscribe(ctx)
	if err != nil {
		return err
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case event, ok := <-ch:
			if !ok {
				return nil
			}
			t.mu.Lock()
			switch event.Type {
			case oci.EventCreate:
				t.digests[event.Digest] = struct{}{}
				slog.Debug("digest added", "digest", event.Digest)
			case oci.EventDelete:
				delete(t.digests, event.Digest)
				slog.Debug("digest removed", "digest", event.Digest)
			}
			t.mu.Unlock()
		}
	}
}

// HasDigest reports whether the given digest is in the local index.
func (t *Tracker) HasDigest(dgst digest.Digest) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	_, ok := t.digests[dgst]
	return ok
}

// AllDigests returns a copy of all digests in the local index.
func (t *Tracker) AllDigests() []digest.Digest {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]digest.Digest, 0, len(t.digests))
	for d := range t.digests {
		out = append(out, d)
	}
	return out
}

// Count returns the number of digests in the index.
func (t *Tracker) Count() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return len(t.digests)
}

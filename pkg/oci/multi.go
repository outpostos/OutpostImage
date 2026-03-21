package oci

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"

	"github.com/opencontainers/go-digest"
)

// MultiStore wraps multiple Store implementations and provides a unified view.
// ListImages merges and deduplicates across all stores.
// GetBlob/GetManifest queries stores in priority order (first match wins).
// Subscribe fans-in events from all stores.
type MultiStore struct {
	mu     sync.RWMutex
	stores []Store
}

// NewMultiStore creates a MultiStore from the given stores.
// Stores are queried in order; the first store to satisfy a request wins.
func NewMultiStore(stores ...Store) *MultiStore {
	return &MultiStore{stores: stores}
}

// Name returns "multi".
func (ms *MultiStore) Name() string {
	return "multi"
}

// ListImages returns all images from all stores, deduplicated by digest.
func (ms *MultiStore) ListImages(ctx context.Context) ([]Image, error) {
	ms.mu.RLock()
	defer ms.mu.RUnlock()

	seen := make(map[digest.Digest]int) // digest → index in result
	var result []Image

	for _, s := range ms.stores {
		images, err := s.ListImages(ctx)
		if err != nil {
			slog.Warn("store ListImages failed", "store", s.Name(), "error", err)
			continue
		}
		for _, img := range images {
			if idx, ok := seen[img.Digest]; ok {
				// Merge tags from additional stores
				existing := result[idx]
				tagSet := make(map[string]struct{})
				for _, t := range existing.RepoTags {
					tagSet[t] = struct{}{}
				}
				for _, t := range img.RepoTags {
					if _, exists := tagSet[t]; !exists {
						result[idx].RepoTags = append(result[idx].RepoTags, t)
					}
				}
			} else {
				seen[img.Digest] = len(result)
				result = append(result, img)
			}
		}
	}
	return result, nil
}

// Resolve resolves a reference by querying stores in order.
func (ms *MultiStore) Resolve(ctx context.Context, ref string) (digest.Digest, error) {
	ms.mu.RLock()
	defer ms.mu.RUnlock()

	for _, s := range ms.stores {
		d, err := s.Resolve(ctx, ref)
		if err == nil {
			return d, nil
		}
	}
	return "", fmt.Errorf("reference not found in any store: %s", ref)
}

// GetManifest returns the manifest from the first store that has it.
func (ms *MultiStore) GetManifest(ctx context.Context, dgst digest.Digest) ([]byte, string, error) {
	ms.mu.RLock()
	defer ms.mu.RUnlock()

	for _, s := range ms.stores {
		data, mediaType, err := s.GetManifest(ctx, dgst)
		if err == nil {
			return data, mediaType, nil
		}
	}
	return nil, "", fmt.Errorf("manifest not found in any store: %s", dgst)
}

// GetBlob returns the blob from the first store that has it.
func (ms *MultiStore) GetBlob(ctx context.Context, dgst digest.Digest) (io.ReadCloser, int64, error) {
	ms.mu.RLock()
	defer ms.mu.RUnlock()

	for _, s := range ms.stores {
		rc, size, err := s.GetBlob(ctx, dgst)
		if err == nil {
			return rc, size, nil
		}
	}
	return nil, 0, fmt.Errorf("blob not found in any store: %s", dgst)
}

// HasBlob reports whether any store has the blob.
func (ms *MultiStore) HasBlob(ctx context.Context, dgst digest.Digest) bool {
	ms.mu.RLock()
	defer ms.mu.RUnlock()

	for _, s := range ms.stores {
		if s.HasBlob(ctx, dgst) {
			return true
		}
	}
	return false
}

// Subscribe fans-in events from all stores into a single channel.
func (ms *MultiStore) Subscribe(ctx context.Context) (<-chan Event, error) {
	ms.mu.RLock()
	defer ms.mu.RUnlock()

	merged := make(chan Event, 16)
	var wg sync.WaitGroup

	for _, s := range ms.stores {
		ch, err := s.Subscribe(ctx)
		if err != nil {
			slog.Warn("store Subscribe failed", "store", s.Name(), "error", err)
			continue
		}
		wg.Add(1)
		go func(storeCh <-chan Event) {
			defer wg.Done()
			for ev := range storeCh {
				select {
				case merged <- ev:
				case <-ctx.Done():
					return
				}
			}
		}(ch)
	}

	go func() {
		wg.Wait()
		close(merged)
	}()

	return merged, nil
}

// DetectStores auto-detects available container runtime stores.
// It checks for Docker (/var/lib/docker) and containerd (/var/lib/containerd)
// storage directories and returns initialized stores.
func DetectStores(ctx context.Context, dockerRoot, containerdRoot string) []Store {
	var stores []Store

	// Check Docker
	dockerImageDB := dockerRoot + "/image/overlay2/imagedb"
	if _, err := os.Stat(dockerImageDB); err == nil {
		ds := NewDockerStore(dockerRoot)
		if err := ds.Init(ctx); err != nil {
			slog.Warn("docker store init failed", "root", dockerRoot, "error", err)
		} else {
			stores = append(stores, ds)
			slog.Info("detected docker store", "root", dockerRoot)
		}
	}

	// Check containerd
	containerdContent := containerdRoot + "/io.containerd.content.v1.content/blobs"
	if _, err := os.Stat(containerdContent); err == nil {
		cs := NewContainerdStore(containerdRoot)
		if err := cs.Init(ctx); err != nil {
			slog.Warn("containerd store init failed", "root", containerdRoot, "error", err)
		} else {
			stores = append(stores, cs)
			slog.Info("detected containerd store", "root", containerdRoot)
		}
	}

	return stores
}

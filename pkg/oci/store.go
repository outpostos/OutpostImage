package oci

import (
	"context"
	"io"

	"github.com/opencontainers/go-digest"
)

// Store provides read-only access to an OCI-compatible container image store.
// Implementations adapt specific container runtimes (Docker overlay2, containerd)
// to expose their local images via a unified interface.
type Store interface {
	// Name returns the name of this store implementation (e.g., "docker", "containerd").
	Name() string

	// ListImages returns all images currently available in the local store.
	ListImages(ctx context.Context) ([]Image, error)

	// Resolve resolves a human-readable image reference (e.g., "nginx:latest")
	// to a manifest digest.
	Resolve(ctx context.Context, ref string) (digest.Digest, error)

	// GetManifest returns the raw manifest bytes and its media type for the given digest.
	GetManifest(ctx context.Context, dgst digest.Digest) ([]byte, string, error)

	// GetBlob returns a ReadCloser for reading the blob content and its size.
	// The caller is responsible for closing the reader.
	GetBlob(ctx context.Context, dgst digest.Digest) (io.ReadCloser, int64, error)

	// HasBlob reports whether a blob with the given digest exists in the store.
	HasBlob(ctx context.Context, dgst digest.Digest) bool

	// Subscribe returns a channel that emits events when the store contents change.
	// The channel is closed when the context is cancelled.
	Subscribe(ctx context.Context) (<-chan Event, error)
}

// Image represents a container image in the local store.
type Image struct {
	// Digest is the manifest digest (e.g., "sha256:abc123...").
	Digest digest.Digest

	// RepoTags is the list of repository:tag references for this image
	// (e.g., ["docker.io/library/nginx:latest"]).
	RepoTags []string

	// Size is the total size of the image in bytes (sum of all layers).
	Size int64
}

// EventType indicates the kind of store change event.
type EventType int

const (
	// EventCreate indicates a new image or blob was added to the store.
	EventCreate EventType = iota + 1

	// EventDelete indicates an image or blob was removed from the store.
	EventDelete
)

// Event represents a change in the container image store.
type Event struct {
	// Type is the kind of event (create or delete).
	Type EventType

	// Digest is the content digest affected by this event.
	Digest digest.Digest

	// Image is the associated image, if available.
	// May be empty for blob-level events.
	Image string
}

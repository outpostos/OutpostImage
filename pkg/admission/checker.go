// Package admission implements artifact hash whitelist admission control.
//
// Product requirement (framework clause 6.1.2.5.2.6(3), ticket T12): edge
// nodes verify artifacts against a hash whitelist table. On the pull path
// the image manifest digest is checked against a locally cached whitelist;
// digests not in the table are rejected with an alert. Scanning and signature
// verification are handled by external security vendor products — this
// component only performs digest comparison admission.
//
// The whitelist is fully loaded into memory, so verification keeps working
// while the node is offline. The file is hot-reloaded by mtime polling; a
// corrupt update keeps the previous table serving (fail-safe, not fail-open).
package admission

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/opencontainers/go-digest"
)

// MaxDigestEntryLen is the maximum accepted length of a single digest entry
// in the whitelist file. A sha256 digest string is 71 bytes
// ("sha256:" + 64 hex chars); the cap leaves headroom for longer algorithms
// while rejecting unbounded external input.
const MaxDigestEntryLen = 256

// Whitelist is the on-disk JSON format of the digest whitelist table.
type Whitelist struct {
	// Version identifies the whitelist table revision (opaque string).
	Version string `json:"version"`
	// Digests lists the allowed manifest digests ("sha256:<hex>").
	Digests []string `json:"digests"`
	// Signature is reserved for future whitelist signature verification.
	// It is carried but not validated by this component.
	Signature string `json:"signature,omitempty"`
}

// Checker verifies manifest digests against an in-memory whitelist table
// loaded from a JSON file. All lookups are served from memory, so admission
// keeps working when the network (or even the file) becomes unavailable.
type Checker struct {
	path string

	mu      sync.RWMutex
	allowed map[digest.Digest]struct{}
	version string
	mtime   time.Time
	size    int64
}

// NewChecker loads the whitelist file at path and returns a Checker.
// A missing or invalid file is a hard error (fail-fast): when admission is
// configured, it must never be silently skipped.
func NewChecker(path string) (*Checker, error) {
	c := &Checker{path: path}
	if err := c.Reload(); err != nil {
		return nil, fmt.Errorf("admission whitelist load failed: %w", err)
	}
	return c, nil
}

// Reload re-reads the whitelist file. On any error the previous in-memory
// table is kept serving (fail-safe, not fail-open) and the error is returned.
func (c *Checker) Reload() error {
	f, err := os.Open(c.path)
	if err != nil {
		return err
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return err
	}

	var wl Whitelist
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&wl); err != nil {
		return fmt.Errorf("parse %s: %w", c.path, err)
	}

	table := make(map[digest.Digest]struct{}, len(wl.Digests))
	for i, s := range wl.Digests {
		if len(s) > MaxDigestEntryLen {
			return fmt.Errorf("digest entry %d exceeds max length %d", i, MaxDigestEntryLen)
		}
		d, err := digest.Parse(s)
		if err != nil {
			return fmt.Errorf("digest entry %d invalid: %w", i, err)
		}
		table[d] = struct{}{}
	}

	c.mu.Lock()
	c.allowed = table
	c.version = wl.Version
	c.mtime = fi.ModTime()
	c.size = fi.Size()
	c.mu.Unlock()

	slog.Info("admission whitelist loaded",
		"path", c.path, "version", wl.Version, "digests", len(table))
	return nil
}

// Allowed reports whether the given manifest digest is in the whitelist.
// The lookup is fully in-memory and never touches disk or network.
func (c *Checker) Allowed(dgst digest.Digest) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	_, ok := c.allowed[dgst]
	return ok
}

// Version returns the version of the currently active whitelist table.
func (c *Checker) Version() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.version
}

// Watch polls the whitelist file for changes (mtime or size) every interval
// and hot-reloads the table when it changes. A failed reload (missing file,
// corrupt JSON, invalid entry) logs an alert and keeps the old table serving.
// Watch blocks until ctx is cancelled and then returns nil.
func (c *Checker) Watch(ctx context.Context, interval time.Duration) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			fi, err := os.Stat(c.path)
			if err != nil {
				// File temporarily missing (e.g., mid-replace): keep old table.
				slog.Warn("admission whitelist stat failed, keeping current table",
					"path", c.path, "error", err)
				continue
			}
			c.mu.RLock()
			changed := !fi.ModTime().Equal(c.mtime) || fi.Size() != c.size
			c.mu.RUnlock()
			if !changed {
				continue
			}
			if err := c.Reload(); err != nil {
				slog.Error("admission whitelist reload failed, keeping current table",
					"path", c.path, "error", err)
				// Record the observed stat so we do not re-parse the same
				// broken file every tick; a subsequent change retriggers.
				c.mu.Lock()
				c.mtime = fi.ModTime()
				c.size = fi.Size()
				c.mu.Unlock()
			}
		}
	}
}

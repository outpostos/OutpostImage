package admission

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
)

var (
	digestA = digest.FromString("manifest-a")
	digestB = digest.FromString("manifest-b")
)

func writeWhitelist(t *testing.T, path, version string, digests ...digest.Digest) {
	t.Helper()
	strs := make([]string, len(digests))
	for i, d := range digests {
		strs[i] = d.String()
	}
	data, err := json.Marshal(Whitelist{Version: version, Digests: strs})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestNewChecker_LoadsWhitelist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "whitelist.json")
	writeWhitelist(t, path, "v1", digestA)

	c, err := NewChecker(path)
	if err != nil {
		t.Fatalf("NewChecker() error = %v", err)
	}
	if !c.Allowed(digestA) {
		t.Error("digestA should be allowed")
	}
	if c.Allowed(digestB) {
		t.Error("digestB should be denied")
	}
	if got := c.Version(); got != "v1" {
		t.Errorf("Version() = %q, want %q", got, "v1")
	}
}

func TestNewChecker_MissingFileFailsFast(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist.json")
	if _, err := NewChecker(path); err == nil {
		t.Fatal("NewChecker() with missing file should fail fast, got nil error")
	}
}

func TestNewChecker_InvalidJSONFailsFast(t *testing.T) {
	path := filepath.Join(t.TempDir(), "whitelist.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewChecker(path); err == nil {
		t.Fatal("NewChecker() with invalid JSON should fail, got nil error")
	}
}

func TestNewChecker_InvalidDigestEntryFailsFast(t *testing.T) {
	path := filepath.Join(t.TempDir(), "whitelist.json")
	data, _ := json.Marshal(Whitelist{Version: "v1", Digests: []string{"not-a-digest"}})
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewChecker(path); err == nil {
		t.Fatal("NewChecker() with invalid digest entry should fail, got nil error")
	}
}

func TestNewChecker_OversizedDigestEntryFailsFast(t *testing.T) {
	path := filepath.Join(t.TempDir(), "whitelist.json")
	long := "sha256:" + strings.Repeat("a", MaxDigestEntryLen)
	data, _ := json.Marshal(Whitelist{Version: "v1", Digests: []string{long}})
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewChecker(path); err == nil {
		t.Fatal("NewChecker() with oversized digest entry should fail, got nil error")
	}
}

func TestChecker_OfflineAfterLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "whitelist.json")
	writeWhitelist(t, path, "v1", digestA)

	c, err := NewChecker(path)
	if err != nil {
		t.Fatalf("NewChecker() error = %v", err)
	}
	// Whitelist is fully in-memory: removing the file must not affect checks.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if !c.Allowed(digestA) {
		t.Error("digestA should still be allowed after file removal")
	}
	if c.Allowed(digestB) {
		t.Error("digestB should still be denied after file removal")
	}
}

func TestChecker_ReloadCorruptKeepsOldTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "whitelist.json")
	writeWhitelist(t, path, "v1", digestA)

	c, err := NewChecker(path)
	if err != nil {
		t.Fatalf("NewChecker() error = %v", err)
	}

	if err := os.WriteFile(path, []byte("{corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.Reload(); err == nil {
		t.Fatal("Reload() with corrupt JSON should return error")
	}
	// fail-safe, not fail-open: old table keeps serving
	if !c.Allowed(digestA) {
		t.Error("digestA should still be allowed after failed reload")
	}
	if c.Allowed(digestB) {
		t.Error("digestB should still be denied after failed reload")
	}
	if got := c.Version(); got != "v1" {
		t.Errorf("Version() = %q, want old %q", got, "v1")
	}
}

func TestChecker_WatchHotReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "whitelist.json")
	writeWhitelist(t, path, "v1", digestA)

	c, err := NewChecker(path)
	if err != nil {
		t.Fatalf("NewChecker() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.Watch(ctx, 10*time.Millisecond) }()

	// Replace the whitelist with a new table (mtime bumped explicitly to
	// defeat coarse filesystem timestamp granularity).
	writeWhitelist(t, path, "v2", digestB)
	if err := os.Chtimes(path, time.Now().Add(2*time.Second), time.Now().Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if c.Allowed(digestB) && !c.Allowed(digestA) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !c.Allowed(digestB) {
		t.Error("digestB should be allowed after hot reload")
	}
	if c.Allowed(digestA) {
		t.Error("digestA should be denied after hot reload")
	}
	if got := c.Version(); got != "v2" {
		t.Errorf("Version() = %q, want %q", got, "v2")
	}

	cancel()
	if err := <-done; err != nil {
		t.Errorf("Watch() error = %v", err)
	}
}

func TestChecker_WatchCorruptUpdateKeepsOldTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "whitelist.json")
	writeWhitelist(t, path, "v1", digestA)

	c, err := NewChecker(path)
	if err != nil {
		t.Fatalf("NewChecker() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = c.Watch(ctx, 10*time.Millisecond) }()

	if err := os.WriteFile(path, []byte("{corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, time.Now().Add(2*time.Second), time.Now().Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}

	time.Sleep(200 * time.Millisecond)
	if !c.Allowed(digestA) {
		t.Error("digestA should still be allowed after corrupt update")
	}
	if got := c.Version(); got != "v1" {
		t.Errorf("Version() = %q, want old %q", got, "v1")
	}
}

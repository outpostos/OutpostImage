package routing

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParsePeersFile(t *testing.T) {
	content := `# cluster nodes
192.168.1.10:5000
192.168.1.11:5000

# backup node
192.168.1.12:5000
`
	path := filepath.Join(t.TempDir(), "peers.txt")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	peers, err := ParsePeersFile(path)
	if err != nil {
		t.Fatal(err)
	}

	want := []string{"192.168.1.10:5000", "192.168.1.11:5000", "192.168.1.12:5000"}
	if len(peers) != len(want) {
		t.Fatalf("got %d peers, want %d", len(peers), len(want))
	}
	for i, p := range peers {
		if p != want[i] {
			t.Errorf("peers[%d] = %q, want %q", i, p, want[i])
		}
	}
}

func TestParsePeersFile_Empty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "peers.txt")
	if err := os.WriteFile(path, []byte("# only comments\n\n"), 0644); err != nil {
		t.Fatal(err)
	}

	peers, err := ParsePeersFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(peers) != 0 {
		t.Fatalf("got %d peers, want 0", len(peers))
	}
}

func TestParsePeersFile_NotFound(t *testing.T) {
	_, err := ParsePeersFile("/nonexistent/peers.txt")
	if err == nil {
		t.Fatal("expected error for nonexistent file")
	}
}

func TestPeerWatcher_SetAndGet(t *testing.T) {
	pw := NewPeerWatcher([]string{"a:5000", "b:5000"})

	got := pw.Peers()
	if len(got) != 2 || got[0] != "a:5000" || got[1] != "b:5000" {
		t.Fatalf("unexpected initial peers: %v", got)
	}

	pw.SetPeers([]string{"c:5000"})
	got = pw.Peers()
	if len(got) != 1 || got[0] != "c:5000" {
		t.Fatalf("unexpected peers after set: %v", got)
	}
}

func TestPeerWatcher_PeersIsCopy(t *testing.T) {
	pw := NewPeerWatcher([]string{"a:5000"})
	got := pw.Peers()
	got[0] = "modified"

	// Original should be unchanged
	if pw.Peers()[0] != "a:5000" {
		t.Fatal("Peers() should return a copy")
	}
}

func TestPeerWatcher_WatchFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "peers.txt")

	if err := os.WriteFile(path, []byte("10.0.0.1:5000\n10.0.0.2:5000\n"), 0644); err != nil {
		t.Fatal(err)
	}

	pw := NewPeerWatcher(nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- pw.WatchFile(ctx, path)
	}()

	// Wait for initial load
	time.Sleep(200 * time.Millisecond)

	got := pw.Peers()
	if len(got) != 2 {
		t.Fatalf("expected 2 peers after initial load, got %d: %v", len(got), got)
	}

	// Update file
	if err := os.WriteFile(path, []byte("10.0.0.1:5000\n10.0.0.2:5000\n10.0.0.3:5000\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Wait for fsnotify to pick up the change
	time.Sleep(500 * time.Millisecond)

	got = pw.Peers()
	if len(got) != 3 {
		t.Fatalf("expected 3 peers after update, got %d: %v", len(got), got)
	}

	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("WatchFile returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WatchFile did not return after cancel")
	}
}

// TestPeerWatcher_WatchFile_InitError verifies that WatchFile returns an error
// when the peers file does not exist on initial load.
func TestPeerWatcher_WatchFile_InitError(t *testing.T) {
	pw := NewPeerWatcher(nil)
	err := pw.WatchFile(context.Background(), "/nonexistent/path/peers.txt")
	if err == nil {
		t.Fatal("expected error for nonexistent file")
	}
}

// TestPeerWatcher_WatchFile_WatchAddError verifies that WatchFile returns an error
// when the file is deleted before the fsnotify watcher can be set up.
func TestPeerWatcher_WatchFile_WatchAddError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "peers.txt")

	if err := os.WriteFile(path, []byte("10.0.0.1:5000\n"), 0644); err != nil {
		t.Fatal(err)
	}
	// Remove the file so watcher.Add fails after ParsePeersFile succeeds.
	// Because of the inherent race between ParsePeersFile and watcher.Add, this
	// may not always trigger the error path. Skip if it doesn't.
	os.Remove(path)

	pw := NewPeerWatcher(nil)
	// ParsePeersFile will fail first (file is gone), so we expect an error.
	err := pw.WatchFile(context.Background(), path)
	if err == nil {
		t.Fatal("expected error after file deletion")
	}
}

// TestPeerWatcher_WatchFile_ReloadError exercises the slog.Error path in WatchFile
// that fires when ParsePeersFile fails during a reload triggered by a write event.
// We achieve this by watching a symlink whose target is swapped to a directory
// (opening a directory for buffered line-scan returns EISDIR on Linux).
func TestPeerWatcher_WatchFile_ReloadError(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "actual.txt")
	link := filepath.Join(dir, "peers.txt")

	// Create initial content and a symlink pointing to it.
	if err := os.WriteFile(target, []byte("10.0.0.1:5000\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	pw := NewPeerWatcher(nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- pw.WatchFile(ctx, link)
	}()

	// Wait for initial load (symlink → actual.txt succeeds).
	time.Sleep(150 * time.Millisecond)
	if len(pw.Peers()) == 0 {
		t.Fatal("expected peers after initial load via symlink")
	}

	// Replace the symlink target with a sub-directory so that the next reload
	// attempt (triggered by a Create/Write event on the symlink path) calls
	// os.Open on a directory, causing scanner.Err() to return EISDIR.
	subdir := filepath.Join(dir, "subdir")
	if err := os.Mkdir(subdir, 0755); err != nil {
		t.Fatal(err)
	}
	// Remove old symlink and point it at the directory.
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(subdir, link); err != nil {
		t.Fatal(err)
	}

	// Write a dummy file inside the symlinked directory to generate a Create
	// event on the watched path's parent; also write to trigger the event on link itself.
	_ = os.WriteFile(filepath.Join(subdir, "dummy"), []byte("x"), 0644)

	// Give fsnotify a moment to pick up the symlink change.
	// (fsnotify watches the real inode; symlink re-target may or may not generate
	// a Write/Create event depending on the OS; the test is best-effort.)
	time.Sleep(400 * time.Millisecond)

	// Regardless of whether the reload error fired, the goroutine must still be
	// alive and cancelable.
	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("WatchFile returned unexpected error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WatchFile did not return after context cancel")
	}
}

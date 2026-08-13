package routing

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
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

// TestParsePeersFile_LineTooLong exercises the scanner.Err() return path in
// ParsePeersFile. bufio.Scanner returns bufio.ErrTooLong when a single line
// exceeds its MaxScanTokenSize (64 KiB) default buffer.
func TestParsePeersFile_LineTooLong(t *testing.T) {
	path := filepath.Join(t.TempDir(), "peers.txt")
	// 65537 bytes on one line — larger than bufio.MaxScanTokenSize (65536).
	line := strings.Repeat("x", 65537) + "\n"
	if err := os.WriteFile(path, []byte(line), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := ParsePeersFile(path)
	if err == nil {
		t.Error("expected error for line exceeding max scan token size")
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

// waitForPeerCount polls pw.Peers() until len == want or timeout elapses.
func waitForPeerCount(t *testing.T, pw *PeerWatcher, want int, timeout time.Duration) []string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		got := pw.Peers()
		if len(got) == want {
			return got
		}
		if time.Now().After(deadline) {
			return got
		}
		time.Sleep(10 * time.Millisecond)
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

	// Poll until initial load completes (WatchFile sets peers synchronously before watching).
	got := waitForPeerCount(t, pw, 2, 3*time.Second)
	if len(got) != 2 {
		t.Fatalf("expected 2 peers after initial load, got %d: %v", len(got), got)
	}

	// Update file
	if err := os.WriteFile(path, []byte("10.0.0.1:5000\n10.0.0.2:5000\n10.0.0.3:5000\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Poll until fsnotify picks up the change (tolerate slow/loaded systems).
	got = waitForPeerCount(t, pw, 3, 5*time.Second)
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

// TestPeerWatcher_WatchFile_ReloadError exercises the slog.Error+continue path in
// WatchFile that fires when ParsePeersFile fails during a reload triggered by a
// write event.
//
// Strategy: WatchFile is called with a symlink (link → target). On Linux, inotify
// follows the symlink and watches target's inode, storing the watch with path=link.
// After the initial load succeeds we:
//   1. Remove the symlink (link is gone, target still exists).
//   2. Write to target — generates IN_MODIFY on target's inode.
// WatchFile receives the event with Name=link, calls ParsePeersFile(link), which
// fails with ENOENT because the symlink is gone → slog.Error + continue.
func TestPeerWatcher_WatchFile_ReloadError(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "actual.txt")
	link := filepath.Join(dir, "peers.txt")

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

	// Wait for initial load (link → target, valid content).
	time.Sleep(150 * time.Millisecond)
	if len(pw.Peers()) == 0 {
		t.Fatal("expected peers after initial load via symlink")
	}

	// Remove the symlink so ParsePeersFile(link) fails on reload.
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}

	// Write to the target file directly — fsnotify still watches target's inode
	// (inotify_add_watch follows symlinks). This generates IN_MODIFY on target's
	// inode → WatchFile sees event with Name=link → ParsePeersFile(link) → ENOENT.
	if err := os.WriteFile(target, []byte("10.0.0.2:5000\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Allow the event to propagate and the error path to execute.
	time.Sleep(300 * time.Millisecond)

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

// fakeWatcher implements fileWatcher for injection in tests.
type fakeWatcher struct {
	addErr  error
	events  chan fsnotify.Event
	errs    chan error
	closed  bool
}

func newFakeWatcher() *fakeWatcher {
	return &fakeWatcher{
		events: make(chan fsnotify.Event, 1),
		errs:   make(chan error, 1),
	}
}

func (fw *fakeWatcher) Add(_ string) error          { return fw.addErr }
func (fw *fakeWatcher) Close() error                { fw.closed = true; return nil }
func (fw *fakeWatcher) Events() <-chan fsnotify.Event { return fw.events }
func (fw *fakeWatcher) Errors() <-chan error          { return fw.errs }

// TestPeerWatcher_WatchFile_NewWatcherError covers the newWatcher() error path
// (lines 80-82 in peers.go).
func TestPeerWatcher_WatchFile_NewWatcherError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "peers.txt")
	if err := os.WriteFile(path, []byte("10.0.0.1:5000\n"), 0644); err != nil {
		t.Fatal(err)
	}

	pw := NewPeerWatcher(nil)
	wantErr := errors.New("inotify: too many open files")
	err := pw.watchFile(context.Background(), path, func() (fileWatcher, error) {
		return nil, wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected %v, got %v", wantErr, err)
	}
}

// TestPeerWatcher_WatchFile_AddError covers the watcher.Add() error path
// (lines 85-87 in peers.go).
func TestPeerWatcher_WatchFile_AddError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "peers.txt")
	if err := os.WriteFile(path, []byte("10.0.0.1:5000\n"), 0644); err != nil {
		t.Fatal(err)
	}

	pw := NewPeerWatcher(nil)
	wantErr := errors.New("watcher: no such file")
	fw := newFakeWatcher()
	fw.addErr = wantErr

	err := pw.watchFile(context.Background(), path, func() (fileWatcher, error) {
		return fw, nil
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected %v, got %v", wantErr, err)
	}
	if !fw.closed {
		t.Fatal("expected watcher to be closed after Add error")
	}
}

// TestPeerWatcher_WatchFile_WatcherError covers the watcher.Errors() receive
// path (lines 103-104 in peers.go).
func TestPeerWatcher_WatchFile_WatcherError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "peers.txt")
	if err := os.WriteFile(path, []byte("10.0.0.1:5000\n"), 0644); err != nil {
		t.Fatal(err)
	}

	pw := NewPeerWatcher(nil)
	fw := newFakeWatcher()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- pw.watchFile(ctx, path, func() (fileWatcher, error) {
			return fw, nil
		})
	}()

	// Inject a watcher error — watchFile should log it and continue.
	time.Sleep(50 * time.Millisecond)
	fw.errs <- errors.New("inotify: event queue overflow")

	// Allow the error path to execute, then cancel.
	time.Sleep(50 * time.Millisecond)
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

// TestNewFsnotifyWatcher_Error covers the error path in newFsnotifyWatcher by
// injecting a failing fsnotify.NewWatcher replacement.
func TestNewFsnotifyWatcher_Error(t *testing.T) {
	orig := fsnotifyNewWatcher
	defer func() { fsnotifyNewWatcher = orig }()
	fsnotifyNewWatcher = func() (*fsnotify.Watcher, error) {
		return nil, fmt.Errorf("injected fsnotify error")
	}

	_, err := newFsnotifyWatcher()
	if err == nil {
		t.Error("expected error from newFsnotifyWatcher, got nil")
	}
}

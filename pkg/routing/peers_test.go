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

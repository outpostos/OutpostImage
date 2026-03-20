package routing

import (
	"bufio"
	"context"
	"log/slog"
	"os"
	"strings"
	"sync"

	"github.com/fsnotify/fsnotify"
)

// PeerWatcher watches a peers file for changes and provides
// thread-safe access to the current peer list.
type PeerWatcher struct {
	mu    sync.RWMutex
	peers []string
}

// NewPeerWatcher creates a PeerWatcher initialized with the given peers.
func NewPeerWatcher(initial []string) *PeerWatcher {
	peers := make([]string, len(initial))
	copy(peers, initial)
	return &PeerWatcher{peers: peers}
}

// Peers returns a copy of the current peer list.
func (pw *PeerWatcher) Peers() []string {
	pw.mu.RLock()
	defer pw.mu.RUnlock()
	out := make([]string, len(pw.peers))
	copy(out, pw.peers)
	return out
}

// SetPeers atomically replaces the peer list.
func (pw *PeerWatcher) SetPeers(peers []string) {
	pw.mu.Lock()
	defer pw.mu.Unlock()
	pw.peers = make([]string, len(peers))
	copy(pw.peers, peers)
}

// WatchFile watches the given file for changes and reloads the peer list.
// It blocks until the context is cancelled. Intended to run in an errgroup.
func (pw *PeerWatcher) WatchFile(ctx context.Context, path string) error {
	// Initial load
	peers, err := ParsePeersFile(path)
	if err != nil {
		return err
	}
	pw.SetPeers(peers)
	slog.Info("loaded peers from file", "path", path, "count", len(peers))

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer watcher.Close()

	if err := watcher.Add(path); err != nil {
		return err
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case event, ok := <-watcher.Events:
			if !ok {
				return nil
			}
			if event.Has(fsnotify.Write) || event.Has(fsnotify.Create) {
				peers, err := ParsePeersFile(path)
				if err != nil {
					slog.Error("failed to reload peers file", "path", path, "error", err)
					continue
				}
				pw.SetPeers(peers)
				slog.Info("reloaded peers from file", "path", path, "count", len(peers))
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return nil
			}
			slog.Error("peers file watcher error", "error", err)
		}
	}
}

// ParsePeersFile reads a peers file and returns the list of peer addresses.
// Each line should contain a host:port pair. Lines starting with # are comments.
// Empty lines are ignored.
func ParsePeersFile(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var peers []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		peers = append(peers, line)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return peers, nil
}

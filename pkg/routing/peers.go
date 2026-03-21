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
// It supports both static peers (from CLI/file) and dynamic peers (from mDNS).
type PeerWatcher struct {
	mu      sync.RWMutex
	static  []string // from CLI flags or peers file
	dynamic []string // from mDNS discovery
}

// NewPeerWatcher creates a PeerWatcher initialized with the given static peers.
func NewPeerWatcher(initial []string) *PeerWatcher {
	static := make([]string, len(initial))
	copy(static, initial)
	return &PeerWatcher{static: static}
}

// Peers returns a deduplicated list of all peers (static + dynamic).
func (pw *PeerWatcher) Peers() []string {
	pw.mu.RLock()
	defer pw.mu.RUnlock()

	seen := make(map[string]struct{}, len(pw.static)+len(pw.dynamic))
	var out []string
	for _, p := range pw.static {
		if _, ok := seen[p]; !ok {
			seen[p] = struct{}{}
			out = append(out, p)
		}
	}
	for _, p := range pw.dynamic {
		if _, ok := seen[p]; !ok {
			seen[p] = struct{}{}
			out = append(out, p)
		}
	}
	return out
}

// SetPeers atomically replaces the static peer list.
func (pw *PeerWatcher) SetPeers(peers []string) {
	pw.mu.Lock()
	defer pw.mu.Unlock()
	pw.static = make([]string, len(peers))
	copy(pw.static, peers)
}

// MergeDynamic replaces the dynamic peer list (e.g., from mDNS discovery).
func (pw *PeerWatcher) MergeDynamic(peers []string) {
	pw.mu.Lock()
	defer pw.mu.Unlock()
	pw.dynamic = make([]string, len(peers))
	copy(pw.dynamic, peers)
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

package oci

import "github.com/fsnotify/fsnotify"

// ociWatcher is the subset of *fsnotify.Watcher used by store Subscribe methods,
// allowing tests to inject a fake implementation to cover error channel branches.
type ociWatcher interface {
	Add(name string) error
	Close() error
	Events() <-chan fsnotify.Event
	Errors() <-chan error
}

type fsnotifyOCIWatcher struct{ w *fsnotify.Watcher }

func (fw *fsnotifyOCIWatcher) Add(name string) error         { return fw.w.Add(name) }
func (fw *fsnotifyOCIWatcher) Close() error                  { return fw.w.Close() }
func (fw *fsnotifyOCIWatcher) Events() <-chan fsnotify.Event { return fw.w.Events }
func (fw *fsnotifyOCIWatcher) Errors() <-chan error           { return fw.w.Errors }

// ociNewFSWatcher is the fsnotify.NewWatcher constructor, injectable in tests.
var ociNewFSWatcher = fsnotify.NewWatcher

func newOCIWatcher() (ociWatcher, error) {
	w, err := ociNewFSWatcher()
	if err != nil {
		return nil, err
	}
	return &fsnotifyOCIWatcher{w}, nil
}

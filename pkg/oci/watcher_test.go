package oci

import "github.com/fsnotify/fsnotify"

// fakeOCIWatcher implements ociWatcher for tests.
type fakeOCIWatcher struct {
	addErr bool // if true, Add returns an error
	events chan fsnotify.Event
	errs   chan error
	closed bool
}

func newFakeOCIWatcher() *fakeOCIWatcher {
	return &fakeOCIWatcher{
		events: make(chan fsnotify.Event, 8),
		errs:   make(chan error, 8),
	}
}

func (fw *fakeOCIWatcher) Add(_ string) error {
	if fw.addErr {
		return errFakeWatcher
	}
	return nil
}
func (fw *fakeOCIWatcher) Close() error                  { fw.closed = true; return nil }
func (fw *fakeOCIWatcher) Events() <-chan fsnotify.Event { return fw.events }
func (fw *fakeOCIWatcher) Errors() <-chan error           { return fw.errs }

// errFakeWatcher is the sentinel error returned by fakeOCIWatcher when addErr is set.
var errFakeWatcher = &fakeWatcherError{}

type fakeWatcherError struct{}

func (e *fakeWatcherError) Error() string { return "fake watcher error" }

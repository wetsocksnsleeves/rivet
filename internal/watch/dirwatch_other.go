//go:build !darwin && !linux

package watch

import "errors"

type dirWatcher struct {
	events chan string
	errors chan error
}

func newDirWatcher() (*dirWatcher, error) {
	return nil, errors.New("the watcher is only supported on macOS and Linux")
}

func (w *dirWatcher) Add(string) error { return nil }
func (w *dirWatcher) Remove(string)    {}
func (w *dirWatcher) Close()           {}

// Package service holds what the OS service managers that run the watcher
// in the background (launchd on macOS, systemd on Linux) have in common.
package service

import "errors"

// Status is what the service manager knows about the watcher service.
type Status struct {
	// Installed is whether the service's definition file exists.
	Installed bool
	// Loaded is whether the service manager has the service started: it is
	// running, or being started or restarted.
	Loaded  bool
	Running bool
	PID     int
}

// ErrNotInstalled is returned by Start when the service is not installed.
var ErrNotInstalled = errors.New("the watcher service is not installed; run `rivet watch install`")

// Info describes an installed service for display.
type Info struct {
	// Manager names the service manager, e.g. launchd.
	Manager string
	// File is the service's definition file.
	File    string
	Program string
	Env     map[string]string
	// Log is where the service's output goes: a path or how to read it.
	Log string
}

package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wetsocksnsleeves/rivet/internal/launchd"
)

// fakeLaunchctl models a single agent being loaded and running.
type fakeLaunchctl struct {
	loaded bool
	calls  []string
}

func (f *fakeLaunchctl) Bootstrap(domain, plistPath string) error {
	f.calls = append(f.calls, "bootstrap")
	f.loaded = true
	return nil
}

func (f *fakeLaunchctl) Bootout(service string) error {
	f.calls = append(f.calls, "bootout")
	f.loaded = false
	return nil
}

func (f *fakeLaunchctl) Kickstart(service string) error {
	f.calls = append(f.calls, "kickstart")
	return nil
}

func (f *fakeLaunchctl) Print(service string) (string, error) {
	if !f.loaded {
		return "", errors.New("could not find service")
	}
	return "\tstate = running\n\tpid = 4121\n", nil
}

// fakeManager points the watch commands at a temp home and a fake
// launchctl for the rest of the test.
func fakeManager(t *testing.T) (*launchd.Manager, *fakeLaunchctl) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	ctl := &fakeLaunchctl{}
	m, err := launchd.NewManager("/opt/homebrew/bin/rivet", map[string]string{"XDG_STATE_HOME": filepath.Join(home, "state")})
	if err != nil {
		t.Fatal(err)
	}
	m.Ctl = ctl
	old := newManager
	newManager = func() (watcherService, error) { return m, nil }
	t.Cleanup(func() { newManager = old })
	return m, ctl
}

func TestWatchInstall(t *testing.T) {
	m, ctl := fakeManager(t)
	out, err := run(t, "watch", "install")
	if err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	if _, err := os.Stat(m.PlistPath); err != nil {
		t.Errorf("plist not written: %v", err)
	}
	if !ctl.loaded {
		t.Error("agent not loaded")
	}
	for _, want := range []string{"~/Library/LaunchAgents/" + launchd.Label + ".plist", "/opt/homebrew/bin/rivet", "XDG_STATE_HOME=~/state", "~/Library/Logs/rivet/watch.log"} {
		if !strings.Contains(out, want) {
			t.Errorf("install output missing %q:\n%s", want, out)
		}
	}
}

func TestWatchStopStartUninstall(t *testing.T) {
	m, ctl := fakeManager(t)
	for _, step := range []struct {
		args       []string
		wantOut    string
		wantLoaded bool
	}{
		{[]string{"watch", "install"}, "Installed", true},
		{[]string{"watch", "stop"}, "Stopped the watcher", false},
		{[]string{"watch", "stop"}, "not running", false},
		{[]string{"watch", "start"}, "Started", true},
		{[]string{"watch", "uninstall"}, "Uninstalled", false},
	} {
		out, err := run(t, step.args...)
		if err != nil {
			t.Fatalf("%v: %v\n%s", step.args, err, out)
		}
		if !strings.Contains(out, step.wantOut) {
			t.Errorf("%v output = %q, want %q", step.args, out, step.wantOut)
		}
		if ctl.loaded != step.wantLoaded {
			t.Errorf("after %v loaded = %v, want %v", step.args, ctl.loaded, step.wantLoaded)
		}
	}
	if _, err := os.Stat(m.PlistPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("plist left after uninstall: %v", err)
	}
}

func TestWatchStartNotInstalled(t *testing.T) {
	fakeManager(t)
	if _, err := run(t, "watch", "start"); !errors.Is(err, launchd.ErrNotInstalled) {
		t.Errorf("err = %v, want ErrNotInstalled", err)
	}
}

func TestWatchStatus(t *testing.T) {
	fakeManager(t)
	out, err := run(t, "watch", "status")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "service:  not installed") || !strings.Contains(out, "launchd:  not loaded") {
		t.Errorf("status before install:\n%s", out)
	}
	if _, err := run(t, "watch", "install"); err != nil {
		t.Fatal(err)
	}
	out, err = run(t, "watch", "status")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "service:  installed (~/Library/LaunchAgents/") || !strings.Contains(out, "launchd:  running (pid 4121)") {
		t.Errorf("status after install:\n%s", out)
	}
}

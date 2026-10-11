package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wetsocksnsleeves/rivet/internal/systemd"
)

// fakeSystemctl models the watcher unit being started and stopped.
type fakeSystemctl struct {
	running bool
}

func (f *fakeSystemctl) DaemonReload() error       { return nil }
func (f *fakeSystemctl) Enable(unit string) error  { return nil }
func (f *fakeSystemctl) Disable(unit string) error { return nil }
func (f *fakeSystemctl) Start(unit string) error   { f.running = true; return nil }
func (f *fakeSystemctl) Stop(unit string) error    { f.running = false; return nil }
func (f *fakeSystemctl) Restart(unit string) error { f.running = true; return nil }

func (f *fakeSystemctl) Show(unit string) (map[string]string, error) {
	if !f.running {
		return map[string]string{"ActiveState": "inactive", "MainPID": "0"}, nil
	}
	return map[string]string{"ActiveState": "active", "MainPID": "4121"}, nil
}

// fakeSystemdManager points the watch commands at a temp home and a fake
// systemctl for the rest of the test.
func fakeSystemdManager(t *testing.T) (*systemd.Manager, *fakeSystemctl) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	ctl := &fakeSystemctl{}
	m, err := systemd.NewManager("/home/me/.local/bin/rivet", map[string]string{"XDG_STATE_HOME": filepath.Join(home, "state")})
	if err != nil {
		t.Fatal(err)
	}
	m.Ctl = ctl
	old := newManager
	newManager = func() (watcherService, error) { return m, nil }
	t.Cleanup(func() { newManager = old })
	return m, ctl
}

func TestWatchInstallSystemd(t *testing.T) {
	m, ctl := fakeSystemdManager(t)
	out, err := run(t, "watch", "install")
	if err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	if _, err := os.Stat(m.UnitPath); err != nil {
		t.Errorf("unit file not written: %v", err)
	}
	if !ctl.running {
		t.Error("service not started")
	}
	for _, want := range []string{"~/.config/systemd/user/rivet.service", "/home/me/.local/bin/rivet", "XDG_STATE_HOME=~/state", "journalctl --user -u rivet"} {
		if !strings.Contains(out, want) {
			t.Errorf("install output missing %q:\n%s", want, out)
		}
	}
}

func TestWatchStopStartUninstallSystemd(t *testing.T) {
	m, ctl := fakeSystemdManager(t)
	for _, step := range []struct {
		args        []string
		wantOut     string
		wantRunning bool
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
		if ctl.running != step.wantRunning {
			t.Errorf("after %v running = %v, want %v", step.args, ctl.running, step.wantRunning)
		}
	}
	if _, err := os.Stat(m.UnitPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("unit file left after uninstall: %v", err)
	}
}

func TestWatchStatusSystemd(t *testing.T) {
	fakeSystemdManager(t)
	out, err := run(t, "watch", "status")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "service:  not installed") || !strings.Contains(out, "systemd:  not loaded") {
		t.Errorf("status before install:\n%s", out)
	}
	if _, err := run(t, "watch", "install"); err != nil {
		t.Fatal(err)
	}
	out, err = run(t, "watch", "status")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "service:  installed (~/.config/systemd/user/rivet.service)") || !strings.Contains(out, "systemd:  running (pid 4121)") {
		t.Errorf("status after install:\n%s", out)
	}
}

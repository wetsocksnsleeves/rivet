package launchd

import (
	"errors"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

var update = flag.Bool("update", false, "rewrite golden files")

func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	p := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(p, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("%v (run go test -update to create it)", err)
	}
	if string(got) != string(want) {
		t.Errorf("differs from %s:\n--- got\n%s--- want\n%s", p, got, want)
	}
}

func TestPlist(t *testing.T) {
	a := Agent{Label: Label, Program: "/opt/homebrew/bin/rivet", LogPath: "/Users/me/Library/Logs/rivet/watch.log"}
	golden(t, "agent.plist", a.Plist())
}

func TestPlistWithEnvAndEscaping(t *testing.T) {
	a := Agent{
		Label:   Label,
		Program: "/Users/me/code/a&b/rivet",
		Env:     map[string]string{"XDG_STATE_HOME": "/tmp/state", "A": "<1>"},
		LogPath: "/Users/me/Library/Logs/rivet/watch.log",
	}
	golden(t, "agent-env.plist", a.Plist())
}

// fakeLaunchctl records calls and models whether the agent is loaded.
type fakeLaunchctl struct {
	loaded  bool
	running bool
	calls   []string
}

func (f *fakeLaunchctl) Bootstrap(domain, plistPath string) error {
	f.calls = append(f.calls, "bootstrap "+domain+" "+filepath.Base(plistPath))
	if f.loaded {
		return errors.New("already loaded")
	}
	f.loaded, f.running = true, true
	return nil
}

func (f *fakeLaunchctl) Bootout(service string) error {
	f.calls = append(f.calls, "bootout "+service)
	if !f.loaded {
		return errors.New("not loaded")
	}
	f.loaded, f.running = false, false
	return nil
}

func (f *fakeLaunchctl) Kickstart(service string) error {
	f.calls = append(f.calls, "kickstart "+service)
	if !f.loaded {
		return errors.New("not loaded")
	}
	f.running = true
	return nil
}

func (f *fakeLaunchctl) Print(service string) (string, error) {
	if !f.loaded {
		return "", errors.New("could not find service")
	}
	if f.running {
		return "gui/501/" + Label + " = {\n\tstate = running\n\tpid = 4121\n}\n", nil
	}
	return "gui/501/" + Label + " = {\n\tstate = not running\n}\n", nil
}

func newTestManager(t *testing.T) (*Manager, *fakeLaunchctl) {
	t.Helper()
	dir := t.TempDir()
	ctl := &fakeLaunchctl{}
	return &Manager{
		Agent: Agent{
			Label: Label, Program: "/bin/rivet",
			LogPath: filepath.Join(dir, "Logs", "rivet", "watch.log"),
		},
		PlistPath: filepath.Join(dir, "LaunchAgents", Label+".plist"),
		Domain:    "gui/501",
		Ctl:       ctl,
	}, ctl
}

const svc = "gui/501/" + Label

func assertCalls(t *testing.T, ctl *fakeLaunchctl, want ...string) {
	t.Helper()
	if want == nil {
		want = []string{}
	}
	got := ctl.calls
	if got == nil {
		got = []string{}
	}
	if !slices.Equal(got, want) {
		t.Errorf("launchctl calls = %q, want %q", got, want)
	}
}

func TestInstall(t *testing.T) {
	m, ctl := newTestManager(t)
	if err := m.Install(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(m.PlistPath)
	if err != nil {
		t.Fatalf("plist not written: %v", err)
	}
	if string(data) != string(m.Agent.Plist()) {
		t.Error("written plist differs from Agent.Plist")
	}
	if _, err := os.Stat(filepath.Dir(m.Agent.LogPath)); err != nil {
		t.Errorf("log directory not created: %v", err)
	}
	assertCalls(t, ctl, "bootstrap gui/501 "+Label+".plist")
}

func TestInstallReloadsLoadedAgent(t *testing.T) {
	m, ctl := newTestManager(t)
	ctl.loaded = true
	if err := m.Install(); err != nil {
		t.Fatal(err)
	}
	assertCalls(t, ctl, "bootout "+svc, "bootstrap gui/501 "+Label+".plist")
}

func TestUninstall(t *testing.T) {
	m, ctl := newTestManager(t)
	if err := m.Install(); err != nil {
		t.Fatal(err)
	}
	ctl.calls = nil
	if err := m.Uninstall(); err != nil {
		t.Fatal(err)
	}
	assertCalls(t, ctl, "bootout "+svc)
	if _, err := os.Stat(m.PlistPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("plist not removed: %v", err)
	}
}

func TestUninstallWhenNotInstalled(t *testing.T) {
	m, ctl := newTestManager(t)
	if err := m.Uninstall(); err != nil {
		t.Fatal(err)
	}
	assertCalls(t, ctl)
}

func TestStartNotInstalled(t *testing.T) {
	m, ctl := newTestManager(t)
	if err := m.Start(); !errors.Is(err, ErrNotInstalled) {
		t.Errorf("err = %v, want ErrNotInstalled", err)
	}
	assertCalls(t, ctl)
}

func TestStartAfterStop(t *testing.T) {
	m, ctl := newTestManager(t)
	if err := m.Install(); err != nil {
		t.Fatal(err)
	}
	if err := m.Stop(); err != nil {
		t.Fatal(err)
	}
	ctl.calls = nil
	if err := m.Start(); err != nil {
		t.Fatal(err)
	}
	assertCalls(t, ctl, "bootstrap gui/501 "+Label+".plist")
}

func TestStartLoadedButNotRunning(t *testing.T) {
	m, ctl := newTestManager(t)
	if err := m.Install(); err != nil {
		t.Fatal(err)
	}
	ctl.running = false
	ctl.calls = nil
	if err := m.Start(); err != nil {
		t.Fatal(err)
	}
	assertCalls(t, ctl, "kickstart "+svc)
}

func TestStop(t *testing.T) {
	m, ctl := newTestManager(t)
	if err := m.Install(); err != nil {
		t.Fatal(err)
	}
	ctl.calls = nil
	if err := m.Stop(); err != nil {
		t.Fatal(err)
	}
	assertCalls(t, ctl, "bootout "+svc)
	ctl.calls = nil
	if err := m.Stop(); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
	assertCalls(t, ctl)
}

func TestStatus(t *testing.T) {
	m, ctl := newTestManager(t)
	if got := m.Status(); got != (Status{}) {
		t.Errorf("before install: %+v", got)
	}
	if err := m.Install(); err != nil {
		t.Fatal(err)
	}
	if got := m.Status(); got != (Status{Installed: true, Loaded: true, Running: true, PID: 4121}) {
		t.Errorf("after install: %+v", got)
	}
	ctl.running = false
	if got := m.Status(); got != (Status{Installed: true, Loaded: true}) {
		t.Errorf("loaded, not running: %+v", got)
	}
	if err := m.Stop(); err != nil {
		t.Fatal(err)
	}
	if got := m.Status(); got != (Status{Installed: true}) {
		t.Errorf("after stop: %+v", got)
	}
}

func TestParsePrintIgnoresNestedState(t *testing.T) {
	// Trimmed from real `launchctl print` output for a running agent.
	out := "gui/501/com.example = {\n\tactive count = 1\n\tstate = running\n\n\tprogram = /bin/x\n\tpid = 742\n\tendpoints = {\n\t\t\"com.example.xpc\" = {\n\t\t\tport = 0x1\n\t\t\tstate = active\n\t\t}\n\t}\n}\n"
	running, pid := parsePrint(out)
	if !running || pid != 742 {
		t.Errorf("parsePrint = %v, %d; want true, 742", running, pid)
	}
}

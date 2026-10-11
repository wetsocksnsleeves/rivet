package systemd

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

func TestUnitFile(t *testing.T) {
	s := Service{Program: "/home/me/.local/bin/rivet"}
	golden(t, "rivet.service", s.UnitFile())
}

func TestUnitFileWithEnvAndEscaping(t *testing.T) {
	s := Service{
		Program: `/home/me/code/a "b" 100%/$HOME\rivet`,
		Env:     map[string]string{"XDG_STATE_HOME": "/tmp/state", "A": "50% $x"},
	}
	golden(t, "rivet-env.service", s.UnitFile())
}

// fakeSystemctl records calls and models whether the unit is running.
type fakeSystemctl struct {
	enabled bool
	running bool
	calls   []string
}

func (f *fakeSystemctl) DaemonReload() error {
	f.calls = append(f.calls, "daemon-reload")
	return nil
}

func (f *fakeSystemctl) Enable(unit string) error {
	f.calls = append(f.calls, "enable "+unit)
	f.enabled = true
	return nil
}

func (f *fakeSystemctl) Disable(unit string) error {
	f.calls = append(f.calls, "disable "+unit)
	f.enabled = false
	return nil
}

func (f *fakeSystemctl) Start(unit string) error {
	f.calls = append(f.calls, "start "+unit)
	f.running = true
	return nil
}

func (f *fakeSystemctl) Stop(unit string) error {
	f.calls = append(f.calls, "stop "+unit)
	f.running = false
	return nil
}

func (f *fakeSystemctl) Restart(unit string) error {
	f.calls = append(f.calls, "restart "+unit)
	f.running = true
	return nil
}

func (f *fakeSystemctl) Show(unit string) (map[string]string, error) {
	if f.running {
		return map[string]string{"ActiveState": "active", "MainPID": "4121"}, nil
	}
	return map[string]string{"ActiveState": "inactive", "MainPID": "0"}, nil
}

func newTestManager(t *testing.T) (*Manager, *fakeSystemctl) {
	t.Helper()
	ctl := &fakeSystemctl{}
	return &Manager{
		Service:  Service{Program: "/bin/rivet"},
		UnitPath: filepath.Join(t.TempDir(), "systemd", "user", Unit),
		Ctl:      ctl,
	}, ctl
}

func assertCalls(t *testing.T, ctl *fakeSystemctl, want ...string) {
	t.Helper()
	if want == nil {
		want = []string{}
	}
	got := ctl.calls
	if got == nil {
		got = []string{}
	}
	if !slices.Equal(got, want) {
		t.Errorf("systemctl calls = %q, want %q", got, want)
	}
}

func TestInstall(t *testing.T) {
	m, ctl := newTestManager(t)
	if err := m.Install(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(m.UnitPath)
	if err != nil {
		t.Fatalf("unit file not written: %v", err)
	}
	if string(data) != string(m.Service.UnitFile()) {
		t.Error("written unit file differs from Service.UnitFile")
	}
	assertCalls(t, ctl, "daemon-reload", "enable "+Unit, "restart "+Unit)
	if s := m.Status(); !s.Installed || !s.Loaded || !s.Running || s.PID != 4121 {
		t.Errorf("status after install = %+v", s)
	}
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
	assertCalls(t, ctl, "stop "+Unit, "disable "+Unit, "daemon-reload")
	if _, err := os.Stat(m.UnitPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("unit file not removed: %v", err)
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

func TestStopAndStart(t *testing.T) {
	m, ctl := newTestManager(t)
	if err := m.Install(); err != nil {
		t.Fatal(err)
	}
	ctl.calls = nil
	if err := m.Stop(); err != nil {
		t.Fatal(err)
	}
	if err := m.Stop(); err != nil {
		t.Fatal(err)
	}
	if s := m.Status(); s.Loaded || s.Running || s.PID != 0 || !s.Installed {
		t.Errorf("status after stop = %+v", s)
	}
	if err := m.Start(); err != nil {
		t.Fatal(err)
	}
	assertCalls(t, ctl, "stop "+Unit, "start "+Unit)
	if !ctl.enabled {
		t.Error("stop disabled the service; it should still start at login")
	}
}

func TestParseShow(t *testing.T) {
	got := parseShow("ActiveState=active\nMainPID=812\n")
	if got["ActiveState"] != "active" || got["MainPID"] != "812" {
		t.Errorf("parseShow = %v", got)
	}
}

func TestNewManagerUnitPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, tc := range []struct{ xdg, want string }{
		{"", filepath.Join(home, ".config", "systemd", "user", Unit)},
		{"/xdg", filepath.Join("/xdg", "systemd", "user", Unit)},
	} {
		t.Setenv("XDG_CONFIG_HOME", tc.xdg)
		m, err := NewManager("/bin/rivet", nil)
		if err != nil {
			t.Fatal(err)
		}
		if m.UnitPath != tc.want {
			t.Errorf("XDG_CONFIG_HOME=%q: UnitPath = %q, want %q", tc.xdg, m.UnitPath, tc.want)
		}
	}
}

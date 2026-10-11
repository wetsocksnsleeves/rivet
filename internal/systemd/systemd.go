// Package systemd installs and controls the watcher as a systemd user
// service.
package systemd

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/wetsocksnsleeves/rivet/internal/service"
)

// Unit is the name of the watcher's systemd unit.
const Unit = "rivet.service"

// Service describes the user service that runs the watcher.
type Service struct {
	// Program is the rivet executable; it is run as `Program watch run`.
	Program string
	// Env is set in the service's environment, e.g. XDG_STATE_HOME so the
	// service uses the same state as the shell it was installed from.
	Env map[string]string
}

// UnitFile renders the service's unit file. The service starts at login and
// is restarted if it exits with an error; a clean exit leaves it stopped.
// Its output goes to the journal.
func (s Service) UnitFile() []byte {
	var b strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&b, format, args...) }
	w("[Unit]\n")
	w("Description=rivet watcher: keeps linked sets and their dests in sync\n")
	w("\n[Service]\n")
	w("ExecStart=%s watch run\n", quote(strings.ReplaceAll(s.Program, "$", "$$")))
	keys := make([]string, 0, len(s.Env))
	for k := range s.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		w("Environment=%s\n", quote(k+"="+s.Env[k]))
	}
	w("Restart=on-failure\n")
	w("\n[Install]\n")
	w("WantedBy=default.target\n")
	return []byte(b.String())
}

// quote makes s one word of a unit file setting, with specifiers escaped.
func quote(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "%", "%%")
	return `"` + r.Replace(s) + `"`
}

// Systemctl is the subset of `systemctl --user` the manager uses.
type Systemctl interface {
	// DaemonReload makes systemd reread unit files.
	DaemonReload() error
	// Enable makes the unit start at login; Disable undoes it.
	Enable(unit string) error
	Disable(unit string) error
	Start(unit string) error
	Stop(unit string) error
	// Restart starts the unit, stopping it first if it is running.
	Restart(unit string) error
	// Show returns the unit's properties, such as ActiveState and MainPID.
	Show(unit string) (map[string]string, error)
}

// Manager installs and controls the watcher service.
type Manager struct {
	Service  Service
	UnitPath string
	Ctl      Systemctl
}

// NewManager returns a Manager for the watcher service of the current user.
func NewManager(program string, env map[string]string) (*Manager, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return nil, err
	}
	return &Manager{
		Service:  Service{Program: program, Env: env},
		UnitPath: filepath.Join(dir, "systemd", "user", Unit),
		Ctl:      execSystemctl{},
	}, nil
}

// Info describes the service for display.
func (m *Manager) Info() service.Info {
	return service.Info{
		Manager: "systemd",
		File:    m.UnitPath,
		Program: m.Service.Program,
		Env:     m.Service.Env,
		Log:     "the journal (`journalctl --user -u rivet`)",
	}
}

// Install writes the unit file, enables the service and (re)starts it, so a
// running service picks up the new unit.
func (m *Manager) Install() error {
	if err := os.MkdirAll(filepath.Dir(m.UnitPath), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(m.UnitPath, m.Service.UnitFile(), 0o644); err != nil {
		return err
	}
	if err := m.Ctl.DaemonReload(); err != nil {
		return err
	}
	if err := m.Ctl.Enable(Unit); err != nil {
		return err
	}
	return m.Ctl.Restart(Unit)
}

// Uninstall stops and disables the service and removes its unit file.
func (m *Manager) Uninstall() error {
	if !m.installed() {
		return nil
	}
	if m.Status().Loaded {
		if err := m.Ctl.Stop(Unit); err != nil {
			return err
		}
	}
	if err := m.Ctl.Disable(Unit); err != nil {
		return err
	}
	if err := os.Remove(m.UnitPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return m.Ctl.DaemonReload()
}

// ErrNotInstalled is returned by Start when there is no unit file.
var ErrNotInstalled = service.ErrNotInstalled

// Start starts the installed service.
func (m *Manager) Start() error {
	if !m.installed() {
		return ErrNotInstalled
	}
	return m.Ctl.Start(Unit)
}

// Stop stops the service. It starts again at the next login, or with Start.
func (m *Manager) Stop() error {
	if !m.Status().Loaded {
		return nil
	}
	return m.Ctl.Stop(Unit)
}

// Status is what systemd knows about the service.
type Status = service.Status

// Status reports whether the service is installed, started and running.
func (m *Manager) Status() Status {
	s := Status{Installed: m.installed()}
	props, err := m.Ctl.Show(Unit)
	if err != nil {
		return s
	}
	switch props["ActiveState"] {
	case "active", "activating", "reloading", "deactivating":
		s.Loaded = true
	}
	s.PID, _ = strconv.Atoi(props["MainPID"])
	s.Running = props["ActiveState"] == "active" && s.PID != 0
	if !s.Running {
		s.PID = 0
	}
	return s
}

func (m *Manager) installed() bool {
	_, err := os.Stat(m.UnitPath)
	return err == nil
}

// parseShow reads `systemctl show` output: one Key=Value per line.
func parseShow(out string) map[string]string {
	props := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		if k, v, ok := strings.Cut(sc.Text(), "="); ok {
			props[k] = v
		}
	}
	return props
}

type execSystemctl struct{}

func (execSystemctl) DaemonReload() error       { return systemctl("daemon-reload") }
func (execSystemctl) Enable(unit string) error  { return systemctl("enable", unit) }
func (execSystemctl) Disable(unit string) error { return systemctl("disable", unit) }
func (execSystemctl) Start(unit string) error   { return systemctl("start", unit) }
func (execSystemctl) Stop(unit string) error    { return systemctl("stop", unit) }
func (execSystemctl) Restart(unit string) error { return systemctl("restart", unit) }

func (execSystemctl) Show(unit string) (map[string]string, error) {
	out, err := systemctlOutput("show", unit, "--property=ActiveState,MainPID")
	if err != nil {
		return nil, err
	}
	return parseShow(out), nil
}

func systemctl(args ...string) error {
	_, err := systemctlOutput(args...)
	return err
}

func systemctlOutput(args ...string) (string, error) {
	args = append([]string{"--user"}, args...)
	out, err := exec.Command("systemctl", args...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("systemctl %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

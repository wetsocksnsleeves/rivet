// Package launchd installs and controls the watcher as a launchd user agent.
package launchd

import (
	"bufio"
	"bytes"
	"encoding/xml"
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

// Label is the launchd label of the watcher agent.
const Label = "io.github.wetsocksnsleeves.rivet"

// Agent describes the launch agent that runs the watcher.
type Agent struct {
	Label string
	// Program is the rivet executable; it is run as `Program watch run`.
	Program string
	// Env is set in the agent's environment, e.g. XDG_STATE_HOME so the
	// agent uses the same state as the shell it was installed from.
	Env     map[string]string
	LogPath string
}

// Plist renders the agent's property list. The agent starts at login and
// is restarted if it exits with an error; a clean exit leaves it stopped.
func (a Agent) Plist() []byte {
	var b bytes.Buffer
	w := func(format string, args ...any) { fmt.Fprintf(&b, format, args...) }
	w("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n")
	w("<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n")
	w("<plist version=\"1.0\">\n<dict>\n")
	w("\t<key>Label</key>\n\t<string>%s</string>\n", esc(a.Label))
	w("\t<key>ProgramArguments</key>\n\t<array>\n")
	for _, arg := range []string{a.Program, "watch", "run"} {
		w("\t\t<string>%s</string>\n", esc(arg))
	}
	w("\t</array>\n")
	if len(a.Env) > 0 {
		keys := make([]string, 0, len(a.Env))
		for k := range a.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		w("\t<key>EnvironmentVariables</key>\n\t<dict>\n")
		for _, k := range keys {
			w("\t\t<key>%s</key>\n\t\t<string>%s</string>\n", esc(k), esc(a.Env[k]))
		}
		w("\t</dict>\n")
	}
	w("\t<key>RunAtLoad</key>\n\t<true/>\n")
	w("\t<key>KeepAlive</key>\n\t<dict>\n\t\t<key>SuccessfulExit</key>\n\t\t<false/>\n\t</dict>\n")
	w("\t<key>StandardOutPath</key>\n\t<string>%s</string>\n", esc(a.LogPath))
	w("\t<key>StandardErrorPath</key>\n\t<string>%s</string>\n", esc(a.LogPath))
	w("</dict>\n</plist>\n")
	return b.Bytes()
}

func esc(s string) string {
	var b strings.Builder
	xml.EscapeText(&b, []byte(s))
	return b.String()
}

// Launchctl is the subset of launchctl the manager uses.
type Launchctl interface {
	// Bootstrap loads (and, with RunAtLoad, starts) the agent in plistPath.
	Bootstrap(domain, plistPath string) error
	// Bootout stops and unloads a service.
	Bootout(service string) error
	// Kickstart starts a loaded service that is not running.
	Kickstart(service string) error
	// Print describes a service; it fails if the service is not loaded.
	Print(service string) (string, error)
}

// Manager installs and controls one agent.
type Manager struct {
	Agent     Agent
	PlistPath string
	// Domain is the launchd domain, gui/<uid> for a user agent.
	Domain string
	Ctl    Launchctl
}

// NewManager returns a Manager for the watcher agent of the current user.
func NewManager(program string, env map[string]string) (*Manager, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return &Manager{
		Agent: Agent{
			Label:   Label,
			Program: program,
			Env:     env,
			LogPath: filepath.Join(home, "Library", "Logs", "rivet", "watch.log"),
		},
		PlistPath: filepath.Join(home, "Library", "LaunchAgents", Label+".plist"),
		Domain:    fmt.Sprintf("gui/%d", os.Getuid()),
		Ctl:       execLaunchctl{},
	}, nil
}

// Info describes the agent for display.
func (m *Manager) Info() service.Info {
	return service.Info{Manager: "launchd", File: m.PlistPath, Program: m.Agent.Program, Env: m.Agent.Env, Log: m.Agent.LogPath}
}

func (m *Manager) service() string { return m.Domain + "/" + m.Agent.Label }

// Install writes the plist and loads the agent, which starts it. An agent
// that is already loaded is reloaded with the new plist.
func (m *Manager) Install() error {
	if m.loaded() {
		if err := m.Ctl.Bootout(m.service()); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(m.PlistPath), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(m.Agent.LogPath), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(m.PlistPath, m.Agent.Plist(), 0o644); err != nil {
		return err
	}
	return m.Ctl.Bootstrap(m.Domain, m.PlistPath)
}

// Uninstall stops and unloads the agent and removes its plist.
func (m *Manager) Uninstall() error {
	if m.loaded() {
		if err := m.Ctl.Bootout(m.service()); err != nil {
			return err
		}
	}
	if err := os.Remove(m.PlistPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// ErrNotInstalled is returned by Start when there is no plist.
var ErrNotInstalled = service.ErrNotInstalled

// Start starts the installed agent, loading it first if needed.
func (m *Manager) Start() error {
	if !m.installed() {
		return ErrNotInstalled
	}
	if m.loaded() {
		return m.Ctl.Kickstart(m.service())
	}
	return m.Ctl.Bootstrap(m.Domain, m.PlistPath)
}

// Stop stops and unloads the agent. It starts again at the next login, or
// with Start.
func (m *Manager) Stop() error {
	if !m.loaded() {
		return nil
	}
	return m.Ctl.Bootout(m.service())
}

// Status is what launchd knows about the agent.
type Status = service.Status

// Status reports whether the agent is installed, loaded and running.
func (m *Manager) Status() Status {
	s := Status{Installed: m.installed()}
	out, err := m.Ctl.Print(m.service())
	if err != nil {
		return s
	}
	s.Loaded = true
	s.Running, s.PID = parsePrint(out)
	return s
}

func (m *Manager) installed() bool {
	_, err := os.Stat(m.PlistPath)
	return err == nil
}

func (m *Manager) loaded() bool {
	_, err := m.Ctl.Print(m.service())
	return err == nil
}

// parsePrint reads the service's state and pid from `launchctl print`
// output. Nested sections (endpoints, sockets) have state lines of their
// own, so only the first of each key, which is the service's, counts.
func parsePrint(out string) (running bool, pid int) {
	seenState, seenPID := false, false
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		k, v, ok := strings.Cut(strings.TrimSpace(sc.Text()), " = ")
		if !ok {
			continue
		}
		switch {
		case k == "state" && !seenState:
			running, seenState = v == "running", true
		case k == "pid" && !seenPID:
			pid, _ = strconv.Atoi(v)
			seenPID = true
		}
	}
	return running, pid
}

type execLaunchctl struct{}

func (execLaunchctl) Bootstrap(domain, plistPath string) error {
	_, err := launchctl("bootstrap", domain, plistPath)
	return err
}

func (execLaunchctl) Bootout(service string) error {
	_, err := launchctl("bootout", service)
	return err
}

func (execLaunchctl) Kickstart(service string) error {
	_, err := launchctl("kickstart", service)
	return err
}

func (execLaunchctl) Print(service string) (string, error) {
	return launchctl("print", service)
}

func launchctl(args ...string) (string, error) {
	out, err := exec.Command("launchctl", args...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("launchctl %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

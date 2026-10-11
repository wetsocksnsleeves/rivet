package main

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/wetsocksnsleeves/rivet/internal/config"
	"github.com/wetsocksnsleeves/rivet/internal/launchd"
	"github.com/wetsocksnsleeves/rivet/internal/service"
	"github.com/wetsocksnsleeves/rivet/internal/state"
	"github.com/wetsocksnsleeves/rivet/internal/systemd"
	"github.com/wetsocksnsleeves/rivet/internal/watch"
)

func newWatchCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "watch",
		Short: "Run and manage the watcher that keeps linked sets and their dests in sync",
	}
	var debounce time.Duration
	run := &cobra.Command{
		Use:   "run",
		Short: "Run the watcher in the foreground until interrupted",
		Long: `Run the watcher in the foreground until interrupted.

On start it reconciles every linked set, then watches each set and its dest:
new files in dest are adopted into the set, real files that replaced links
(atomic saves) are adopted and relinked, new set files are linked, and
deletions propagate both ways. Conflicts are left for ` + "`rivet status`" + `.

It reads linked sets from state, so it runs from any directory.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			stateDir, err := state.DefaultDir()
			if err != nil {
				return err
			}
			env, err := config.OSEnv()
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return (&watch.Runtime{
				StateDir: stateDir,
				Window:   debounce,
				Env:      env,
				Log:      cmd.ErrOrStderr(),
			}).Run(ctx)
		},
	}
	run.Flags().DurationVar(&debounce, "debounce", 500*time.Millisecond, "how long a path must be quiet before it is acted on")

	install := &cobra.Command{
		Use:   "install",
		Short: "Install the watcher as a service that starts at login (launchd on macOS, systemd on Linux), and start it",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := newManager()
			if err != nil {
				return err
			}
			if err := m.Install(); err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			info := m.Info()
			fmt.Fprintf(w, "Installed %s; the watcher is running and starts at login.\n", displayPath(info.File))
			fmt.Fprintf(w, "It runs %s", displayPath(info.Program))
			if d, ok := info.Env["XDG_STATE_HOME"]; ok {
				fmt.Fprintf(w, " with XDG_STATE_HOME=%s", displayPath(d))
			}
			fmt.Fprintf(w, " and logs to %s.\n", displayPath(info.Log))
			return nil
		},
	}
	uninstall := &cobra.Command{
		Use:   "uninstall",
		Short: "Stop the watcher service and remove it",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := newManager()
			if err != nil {
				return err
			}
			if err := m.Uninstall(); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Uninstalled the watcher service.")
			return nil
		},
	}
	start := &cobra.Command{
		Use:   "start",
		Short: "Start the installed watcher service",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := newManager()
			if err != nil {
				return err
			}
			if err := m.Start(); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Started the watcher.")
			return nil
		},
	}
	stop := &cobra.Command{
		Use:   "stop",
		Short: "Stop the watcher service until the next login or `rivet watch start`",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := newManager()
			if err != nil {
				return err
			}
			if !m.Status().Loaded {
				fmt.Fprintln(cmd.OutOrStdout(), "The watcher service is not running.")
				return nil
			}
			if err := m.Stop(); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Stopped the watcher. It starts again at login, or with `rivet watch start`.")
			return nil
		},
	}
	status := &cobra.Command{
		Use:   "status",
		Short: "Show whether the watcher service is installed and running",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := newManager()
			if err != nil {
				return err
			}
			stateDir, err := state.DefaultDir()
			if err != nil {
				return err
			}
			printWatchStatus(cmd.OutOrStdout(), m, stateDir)
			return nil
		},
	}
	cmd.AddCommand(run, install, uninstall, start, stop, status)
	return cmd
}

// watcherService runs `rivet watch run` in the background through the OS
// service manager.
type watcherService interface {
	Install() error
	Uninstall() error
	Start() error
	Stop() error
	Status() service.Status
	Info() service.Info
}

// newManager builds the service manager for this executable and OS. Tests
// replace it to avoid calling launchctl or systemctl.
var newManager = func() (watcherService, error) {
	program, err := os.Executable()
	if err != nil {
		return nil, err
	}
	// program is not resolved through symlinks: a Homebrew install is a
	// symlink into a versioned Cellar directory that upgrades remove.
	env := map[string]string{}
	if d, ok := os.LookupEnv("XDG_STATE_HOME"); ok && d != "" {
		env["XDG_STATE_HOME"] = d
	}
	switch runtime.GOOS {
	case "darwin":
		return launchd.NewManager(program, env)
	case "linux":
		return systemd.NewManager(program, env)
	}
	return nil, fmt.Errorf("installing the watcher as a service is not supported on %s; run `rivet watch run` instead", runtime.GOOS)
}

func printWatchStatus(w io.Writer, m watcherService, stateDir string) {
	s, info := m.Status(), m.Info()
	installed := "not installed (run `rivet watch install`)"
	if s.Installed {
		installed = "installed (" + displayPath(info.File) + ")"
	}
	managerState := "not loaded"
	switch {
	case s.Running:
		managerState = fmt.Sprintf("running (pid %d)", s.PID)
	case s.Loaded:
		managerState = "loaded, not running"
	}
	fmt.Fprintf(w, "service:  %s\n", installed)
	fmt.Fprintf(w, "%-10s%s\n", info.Manager+":", managerState)
	fmt.Fprintf(w, "watcher:  %s\n", strings.TrimSuffix(watcherLine(stateDir), "."))
	fmt.Fprintf(w, "log:      %s\n", displayPath(info.Log))
}

// watcherLine is the last line of status.
func watcherLine(stateDir string) string {
	pid, running, err := state.WatcherPID(stateDir)
	switch {
	case err != nil:
		return "Watcher state unknown: " + err.Error()
	case running:
		return "Watcher is running (pid " + strconv.Itoa(pid) + ")."
	}
	return "Watcher is not running, so nothing is synced automatically. Start it with `rivet watch install` (or `rivet watch run` in the foreground)."
}

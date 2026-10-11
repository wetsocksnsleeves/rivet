package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wetsocksnsleeves/rivet/internal/config"
	"github.com/wetsocksnsleeves/rivet/internal/state"
	"github.com/wetsocksnsleeves/rivet/internal/watch"
)

// syncBuffer is a log sink the watcher goroutine and the test can share.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// startWatcher runs the watcher until the test ends and returns once its
// startup reconciliation is done.
func startWatcher(t *testing.T, e *env) *syncBuffer {
	t.Helper()
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("the watcher is only supported on macOS and Linux")
	}
	envCfg, err := config.OSEnv()
	if err != nil {
		t.Fatal(err)
	}
	logs := &syncBuffer{}
	ready := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- (&watch.Runtime{
			StateDir: e.stateDir,
			Window:   50 * time.Millisecond,
			Env:      envCfg,
			Log:      logs,
			Ready:    func() { close(ready) },
		}).Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("watcher: %v", err)
		}
	})
	select {
	case <-ready:
	case err := <-done:
		done <- nil // the cleanup is waiting on done; don't leave it blocked
		t.Fatalf("watcher exited during startup: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("watcher did not start")
	}
	return logs
}

// eventually polls cond until it holds or the deadline passes.
func eventually(t *testing.T, logs *syncBuffer, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting until %s\nwatcher log:\n%s", what, logs)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func isLinkTo(p, target string) bool {
	got, err := os.Readlink(p)
	return err == nil && got == target
}

func isGone(p string) bool {
	_, err := os.Lstat(p)
	return errors.Is(err, os.ErrNotExist)
}

func contents(p string) string {
	b, _ := os.ReadFile(p)
	return string(b)
}

func TestWatchAdoptsNewFile(t *testing.T) {
	e := newEnv(t)
	setDir, dest := linkClaude(t, e)
	logs := startWatcher(t, e)

	writeFile(t, filepath.Join(dest, "agents", "draft.md"), "draft")
	eventually(t, logs, "the new file is adopted", func() bool {
		return isLinkTo(filepath.Join(dest, "agents", "draft.md"), filepath.Join(setDir, "agents", "draft.md"))
	})
	assertRealFile(t, filepath.Join(setDir, "agents", "draft.md"), "draft")
	eventually(t, logs, "the manifest lists it", func() bool {
		return slices.Contains(e.state(t).Linked["claude"].Links, "agents/draft.md")
	})
}

func TestWatchAdoptsFileInNewDirectory(t *testing.T) {
	e := newEnv(t)
	setDir, dest := linkClaude(t, e)
	logs := startWatcher(t, e)

	writeFile(t, filepath.Join(dest, "commands", "deep", "cmd.md"), "c")
	eventually(t, logs, "the file in the new directory is adopted", func() bool {
		return isLinkTo(filepath.Join(dest, "commands", "deep", "cmd.md"), filepath.Join(setDir, "commands", "deep", "cmd.md"))
	})
}

func TestWatchRepairsAtomicSave(t *testing.T) {
	e := newEnv(t)
	setDir, dest := linkClaude(t, e)
	logs := startWatcher(t, e)

	tmp := filepath.Join(dest, ".settings.json.tmp")
	writeFile(t, tmp, `{"saved":true}`)
	if err := os.Rename(tmp, filepath.Join(dest, "settings.json")); err != nil {
		t.Fatal(err)
	}
	eventually(t, logs, "the link is restored", func() bool {
		return isLinkTo(filepath.Join(dest, "settings.json"), filepath.Join(setDir, "settings.json"))
	})
	assertRealFile(t, filepath.Join(setDir, "settings.json"), `{"saved":true}`)
	assertMissing(t, filepath.Join(setDir, ".settings.json.tmp"))
}

func TestWatchPropagatesDeletedLink(t *testing.T) {
	e := newEnv(t)
	setDir, dest := linkClaude(t, e)
	logs := startWatcher(t, e)

	if err := os.Remove(filepath.Join(dest, "settings.json")); err != nil {
		t.Fatal(err)
	}
	eventually(t, logs, "the set file is deleted", func() bool { return isGone(filepath.Join(setDir, "settings.json")) })
	eventually(t, logs, "the manifest drops it", func() bool {
		return !slices.Contains(e.state(t).Linked["claude"].Links, "settings.json")
	})
}

func TestWatchPropagatesDeletedSetFile(t *testing.T) {
	e := newEnv(t)
	setDir, dest := linkClaude(t, e)
	logs := startWatcher(t, e)

	if err := os.Remove(filepath.Join(setDir, "settings.json")); err != nil {
		t.Fatal(err)
	}
	eventually(t, logs, "the dangling link is removed", func() bool { return isGone(filepath.Join(dest, "settings.json")) })
}

func TestWatchLinksNewSetFile(t *testing.T) {
	e := newEnv(t)
	setDir, dest := linkClaude(t, e)
	logs := startWatcher(t, e)

	writeFile(t, filepath.Join(setDir, "commands", "new.md"), "new")
	eventually(t, logs, "the new set file is linked", func() bool {
		return isLinkTo(filepath.Join(dest, "commands", "new.md"), filepath.Join(setDir, "commands", "new.md"))
	})
}

func TestWatchLeavesConflicts(t *testing.T) {
	// A strict set never adopts, so a real file already in dest conflicts
	// with a new set file at the same path (e.g. from git pull).
	e := newEnv(t)
	dest := filepath.Join(e.base, "home", ".config", "app")
	setDir := e.set(t, "app", "dest: "+dest+"\nstrict: true\n", map[string]string{"a.conf": "a"})
	if out, err := run(t, "link", "app"); err != nil {
		t.Fatalf("link: %v\n%s", err, out)
	}
	writeFile(t, filepath.Join(dest, "b.conf"), "dest")
	logs := startWatcher(t, e)

	writeFile(t, filepath.Join(setDir, "b.conf"), "set")
	eventually(t, logs, "the conflict is logged", func() bool { return strings.Contains(logs.String(), "conflict b.conf") })
	assertRealFile(t, filepath.Join(dest, "b.conf"), "dest")
	assertRealFile(t, filepath.Join(setDir, "b.conf"), "set")
}

func TestWatchIgnoresExcludedFiles(t *testing.T) {
	e := newEnv(t)
	setDir, dest := linkClaude(t, e)
	logs := startWatcher(t, e)

	writeFile(t, filepath.Join(dest, "projects", "p", "session.json"), "runtime")
	// A file written after it proves the watcher has handled both.
	writeFile(t, filepath.Join(dest, "marker.md"), "m")
	eventually(t, logs, "the marker is adopted", func() bool {
		return isLinkTo(filepath.Join(dest, "marker.md"), filepath.Join(setDir, "marker.md"))
	})
	assertRealFile(t, filepath.Join(dest, "projects", "p", "session.json"), "runtime")
	assertMissing(t, filepath.Join(setDir, "projects", "p", "session.json"))
}

func TestWatchStartupReconciles(t *testing.T) {
	e := newEnv(t)
	setDir, dest := linkClaude(t, e)
	// Changes made while the watcher was stopped.
	writeFile(t, filepath.Join(dest, "agents", "offline.md"), "o")
	if err := os.Remove(filepath.Join(dest, "settings.json")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(setDir, "pulled.md"), "p")

	startWatcher(t, e)
	assertSymlink(t, filepath.Join(dest, "agents", "offline.md"), filepath.Join(setDir, "agents", "offline.md"))
	assertMissing(t, filepath.Join(setDir, "settings.json"))
	assertSymlink(t, filepath.Join(dest, "pulled.md"), filepath.Join(setDir, "pulled.md"))
	links := e.state(t).Linked["claude"].Links
	if want := []string{"agents/offline.md", "agents/reviewer.md", "pulled.md"}; !slices.Equal(links, want) {
		t.Errorf("manifest after startup = %q, want %q", links, want)
	}
}

func TestWatchDoesNotReadoptUnlinkedCopies(t *testing.T) {
	e := newEnv(t)
	setDir, dest := linkClaude(t, e)
	logs := startWatcher(t, e)

	if out, err := run(t, "unlink", "claude"); err != nil {
		t.Fatalf("unlink: %v\n%s", err, out)
	}
	// Give the watcher several windows to (wrongly) react to the copies.
	writeFile(t, filepath.Join(e.base, "unrelated"), "x")
	time.Sleep(500 * time.Millisecond)
	assertRealFile(t, filepath.Join(dest, "settings.json"), "{}")
	assertRealFile(t, filepath.Join(setDir, "settings.json"), "{}")
	if strings.Contains(logs.String(), "adopted") {
		t.Errorf("watcher adopted after unlink:\n%s", logs)
	}
}

func TestWatchPicksUpNewlyLinkedSet(t *testing.T) {
	e := newEnv(t)
	logs := startWatcher(t, e)

	setDir, dest := claudeSet(t, e)
	if out, err := run(t, "link", "claude"); err != nil {
		t.Fatalf("link: %v\n%s", err, out)
	}
	// Until the watcher has picked up the new set, files written into its
	// dest raise no events, so write a fresh one each time.
	i := 0
	eventually(t, logs, "the new set's dest is watched", func() bool {
		i++
		name := fmt.Sprintf("late%d.md", i)
		writeFile(t, filepath.Join(dest, name), "late")
		time.Sleep(200 * time.Millisecond)
		return isLinkTo(filepath.Join(dest, name), filepath.Join(setDir, name))
	})
}

func TestWatchOnlyOneRuns(t *testing.T) {
	e := newEnv(t)
	startWatcher(t, e)
	envCfg, _ := config.OSEnv()
	err := (&watch.Runtime{StateDir: e.stateDir, Window: time.Millisecond, Env: envCfg, Log: &syncBuffer{}}).Run(context.Background())
	if !errors.Is(err, state.ErrWatcherRunning) {
		t.Errorf("second watcher err = %v, want ErrWatcherRunning", err)
	}
}

func TestStatusReportsRunningWatcher(t *testing.T) {
	e := newEnv(t)
	linkClaude(t, e)
	startWatcher(t, e)
	out, err := run(t, "status")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !strings.Contains(out, "Watcher is running (pid ") {
		t.Errorf("status does not report the watcher:\n%s", out)
	}
}

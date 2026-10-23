package e2e

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stayradiated/mosscap/internal/config"
	"github.com/stayradiated/mosscap/internal/protocol"
	"github.com/stayradiated/mosscap/internal/server"
)

// watchProcess is one running `mosscap task watch` with its stdout lines and
// stderr captured separately, proving stdout carries events only.
type watchProcess struct {
	cmd    *exec.Cmd
	lines  chan string
	mu     sync.Mutex
	stderr strings.Builder
	done   chan struct{}
}

func (w *watchProcess) next(t *testing.T) string {
	t.Helper()
	select {
	case line, ok := <-w.lines:
		if !ok {
			t.Fatalf("watch exited early; stderr=%q", w.stderrText())
		}
		return line
	case <-time.After(10 * time.Second):
		t.Fatalf("no watch event; stderr=%q", w.stderrText())
	}
	return ""
}

func (w *watchProcess) quiet(t *testing.T, d time.Duration) {
	t.Helper()
	select {
	case line := <-w.lines:
		t.Fatalf("unexpected watch event %q", line)
	case <-time.After(d):
	}
}

func (w *watchProcess) stderrText() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.stderr.String()
}

func (w *watchProcess) stop(t *testing.T) {
	t.Helper()
	_ = w.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-w.done:
	case <-time.After(5 * time.Second):
		_ = w.cmd.Process.Kill()
		t.Fatal("watch did not stop on SIGTERM")
	}
	if code := w.cmd.ProcessState.ExitCode(); code != 0 {
		t.Fatalf("watch exit=%d stderr=%q", code, w.stderrText())
	}
}

// startWatchProcess runs the built binary's task watch with a fast poll and
// captures stdout lines and stderr separately.
func startWatchProcess(t *testing.T, binary string, env []string, args ...string) *watchProcess {
	t.Helper()
	cmd := exec.Command(binary, append([]string{"task", "watch", "--interval", "100ms"}, args...)...)
	cmd.Env = env
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	w := &watchProcess{cmd: cmd, lines: make(chan string, 64), done: make(chan struct{})}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var readers sync.WaitGroup
	readers.Add(2)
	go func() {
		defer readers.Done()
		defer close(w.lines)
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			w.lines <- scanner.Text()
		}
	}()
	go func() {
		defer readers.Done()
		b := make([]byte, 4096)
		for {
			n, err := stderr.Read(b)
			w.mu.Lock()
			w.stderr.Write(b[:n])
			w.mu.Unlock()
			if err == io.EOF || err != nil {
				return
			}
		}
	}()
	go func() { readers.Wait(); _ = cmd.Wait(); close(w.done) }()
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	return w
}

func TestBuiltBinaryTaskWatchResumesEscapesAndReconnects(t *testing.T) {
	home := effectiveHome(t)
	root, err := os.MkdirTemp(home, ".mosscap-watch-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(t.TempDir(), "mosscap")
	build := exec.Command("go", "build", "-o", binary, "./cmd/mosscap")
	build.Dir = sourceRoot(t)
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build executable: %v\n%s", err, output)
	}
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	env := append(os.Environ(), "HOME="+home)
	run := func(args ...string) []byte {
		t.Helper()
		cmd := exec.Command(binary, args...)
		cmd.Env = env
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("mosscap %s: %v\n%s", strings.Join(args, " "), err, output)
		}
		return output
	}
	createTask := func(text string) string {
		t.Helper()
		cmd := exec.Command(binary, "task", "create", "--stdin", "--json")
		cmd.Env = env
		cmd.Stdin = strings.NewReader(text)
		output, err := cmd.CombinedOutput()
		var created struct {
			Result protocol.TaskMutationDTO `json:"result"`
		}
		if err != nil || json.Unmarshal(output, &created) != nil || created.Result.TaskID == "" {
			t.Fatalf("task create: %v %s", err, output)
		}
		return created.Result.TaskID
	}
	run("setup", "--json")
	loc, err := config.DefaultLocation()
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(loc)
	if err != nil {
		t.Fatal(err)
	}
	startGateway := func() (context.CancelFunc, chan error) {
		t.Helper()
		s := server.NewWithOptions(cfg, loc, "e2e", nil, server.Options{Disabled: true})
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- s.Serve(ctx) }()
		waitClient(t, cfg.Paths.Endpoint)
		return cancel, done
	}
	stopGateway := func(cancel context.CancelFunc, done chan error) {
		t.Helper()
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("gateway exit: %v", err)
			}
		case <-time.After(6 * time.Second):
			t.Fatal("gateway did not stop")
		}
	}
	startWatch := func(args ...string) *watchProcess {
		t.Helper()
		return startWatchProcess(t, binary, env, args...)
	}

	cancel, done := startGateway()
	backlogTask := createTask("already open")
	workingTask := createTask("already taken")
	run("task", "working", workingTask, "--json")
	cursorFile := filepath.Join(root, "watch.cursor")

	// Without a cursor the watcher first emits currently open tasks only.
	watch := startWatch("--cursor-file", cursorFile)
	if got := watch.next(t); !strings.HasPrefix(got, "cursor=1 change=created task="+backlogTask+" version=1 state=open source=local channel= thread= parent= text=already open") {
		t.Fatalf("backlog line=%q", got)
	}
	multiline := createTask("line one\n\tline two \\ end\n")
	if got, want := watch.next(t), "cursor=4 change=created task="+multiline+" version=1 state=open source=local channel= thread= parent= text=line one\\n\\tline two \\\\ end\\n"; got != want {
		t.Fatalf("multiline line=%q want %q", got, want)
	}
	run("task", "update", backlogTask, "--text", "already open, edited", "--json")
	if got, want := watch.next(t), "cursor=5 change=edited task="+backlogTask+" version=2 state=open source=local channel= thread= parent= text=already open, edited"; got != want {
		t.Fatalf("edit line=%q want %q", got, want)
	}
	// State changes away from open are filtered by default but still advance
	// the cursor, so a restart does not replay them.
	run("task", "done", multiline, "--json")
	watch.quiet(t, 500*time.Millisecond)
	watch.stop(t)
	if b, err := os.ReadFile(cursorFile); err != nil || strings.TrimSpace(string(b)) != "6" {
		t.Fatalf("cursor file=%q err=%v", b, err)
	}

	// Work committed while no watcher runs is delivered exactly once on resume.
	missed := createTask("created while stopped")
	watch = startWatch("--cursor-file", cursorFile)
	if got := watch.next(t); !strings.HasPrefix(got, "cursor=7 change=created task="+missed+" ") {
		t.Fatalf("resumed line=%q", got)
	}
	watch.quiet(t, 500*time.Millisecond)

	// A gateway restart is reported on stderr only and the stream continues.
	stopGateway(cancel, done)
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(watch.stderrText(), "gateway unavailable") && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	cancel, done = startGateway()
	defer func() { stopGateway(cancel, done) }()
	afterRestart := createTask("after gateway restart")
	if got := watch.next(t); !strings.HasPrefix(got, "cursor=8 change=created task="+afterRestart+" ") {
		t.Fatalf("post-restart line=%q stderr=%q", got, watch.stderrText())
	}
	if stderr := watch.stderrText(); !strings.Contains(stderr, "gateway unavailable") || !strings.Contains(stderr, "reconnected to gateway") {
		t.Fatalf("reconnect not reported on stderr: %q", stderr)
	}
	watch.stop(t)

	// --json emits one TaskChangeDTO per line; --state-changes includes done.
	jsonWatch := startWatch("--since", "7", "--json", "--state-changes")
	var change protocol.TaskChangeDTO
	if line := jsonWatch.next(t); json.Unmarshal([]byte(line), &change) != nil || change.ChangeSeq != 8 || change.Change != "created" || change.Task.TaskID != afterRestart || change.Task.Text != "after gateway restart" {
		t.Fatalf("json line=%q", line)
	}
	run("task", "done", afterRestart, "--json")
	if line := jsonWatch.next(t); json.Unmarshal([]byte(line), &change) != nil || change.ChangeSeq != 9 || change.Change != "state" || change.Task.State != "done" {
		t.Fatalf("json state line=%q", line)
	}
	jsonWatch.stop(t)

	// --no-backlog starts at the current head; a cursor from another ledger is refused.
	fresh := startWatch("--no-backlog")
	fresh.quiet(t, 500*time.Millisecond)
	fresh.stop(t)
	ahead := exec.Command(binary, "task", "watch", "--since", "999")
	ahead.Env = env
	if output, err := ahead.CombinedOutput(); err == nil || ahead.ProcessState.ExitCode() != 4 || !strings.Contains(string(output), "cursor_ahead") {
		t.Fatalf("cursor ahead: err=%v output=%s", err, output)
	}
}

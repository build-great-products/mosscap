// Package e2e verifies the executable-level Phase 0 release gate.
package e2e

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestBuiltBinaryRestartReplayGate(t *testing.T) {
	home := effectiveHome(t)
	root, err := os.MkdirTemp(home, ".mosscap-phase0-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}

	binary := filepath.Join(t.TempDir(), "mosscap")
	source := sourceRoot(t)
	build := exec.Command("go", "build", "-o", binary, "./cmd/mosscap")
	build.Dir = source
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build executable: %v\n%s", err, output)
	}

	configPath := filepath.Join(root, "config", "config.toml")
	env := append(os.Environ(), "HOME="+home, "XDG_CONFIG_HOME="+filepath.Join(root, "config"), "XDG_STATE_HOME="+filepath.Join(root, "state"))
	run := func(args ...string) []byte {
		t.Helper()
		cmd := exec.Command(binary, append([]string{"--config", configPath}, args...)...)
		cmd.Env = env
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("mosscap %s: %v\n%s", strings.Join(args, " "), err, output)
		}
		return output
	}
	run("setup", "--json")

	start := func() *exec.Cmd {
		t.Helper()
		cmd := exec.Command(binary, "--config", configPath, "gateway", "run")
		cmd.Env = env
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			probe := exec.Command(binary, "--config", configPath, "gateway", "status", "--json")
			probe.Env = env
			if output, err := probe.Output(); err == nil && json.Valid(output) {
				return cmd
			}
			time.Sleep(20 * time.Millisecond)
		}
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("gateway did not become ready: %s", stderr.String())
		return nil
	}
	stop := func(cmd *exec.Cmd) {
		t.Helper()
		if err := cmd.Process.Signal(os.Interrupt); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("gateway exit: %v", err)
			}
		case <-time.After(6 * time.Second):
			_ = cmd.Process.Kill()
			t.Fatal("gateway did not stop")
		}
	}
	mutation := func(output []byte) map[string]any {
		t.Helper()
		var value struct {
			RequestID string         `json:"request_id"`
			Result    map[string]any `json:"result"`
		}
		if err := json.Unmarshal(output, &value); err != nil {
			t.Fatalf("decode mutation response %q: %v", output, err)
		}
		if value.RequestID == "" || value.Result["task_id"] == "" {
			t.Fatalf("incomplete mutation response: %s", output)
		}
		return value.Result
	}

	gateway := start()
	created := mutation(run("task", "create", "--text", "survives restart", "--request-id", "gate-create", "--json"))
	taskID := created["task_id"].(string)
	transitioned := mutation(run("task", "working", taskID, "--request-id", "gate-working", "--json"))
	stop(gateway)

	gateway = start()
	if replay := mutation(run("task", "create", "--text", "survives restart", "--request-id", "gate-create", "--json")); !sameJSON(created, replay) {
		t.Fatalf("create replay changed result: first=%v replay=%v", created, replay)
	}
	if replay := mutation(run("task", "working", taskID, "--request-id", "gate-working", "--json")); !sameJSON(transitioned, replay) {
		t.Fatalf("transition replay changed result: first=%v replay=%v", transitioned, replay)
	}
	var listed struct {
		Tasks []struct {
			TaskID  string  `json:"task_id"`
			Version float64 `json:"version"`
			State   string  `json:"state"`
			Text    string  `json:"text"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(run("task", "list", "--all", "--json"), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Tasks) != 1 || listed.Tasks[0].TaskID != taskID || listed.Tasks[0].Version != 1 || listed.Tasks[0].State != "working" || listed.Tasks[0].Text != "survives restart" {
		t.Fatalf("unexpected durable task list: %+v", listed.Tasks)
	}
	var shown struct {
		TaskID  string  `json:"task_id"`
		Version float64 `json:"version"`
		State   string  `json:"state"`
		Text    string  `json:"text"`
	}
	if err := json.Unmarshal(run("task", "show", taskID, "--json"), &shown); err != nil {
		t.Fatal(err)
	}
	if shown.TaskID != taskID || shown.Version != 1 || shown.State != "working" || shown.Text != "survives restart" {
		t.Fatalf("unexpected durable task show: %+v", shown)
	}
	stop(gateway)

	db, err := sql.Open("sqlite", filepath.Join(root, "state", "mosscap", "mosscap.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var tasks, events, requests int
	if err := db.QueryRowContext(context.Background(), "SELECT count(*) FROM tasks").Scan(&tasks); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT count(*) FROM task_state_events WHERE task_id=?", taskID).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT count(*) FROM mutation_requests").Scan(&requests); err != nil {
		t.Fatal(err)
	}
	if tasks != 1 || events != 1 || requests != 2 {
		t.Fatalf("durable counts tasks=%d events=%d requests=%d, want 1,1,2", tasks, events, requests)
	}
}

func effectiveHome(t *testing.T) string {
	t.Helper()
	u, err := user.Current()
	if err != nil || u.HomeDir == "" {
		t.Fatalf("effective home: %v", err)
	}
	return u.HomeDir
}

func sourceRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		next := filepath.Dir(dir)
		if next == dir {
			t.Fatal("go.mod not found")
		}
		dir = next
	}
}

func sameJSON(a, b any) bool {
	left, _ := json.Marshal(a)
	right, _ := json.Marshal(b)
	return bytes.Equal(left, right)
}

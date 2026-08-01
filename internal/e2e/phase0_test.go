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

	"github.com/stayradiated/mosscap/internal/client"
	"github.com/stayradiated/mosscap/internal/config"
	"github.com/stayradiated/mosscap/internal/protocol"
	"github.com/stayradiated/mosscap/internal/server"
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
	run("setup", "--json")
	loc, err := config.DefaultLocation()
	if err != nil {
		t.Fatal(err)
	}
	type gatewayProcess struct {
		cancel context.CancelFunc
		done   chan error
	}
	start := func() gatewayProcess {
		t.Helper()
		cfg, err := config.Load(loc)
		if err != nil {
			t.Fatal(err)
		}
		// Production startup always requires a real Slack runtime. This harness
		// injects the disabled runtime directly, retaining a real private socket
		// and SQLite ledger while keeping this Phase 0 replay proof offline.
		s := server.NewWithOptions(cfg, loc, "e2e", nil, server.Options{Disabled: true})
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- s.Serve(ctx) }()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			probe := exec.Command(binary, "gateway", "status", "--json")
			probe.Env = env
			if output, err := probe.Output(); err == nil && json.Valid(output) {
				return gatewayProcess{cancel: cancel, done: done}
			}
			select {
			case err := <-done:
				cancel()
				t.Fatalf("gateway start: %v", err)
			default:
			}
			time.Sleep(20 * time.Millisecond)
		}
		cancel()
		select {
		case err := <-done:
			t.Fatalf("gateway did not become ready: %v", err)
		case <-time.After(6 * time.Second):
			t.Fatal("gateway did not become ready")
		}
		return gatewayProcess{}
	}
	stop := func(g gatewayProcess) {
		t.Helper()
		g.cancel()
		select {
		case err := <-g.done:
			if err != nil {
				t.Fatalf("gateway exit: %v", err)
			}
		case <-time.After(6 * time.Second):
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
		if value.RequestID == "" || len(value.Result) == 0 {
			t.Fatalf("incomplete mutation response: %s", output)
		}
		return value.Result
	}

	gateway := start()
	// File edits are allowed while the gateway runs, but take effect only after
	// an explicit failure-atomic reload.
	if output := run("config", "set", "maximum_file_size", "200MiB", "--json"); !json.Valid(output) {
		t.Fatalf("config set output is not JSON: %s", output)
	}
	var reloaded struct {
		RequestID string `json:"request_id"`
		Result    struct {
			Activated                        bool `json:"activated"`
			SlackNetworkVerificationDeferred bool `json:"slack_network_verification_deferred"`
		} `json:"result"`
	}
	if output := run("gateway", "reload", "--request-id", "gate-reload", "--json"); json.Unmarshal(output, &reloaded) != nil || !reloaded.Result.Activated || !reloaded.Result.SlackNetworkVerificationDeferred {
		t.Fatalf("reload result: %s", output)
	}
	// This uses only the public local wire protocol against the launched binary.
	// A malformed staged file must yield a typed error while its running snapshot
	// (and therefore its generation) remains unchanged.
	configPath := filepath.Join(root, "config", "mosscap", "config.toml")
	validConfig, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	endpoint := filepath.Join(root, "state", "mosscap", "control.sock")
	call := func(request protocol.Request) protocol.Response {
		t.Helper()
		cl, err := client.Dial(context.Background(), endpoint, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		defer cl.Close()
		if _, err := cl.Hello(context.Background(), "e2e-reload-hello-"+request.RequestID, "cli", "e2e", nil); err != nil {
			t.Fatal(err)
		}
		response, err := cl.Call(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		return response
	}
	before := call(protocol.Request{ProtocolVersion: protocol.Version, RequestID: "e2e-status-before", ClientKind: "cli", ClientVersion: "e2e", Command: "status.get", Params: []byte(`{}`)})
	var beforeStatus struct {
		ActiveConfigGeneration int64  `json:"active_config_generation"`
		ActiveConfigDigest     string `json:"active_config_digest"`
	}
	if err := json.Unmarshal(before.Result, &beforeStatus); err != nil || beforeStatus.ActiveConfigGeneration == 0 || beforeStatus.ActiveConfigDigest == "" {
		t.Fatalf("initial active status=%s err=%v", before.Result, err)
	}
	if err := os.WriteFile(configPath, []byte("version = 1\n[paths]\nstate_dir = \"/tmp/obsolete\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	failed := call(protocol.Request{ProtocolVersion: protocol.Version, RequestID: "e2e-invalid-reload", ClientKind: "cli", ClientVersion: "e2e", Command: "config.reload", Params: []byte(`{}`)})
	if failed.Error == nil || failed.Error.Code != "config_reload_failed" || failed.Error.Category != "unsafe" {
		t.Fatalf("invalid reload response = %#v", failed)
	}
	var doctorReport struct {
		Checks []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
		} `json:"checks"`
	}
	if output := run("doctor", "--json"); json.Unmarshal(output, &doctorReport) != nil || !hasCheck(doctorReport.Checks, "config", "fail") || !hasCheck(doctorReport.Checks, "sqlite_integrity", "pass") {
		t.Fatalf("doctor did not diagnose staged config while reaching gateway: %s", output)
	}
	after := call(protocol.Request{ProtocolVersion: protocol.Version, RequestID: "e2e-status-after", ClientKind: "cli", ClientVersion: "e2e", Command: "status.get", Params: []byte(`{}`)})
	var afterStatus struct {
		ActiveConfigGeneration int64  `json:"active_config_generation"`
		ActiveConfigDigest     string `json:"active_config_digest"`
	}
	if err := json.Unmarshal(after.Result, &afterStatus); err != nil || afterStatus != beforeStatus {
		t.Fatalf("invalid reload changed active configuration before=%+v after=%+v err=%v", beforeStatus, afterStatus, err)
	}
	if err := os.WriteFile(configPath, validConfig, 0600); err != nil {
		t.Fatal(err)
	}
	var trusted struct {
		SlackUserIDs []string `json:"slack_user_ids"`
	}
	if output := run("trust", "list", "--json"); json.Unmarshal(output, &trusted) != nil || len(trusted.SlackUserIDs) != 0 {
		t.Fatalf("initial trust list: %s", output)
	}
	firstTrust := mutation(run("trust", "add", "U01234567", "--request-id", "gate-trust-add", "--json"))
	if firstTrust["trusted"] != true || firstTrust["changed"] != true {
		t.Fatalf("trust add result: %v", firstTrust)
	}
	if replay := mutation(run("trust", "add", "U01234567", "--request-id", "gate-trust-add", "--json")); !sameJSON(firstTrust, replay) {
		t.Fatalf("trust add replay changed result: first=%v replay=%v", firstTrust, replay)
	}
	created := mutation(run("task", "create", "--text", "survives restart", "--request-id", "gate-create", "--json"))
	taskID := created["task_id"].(string)
	transitioned := mutation(run("task", "working", taskID, "--request-id", "gate-working", "--json"))
	stop(gateway)

	gateway = start()
	trusted = struct {
		SlackUserIDs []string `json:"slack_user_ids"`
	}{}
	if output := run("trust", "list", "--json"); json.Unmarshal(output, &trusted) != nil || len(trusted.SlackUserIDs) != 1 || trusted.SlackUserIDs[0] != "U01234567" {
		t.Fatalf("trust list after restart: %s", output)
	}
	removed := mutation(run("trust", "remove", "U01234567", "--request-id", "gate-trust-remove", "--json"))
	if removed["trusted"] != false || removed["changed"] != true {
		t.Fatalf("trust remove result: %v", removed)
	}
	if replay := mutation(run("trust", "remove", "U01234567", "--request-id", "gate-trust-remove", "--json")); !sameJSON(removed, replay) {
		t.Fatalf("trust remove replay changed result: first=%v replay=%v", removed, replay)
	}
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
	var tasks, events, requests, trustedUsers int
	if err := db.QueryRowContext(context.Background(), "SELECT count(*) FROM tasks").Scan(&tasks); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT count(*) FROM task_state_events WHERE task_id=?", taskID).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT count(*) FROM mutation_requests").Scan(&requests); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT count(*) FROM trusted_users").Scan(&trustedUsers); err != nil {
		t.Fatal(err)
	}
	if tasks != 1 || events != 1 || requests != 6 || trustedUsers != 0 {
		t.Fatalf("durable counts tasks=%d events=%d requests=%d trusted=%d, want 1,1,6,0", tasks, events, requests, trustedUsers)
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

func hasCheck(checks []struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}, name, status string) bool {
	for _, check := range checks {
		if check.Name == name && check.Status == status {
			return true
		}
	}
	return false
}

func sameJSON(a, b any) bool {
	left, _ := json.Marshal(a)
	right, _ := json.Marshal(b)
	return bytes.Equal(left, right)
}

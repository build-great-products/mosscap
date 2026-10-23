package e2e

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stayradiated/mosscap/internal/client"
	"github.com/stayradiated/mosscap/internal/config"
	"github.com/stayradiated/mosscap/internal/protocol"
	"github.com/stayradiated/mosscap/internal/server"
	"github.com/stayradiated/mosscap/internal/slackapi"
	"github.com/stayradiated/mosscap/internal/slackapi/fake"
)

// slackHarness runs one gateway against strict fake Slack adapters and a
// private XDG root, plus the built CLI that talks to it.
type slackHarness struct {
	t      *testing.T
	cfg    config.Config
	web    *fake.Web
	binary string
	env    []string
	db     *sql.DB
}

const (
	harnessTeam = "T012ABC"
	harnessBot  = "U012BOT"
	harnessUser = "U12345678"
)

// verificationSteps are the Web calls a gateway makes before it starts: one
// channel check per allowlisted channel.
func verificationSteps(allowlist ...string) []fake.Step {
	out := []fake.Step{
		{Operation: fake.Auth, Result: slackapi.AuthIdentity{TeamID: harnessTeam, BotUserID: harnessBot}},
		{Operation: fake.Team, Result: slackapi.Team{ID: harnessTeam}},
	}
	for _, channelID := range allowlist {
		out = append(out, fake.Step{Operation: fake.Channel, Request: channelID, Result: slackapi.Channel{ID: channelID, TeamID: harnessTeam, Member: true, Accessible: true}})
	}
	return append(out, fake.Step{Operation: fake.Scopes, Result: slackapi.RequiredBotScopes()})
}

// reactionSteps scripts the preflight read and write of one reaction effect.
func reactionSteps(operation fake.Operation, channelID, messageTS, emoji string) []fake.Step {
	return []fake.Step{
		{Operation: fake.Permalink, Result: "https://example.invalid/message"},
		{Operation: operation, Request: slackapi.ReactionRequest{ChannelID: channelID, MessageTS: slackapi.Timestamp(messageTS), Emoji: emoji}},
	}
}

func steps(groups ...[]fake.Step) []fake.Step {
	var out []fake.Step
	for _, group := range groups {
		out = append(out, group...)
	}
	return out
}

// newSlackHarness starts the gateway with a scripted Slack session. Envelopes
// are released only after the gateway answers on its local endpoint.
func newSlackHarness(t *testing.T, settings map[string]string, receives []fake.ReceiveStep, script []fake.Step) *slackHarness {
	t.Helper()
	home := effectiveHome(t)
	root, err := os.MkdirTemp(home, ".mosscap-harness-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	loc, err := config.DefaultLocation()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := config.Setup(loc); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"slack_app_token", "slack_bot_token"} {
		if _, err := config.SetSecret(loc, key, map[string]string{"slack_app_token": "xapp-test", "slack_bot_token": "xoxb-test"}[key]); err != nil {
			t.Fatal(err)
		}
	}
	for key, value := range settings {
		if _, err := config.Set(loc, key, value); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := config.Load(loc)
	if err != nil {
		t.Fatal(err)
	}
	gate := fake.NewBarrier()
	if len(receives) != 0 && receives[0].Barrier == nil {
		receives[0].Barrier = gate
	}
	socket := fake.NewSocket(fake.SocketSession{Receives: receives})
	web := fake.NewWeb(script...)
	s := server.NewWithOptions(cfg, loc, "e2e", nil, server.Options{Factory: slackFactory{web: web, socket: socket}})
	ctx, cancel := context.WithCancel(context.Background())
	serve := make(chan error, 1)
	go func() { serve <- s.Serve(ctx) }()
	waitClient(t, cfg.Paths.Endpoint)
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-serve:
			if err != nil {
				t.Errorf("serve: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("server did not stop")
		}
	})
	binary := filepath.Join(t.TempDir(), "mosscap")
	build := exec.Command("go", "build", "-o", binary, "./cmd/mosscap")
	build.Dir = sourceRoot(t)
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v: %s", err, output)
	}
	db, err := sql.Open("sqlite", cfg.Paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	h := &slackHarness{t: t, cfg: cfg, web: web, binary: binary, env: append(os.Environ(), "HOME="+home), db: db}
	gate.Release()
	return h
}

func (h *slackHarness) call(id, command string, params any) protocol.Response {
	h.t.Helper()
	c, err := client.Dial(context.Background(), h.cfg.Paths.Endpoint, uint32(h.cfg.LocalProtocolMaximumFrame))
	if err != nil {
		h.t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Hello(context.Background(), "hello-"+id, "cli", "e2e", nil); err != nil {
		h.t.Fatal(err)
	}
	b, _ := json.Marshal(params)
	out, err := c.Call(context.Background(), protocol.Request{ProtocolVersion: protocol.Version, RequestID: id, ClientKind: "cli", ClientVersion: "e2e", Command: command, Params: b})
	if err != nil {
		h.t.Fatal(err)
	}
	return out
}

// runExit executes the built CLI and returns its combined output and exit code.
func (h *slackHarness) runExit(stdin string, args ...string) ([]byte, int) {
	h.t.Helper()
	cmd := exec.Command(h.binary, args...)
	cmd.Env = h.env
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	out, err := cmd.CombinedOutput()
	if exit, ok := err.(*exec.ExitError); ok {
		return out, exit.ExitCode()
	}
	if err != nil {
		h.t.Fatalf("mosscap %s: %v", strings.Join(args, " "), err)
	}
	return out, 0
}

// run executes the built CLI and fails the test on a non-zero exit.
func (h *slackHarness) run(stdin string, args ...string) []byte {
	h.t.Helper()
	cmd := exec.Command(h.binary, args...)
	cmd.Env = h.env
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		h.t.Fatalf("mosscap %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

// waitWebDone waits until every scripted Web call has been consumed, which
// covers asynchronous admission work such as the queued reaction.
func (h *slackHarness) waitWebDone() {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if h.web.Remaining() == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	h.t.Fatalf("scripted Slack calls remain: %d (calls=%+v)", h.web.Remaining(), h.web.Calls())
}

// waitWebRemaining waits until exactly n scripted calls remain.
func (h *slackHarness) waitWebRemaining(n int) {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if h.web.Remaining() == n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	h.t.Fatalf("scripted Slack calls remaining=%d; want %d (calls=%+v)", h.web.Remaining(), n, h.web.Calls())
}

// count returns one integer from the ledger.
func (h *slackHarness) count(query string, args ...any) int {
	h.t.Helper()
	var n int
	if err := h.db.QueryRow(query, args...).Scan(&n); err != nil {
		h.t.Fatal(err)
	}
	return n
}

// onlyTaskID returns the ID of the single admitted task.
func (h *slackHarness) taskIDs() []string {
	h.t.Helper()
	rows, err := h.db.Query("SELECT task_id FROM tasks ORDER BY created_at, task_id")
	if err != nil {
		h.t.Fatal(err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			h.t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return ids
}

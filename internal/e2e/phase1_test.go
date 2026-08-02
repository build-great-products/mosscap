// Package e2e verifies the Phase 1 Slack release path through its real local protocol.
package e2e

import (
	"context"
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

// TestPhase1SlackVerticalSlice drives Socket admission, the Unix server/client
// protocol, and the built CLI against a real temporary SQLite ledger. The fake
// adapters are deliberately strict: unexpected Web calls (including an
// automatic retry of an ambiguous write) fail the gate.
func TestPhase1SlackVerticalSlice(t *testing.T) {
	home := effectiveHome(t)
	root, err := os.MkdirTemp(home, ".mosscap-phase1-")
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
	cfg, err := config.Setup(loc)
	if err != nil {
		t.Fatal(err)
	}
	for key, value := range map[string]string{"slack_app_token": "xapp-test", "slack_bot_token": "xoxb-test", "slack_channel_id": "C012ABC"} {
		if _, err := config.Set(loc, key, value); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err = config.Load(loc)
	if err != nil {
		t.Fatal(err)
	}

	ack, acks := fake.NewAckRecorder(nil)
	dupAck, dupAcks := fake.NewAckRecorder(nil)
	editAck, editAcks := fake.NewAckRecorder(nil)
	deleteAck, deleteAcks := fake.NewAckRecorder(nil)
	gate := fake.NewBarrier()
	initial := slackapi.Envelope{EnvelopeID: "env-1", EventID: "Ev-1", Kind: slackapi.EventAppMention, TeamID: "T012ABC", ChannelID: "C012ABC", UserID: "U12345678", MessageTS: "1700000000.000001", EventTS: "1700000001.000001", Text: "do this <@U012BOT>", Ack: ack, Files: []slackapi.File{{ID: "F012ABC", Name: "brief.txt", MIMEType: "text/plain", Permalink: "https://example.invalid/file"}}}
	duplicate := initial
	duplicate.EnvelopeID = "env-2"
	duplicate.Ack = dupAck
	updated := initial
	updated.EnvelopeID = "env-3"
	updated.EventID = "Ev-2"
	updated.Kind = slackapi.EventMessageChange
	updated.EventTS = "1700000002.000001"
	updated.Text = "updated <@U012BOT>"
	updated.Ack = editAck
	deleted := updated
	deleted.EnvelopeID = "env-4"
	deleted.EventID = "Ev-3"
	deleted.Kind = slackapi.EventMessageDelete
	deleted.EventTS = "1700000003.000001"
	deleted.Text = ""
	deleted.Files = nil
	deleted.Ack = deleteAck
	socket := fake.NewSocket(fake.SocketSession{Receives: []fake.ReceiveStep{{Envelope: initial, Barrier: gate}, {Envelope: duplicate}, {Envelope: updated}, {Envelope: deleted}}})
	web := fake.NewWeb(
		fake.Step{Operation: fake.Auth, Result: slackapi.AuthIdentity{TeamID: "T012ABC", BotUserID: "U012BOT"}},
		fake.Step{Operation: fake.Team, Result: slackapi.Team{ID: "T012ABC"}},
		fake.Step{Operation: fake.Channel, Result: slackapi.Channel{ID: "C012ABC", TeamID: "T012ABC", Member: true, Accessible: true}},
		fake.Step{Operation: fake.Scopes, Result: slackapi.RequiredBotScopes(false)},
		fake.Step{Operation: fake.History, Request: slackapi.HistoryRequest{ChannelID: "C012ABC", Limit: 50}},
		fake.Step{Operation: fake.PostMessage, Result: slackapi.MessageResult{ChannelID: "C012ABC", MessageTS: "1700000009.000001"}},
		fake.Step{Operation: fake.PostMessage, PostCall: fake.CallScript{Fault: fake.Timeout}},
	)
	s := server.NewWithOptions(cfg, loc, "e2e", nil, server.Options{Factory: slackFactory{web: web, socket: socket}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
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
		case <-time.After(3 * time.Second):
			t.Error("server did not stop")
		}
	})
	call := func(id, command string, params any) protocol.Response {
		t.Helper()
		c, err := client.Dial(context.Background(), cfg.Paths.Endpoint, uint32(cfg.LocalProtocolMaximumFrame))
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		if _, err := c.Hello(context.Background(), "hello-"+id, "cli", "e2e", nil); err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(params)
		out, err := c.Call(context.Background(), protocol.Request{ProtocolVersion: protocol.Version, RequestID: id, ClientKind: "cli", ClientVersion: "e2e", Command: command, Params: b})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	if out := call("trust", "trust.add", protocol.TrustMutationParams{SlackUserID: "U12345678"}); out.Error != nil {
		t.Fatalf("trust: %#v", out.Error)
	}
	gate.Release()
	waitAcks(t, acks, dupAcks, editAcks, deleteAcks)
	listed := call("list", "task.list", protocol.TaskListParams{IncludeDone: true, Limit: 10})
	var tasks struct {
		Tasks []struct {
			TaskID string `json:"task_id"`
		} `json:"tasks"`
	}
	if listed.Error != nil || json.Unmarshal(listed.Result, &tasks) != nil || len(tasks.Tasks) != 1 {
		t.Fatalf("task list=%s error=%#v", listed.Result, listed.Error)
	}
	shown := call("show", "task.get", protocol.TaskSelector{Task: tasks.Tasks[0].TaskID})
	if shown.Error != nil || !strings.Contains(string(shown.Result), `"version":3`) || !strings.Contains(string(shown.Result), `"source_deleted_at"`) || !strings.Contains(string(shown.Result), `"slack_file_id":"F012ABC"`) {
		t.Fatalf("source lifecycle result=%s error=%#v", shown.Result, shown.Error)
	}

	// The executable CLI takes the same negotiated protocol route for reads and writes.
	binary := filepath.Join(t.TempDir(), "mosscap")
	build := exec.Command("go", "build", "-o", binary, "./cmd/mosscap")
	build.Dir = sourceRoot(t)
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v: %s", err, output)
	}
	env := append(os.Environ(), "XDG_CONFIG_HOME="+filepath.Join(root, "config"), "XDG_STATE_HOME="+filepath.Join(root, "state"))
	run := func(args ...string) []byte {
		cmd := exec.Command(binary, args...)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("CLI %v: %v: %s", args, err, out)
		}
		return out
	}
	if out := run("slack", "history", "--json"); !json.Valid(out) {
		t.Fatalf("history=%s", out)
	}
	if out := run("slack", "send", "--text", "confirmed", "--request-id", "confirmed", "--json"); !json.Valid(out) {
		t.Fatalf("send=%s", out)
	}
	// A post-submit timeout is ambiguous. Restart/recovery must not consume a
	// further fake write step; inspection is explicit and preserves the record.
	ambiguous := call("ambiguous", "slack.send", protocol.SlackSendParams{Text: "maybe"})
	if ambiguous.Error == nil || ambiguous.Error.Category != "ambiguous" {
		t.Fatalf("ambiguous=%#v", ambiguous)
	}
	if got := len(web.Calls()); got != 7 {
		t.Fatalf("Web calls=%d; ambiguous retry occurred", got)
	}
	if err := web.AssertDone(); err != nil {
		t.Fatal(err)
	}
}

type slackFactory struct {
	web    slackapi.Web
	socket slackapi.Socket
}

func (f slackFactory) Build(config.Config) (slackapi.Web, slackapi.Socket, error) {
	return f.web, f.socket, nil
}
func waitClient(t *testing.T, endpoint string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		c, err := client.Dial(context.Background(), endpoint, 1<<20)
		if err == nil {
			_ = c.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("gateway did not become ready")
}
func waitAcks(t *testing.T, rs ...*fake.AckRecorder) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		ok := true
		for _, r := range rs {
			ok = ok && r.Calls() == 1
		}
		if ok {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("not every committed envelope was acknowledged")
}

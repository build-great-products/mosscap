package app

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/stayradiated/mosscap/internal/config"
	"github.com/stayradiated/mosscap/internal/protocol"
)

// TestCommandTreeCharacterization records the complete user-visible command
// tree before the CLI parser is replaced. Cobra migration must retain these
// leaves (apart from intentionally documented compatibility changes).
func TestCommandTreeCharacterization(t *testing.T) {
	branches := map[string][]string{
		"config":   {"show", "set", "secret set"},
		"gateway":  {"run", "reload", "status"},
		"trust":    {"list", "add", "remove"},
		"task":     {"create", "list", "show", "update", "open", "working", "done", "reply"},
		"slack":    {"history", "thread", "send", "file", "edit", "delete", "react", "unreact"},
		"effect":   {"list", "inspect", "retry"},
		"file":     {"show", "get"},
		"delivery": {"list", "inspect", "requeue"},
	}
	for branch, children := range branches {
		t.Run(branch, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			exit := (App{Out: &stdout, Err: &stderr, Version: "test"}).Run(context.Background(), []string{branch, "--help"})
			if exit != ExitSuccess || stderr.Len() != 0 {
				t.Fatalf("%s help exit=%d stderr=%q", branch, exit, stderr.String())
			}
			for _, child := range children {
				if !strings.Contains(stdout.String(), "\n  "+child+"\n") {
					t.Errorf("%s help omitted %q:\n%s", branch, child, stdout.String())
				}
			}
		})
	}

	leaves := [][]string{
		{"setup"}, {"config", "show"}, {"config", "set"}, {"config", "secret", "set"},
		{"gateway", "run"}, {"gateway", "reload"}, {"gateway", "status"}, {"doctor"}, {"purge"},
		{"trust", "list"}, {"trust", "add"}, {"trust", "remove"},
		{"task", "create"}, {"task", "list"}, {"task", "show"}, {"task", "update"}, {"task", "open"}, {"task", "working"}, {"task", "done"}, {"task", "reply"},
		{"slack", "history"}, {"slack", "thread"}, {"slack", "send"}, {"slack", "file"}, {"slack", "edit"}, {"slack", "delete"}, {"slack", "react"}, {"slack", "unreact"},
		{"effect", "list"}, {"effect", "inspect"}, {"effect", "retry"}, {"file", "show"}, {"file", "get"},
		{"delivery", "list"}, {"delivery", "inspect"}, {"delivery", "requeue"},
	}
	for _, leaf := range leaves {
		t.Run(strings.Join(leaf, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			args := append(append([]string(nil), leaf...), "--help")
			if exit := (App{Out: &stdout, Err: &stderr, Version: "test"}).Run(context.Background(), args); exit != ExitSuccess || !strings.HasPrefix(stdout.String(), "Usage: mosscap ") || stderr.Len() != 0 {
				t.Fatalf("help exit=%d stdout=%q stderr=%q", exit, stdout.String(), stderr.String())
			}
		})
	}
}

// TestGatewayRequestConstructionCharacterization locks representative parsing
// forms and the exact protocol requests that reach the gateway. In particular,
// positional selectors preceding flags are part of today's CLI contract.
func TestGatewayRequestConstructionCharacterization(t *testing.T) {
	endpoint := characterizationEndpoint(t)
	cases := []struct {
		name       string
		args       []string
		capability string
		command    string
		requestID  string
		params     any
	}{
		{"task create", []string{"task", "create", "--text", "write tests", "--request-id", "req_characterization_create"}, protocol.CapabilityLocalTasks, "task.create", "req_characterization_create", protocol.TaskCreateParams{Text: "write tests"}},
		{"task update selector before flags", []string{"task", "update", "abcdefghijklmnopqrstuv", "--text", "revised", "--request-id", "req_characterization_update"}, protocol.CapabilityLocalTasks, "task.update", "req_characterization_update", protocol.TaskUpdateParams{Task: "abcdefghijklmnopqrstuv", Text: "revised"}},
		{"task transition", []string{"task", "working", "abcdefghijklmnopqrstuv", "--note", "started", "--request-id", "req_characterization_working"}, protocol.CapabilityLocalTasks, "task.transition", "req_characterization_working", protocol.TaskTransitionParams{Task: "abcdefghijklmnopqrstuv", State: "working", Note: stringPointer("started")}},
		{"task list", []string{"task", "list", "--state", "open", "--limit", "7"}, protocol.CapabilityLocalTasks, "task.list", "req_readlist", protocol.TaskListParams{State: stringPointer("open"), Limit: 7}},
		{"task reply", []string{"task", "reply", "abcdefghijklmnopqrstuv", "--text", "reply", "--done", "--request-id", "req_characterization_reply"}, protocol.CapabilityTaskReply, "task.reply", "req_characterization_reply", protocol.TaskReplyParams{Task: "abcdefghijklmnopqrstuv", Text: "reply", Done: true}},
		{"slack send", []string{"slack", "send", "--thread", "1700000000.000001", "--text", "hello", "--task", "abcdefghijklmnopqrstuv", "--request-id", "req_characterization_send"}, protocol.CapabilitySlackEffects, "slack.send", "req_characterization_send", protocol.SlackSendParams{ThreadTS: "1700000000.000001", Text: "hello", Task: "abcdefghijklmnopqrstuv"}},
		{"slack delete selector before flags", []string{"slack", "delete", "1700000000.000002", "--reason", "cleanup", "--request-id", "req_characterization_delete"}, protocol.CapabilitySlackEffects, "slack.delete", "req_characterization_delete", protocol.SlackDeleteParams{MessageTS: "1700000000.000002", Reason: "cleanup"}},
		{"effect retry", []string{"effect", "retry", "eff_AbCdEfGhIjKlMnOpQrStUv", "--reason", "retry", "--confirm-duplicate-risk", "--request-id", "req_characterization_retry"}, protocol.CapabilitySlackEffects, "effect.retry", "req_characterization_retry", protocol.EffectRetryParams{Effect: "eff_AbCdEfGhIjKlMnOpQrStUv", Reason: "retry", ConfirmDuplicateRisk: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			requests := serveCharacterizationGateway(t, endpoint, tc.capability)
			var stdout, stderr bytes.Buffer
			exit := (App{Out: &stdout, Err: &stderr, Version: "characterization"}).Run(context.Background(), tc.args)
			if exit != ExitSuccess {
				t.Fatalf("Run(%q) exit=%d stdout=%q stderr=%q", tc.args, exit, stdout.String(), stderr.String())
			}
			req := <-requests
			if req.Command != tc.command || req.RequestID != tc.requestID || req.ClientKind != "cli" || req.ClientVersion != "characterization" {
				t.Fatalf("request=%+v, want command=%q request_id=%q", req, tc.command, tc.requestID)
			}
			got := reflect.New(reflect.TypeOf(tc.params)).Interface()
			if err := json.Unmarshal(req.Params, got); err != nil || !reflect.DeepEqual(reflect.ValueOf(got).Elem().Interface(), tc.params) {
				t.Fatalf("params=%s decoded=%#v err=%v want=%#v", req.Params, got, err, tc.params)
			}
		})
	}
}

func stringPointer(value string) *string { return &value }

func characterizationEndpoint(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp(home, ".mosscap-characterization-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	var out, stderr bytes.Buffer
	if exit := (App{Out: &out, Err: &stderr}).Run(context.Background(), []string{"setup"}); exit != ExitSuccess {
		t.Fatalf("setup exit=%d stderr=%q", exit, stderr.String())
	}
	location, err := config.DefaultLocation()
	if err != nil {
		t.Fatal(err)
	}
	return location.Paths.Endpoint
}

func serveCharacterizationGateway(t *testing.T, endpoint, capability string) <-chan protocol.Request {
	t.Helper()
	listener, err := net.Listen("unix", endpoint)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	requests := make(chan protocol.Request, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		codec := protocol.NewCodec(conn, conn, 1<<20)
		for index := 0; index < 2; index++ {
			frame, err := codec.ReadFrame()
			if err != nil {
				return
			}
			req, err := protocol.DecodeRequest(frame)
			if err != nil {
				return
			}
			resultType, result := "characterization", []byte(`{}`)
			if index == 0 {
				resultType = "hello"
				result, _ = json.Marshal(protocol.HelloResult{SelectedVersion: protocol.Version, Capabilities: []string{capability}})
			} else {
				requests <- req
			}
			response, _ := protocol.EncodeResponse(protocol.Response{ProtocolVersion: protocol.Version, RequestID: req.RequestID, ServerVersion: "test", ResultType: resultType, Result: result})
			if codec.WriteFrame(response) != nil {
				return
			}
		}
	}()
	return requests
}

package app

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/stayradiated/mosscap/internal/protocol"
)

func TestStdinTextBodiesReachGatewayExactly(t *testing.T) {
	endpoint := characterizationEndpoint(t)
	cases := []struct {
		name, input, command string
		args                 []string
		want                 any
		capability           string
	}{
		{"task create multiline stdin", "first line\nsecond line\n", "task.create", []string{"task", "create", "--stdin", "--request-id", "req_stdin_create"}, protocol.TaskCreateParams{Text: "first line\nsecond line\n"}, protocol.CapabilityLocalTasks},
		{"task create literal backslash-n text", "", "task.create", []string{"task", "create", "--text", "first\\nsecond", "--request-id", "req_text_literal"}, protocol.TaskCreateParams{Text: "first\\nsecond"}, protocol.CapabilityLocalTasks},
		{"task update", "update\n\n", "task.update", []string{"task", "update", "abcdefghijklmnopqrstuv", "--stdin", "--request-id", "req_stdin_update"}, protocol.TaskUpdateParams{Task: "abcdefghijklmnopqrstuv", Text: "update\n\n"}, protocol.CapabilityLocalTasks},
		{"task reply", "reply\n", "task.reply", []string{"task", "reply", "abcdefghijklmnopqrstuv", "--stdin", "--request-id", "req_stdin_reply"}, protocol.TaskReplyParams{Task: "abcdefghijklmnopqrstuv", Text: "reply\n"}, protocol.CapabilityTaskReply},
		{"slack send", "send\n", "slack.send", []string{"slack", "send", "--stdin", "--request-id", "req_stdin_send"}, protocol.SlackSendParams{Text: "send\n"}, protocol.CapabilitySlackEffects},
		{"slack edit", "edit\n", "slack.edit", []string{"slack", "edit", "1700000000.000001", "--stdin", "--request-id", "req_stdin_edit"}, protocol.SlackEditParams{MessageTS: "1700000000.000001", Text: "edit\n"}, protocol.CapabilitySlackEffects},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			requests := serveCharacterizationGateway(t, endpoint, tc.capability)
			var out, err bytes.Buffer
			exit := (App{In: strings.NewReader(tc.input), Out: &out, Err: &err, Version: "stdin-test"}).Run(context.Background(), tc.args)
			if exit != ExitSuccess {
				t.Fatalf("exit=%d stdout=%q stderr=%q", exit, out.String(), err.String())
			}
			req := <-requests
			got := reflect.New(reflect.TypeOf(tc.want)).Interface()
			if req.Command != tc.command || json.Unmarshal(req.Params, got) != nil || !reflect.DeepEqual(reflect.ValueOf(got).Elem().Interface(), tc.want) {
				t.Fatalf("request=%+v params=%s want=%#v", req, req.Params, tc.want)
			}
		})
	}
}

func TestStdinTextBodiesRejectLocally(t *testing.T) {
	_ = characterizationEndpoint(t) // install a valid config but no listener
	for _, tc := range []struct {
		name, input string
		args        []string
		want        string
	}{
		{"empty", "", []string{"task", "create", "--stdin"}, "message text must not be empty"},
		{"nul", "hello\x00there", []string{"task", "create", "--stdin"}, "message text must not contain NUL"},
		{"invalid utf8", "\xff", []string{"task", "create", "--stdin"}, "message text must be valid UTF-8"},
		{"multiple sources", "stdin", []string{"slack", "send", "--stdin", "--text", "also"}, "were all set"},
		{"over limit", strings.Repeat("x", 1<<20+1), []string{"slack", "send", "--stdin"}, "--stdin exceeds the protocol frame limit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, err bytes.Buffer
			exit := (App{In: strings.NewReader(tc.input), Out: &out, Err: &err}).Run(context.Background(), tc.args)
			if exit != ExitUsage || !strings.Contains(err.String(), tc.want) {
				t.Fatalf("exit=%d stdout=%q stderr=%q, want %q", exit, out.String(), err.String(), tc.want)
			}
		})
	}
}

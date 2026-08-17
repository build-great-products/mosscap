package app

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stayradiated/mosscap/internal/protocol"
)

func TestJSONMutationErrorPreservesProtocolFields(t *testing.T) {
	var stdout, stderr bytes.Buffer
	a := App{Out: &stdout, Err: &stderr}
	progress := &protocol.PurgeProgressDTO{}
	err := &commandError{code: ExitUnavailable, message: "unavailable: interrupted", protocolError: &protocol.Error{
		Code: "interrupted", Category: "unavailable", Message: "transport interrupted", Retriable: true,
		Details: map[string]string{"phase": "commit"}, Progress: progress,
	}}
	a.renderMutationError(mutationJSONError(err, true, "req_same").(*jsonMutationError))

	var got mutationErrorJSON
	if decodeErr := json.Unmarshal(stderr.Bytes(), &got); decodeErr != nil {
		t.Fatalf("stderr is not one JSON error: %v: %q", decodeErr, stderr.String())
	}
	if got.Code != "interrupted" || got.Category != "unavailable" || got.Message != "transport interrupted" || !got.Retriable || got.Details["phase"] != "commit" || got.Progress == nil || got.RequestID != "req_same" {
		t.Fatalf("protocol fields lost: %+v", got)
	}
	if got.Guidance != "Replay the exact mutation with the same request ID." {
		t.Fatalf("guidance = %q", got.Guidance)
	}
	if stdout.Len() != 0 {
		t.Fatalf("error wrote to stdout: %q", stdout.String())
	}
	if exitCode(err) != ExitUnavailable {
		t.Fatalf("exit code = %d", exitCode(err))
	}
}

func TestJSONMutationErrorGuidanceAndNonJSONCompatibility(t *testing.T) {
	for _, tc := range []struct{ code, category, want string }{
		{"request_id_conflict", "conflict", "This request ID is bound to another input; do not retry changed input with it."},
		{"x", "ambiguous", "Inspect the durable effect; never automatically resend this mutation."},
		{"x", "external_failed", "Inspect and revise the external failure; do not replay it as if uncertain."},
	} {
		t.Run(tc.code+tc.category, func(t *testing.T) {
			var stderr bytes.Buffer
			a := App{Err: &stderr}
			a.renderMutationError(&jsonMutationError{err: &commandError{code: ExitUnsafe, message: "human", protocolError: &protocol.Error{Code: tc.code, Category: tc.category, Message: "safe"}}, requestID: "req_x"})
			var got mutationErrorJSON
			if err := json.Unmarshal(stderr.Bytes(), &got); err != nil || got.Guidance != tc.want {
				t.Fatalf("got %+v, err %v", got, err)
			}
		})
	}

	err := &commandError{code: ExitUnavailable, message: "existing human error"}
	if got := mutationJSONError(err, false, "req_x"); got != err {
		t.Fatal("non-JSON mutation error was wrapped")
	}
}

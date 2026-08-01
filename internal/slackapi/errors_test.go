package slackapi

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestClassify(t *testing.T) {
	rejected := NewExternalError(ReasonRejected, DiagnosticChannelNotFound, "req-123", errors.New("raw SDK body"))
	malformed := NewExternalError(ReasonInvalidResponse, DiagnosticMalformedResponse, "", errors.New("raw malformed body"))
	tests := []struct {
		name  string
		err   error
		point FailurePoint
		class FailureClass
		code  DiagnosticCode
	}{
		{"nil", nil, FailureAfterSubmit, FailureNone, ""},
		{"conclusive rejection after submit", rejected, FailureAfterSubmit, FailureConclusive, DiagnosticChannelNotFound},
		{"read timeout", context.DeadlineExceeded, FailureRead, FailureUnavailable, DiagnosticTimeout},
		{"pre-submit cancellation", context.Canceled, FailureBeforeSubmit, FailureNotSubmitted, DiagnosticCanceled},
		{"post-submit cancellation", context.Canceled, FailureAfterSubmit, FailureAmbiguous, DiagnosticCanceled},
		{"post-submit disconnect", NewExternalError(ReasonDisconnected, DiagnosticDisconnected, "", nil), FailureAfterSubmit, FailureAmbiguous, DiagnosticDisconnected},
		{"post-submit malformed", malformed, FailureAfterSubmit, FailureAmbiguous, DiagnosticMalformedResponse},
		{"socket disconnect", malformed, FailureSocket, FailureUnavailable, DiagnosticMalformedResponse},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := Classify(test.err, test.point)
			if got.Class != test.class || got.Diagnostic != test.code {
				t.Fatalf("Classify = %#v, want class=%q code=%q", got, test.class, test.code)
			}
		})
	}
}

func TestExternalErrorIsSanitized(t *testing.T) {
	const secret = "xoxb-TOKEN_CANARY raw message body"
	cause := errors.New(secret)
	err := NewExternalError(ReasonRejected, "not-allowlisted", "unsafe request id\n", cause)
	for _, rendered := range []string{
		err.Error(),
		fmt.Sprintf("%s", err),
		fmt.Sprintf("%v", err),
		fmt.Sprintf("%+v", err),
		fmt.Sprintf("%#v", err),
	} {
		if strings.Contains(rendered, secret) || strings.Contains(rendered, "unsafe") {
			t.Fatalf("error rendering leaked unsafe data: %q", rendered)
		}
	}
	classified := Classify(err, FailureRead)
	if classified.Diagnostic != DiagnosticRejected || classified.RequestID != "" {
		t.Fatalf("sanitized classification = %#v", classified)
	}
	if errors.Unwrap(err) != nil || errors.Is(err, cause) {
		t.Fatal("ExternalError exposed its arbitrary SDK cause")
	}
}

func TestExternalErrorRejectsTokenShapedRequestIDs(t *testing.T) {
	for _, requestID := range []string{
		"xoxb-123456789-secret",
		"prefix-xapp-1-A-canary",
		"request/debug",
		strings.Repeat("r", MaxIDBytes+1),
	} {
		err := NewExternalError(ReasonUnavailable, DiagnosticUnavailable, requestID, nil)
		classified := Classify(err, FailureRead)
		if classified.RequestID != "" {
			t.Errorf("request ID %q was retained", requestID)
		}
		if rendered := fmt.Sprintf("%#v", err); strings.Contains(rendered, requestID) {
			t.Errorf("debug rendering retained request ID %q: %q", requestID, rendered)
		}
	}

	err := NewExternalError(ReasonUnavailable, DiagnosticUnavailable, "req-ABC_123.9", nil)
	if got := Classify(err, FailureRead).RequestID; got != "req-ABC_123.9" {
		t.Fatalf("safe request ID = %q", got)
	}
}

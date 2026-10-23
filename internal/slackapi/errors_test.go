package slackapi

import (
	"context"
	"errors"
	"fmt"
	"reflect"
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

func TestMissingScopesErrorOnlyRetainsAllowedScopeNames(t *testing.T) {
	const unsafe = "xoxb:token-canary"
	err := NewMissingScopesError([]Scope{ScopeUsersRead, ScopeChannelsHistory, unsafe, ScopeUsersRead})
	if got := err.MissingScopes(); !reflect.DeepEqual(got, []Scope{ScopeChannelsHistory, ScopeUsersRead}) {
		t.Fatalf("MissingScopes = %v", got)
	}
	for _, rendered := range []string{err.Error(), fmt.Sprintf("%#v", err)} {
		if strings.Contains(rendered, unsafe) {
			t.Fatalf("missing-scope error leaked unsafe input: %q", rendered)
		}
	}
	failure := Classify(err, FailureRead)
	if failure.Class != FailureConclusive || failure.Diagnostic != DiagnosticMissingScope {
		t.Fatalf("Classify = %#v", failure)
	}
	diagnostic, ok := VerificationDiagnosticFromError(err)
	if !ok || diagnostic.Code != DiagnosticMissingScope || !reflect.DeepEqual(diagnostic.MissingScopes, []Scope{ScopeChannelsHistory, ScopeUsersRead}) {
		t.Fatalf("VerificationDiagnosticFromError = %#v, %t", diagnostic, ok)
	}
	if advice := diagnostic.Advice(); strings.Contains(advice, unsafe) || !strings.Contains(advice, "OAuth") {
		t.Fatalf("unsafe or unhelpful advice: %q", advice)
	}
}

func TestVerificationDiagnosticsAreTypedAndActionable(t *testing.T) {
	tests := []struct {
		code DiagnosticCode
		want string
	}{
		{DiagnosticInvalidAuth, "Replace the bot token"},
		{DiagnosticTeamMismatch, "intended workspace"},
		{DiagnosticChannelNotFound, "invite the bot"},
		{DiagnosticChannelArchived, "writable public or private channel"},
		{DiagnosticInvalidRequest, "configured Slack IDs"},
	}
	for _, test := range tests {
		t.Run(string(test.code), func(t *testing.T) {
			err := NewVerificationError(test.code)
			diagnostic, ok := VerificationDiagnosticFromError(err)
			if !ok || diagnostic.Code != test.code || !strings.Contains(diagnostic.Advice(), test.want) {
				t.Fatalf("diagnostic = %#v, %t; advice=%q", diagnostic, ok, diagnostic.Advice())
			}
			if failure := Classify(err, FailureRead); failure.Class != FailureConclusive || failure.Diagnostic != test.code {
				t.Fatalf("Classify = %#v", failure)
			}
		})
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

package slackapi

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// FailurePoint captures what is known about external submission when an error
// is observed.
type FailurePoint string

const (
	FailureRead         FailurePoint = "read"
	FailureSocket       FailurePoint = "socket"
	FailureBeforeSubmit FailurePoint = "before_submit"
	FailureAfterSubmit  FailurePoint = "after_submit"
)

// FailureClass is the durable decision domain code should make.
type FailureClass string

const (
	FailureNone         FailureClass = ""
	FailureConclusive   FailureClass = "conclusive"
	FailureUnavailable  FailureClass = "unavailable"
	FailureNotSubmitted FailureClass = "not_submitted"
	FailureAmbiguous    FailureClass = "ambiguous"
)

// ErrorReason is an adapter-provided, SDK-independent reason.
type ErrorReason string

const (
	ReasonRejected        ErrorReason = "rejected"
	ReasonInvalidResponse ErrorReason = "invalid_response"
	ReasonTimeout         ErrorReason = "timeout"
	ReasonCanceled        ErrorReason = "canceled"
	ReasonDisconnected    ErrorReason = "disconnected"
	ReasonUnavailable     ErrorReason = "unavailable"
)

// DiagnosticCode is a bounded allowlisted value safe for persistence/logging.
type DiagnosticCode string

const (
	DiagnosticInvalidAuth          DiagnosticCode = "invalid_auth"
	DiagnosticTokenRevoked         DiagnosticCode = "token_revoked"
	DiagnosticMissingScope         DiagnosticCode = "missing_scope"
	DiagnosticTeamMismatch         DiagnosticCode = "team_mismatch"
	DiagnosticBotMismatch          DiagnosticCode = "bot_mismatch"
	DiagnosticChannelNotFound      DiagnosticCode = "channel_not_found"
	DiagnosticChannelArchived      DiagnosticCode = "channel_archived"
	DiagnosticChannelReadOnly      DiagnosticCode = "channel_read_only"
	DiagnosticUnsupportedChannel   DiagnosticCode = "unsupported_channel"
	DiagnosticNotInChannel         DiagnosticCode = "not_in_channel"
	DiagnosticUserNotFound         DiagnosticCode = "user_not_found"
	DiagnosticMessageNotFound      DiagnosticCode = "message_not_found"
	DiagnosticRejected             DiagnosticCode = "external_rejected"
	DiagnosticRateLimited          DiagnosticCode = "rate_limited"
	DiagnosticTimeout              DiagnosticCode = "timeout"
	DiagnosticCanceled             DiagnosticCode = "canceled"
	DiagnosticDisconnected         DiagnosticCode = "disconnected"
	DiagnosticMalformedResponse    DiagnosticCode = "malformed_response"
	DiagnosticUnavailable          DiagnosticCode = "unavailable"
	DiagnosticInvalidRequest       DiagnosticCode = "invalid_request"
	DiagnosticUnsafeFileRedirect   DiagnosticCode = "unsafe_file_redirect"
	DiagnosticFileTooLarge         DiagnosticCode = "file_too_large"
	DiagnosticSlackFileUnavailable DiagnosticCode = "slack_file_unavailable"
	DiagnosticPermalinkUnavailable DiagnosticCode = "permalink_unavailable"
)

// Valid reports whether d is in the durable diagnostic allowlist.
func (d DiagnosticCode) Valid() bool {
	switch d {
	case DiagnosticInvalidAuth, DiagnosticTokenRevoked, DiagnosticMissingScope,
		DiagnosticTeamMismatch, DiagnosticBotMismatch, DiagnosticChannelNotFound,
		DiagnosticChannelArchived, DiagnosticChannelReadOnly, DiagnosticUnsupportedChannel,
		DiagnosticNotInChannel, DiagnosticUserNotFound, DiagnosticMessageNotFound,
		DiagnosticRejected, DiagnosticRateLimited, DiagnosticTimeout,
		DiagnosticCanceled, DiagnosticDisconnected, DiagnosticMalformedResponse,
		DiagnosticUnavailable, DiagnosticInvalidRequest, DiagnosticUnsafeFileRedirect,
		DiagnosticFileTooLarge, DiagnosticSlackFileUnavailable, DiagnosticPermalinkUnavailable:
		return true
	default:
		return false
	}
}

// ExternalError carries only sanitized adapter metadata. It deliberately does
// not retain or unwrap an SDK cause: generic error walkers and debug renderers
// must never recover arbitrary external response text.
type ExternalError struct {
	reason     ErrorReason
	diagnostic DiagnosticCode
	requestID  string
}

// MissingScopesError is a secret-safe, typed verification rejection. Its
// scopes are restricted to Mosscap's required bot OAuth scope allowlist.
type MissingScopesError struct {
	missing []Scope
}

// NewMissingScopesError retains a sorted, deduplicated copy of required bot
// scopes only. Invalid or unrelated values are discarded so error rendering
// cannot expose arbitrary external response content.
func NewMissingScopesError(missing []Scope) *MissingScopesError {
	seen := make(map[Scope]struct{}, len(missing))
	safe := make([]Scope, 0, len(missing))
	for _, scope := range missing {
		if !requiredBotScope(scope) {
			continue
		}
		if _, duplicate := seen[scope]; duplicate {
			continue
		}
		seen[scope] = struct{}{}
		safe = append(safe, scope)
	}
	sort.Slice(safe, func(i, j int) bool { return safe[i] < safe[j] })
	return &MissingScopesError{missing: safe}
}

func (e *MissingScopesError) Error() string { return "Slack external failure (missing_scope)" }

// Format prevents debug renderers from observing the private scope slice.
func (e *MissingScopesError) Format(state fmt.State, _ rune) {
	_, _ = state.Write([]byte(e.Error()))
}

// MissingScopes returns a copy of the exact sorted bot OAuth scope names.
func (e *MissingScopesError) MissingScopes() []Scope {
	if e == nil {
		return nil
	}
	return append([]Scope(nil), e.missing...)
}

// MissingScopesFromError extracts safe missing scope names from a verification
// error without exposing arbitrary wrapped causes.
func MissingScopesFromError(err error) ([]Scope, bool) {
	var missing *MissingScopesError
	if !errors.As(err, &missing) {
		return nil, false
	}
	return missing.MissingScopes(), true
}

func requiredBotScope(scope Scope) bool {
	switch scope {
	case ScopeAppMentionsRead, ScopeChatWrite, ScopeReactionsRead, ScopeReactionsWrite,
		ScopeUsersRead, ScopeTeamRead, ScopeFilesRead, ScopeFilesWrite, ScopeChannelsRead, ScopeChannelsHistory,
		ScopeGroupsRead, ScopeGroupsHistory:
		return true
	default:
		return false
	}
}

// NewExternalError creates a classified adapter error, replacing unknown
// diagnostics and unsafe request IDs with bounded defaults. cause is accepted
// at the adapter boundary but deliberately discarded; callers must encode all
// classification facts in reason and diagnostic.
func NewExternalError(reason ErrorReason, diagnostic DiagnosticCode, requestID string, _ error) *ExternalError {
	if !diagnostic.Valid() {
		diagnostic = defaultDiagnostic(reason)
	}
	if !safeRequestID(requestID) {
		requestID = ""
	}
	return &ExternalError{reason: reason, diagnostic: diagnostic, requestID: requestID}
}

func (e *ExternalError) Error() string {
	if e == nil {
		return "Slack external failure"
	}
	if e.requestID != "" {
		return fmt.Sprintf("Slack external failure (%s, request %s)", e.diagnostic, e.requestID)
	}
	return fmt.Sprintf("Slack external failure (%s)", e.diagnostic)
}

// Format forces every fmt rendering, including %#v debug output, through the
// same secret-safe representation as Error.
func (e *ExternalError) Format(state fmt.State, _ rune) {
	_, _ = state.Write([]byte(e.Error()))
}

// Failure is the secret-safe result of classifying an adapter error.
type Failure struct {
	Class      FailureClass
	Diagnostic DiagnosticCode
	RequestID  string
}

// Classify centralizes the conservative effect/read/socket failure policy.
// Rejections are conclusive at every point. Any non-conclusive failure after a
// mutation may have started is ambiguous.
func Classify(err error, point FailurePoint) Failure {
	if err == nil {
		return Failure{}
	}
	reason := reasonFromError(err)
	diagnostic := defaultDiagnostic(reason)
	requestID := ""
	var external *ExternalError
	if errors.As(err, &external) {
		reason = external.reason
		diagnostic = external.diagnostic
		requestID = external.requestID
	}
	var missingScopes *MissingScopesError
	if errors.As(err, &missingScopes) {
		reason = ReasonRejected
		diagnostic = DiagnosticMissingScope
	}
	if reason == ReasonRejected {
		return Failure{Class: FailureConclusive, Diagnostic: diagnostic, RequestID: requestID}
	}
	switch point {
	case FailureRead, FailureSocket:
		return Failure{Class: FailureUnavailable, Diagnostic: diagnostic, RequestID: requestID}
	case FailureBeforeSubmit:
		return Failure{Class: FailureNotSubmitted, Diagnostic: diagnostic, RequestID: requestID}
	case FailureAfterSubmit:
		return Failure{Class: FailureAmbiguous, Diagnostic: diagnostic, RequestID: requestID}
	default:
		return Failure{Class: FailureAmbiguous, Diagnostic: diagnostic, RequestID: requestID}
	}
}

func reasonFromError(err error) ErrorReason {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return ReasonTimeout
	case errors.Is(err, context.Canceled):
		return ReasonCanceled
	default:
		return ReasonUnavailable
	}
}

func defaultDiagnostic(reason ErrorReason) DiagnosticCode {
	switch reason {
	case ReasonRejected:
		return DiagnosticRejected
	case ReasonInvalidResponse:
		return DiagnosticMalformedResponse
	case ReasonTimeout:
		return DiagnosticTimeout
	case ReasonCanceled:
		return DiagnosticCanceled
	case ReasonDisconnected:
		return DiagnosticDisconnected
	default:
		return DiagnosticUnavailable
	}
}

func safeRequestID(value string) bool {
	if value == "" {
		return true
	}
	if len(value) > MaxIDBytes {
		return false
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z') && !(r >= 'A' && r <= 'Z') &&
			!(r >= '0' && r <= '9') && r != '-' && r != '_' && r != '.' {
			return false
		}
	}
	// Slack credentials share recognizable xox*/xapp stems. A request ID that
	// contains either stem is safer to omit than to render or persist.
	lower := strings.ToLower(value)
	return !strings.Contains(lower, "xox") && !strings.Contains(lower, "xapp")
}

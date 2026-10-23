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
	DiagnosticInvalidAuth           DiagnosticCode = "invalid_auth"
	DiagnosticTokenRevoked          DiagnosticCode = "token_revoked"
	DiagnosticMissingScope          DiagnosticCode = "missing_scope"
	DiagnosticTeamMismatch          DiagnosticCode = "team_mismatch"
	DiagnosticBotMismatch           DiagnosticCode = "bot_mismatch"
	DiagnosticChannelNotFound       DiagnosticCode = "channel_not_found"
	DiagnosticChannelArchived       DiagnosticCode = "channel_archived"
	DiagnosticChannelReadOnly       DiagnosticCode = "channel_read_only"
	DiagnosticUnsupportedChannel    DiagnosticCode = "unsupported_channel"
	DiagnosticNotInChannel          DiagnosticCode = "not_in_channel"
	DiagnosticUserNotFound          DiagnosticCode = "user_not_found"
	DiagnosticMessageNotFound       DiagnosticCode = "message_not_found"
	DiagnosticRejected              DiagnosticCode = "external_rejected"
	DiagnosticRateLimited           DiagnosticCode = "rate_limited"
	DiagnosticSocketConnectionLimit DiagnosticCode = "socket_connection_limit"
	DiagnosticTimeout               DiagnosticCode = "timeout"
	DiagnosticCanceled              DiagnosticCode = "canceled"
	DiagnosticDisconnected          DiagnosticCode = "disconnected"
	DiagnosticMalformedResponse     DiagnosticCode = "malformed_response"
	DiagnosticUnavailable           DiagnosticCode = "unavailable"
	DiagnosticInvalidRequest        DiagnosticCode = "invalid_request"
	DiagnosticUnsafeFileRedirect    DiagnosticCode = "unsafe_file_redirect"
	DiagnosticFileTooLarge          DiagnosticCode = "file_too_large"
	DiagnosticSlackFileUnavailable  DiagnosticCode = "slack_file_unavailable"
	DiagnosticPermalinkUnavailable  DiagnosticCode = "permalink_unavailable"
)

// Valid reports whether d is in the durable diagnostic allowlist.
func (d DiagnosticCode) Valid() bool {
	switch d {
	case DiagnosticInvalidAuth, DiagnosticTokenRevoked, DiagnosticMissingScope,
		DiagnosticTeamMismatch, DiagnosticBotMismatch, DiagnosticChannelNotFound,
		DiagnosticChannelArchived, DiagnosticChannelReadOnly, DiagnosticUnsupportedChannel,
		DiagnosticNotInChannel, DiagnosticUserNotFound, DiagnosticMessageNotFound,
		DiagnosticRejected, DiagnosticRateLimited, DiagnosticSocketConnectionLimit,
		DiagnosticTimeout, DiagnosticCanceled, DiagnosticDisconnected, DiagnosticMalformedResponse,
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

// VerificationDiagnostic is the bounded, actionable result of a failed
// startup authority check. It deliberately contains no configured IDs, token,
// response body, or display name. MissingScopes is populated only for
// DiagnosticMissingScope and only with Mosscap's allowlisted bot scopes.
type VerificationDiagnostic struct {
	Code          DiagnosticCode
	MissingScopes []Scope
}

// VerificationError is a typed, secret-safe conclusive verification failure.
// Use VerificationDiagnosticFromError rather than inspecting its rendering.
type VerificationError struct {
	diagnostic VerificationDiagnostic
}

// NewVerificationError creates a typed verification rejection. Only
// conclusive authority/configuration diagnostics are retained; callers cannot
// attach arbitrary text, identifiers, or transient transport outcomes.
func NewVerificationError(code DiagnosticCode) *VerificationError {
	if !verificationRejectionCode(code) {
		code = DiagnosticRejected
	}
	return &VerificationError{diagnostic: VerificationDiagnostic{Code: code}}
}

func verificationRejectionCode(code DiagnosticCode) bool {
	switch code {
	case DiagnosticInvalidAuth, DiagnosticTokenRevoked, DiagnosticMissingScope,
		DiagnosticTeamMismatch, DiagnosticBotMismatch, DiagnosticChannelNotFound,
		DiagnosticChannelArchived, DiagnosticChannelReadOnly, DiagnosticUnsupportedChannel,
		DiagnosticNotInChannel, DiagnosticInvalidRequest, DiagnosticRejected:
		return true
	default:
		return false
	}
}

// NewMissingScopesError retains a sorted, deduplicated copy of required bot
// scopes only. Invalid or unrelated values are discarded so error rendering
// cannot expose arbitrary external response content.
func NewMissingScopesError(missing []Scope) *VerificationError {
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
	return &VerificationError{diagnostic: VerificationDiagnostic{Code: DiagnosticMissingScope, MissingScopes: safe}}
}

func (e *VerificationError) Error() string {
	if e == nil || !e.diagnostic.Code.Valid() {
		return "Slack verification failed"
	}
	return fmt.Sprintf("Slack verification failed (%s)", e.diagnostic.Code)
}

// Format prevents debug renderers from observing the private scope slice.
func (e *VerificationError) Format(state fmt.State, _ rune) { _, _ = state.Write([]byte(e.Error())) }

// MissingScopes returns a copy of the exact sorted bot OAuth scope names.
// It is empty for every verification failure other than missing_scope.
func (e *VerificationError) MissingScopes() []Scope {
	if e == nil || e.diagnostic.Code != DiagnosticMissingScope {
		return nil
	}
	return append([]Scope(nil), e.diagnostic.MissingScopes...)
}

// VerificationDiagnosticFromError returns a copied typed diagnostic.
func VerificationDiagnosticFromError(err error) (VerificationDiagnostic, bool) {
	var verification *VerificationError
	if !errors.As(err, &verification) || verification == nil {
		return VerificationDiagnostic{}, false
	}
	diagnostic := verification.diagnostic
	diagnostic.MissingScopes = append([]Scope(nil), diagnostic.MissingScopes...)
	return diagnostic, true
}

// MissingScopesFromError extracts safe missing scope names from a verification
// error without exposing arbitrary wrapped causes.
func MissingScopesFromError(err error) ([]Scope, bool) {
	diagnostic, ok := VerificationDiagnosticFromError(err)
	if !ok || diagnostic.Code != DiagnosticMissingScope {
		return nil, false
	}
	return diagnostic.MissingScopes, true
}

// Advice gives a stable operator action for a verification diagnostic. Its
// strings are allowlisted and never interpolate Slack-supplied values.
func (d VerificationDiagnostic) Advice() string { return VerificationAdvice(d.Code) }

func VerificationAdvice(code DiagnosticCode) string {
	switch code {
	case DiagnosticInvalidAuth, DiagnosticTokenRevoked:
		return "Replace the bot token with the installed app token, then restart or gateway reload."
	case DiagnosticMissingScope:
		return "Add them in Slack app OAuth & Permissions, reinstall/re-authorize the app, then restart or gateway reload."
	case DiagnosticTeamMismatch, DiagnosticBotMismatch:
		return "Check that the installed Slack app belongs to the intended workspace, then restart or gateway reload."
	case DiagnosticChannelNotFound, DiagnosticNotInChannel:
		return "Check allowlisted channel access and invite the bot to that channel, then restart or gateway reload."
	case DiagnosticChannelArchived, DiagnosticChannelReadOnly, DiagnosticUnsupportedChannel:
		return "Configure a writable public or private channel where the bot is a member, then restart or gateway reload."
	case DiagnosticInvalidRequest:
		return "Check the configured Slack IDs and runtime configuration, then restart or gateway reload."
	case DiagnosticSocketConnectionLimit:
		return "Slack has reached this app's Socket Mode connection limit. Stop duplicate gateway instances, wait for stale connections to close, then retry startup or gateway reload."
	case DiagnosticTimeout, DiagnosticCanceled, DiagnosticDisconnected, DiagnosticUnavailable, DiagnosticRateLimited:
		return "Check Slack network availability and retry startup or gateway reload."
	case DiagnosticMalformedResponse:
		return "Retry startup or gateway reload; if it persists, check Slack service health and app configuration."
	default:
		return "Check Slack app credentials, workspace, bot, and channel access, then restart or gateway reload."
	}
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
	if verification, ok := VerificationDiagnosticFromError(err); ok {
		reason = ReasonRejected
		diagnostic = verification.Code
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

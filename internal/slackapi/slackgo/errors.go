package slackgo

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"

	"github.com/gorilla/websocket"
	"github.com/slack-go/slack"
	"github.com/stayradiated/mosscap/internal/slackapi"
)

var (
	errResponseTooLarge = errors.New("Slack response exceeds adapter limit")
	errRedirectRefused  = errors.New("Slack redirect refused")
	errUnprovenResponse = errors.New("Slack response did not prove success")
)

func invalidRequest() error {
	return slackapi.NewExternalError(slackapi.ReasonRejected, slackapi.DiagnosticInvalidRequest, "", nil)
}

func invalidResponse() error {
	return slackapi.NewExternalError(slackapi.ReasonInvalidResponse, slackapi.DiagnosticMalformedResponse, "", nil)
}

// sanitizeError maps SDK, HTTP, JSON, and transport failures to a bounded error
// and deliberately drops the original error text and object graph.
func sanitizeError(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return slackapi.NewExternalError(slackapi.ReasonTimeout, slackapi.DiagnosticTimeout, "", nil)
	case errors.Is(err, context.Canceled):
		return slackapi.NewExternalError(slackapi.ReasonCanceled, slackapi.DiagnosticCanceled, "", nil)
	case errors.Is(err, errResponseTooLarge), errors.Is(err, errRedirectRefused), errors.Is(err, errUnprovenResponse), errors.Is(err, io.ErrUnexpectedEOF):
		return invalidResponse()
	}

	var slackError slack.SlackErrorResponse
	if errors.As(err, &slackError) {
		reason, diagnostic := slackAPIError(slackError.Err)
		return slackapi.NewExternalError(reason, diagnostic, "", nil)
	}
	var rateLimited *slack.RateLimitedError
	if errors.As(err, &rateLimited) {
		return slackapi.NewExternalError(slackapi.ReasonUnavailable, slackapi.DiagnosticRateLimited, "", nil)
	}
	var status interface{ HTTPStatusCode() int }
	if errors.As(err, &status) {
		code := status.HTTPStatusCode()
		if code == http.StatusRequestTimeout {
			return slackapi.NewExternalError(slackapi.ReasonTimeout, slackapi.DiagnosticTimeout, "", nil)
		}
		if code == http.StatusTooManyRequests {
			return slackapi.NewExternalError(slackapi.ReasonUnavailable, slackapi.DiagnosticRateLimited, "", nil)
		}
		if code >= 400 && code < 500 {
			return slackapi.NewExternalError(slackapi.ReasonRejected, slackapi.DiagnosticRejected, "", nil)
		}
		return slackapi.NewExternalError(slackapi.ReasonUnavailable, slackapi.DiagnosticUnavailable, "", nil)
	}
	var syntax *json.SyntaxError
	var typeError *json.UnmarshalTypeError
	if errors.As(err, &syntax) || errors.As(err, &typeError) {
		return invalidResponse()
	}
	var closeError *websocket.CloseError
	if errors.As(err, &closeError) {
		return slackapi.NewExternalError(slackapi.ReasonDisconnected, slackapi.DiagnosticDisconnected, "", nil)
	}
	var networkError net.Error
	if errors.As(err, &networkError) {
		if networkError.Timeout() {
			return slackapi.NewExternalError(slackapi.ReasonTimeout, slackapi.DiagnosticTimeout, "", nil)
		}
		return slackapi.NewExternalError(slackapi.ReasonUnavailable, slackapi.DiagnosticUnavailable, "", nil)
	}
	return slackapi.NewExternalError(slackapi.ReasonUnavailable, slackapi.DiagnosticUnavailable, "", nil)
}

func slackAPIError(code string) (slackapi.ErrorReason, slackapi.DiagnosticCode) {
	switch strings.ToLower(strings.TrimSpace(code)) {
	case "invalid_auth", "not_authed", "account_inactive":
		return slackapi.ReasonRejected, slackapi.DiagnosticInvalidAuth
	case "token_revoked", "token_expired":
		return slackapi.ReasonRejected, slackapi.DiagnosticTokenRevoked
	case "missing_scope":
		return slackapi.ReasonRejected, slackapi.DiagnosticMissingScope
	case "team_not_found", "team_access_not_granted":
		return slackapi.ReasonRejected, slackapi.DiagnosticTeamMismatch
	case "channel_not_found":
		return slackapi.ReasonRejected, slackapi.DiagnosticChannelNotFound
	case "not_in_channel":
		return slackapi.ReasonRejected, slackapi.DiagnosticNotInChannel
	case "user_not_found", "users_not_found":
		return slackapi.ReasonRejected, slackapi.DiagnosticUserNotFound
	case "message_not_found", "cant_update_message", "cant_delete_message":
		return slackapi.ReasonRejected, slackapi.DiagnosticMessageNotFound
	case "ratelimited", "rate_limited":
		return slackapi.ReasonUnavailable, slackapi.DiagnosticRateLimited
	case "request_timeout":
		return slackapi.ReasonTimeout, slackapi.DiagnosticTimeout
	case "fatal_error", "internal_error", "service_unavailable", "temporarily_unavailable":
		return slackapi.ReasonUnavailable, slackapi.DiagnosticUnavailable
	case "invalid_arguments", "invalid_arg_name", "invalid_array_arg", "invalid_charset", "invalid_form_data", "invalid_post_type", "invalid_ts", "no_text":
		return slackapi.ReasonRejected, slackapi.DiagnosticInvalidRequest
	default:
		// An ok:false Web API response proves Slack conclusively considered the
		// request. Unknown external text is never retained or rendered.
		return slackapi.ReasonRejected, slackapi.DiagnosticRejected
	}
}

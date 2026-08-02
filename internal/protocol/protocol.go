// Package protocol defines Mosscap's version 1 local wire contract. Additive
// commands are negotiated through capabilities; clients must not send a
// command whose capability was absent from hello.
package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/stayradiated/mosscap/internal/identity"
	"github.com/stayradiated/mosscap/internal/slackapi"
)

const Version = 1

const (
	CapabilityStatus              = "status"
	CapabilityDoctor              = "doctor"
	CapabilityLocalTasks          = "local_tasks"
	CapabilityRequestDedup        = "request_dedup"
	CapabilityTrustedUsers        = "trusted_users"
	CapabilityConfigReload        = "config_reload"
	CapabilitySlackRuntime        = "slack_runtime"
	CapabilitySlackReads          = "slack_reads"
	CapabilitySlackEffects        = "slack_effects"
	CapabilitySlackSourceMetadata = "slack_source_metadata"
)

var ServerCapabilities = []string{CapabilityStatus, CapabilityDoctor, CapabilityLocalTasks, CapabilityRequestDedup, CapabilityTrustedUsers, CapabilityConfigReload, CapabilitySlackRuntime, CapabilitySlackReads, CapabilitySlackEffects, CapabilitySlackSourceMetadata}

var (
	ErrEmptyFrame    = errors.New("protocol: empty frame")
	ErrFrameTooLarge = errors.New("protocol: frame exceeds maximum size")
	ErrInvalidUTF8   = errors.New("protocol: frame is not valid UTF-8")
	ErrInvalidJSON   = errors.New("protocol: invalid JSON object")
)

// Request is the common request envelope. Params is decoded according to Command.
type Request struct {
	ProtocolVersion int             `json:"protocol_version"`
	RequestID       string          `json:"request_id"`
	ClientKind      string          `json:"client_kind"`
	ClientVersion   string          `json:"client_version"`
	Command         string          `json:"command"`
	Params          json.RawMessage `json:"params"`
}

// Response is the common response envelope. Exactly one of Result and Error is present.
type Response struct {
	ProtocolVersion int             `json:"protocol_version"`
	RequestID       string          `json:"request_id"`
	ServerVersion   string          `json:"server_version"`
	ResultType      string          `json:"result_type,omitempty"`
	Result          json.RawMessage `json:"result,omitempty"`
	Error           *Error          `json:"error,omitempty"`
}

type Error struct {
	Code      string            `json:"code"`
	Category  string            `json:"category"`
	Message   string            `json:"message"`
	Retriable bool              `json:"retriable"`
	Details   map[string]string `json:"details,omitempty"`
}

// HelloParams initiates a connection. SupportedVersions must include Version.
// Capabilities describe the client; HelloResult capabilities describe the
// server's supported command set.
type HelloParams struct {
	SupportedVersions []int    `json:"supported_versions"`
	Capabilities      []string `json:"capabilities"`
}

type HelloResult struct {
	SelectedVersion int      `json:"selected_version"`
	Capabilities    []string `json:"capabilities"`
}

type EmptyParams struct{}

// TrustMutationParams identifies one immutable Slack user.
type TrustMutationParams struct {
	SlackUserID string `json:"slack_user_id"`
}

type TaskSelector struct {
	Task string `json:"task"`
}
type TaskCreateParams struct {
	Text string `json:"text"`
}
type TaskUpdateParams struct {
	Task string `json:"task"`
	Text string `json:"text"`
}
type TaskTransitionParams struct {
	Task  string  `json:"task"`
	State string  `json:"state"`
	Note  *string `json:"note,omitempty"`
}
type TaskListParams struct {
	State       *string `json:"state,omitempty"`
	IncludeDone bool    `json:"include_done,omitempty"`
	Limit       int     `json:"limit,omitempty"`
	Cursor      string  `json:"cursor,omitempty"`
}

// SlackHistoryParams and SlackThreadParams deliberately contain no workspace
// or channel selector: every operation is pinned to the active runtime.
type SlackHistoryParams struct {
	Limit int `json:"limit,omitempty"`
}
type SlackThreadParams struct {
	ThreadTS string `json:"thread_ts"`
}
type SlackSendParams struct {
	ThreadTS string `json:"thread_ts,omitempty"`
	Text     string `json:"text"`
	Task     string `json:"task,omitempty"`
}
type SlackEditParams struct {
	MessageTS string `json:"message_ts"`
	Text      string `json:"text"`
	Task      string `json:"task,omitempty"`
}
type SlackDeleteParams struct {
	MessageTS string `json:"message_ts"`
	Reason    string `json:"reason"`
	Task      string `json:"task,omitempty"`
}
type SlackReactionParams struct {
	MessageTS string `json:"message_ts"`
	Emoji     string `json:"emoji"`
	Task      string `json:"task,omitempty"`
}
type EffectListParams struct {
	State *string `json:"state,omitempty"`
	Limit int     `json:"limit,omitempty"`
}
type EffectSelector struct {
	Effect string `json:"effect"`
}
type EffectRetryParams struct {
	Effect               string `json:"effect"`
	Reason               string `json:"reason"`
	ConfirmDuplicateRisk bool   `json:"confirm_duplicate_risk"`
}
type FileSelector struct {
	File string `json:"file"`
}

// TaskDTO deliberately represents current task text only; mutation results omit text.
type TaskDTO struct {
	TaskID      string          `json:"task_id"`
	Version     int64           `json:"version"`
	State       string          `json:"state"`
	Text        string          `json:"text"`
	SourceKind  string          `json:"source_kind"`
	CreatedAt   string          `json:"created_at"`
	UpdatedAt   string          `json:"updated_at"`
	DoneAt      *string         `json:"done_at"`
	SlackSource *SlackSourceDTO `json:"slack_source,omitempty"`
	Files       []SlackFileDTO  `json:"files,omitempty"`
}

// SlackSourceDTO and SlackFileDTO expose metadata only. They intentionally
// have no download URL, temporary path, hash, or byte-bearing field.
type SlackSourceDTO struct {
	InitialSourceEventID string  `json:"initial_source_event_id"`
	TeamID               string  `json:"team_id"`
	ChannelID            string  `json:"channel_id"`
	AuthorUserID         string  `json:"author_user_id"`
	MessageTS            string  `json:"message_ts"`
	ThreadTS             string  `json:"thread_ts"`
	Permalink            string  `json:"permalink,omitempty"`
	LatestEventAt        string  `json:"latest_event_at"`
	SourceDeletedAt      *string `json:"source_deleted_at,omitempty"`
}
type SlackFileDTO struct {
	FileID        string `json:"file_id"`
	SlackFileID   string `json:"slack_file_id"`
	Permalink     string `json:"permalink,omitempty"`
	OriginalName  string `json:"original_name"`
	DeclaredMIME  string `json:"declared_mime,omitempty"`
	SizeBytes     *int64 `json:"size_bytes,omitempty"`
	RelatedTaskID string `json:"related_task_id"`
	TransferState string `json:"transfer_state"`
	CreatedAt     string `json:"created_at"`
	UpdatedAt     string `json:"updated_at"`
}

// SlackMessageDTO is returned only to the requesting reader and is never a
// diagnostic type. Text is intentionally absent from effect mutation DTOs.
type SlackReactionDTO struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
	Mine  bool   `json:"mine"`
}
type SlackMessageDTO struct {
	MessageTS    string             `json:"message_ts"`
	ThreadTS     string             `json:"thread_ts"`
	AuthorUserID string             `json:"author_user_id"`
	Text         string             `json:"text"`
	Reactions    []SlackReactionDTO `json:"reactions,omitempty"`
	Files        []SlackFileDTO     `json:"files,omitempty"`
}
type SlackMessagePageDTO struct {
	Messages   []SlackMessageDTO `json:"messages"`
	NextCursor string            `json:"next_cursor,omitempty"`
}

type SlackEffectDTO struct {
	EffectID            string  `json:"effect_id"`
	PredecessorEffectID string  `json:"predecessor_effect_id,omitempty"`
	RelatedTaskID       string  `json:"related_task_id,omitempty"`
	Kind                string  `json:"kind"`
	TargetMessageTS     string  `json:"target_message_ts,omitempty"`
	ThreadTS            string  `json:"thread_ts,omitempty"`
	State               string  `json:"state"`
	AttemptCount        int     `json:"attempt_count"`
	PreparedAt          string  `json:"prepared_at"`
	SubmittedAt         *string `json:"submitted_at,omitempty"`
	CompletedAt         *string `json:"completed_at,omitempty"`
	ResponseMessageTS   string  `json:"response_message_ts,omitempty"`
	DiagnosticCode      string  `json:"diagnostic_code,omitempty"`
}
type SlackEffectIntentDTO struct {
	Text           string `json:"text,omitempty"`
	Emoji          string `json:"emoji,omitempty"`
	OperatorReason string `json:"operator_reason,omitempty"`
}
type EffectInspectDTO struct {
	Effect SlackEffectDTO       `json:"effect"`
	Intent SlackEffectIntentDTO `json:"intent"`
}
type EffectListDTO struct {
	Effects []SlackEffectDTO `json:"effects"`
}
type SlackEffectCountsDTO struct {
	Total          int64            `json:"total"`
	ActionRequired int64            `json:"action_required"`
	ByState        map[string]int64 `json:"by_state"`
}
type SlackRuntimeStatusDTO struct {
	State                  string               `json:"state"`
	TeamID                 string               `json:"team_id,omitempty"`
	BotUserID              string               `json:"bot_user_id,omitempty"`
	ChannelID              string               `json:"channel_id,omitempty"`
	ReconnectAttempt       int                  `json:"reconnect_attempt"`
	BackoffMilliseconds    int64                `json:"backoff_milliseconds"`
	LastDiagnosticCode     string               `json:"last_diagnostic_code,omitempty"`
	LastEnvelopeReceivedAt *string              `json:"last_envelope_received_at,omitempty"`
	LastSourceCommittedAt  *string              `json:"last_source_committed_at,omitempty"`
	SourceResults          map[string]uint64    `json:"source_results,omitempty"`
	Effects                SlackEffectCountsDTO `json:"effects"`
}
type StatusDTO struct {
	Slack *SlackRuntimeStatusDTO `json:"slack,omitempty"`
}
type DoctorDTO struct {
	Checks []DoctorCheckDTO `json:"checks"`
}
type DoctorCheckDTO struct {
	Name    string `json:"name"`
	Status  string `json:"status"`
	Message string `json:"message"`
	Action  string `json:"action,omitempty"`
}
type TaskMutationDTO struct {
	TaskID    string `json:"task_id"`
	Version   int64  `json:"version"`
	State     string `json:"state"`
	Changed   bool   `json:"changed"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}
type TaskListDTO struct {
	Tasks      []TaskDTO `json:"tasks"`
	NextCursor string    `json:"next_cursor,omitempty"`
}

// TrustedUserListDTO contains lexicographically sorted immutable Slack user IDs.
type TrustedUserListDTO struct {
	SlackUserIDs []string `json:"slack_user_ids"`
}

// TrustMutationDTO reports the resulting authorization state for one Slack user.
type TrustMutationDTO struct {
	SlackUserID string `json:"slack_user_id"`
	Trusted     bool   `json:"trusted"`
	Changed     bool   `json:"changed"`
}

// ConfigReloadDTO deliberately contains no configuration values or secrets.
// SlackNetworkVerificationDeferred is true because Phase 1 does not yet make
// Slack network verification part of reload activation.
type ConfigReloadDTO struct {
	Activated                        bool   `json:"activated"`
	SlackNetworkVerificationDeferred bool   `json:"slack_network_verification_deferred"`
	ActiveConfigGeneration           int64  `json:"active_config_generation,omitempty"`
	ActiveConfigDigest               string `json:"active_config_digest,omitempty"`
}

func (r Request) Validate() error {
	if r.ProtocolVersion != Version {
		return fmt.Errorf("protocol: unsupported version %d", r.ProtocolVersion)
	}
	if !identity.ValidRequestID(r.RequestID) {
		return errors.New("protocol: invalid request_id")
	}
	if r.ClientKind != "cli" && r.ClientKind != "extension" {
		return errors.New("protocol: invalid client_kind")
	}
	if r.ClientVersion == "" || len(r.ClientVersion) > 128 {
		return errors.New("protocol: invalid client_version")
	}
	if !validCommand(r.Command) {
		return fmt.Errorf("protocol: unknown command %q", r.Command)
	}
	if len(r.Params) == 0 {
		return errors.New("protocol: missing params")
	}
	return ValidateParams(r.Command, r.Params)
}

func (r Response) Validate() error {
	if r.ProtocolVersion != Version {
		return fmt.Errorf("protocol: unsupported version %d", r.ProtocolVersion)
	}
	if !identity.ValidRequestID(r.RequestID) {
		return errors.New("protocol: invalid request_id")
	}
	if r.ServerVersion == "" || len(r.ServerVersion) > 128 {
		return errors.New("protocol: invalid server_version")
	}
	result := len(r.Result) != 0
	if result == (r.Error != nil) {
		return errors.New("protocol: response must contain exactly one result or error")
	}
	if result && r.ResultType == "" {
		return errors.New("protocol: result missing result_type")
	}
	if !result && r.ResultType != "" {
		return errors.New("protocol: error response has result_type")
	}
	if result && !json.Valid(r.Result) {
		return errors.New("protocol: invalid result JSON")
	}
	if r.Error != nil {
		return r.Error.Validate()
	}
	return nil
}
func (e Error) Validate() error {
	if e.Code == "" || len(e.Code) > 128 || e.Category == "" || len(e.Category) > 32 || e.Message == "" || len(e.Message) > 1024 {
		return errors.New("protocol: invalid error")
	}
	switch e.Category {
	case "usage", "not_found", "conflict", "unsafe", "unavailable", "external_failed", "ambiguous", "unsupported":
	default:
		return errors.New("protocol: invalid error category")
	}
	if len(e.Details) > 16 {
		return errors.New("protocol: too many error details")
	}
	if unsafeDiagnostic(e.Message) {
		return errors.New("protocol: unsafe error message")
	}
	for k, v := range e.Details {
		if k == "" || len(k) > 64 || len(v) > 256 || unsafeDiagnostic(k) || unsafeDiagnostic(v) {
			return errors.New("protocol: invalid error details")
		}
	}
	return nil
}

func ValidateParams(command string, raw json.RawMessage) error {
	var target any
	switch command {
	case "hello":
		target = &HelloParams{}
	case "status.get", "doctor.run", "trust.list", "config.reload":
		target = &EmptyParams{}
	case "slack.history":
		target = &SlackHistoryParams{}
	case "slack.thread":
		target = &SlackThreadParams{}
	case "slack.send":
		target = &SlackSendParams{}
	case "slack.edit":
		target = &SlackEditParams{}
	case "slack.delete":
		target = &SlackDeleteParams{}
	case "slack.react", "slack.unreact":
		target = &SlackReactionParams{}
	case "effect.list":
		target = &EffectListParams{}
	case "effect.inspect":
		target = &EffectSelector{}
	case "effect.retry":
		target = &EffectRetryParams{}
	case "file.show":
		target = &FileSelector{}
	case "trust.add", "trust.remove":
		target = &TrustMutationParams{}
	case "task.create":
		target = &TaskCreateParams{}
	case "task.list":
		target = &TaskListParams{}
	case "task.get":
		target = &TaskSelector{}
	case "task.update":
		target = &TaskUpdateParams{}
	case "task.transition":
		target = &TaskTransitionParams{}
	default:
		return fmt.Errorf("protocol: unknown command %q", command)
	}
	if err := decodeStrict(raw, target); err != nil {
		return fmt.Errorf("protocol: invalid %s params: %w", command, err)
	}
	switch p := target.(type) {
	case *HelloParams:
		if len(p.SupportedVersions) == 0 || len(p.SupportedVersions) > 16 || len(p.Capabilities) > 32 {
			return errors.New("invalid hello parameters")
		}
		supported := false
		for _, v := range p.SupportedVersions {
			if v <= 0 {
				return errors.New("invalid hello version")
			}
			supported = supported || v == Version
		}
		if !supported {
			return errors.New("unsupported hello version")
		}
	case *TrustMutationParams:
		if !validSlackUserID(p.SlackUserID) {
			return errors.New("invalid Slack user ID")
		}
	case *TaskCreateParams:
		if p.Text == "" {
			return errors.New("missing task text")
		}
	case *TaskSelector:
		if p.Task == "" || len(p.Task) > 128 {
			return errors.New("invalid task selector")
		}
	case *TaskUpdateParams:
		if p.Task == "" || p.Text == "" {
			return errors.New("invalid task update")
		}
	case *TaskTransitionParams:
		if p.Task == "" || (p.State != "open" && p.State != "working" && p.State != "done") || (p.Note != nil && len(*p.Note) > 4096) {
			return errors.New("invalid task transition")
		}
	case *TaskListParams:
		if p.State != nil && *p.State != "open" && *p.State != "working" && *p.State != "done" {
			return errors.New("invalid list state")
		}
		if p.Limit < 0 || p.Limit > 1000 || len(p.Cursor) > 1024 || (p.State != nil && p.IncludeDone) {
			return errors.New("invalid task list")
		}
	case *SlackHistoryParams:
		if p.Limit < 0 || p.Limit > slackapi.MaxReadLimit {
			return errors.New("invalid Slack history limit")
		}
	case *SlackThreadParams:
		if !validTimestamp(p.ThreadTS) {
			return errors.New("invalid Slack thread timestamp")
		}
	case *SlackSendParams:
		if (p.ThreadTS != "" && !validTimestamp(p.ThreadTS)) || !validText(p.Text, true) || !validTask(p.Task) {
			return errors.New("invalid Slack send")
		}
	case *SlackEditParams:
		if !validTimestamp(p.MessageTS) || !validText(p.Text, true) || !validTask(p.Task) {
			return errors.New("invalid Slack edit")
		}
	case *SlackDeleteParams:
		if !validTimestamp(p.MessageTS) || !validReason(p.Reason) || !validTask(p.Task) {
			return errors.New("invalid Slack delete")
		}
	case *SlackReactionParams:
		if !validTimestamp(p.MessageTS) || !validTask(p.Task) {
			return errors.New("invalid Slack reaction")
		}
		if _, err := slackapi.NormalizeEmoji(p.Emoji); err != nil {
			return errors.New("invalid Slack emoji")
		}
	case *EffectListParams:
		if p.Limit < 0 || p.Limit > 100 || (p.State != nil && !validEffectState(*p.State)) {
			return errors.New("invalid effect list")
		}
	case *EffectSelector:
		if !validEffectSelector(p.Effect) {
			return errors.New("invalid effect selector")
		}
	case *EffectRetryParams:
		if !validEffectSelector(p.Effect) || !validReason(p.Reason) {
			return errors.New("invalid effect retry")
		}
		// The store additionally requires this confirmation when the resolved
		// predecessor is ambiguous; protocol preserves it as explicit evidence.
	case *FileSelector:
		if !validFileSelector(p.File) {
			return errors.New("invalid file selector")
		}
	}
	return nil
}

// DecodeRequest and DecodeResponse reject unknown fields and trailing JSON values.
func DecodeRequest(payload []byte) (Request, error) {
	var r Request
	if err := decodeStrict(payload, &r); err != nil {
		return r, err
	}
	return r, r.Validate()
}
func DecodeResponse(payload []byte) (Response, error) {
	var r Response
	if err := decodeStrict(payload, &r); err != nil {
		return r, err
	}
	return r, r.Validate()
}
func EncodeRequest(r Request) ([]byte, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(r)
}
func EncodeResponse(r Response) ([]byte, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(r)
}

func decodeStrict(b []byte, v any) error {
	if len(bytes.TrimSpace(b)) == 0 || bytes.TrimSpace(b)[0] != '{' {
		return ErrInvalidJSON
	}
	if !utf8.Valid(b) {
		return ErrInvalidUTF8
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidJSON, err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return ErrInvalidJSON
	}
	return nil
}
func validCommand(c string) bool {
	switch c {
	case "hello", "status.get", "doctor.run", "trust.list", "trust.add", "trust.remove", "config.reload", "task.create", "task.list", "task.get", "task.update", "task.transition", "slack.history", "slack.thread", "slack.send", "slack.edit", "slack.delete", "slack.react", "slack.unreact", "effect.list", "effect.inspect", "effect.retry", "file.show":
		return true
	}
	return false
}

func validTimestamp(value string) bool {
	_, err := slackapi.ParseTimestamp(value)
	return err == nil
}
func validText(value string, required bool) bool {
	return (!required || value != "") && len(value) <= slackapi.MaxTextBytes && utf8.ValidString(value) && !strings.ContainsRune(value, 0)
}
func validReason(value string) bool {
	return value != "" && len(value) <= 4096 && utf8.ValidString(value) && !strings.ContainsRune(value, 0)
}
func validTask(value string) bool           { return value == "" || validTypedSelector(value, "tsk_") }
func validEffectSelector(value string) bool { return validTypedSelector(value, "eff_") }
func validFileSelector(value string) bool   { return validTypedSelector(value, "fil_") }
func validTypedSelector(value, prefix string) bool {
	if !strings.HasPrefix(value, prefix) || len(value) <= len(prefix) || len(value) > 128 || !utf8.ValidString(value) {
		return false
	}
	for _, r := range value[len(prefix):] {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}
func validEffectState(value string) bool {
	switch value {
	case "prepared", "submitted", "confirmed", "ambiguous", "failed":
		return true
	}
	return false
}

// unsafeDiagnostic rejects common credential/header forms from the wire error
// envelope. Errors must use bounded diagnostic codes rather than upstream text.
func unsafeDiagnostic(value string) bool {
	lower := strings.ToLower(value)
	return strings.Contains(lower, "xoxb-") || strings.Contains(lower, "xapp-") || strings.Contains(lower, "authorization:") || strings.Contains(lower, "bearer ")
}

// validSlackUserID accepts Slack's immutable, uppercase user-ID form only.
func validSlackUserID(id string) bool {
	if len(id) < 9 || len(id) > 64 || (id[0] != 'U' && id[0] != 'W') {
		return false
	}
	for _, c := range id[1:] {
		if c < '0' || c > '9' {
			if c < 'A' || c > 'Z' {
				return false
			}
		}
	}
	return true
}

// NewErrorResponse builds a valid typed error envelope.
func NewErrorResponse(requestID, serverVersion, code, category, message string, retriable bool) Response {
	message = strings.TrimSpace(message)
	if unsafeDiagnostic(message) {
		message = "operation failed; inspect safe diagnostic code"
	}
	return Response{ProtocolVersion: Version, RequestID: requestID, ServerVersion: serverVersion, Error: &Error{Code: code, Category: category, Message: message, Retriable: retriable}}
}

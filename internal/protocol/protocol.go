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
	"path"
	"regexp"
	"strings"
	"time"
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
	CapabilityFileTransfer        = "file_transfer_v1"
	CapabilityMetadataPurge       = "metadata_purge_v1"
	CapabilityTaskReply           = "task_reply_v1"
	// CapabilityPiDeliveryMVP is deliberately defined but not advertised until
	// the gateway owns every delivery route and disconnect cleanup (T1).
	CapabilityPiDeliveryMVP = "pi_delivery_mvp"
)

var ServerCapabilities = []string{CapabilityStatus, CapabilityDoctor, CapabilityLocalTasks, CapabilityRequestDedup, CapabilityTrustedUsers, CapabilityConfigReload, CapabilitySlackRuntime, CapabilitySlackReads, CapabilitySlackEffects, CapabilitySlackSourceMetadata, CapabilityFileTransfer, CapabilityMetadataPurge, CapabilityTaskReply}

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
	Progress  *PurgeProgressDTO `json:"progress,omitempty"`
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

// MetadataPurgeParams deliberately carries only a canonical RFC3339 cutoff
// and an explicit mode. The request envelope carries the confirm request ID.
type MetadataPurgeParams struct {
	Cutoff string `json:"cutoff"`
	Mode   string `json:"mode"`
}

// PurgeCountsDTO is bounded to the schema-v5 tables; arbitrary selectors are
// intentionally not part of the protocol.
type PurgeCountsDTO struct {
	Tasks                  int64 `json:"tasks"`
	TaskStateEvents        int64 `json:"task_state_events"`
	SlackSourceEvents      int64 `json:"slack_source_events"`
	SlackTaskSources       int64 `json:"slack_task_sources"`
	PiDeliveries           int64 `json:"pi_deliveries"`
	SlackEffects           int64 `json:"slack_effects"`
	Files                  int64 `json:"files"`
	FileTemporaryTransfers int64 `json:"file_temporary_transfers"`
	MutationRequests       int64 `json:"mutation_requests"`
}

type MetadataPurgeDTO struct {
	Cutoff            string         `json:"cutoff"`
	Mode              string         `json:"mode"`
	Eligible          PurgeCountsDTO `json:"eligible"`
	SkippedLive       PurgeCountsDTO `json:"skipped_live"`
	SkippedReferenced PurgeCountsDTO `json:"skipped_referenced"`
	Removed           PurgeCountsDTO `json:"removed"`
}

// PurgeProgressDTO is included only on a retriable purge error. It reports
// work committed before the interrupted batch, never speculative deletions.
type PurgeProgressDTO struct {
	Removed PurgeCountsDTO `json:"removed"`
}

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

// TaskReplyParams composes a durable Slack reply to a Slack-origin task with
// an optional local completion. The thread is intentionally not caller input.
type TaskReplyParams struct {
	Task string `json:"task"`
	Text string `json:"text"`
	Done bool   `json:"done,omitempty"`
}

type TaskReplyDTO struct {
	Effect SlackEffectDTO   `json:"effect"`
	Task   *TaskMutationDTO `json:"task,omitempty"`
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
	ThreadTS  string   `json:"thread_ts,omitempty"`
	Text      string   `json:"text"`
	Task      string   `json:"task,omitempty"`
	FilePaths []string `json:"file_paths,omitempty"`
}
type SlackFileParams struct {
	ThreadTS string `json:"thread_ts,omitempty"`
	FilePath string `json:"file_path"`
	Comment  string `json:"comment,omitempty"`
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
type FileGetParams struct {
	File string `json:"file"`
}
type FileReleaseParams struct {
	TransferID string `json:"transfer_id"`
}

// DeliveryObservation is exact persisted-session evidence. It intentionally
// contains neither session paths nor visible message contents.
type DeliveryObservation struct {
	DeliveryID  string `json:"delivery_id"`
	DeliveryKey string `json:"delivery_key"`
}
type DeliveryOpenParams struct {
	PiSessionID string                `json:"pi_session_id"`
	Observed    []DeliveryObservation `json:"observed,omitempty"`
}
type DeliveryInsertingParams struct {
	DeliveryID  string `json:"delivery_id"`
	DeliveryKey string `json:"delivery_key"`
}
type DeliveryPersistedParams struct {
	DeliveryID  string `json:"delivery_id"`
	DeliveryKey string `json:"delivery_key"`
}
type DeliveryRejectedParams struct {
	DeliveryID     string `json:"delivery_id"`
	DeliveryKey    string `json:"delivery_key"`
	DiagnosticCode string `json:"diagnostic_code"`
}
type DeliveryListParams struct {
	State *string `json:"state,omitempty"`
	Limit int     `json:"limit,omitempty"`
}
type DeliverySelector struct {
	Delivery string `json:"delivery"`
}
type DeliveryRequeueParams struct {
	Delivery string `json:"delivery"`
	Reason   string `json:"reason"`
}

type DeliveryDTO struct {
	DeliveryID            string  `json:"delivery_id"`
	TaskID                string  `json:"task_id"`
	TaskVersion           int64   `json:"task_version"`
	PiSessionID           string  `json:"pi_session_id"`
	DeliveryKey           string  `json:"delivery_key"`
	PredecessorDeliveryID string  `json:"predecessor_delivery_id,omitempty"`
	State                 string  `json:"state"`
	DiagnosticCode        string  `json:"diagnostic_code,omitempty"`
	RequeueReason         string  `json:"requeue_reason,omitempty"`
	OfferedAt             *string `json:"offered_at,omitempty"`
	InsertingAt           *string `json:"inserting_at,omitempty"`
	CompletedAt           *string `json:"completed_at,omitempty"`
	CreatedAt             string  `json:"created_at"`
	UpdatedAt             string  `json:"updated_at"`
}
type DeliveryCountsDTO struct {
	EligibleBacklogCount int64 `json:"eligible_backlog_count"`
	UncertainCount       int64 `json:"uncertain_count"`
	RejectedCount        int64 `json:"rejected_count"`
}
type DeliveryOpenDTO struct {
	PiSessionID string `json:"pi_session_id"`
	DeliveryCountsDTO
}
type DeliveryWorkDTO struct {
	DeliveryID    string                    `json:"delivery_id"`
	DeliveryKey   string                    `json:"delivery_key"`
	TaskID        string                    `json:"task_id"`
	TaskVersion   int64                     `json:"task_version"`
	TaskState     string                    `json:"task_state"`
	TaskText      string                    `json:"task_text"`
	SourceKind    string                    `json:"source_kind"`
	SlackSource   *DeliverySlackSourceDTO   `json:"slack_source,omitempty"`
	AttachedFiles []DeliveryAttachedFileDTO `json:"attached_files,omitempty"`
}
type DeliverySlackSourceDTO struct {
	AuthorUserID string `json:"author_user_id"`
	MessageTS    string `json:"message_ts"`
	ThreadTS     string `json:"thread_ts"`
}

// DeliveryAttachedFileDTO is the deliberately narrow attachment projection for
// Pi work. It is incoming metadata only: no URLs, paths, bytes, hashes, or
// outgoing/detached records are ever represented here.
type DeliveryAttachedFileDTO struct {
	FileID            string  `json:"file_id"`
	OriginalName      string  `json:"original_name"`
	DeclaredMIME      *string `json:"declared_mime"`
	DeclaredSizeBytes *int64  `json:"declared_size_bytes"`
	TransferState     string  `json:"transfer_state"`
}
type DeliveryPollDTO struct {
	Work *DeliveryWorkDTO `json:"work,omitempty"`
	DeliveryCountsDTO
}
type DeliveryListDTO struct {
	Deliveries []DeliveryDTO `json:"deliveries"`
}
type DeliveryRequeueDTO struct {
	Delivery                   DeliveryDTO `json:"delivery"`
	DuplicateInsertionPossible bool        `json:"duplicate_insertion_possible"`
}

// TaskDTO deliberately represents current task text only; mutation results omit text.
type TaskDTO struct {
	TaskID       string               `json:"task_id"`
	Version      int64                `json:"version"`
	State        string               `json:"state"`
	Text         string               `json:"text"`
	SourceKind   string               `json:"source_kind"`
	CreatedAt    string               `json:"created_at"`
	UpdatedAt    string               `json:"updated_at"`
	DoneAt       *string              `json:"done_at"`
	SlackSource  *SlackSourceDTO      `json:"slack_source,omitempty"`
	Files        []SlackFileDTO       `json:"files,omitempty"`
	SlackActions *SlackTaskActionsDTO `json:"slack_actions,omitempty"`
}

// SlackTaskActionsDTO is the durable recovery state for a Slack-origin task.
// ReplyStatus is none, prepared, submitted, confirmed, failed, or ambiguous;
// CompletionStatus is done or not_done. It never contains reply text.
type SlackTaskActionsDTO struct {
	ReplyTarget      SlackReplyTargetDTO `json:"reply_target"`
	ReplyStatus      string              `json:"reply_status"`
	ReplyEffectID    string              `json:"reply_effect_id,omitempty"`
	CompletionStatus string              `json:"completion_status"`
}

// SlackReplyTargetDTO identifies the exact thread to which task replies go.
type SlackReplyTargetDTO struct {
	TeamID    string `json:"team_id"`
	ChannelID string `json:"channel_id"`
	ThreadTS  string `json:"thread_ts"`
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
	FileID             string  `json:"file_id"`
	Direction          string  `json:"direction"`
	SlackFileID        string  `json:"slack_file_id,omitempty"`
	Permalink          string  `json:"permalink,omitempty"`
	OriginalName       string  `json:"original_name"`
	DeclaredMIME       string  `json:"declared_mime,omitempty"`
	DeclaredSizeBytes  *int64  `json:"declared_size_bytes,omitempty"`
	ValidatedSizeBytes *int64  `json:"validated_size_bytes,omitempty"`
	SHA256             string  `json:"sha256,omitempty"`
	RelatedTaskID      string  `json:"related_task_id,omitempty"`
	RelatedEffectID    string  `json:"related_effect_id,omitempty"`
	EffectOrdinal      *int64  `json:"effect_ordinal,omitempty"`
	SourceAttached     bool    `json:"source_attached"`
	SourceDetachedAt   *string `json:"source_detached_at,omitempty"`
	TransferState      string  `json:"transfer_state"`
	DiagnosticCode     string  `json:"diagnostic_code,omitempty"`
	CreatedAt          string  `json:"created_at"`
	UpdatedAt          string  `json:"updated_at"`
}
type FileGetDTO struct {
	TransferID         string `json:"transfer_id"`
	FileID             string `json:"file_id"`
	TemporaryPath      string `json:"temporary_path"`
	ValidatedSizeBytes int64  `json:"validated_size_bytes"`
	SHA256             string `json:"sha256"`
	ExpiresAt          string `json:"expires_at"`
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
type FileUploadSourceDTO struct {
	FileID       string `json:"file_id"`
	SourcePath   string `json:"source_path"`
	OriginalName string `json:"original_name"`
	SizeBytes    int64  `json:"validated_size_bytes"`
	SHA256       string `json:"sha256"`
}
type EffectInspectDTO struct {
	Effect        SlackEffectDTO        `json:"effect"`
	Intent        SlackEffectIntentDTO  `json:"intent"`
	OutgoingFiles []FileUploadSourceDTO `json:"outgoing_files,omitempty"`
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
	State                          string               `json:"state"`
	TeamID                         string               `json:"team_id,omitempty"`
	BotUserID                      string               `json:"bot_user_id,omitempty"`
	ChannelID                      string               `json:"channel_id,omitempty"`
	IdentitySource                 string               `json:"identity_source,omitempty"`
	ReconnectAttempt               int                  `json:"reconnect_attempt"`
	BackoffMilliseconds            int64                `json:"backoff_milliseconds"`
	LastDiagnosticCode             string               `json:"last_diagnostic_code,omitempty"`
	LastVerificationDiagnosticCode string               `json:"last_verification_diagnostic_code,omitempty"`
	PriorRuntimeRetained           bool                 `json:"prior_runtime_retained,omitempty"`
	LastEnvelopeReceivedAt         *string              `json:"last_envelope_received_at,omitempty"`
	LastSourceCommittedAt          *string              `json:"last_source_committed_at,omitempty"`
	SourceResults                  map[string]uint64    `json:"source_results,omitempty"`
	Effects                        SlackEffectCountsDTO `json:"effects"`
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

// TrustedUserListDTO contains lexicographically sorted immutable Slack user
// IDs and any presentation-only usernames resolved from the active runtime.
type TrustedUserListDTO struct {
	SlackUserIDs   []string          `json:"slack_user_ids"`
	SlackUserNames map[string]string `json:"slack_user_names,omitempty"`
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
	case "status.get", "doctor.run", "trust.list", "config.reload", "delivery.poll", "delivery.close":
		target = &EmptyParams{}
	case "metadata.purge":
		target = &MetadataPurgeParams{}
	case "slack.history":
		target = &SlackHistoryParams{}
	case "slack.thread":
		target = &SlackThreadParams{}
	case "slack.send":
		target = &SlackSendParams{}
	case "slack.file":
		target = &SlackFileParams{}
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
	case "file.get":
		target = &FileGetParams{}
	case "file.release":
		target = &FileReleaseParams{}
	case "delivery.open":
		target = &DeliveryOpenParams{}
	case "delivery.inserting":
		target = &DeliveryInsertingParams{}
	case "delivery.persisted":
		target = &DeliveryPersistedParams{}
	case "delivery.rejected":
		target = &DeliveryRejectedParams{}
	case "delivery.list":
		target = &DeliveryListParams{}
	case "delivery.inspect":
		target = &DeliverySelector{}
	case "delivery.requeue":
		target = &DeliveryRequeueParams{}
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
	case "task.reply":
		target = &TaskReplyParams{}
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
	case *MetadataPurgeParams:
		if p.Mode != "dry_run" && p.Mode != "confirm" {
			return errors.New("invalid metadata purge mode")
		}
		if _, err := ParsePurgeCutoff(p.Cutoff); err != nil {
			return errors.New("invalid metadata purge cutoff")
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
	case *TaskReplyParams:
		if p.Task == "" || !validText(p.Text, true) {
			return errors.New("invalid task reply")
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
		if (p.ThreadTS != "" && !validTimestamp(p.ThreadTS)) || !validText(p.Text, true) || !validTask(p.Task) || !validPaths(p.FilePaths) {
			return errors.New("invalid Slack send")
		}
	case *SlackFileParams:
		if (p.ThreadTS != "" && !validTimestamp(p.ThreadTS)) || !validPath(p.FilePath) || !validText(p.Comment, false) || !validTask(p.Task) {
			return errors.New("invalid Slack file")
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
	case *FileGetParams:
		if !validFileSelector(p.File) {
			return errors.New("invalid file selector")
		}
	case *FileReleaseParams:
		if !validTransferSelector(p.TransferID) {
			return errors.New("invalid transfer selector")
		}
	case *DeliveryOpenParams:
		if !validPiSessionID(p.PiSessionID) || len(p.Observed) > 256 {
			return errors.New("invalid delivery open")
		}
		seen := make(map[string]bool, len(p.Observed))
		for _, o := range p.Observed {
			if !validDeliveryID(o.DeliveryID) || !validDeliveryKey(o.DeliveryKey) || seen[o.DeliveryID+"\x00"+o.DeliveryKey] {
				return errors.New("invalid delivery observation")
			}
			seen[o.DeliveryID+"\x00"+o.DeliveryKey] = true
		}
	case *DeliveryInsertingParams:
		if !validDeliveryID(p.DeliveryID) || !validDeliveryKey(p.DeliveryKey) {
			return errors.New("invalid delivery inserting")
		}
	case *DeliveryPersistedParams:
		if !validDeliveryID(p.DeliveryID) || !validDeliveryKey(p.DeliveryKey) {
			return errors.New("invalid delivery persisted")
		}
	case *DeliveryRejectedParams:
		if !validDeliveryID(p.DeliveryID) || !validDeliveryKey(p.DeliveryKey) || !validDeliveryDiagnostic(p.DiagnosticCode) {
			return errors.New("invalid delivery rejected")
		}
	case *DeliveryListParams:
		if p.Limit < 0 || p.Limit > 1000 || (p.State != nil && !validDeliveryState(*p.State)) {
			return errors.New("invalid delivery list")
		}
	case *DeliverySelector:
		if !validDeliverySelector(p.Delivery) {
			return errors.New("invalid delivery selector")
		}
	case *DeliveryRequeueParams:
		if !validDeliverySelector(p.Delivery) || !validReason(p.Reason) {
			return errors.New("invalid delivery requeue")
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
	case "hello", "status.get", "doctor.run", "trust.list", "trust.add", "trust.remove", "config.reload", "metadata.purge", "task.create", "task.list", "task.get", "task.update", "task.transition", "task.reply", "slack.history", "slack.thread", "slack.send", "slack.file", "slack.edit", "slack.delete", "slack.react", "slack.unreact", "effect.list", "effect.inspect", "effect.retry", "file.show", "file.get", "file.release", "delivery.open", "delivery.poll", "delivery.inserting", "delivery.persisted", "delivery.rejected", "delivery.close", "delivery.list", "delivery.inspect", "delivery.requeue":
		return true
	}
	return false
}

// ParsePurgeCutoff accepts the canonical RFC3339 lexical form used by purge:
// calendar date, T separator, exactly six fractional microseconds, and either
// Z or a numeric ±HH:MM offset. time.Parse is intentionally preceded by this
// grammar check because it accepts useful-but-noncanonical variants.
var purgeCutoffSyntax = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}\.[0-9]{6}(Z|[+-][0-9]{2}:[0-9]{2})$`)

func ParsePurgeCutoff(value string) (time.Time, error) {
	if len(value) == 0 || len(value) > 64 || !purgeCutoffSyntax.MatchString(value) {
		return time.Time{}, errors.New("invalid canonical RFC3339 cutoff")
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, errors.New("invalid canonical RFC3339 cutoff")
	}
	return parsed.UTC().Truncate(time.Microsecond), nil
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
func validTask(value string) bool             { return value == "" || validTypedSelector(value, "tsk_") }
func validEffectSelector(value string) bool   { return validTypedSelector(value, "eff_") }
func validFileSelector(value string) bool     { return validTypedSelector(value, "fil_") }
func validTransferSelector(value string) bool { return validTypedSelector(value, "xfr_") }
func validTypedSelector(value, prefix string) bool {
	if !strings.HasPrefix(value, prefix) || len(value) <= len(prefix) || len(value) > 128 || !utf8.ValidString(value) {
		return false
	}
	for _, r := range value[len(prefix):] {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}
func validPath(value string) bool {
	return value != "" && len(value) <= 4096 && utf8.ValidString(value) && !strings.ContainsRune(value, 0) && strings.HasPrefix(value, "/") && path.Clean(value) == value
}
func validPaths(values []string) bool {
	if len(values) > 20 {
		return false
	}
	for _, value := range values {
		if !validPath(value) {
			return false
		}
	}
	return true
}
func validDeliveryID(value string) bool { return validDeliverySelector(value) && len(value) == 26 }
func validDeliverySelector(value string) bool {
	if !strings.HasPrefix(value, "dlv_") || len(value) <= 4 || len(value) > 128 || !utf8.ValidString(value) {
		return false
	}
	for _, r := range value[4:] {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}
func validDeliveryKey(value string) bool {
	if len(value) != 71 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	for _, r := range value[7:] {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}
func validPiSessionID(value string) bool {
	return value != "" && len(value) <= 256 && utf8.ValidString(value) && !strings.ContainsRune(value, 0)
}
func validDeliveryDiagnostic(value string) bool {
	// This is intentionally an allowlist; diagnostic text is never an
	// extension-to-gateway transport channel.
	switch value {
	case "oversized_task", "invalid_delivery_work", "invalid_visible_message":
		return true
	}
	return false
}
func validDeliveryState(value string) bool {
	return value == "pending" || value == "inserting" || value == "persisted" || value == "uncertain" || value == "rejected"
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

package store

import (
	"errors"
	"time"

	"github.com/stayradiated/mosscap/internal/task"
)

// SlackAdmissionResult is the durable outcome of one normalized Slack event.
type SlackAdmissionResult string

const (
	SlackAdmitted           SlackAdmissionResult = "admitted"
	SlackUpdated            SlackAdmissionResult = "updated"
	SlackDeleted            SlackAdmissionResult = "deleted"
	SlackDuplicate          SlackAdmissionResult = "duplicate"
	SlackIgnoredWorkspace   SlackAdmissionResult = "ignored_workspace"
	SlackIgnoredChannel     SlackAdmissionResult = "ignored_channel"
	SlackIgnoredUntrusted   SlackAdmissionResult = "ignored_untrusted"
	SlackIgnoredUnmentioned SlackAdmissionResult = "ignored_unmentioned"
	SlackIgnoredBot         SlackAdmissionResult = "ignored_bot"
	SlackIgnoredStale       SlackAdmissionResult = "ignored_stale"
	SlackIgnoredUntracked   SlackAdmissionResult = "ignored_untracked"
	SlackMalformed          SlackAdmissionResult = "malformed"
)

// SlackSourceEventKind is a normalized source operation.
type SlackSourceEventKind string

// Slack event kinds accepted by CommitSlackSourceEvent.
const (
	SlackEventAppMention    SlackSourceEventKind = "app_mention"
	SlackEventMessage       SlackSourceEventKind = "message"
	SlackEventMessageChange SlackSourceEventKind = "message_changed"
	SlackEventMessageDelete SlackSourceEventKind = "message_deleted"
)

// ErrSlackInvariant reports a schema-valid row combination that violates the
// stronger repository contract (for example, a source attached to local task).
var ErrSlackInvariant = errors.New("Slack repository invariant violated")

// SlackAdmissionPolicy is the immutable configuration snapshot attached to an
// event receipt. User trust is deliberately not included: it is read from the
// current trusted_users table in the same transaction as admission.
type SlackAdmissionPolicy struct {
	TeamID    string `json:"team_id"`
	ChannelID string `json:"channel_id"`
	BotUserID string `json:"bot_user_id"`
}

// SlackSourceEvent is an SDK-free, normalized event. SlackEventAtUS is used
// only for source ordering; message and thread timestamps remain opaque Slack
// identifiers. ReceivedAt may be zero, in which case the repository clock is
// used.
type SlackSourceEvent struct {
	SourceEventID  string               `json:"source_event_id"`
	EnvelopeID     string               `json:"envelope_id,omitempty"`
	EventKind      SlackSourceEventKind `json:"event_kind"`
	TeamID         string               `json:"team_id"`
	ChannelID      string               `json:"channel_id"`
	UserID         string               `json:"user_id,omitempty"`
	BotID          string               `json:"bot_id,omitempty"`
	Subtype        string               `json:"subtype,omitempty"`
	MessageTS      string               `json:"message_ts"`
	ThreadTS       string               `json:"thread_ts,omitempty"`
	Text           string               `json:"text,omitempty"`
	Permalink      string               `json:"permalink,omitempty"`
	SlackEventAtUS int64                `json:"slack_event_at_us"`
	ReceivedAt     time.Time            `json:"received_at,omitempty"`
	Files          []SlackIncomingFile  `json:"files,omitempty"`
}

// SlackIncomingFile contains metadata only. The repository never accepts file
// bytes, authenticated download URLs, hashes, or temporary paths here.
type SlackIncomingFile struct {
	SlackFileID  string `json:"slack_file_id"`
	Permalink    string `json:"permalink,omitempty"`
	OriginalName string `json:"original_name"`
	DeclaredMIME string `json:"declared_mime,omitempty"`
	SizeBytes    *int64 `json:"size_bytes,omitempty"`
}

// SlackEventResult is the canonical committed result for SourceEventID.
// DuplicateDelivery is true only when this call replayed an existing row;
// AdmissionResult remains the original canonical result.
type SlackEventResult struct {
	SourceEventID     string               `json:"source_event_id"`
	AdmissionResult   SlackAdmissionResult `json:"admission_result"`
	TaskID            string               `json:"task_id,omitempty"`
	DiagnosticCode    string               `json:"diagnostic_code,omitempty"`
	SlackEventAtUS    int64                `json:"slack_event_at_us,omitempty"`
	ReceivedAt        time.Time            `json:"received_at"`
	CommittedAt       time.Time            `json:"committed_at"`
	TaskVersion       int64                `json:"task_version,omitempty"`
	Changed           bool                 `json:"changed"`
	RedeliveryCount   int64                `json:"redelivery_count"`
	LastSeenAt        time.Time            `json:"last_seen_at"`
	DuplicateDelivery bool                 `json:"duplicate_delivery"`
}

// SlackSource is the durable source reference joined to its Slack task.
type SlackSource struct {
	TaskID               string     `json:"task_id"`
	InitialSourceEventID string     `json:"initial_source_event_id"`
	TeamID               string     `json:"team_id"`
	ChannelID            string     `json:"channel_id"`
	AuthorUserID         string     `json:"author_user_id"`
	MessageTS            string     `json:"message_ts"`
	ThreadTS             string     `json:"thread_ts"`
	Permalink            string     `json:"permalink,omitempty"`
	LatestEventAtUS      int64      `json:"latest_event_at_us"`
	SourceDeletedAt      *time.Time `json:"source_deleted_at,omitempty"`
	CreatedAt            time.Time  `json:"created_at"`
	UpdatedAt            time.Time  `json:"updated_at"`
	TaskVersion          int64      `json:"task_version"`
	TaskState            task.State `json:"task_state"`
	TaskText             string     `json:"task_text"`
}

// SlackFile is one metadata-only incoming file attached to an admitted source.
type SlackFile struct {
	FileID        string    `json:"file_id"`
	SlackFileID   string    `json:"slack_file_id"`
	Permalink     string    `json:"permalink,omitempty"`
	OriginalName  string    `json:"original_name"`
	DeclaredMIME  string    `json:"declared_mime,omitempty"`
	SizeBytes     *int64    `json:"size_bytes,omitempty"`
	RelatedTaskID string    `json:"related_task_id"`
	TransferState string    `json:"transfer_state"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// SlackSourceEventCounts is bounded inventory suitable for status output.
type SlackSourceEventCounts struct {
	Total        int64                          `json:"total"`
	Redeliveries int64                          `json:"redeliveries"`
	Sources      int64                          `json:"sources"`
	Files        int64                          `json:"files"`
	ByResult     map[SlackAdmissionResult]int64 `json:"by_result"`
}

package slackapi

import (
	"context"
	"io"
)

// Web is the SDK-free subset of Slack Web API operations required by Phase 1.
// Implementations must return bounded DTOs and classified, secret-safe errors.
type Web interface {
	Auth(context.Context) (AuthIdentity, error)
	Team(context.Context, string) (Team, error)
	User(context.Context, string) (User, error)
	Channel(context.Context, string) (Channel, error)
	Scopes(context.Context) ([]Scope, error)
	Permalink(context.Context, PermalinkRequest) (string, error)
	History(context.Context, HistoryRequest) (MessagePage, error)
	Replies(context.Context, RepliesRequest) (MessagePage, error)
	PostMessage(context.Context, PostMessageRequest) (MessageResult, error)
	UpdateMessage(context.Context, UpdateMessageRequest) (MessageResult, error)
	DeleteMessage(context.Context, DeleteMessageRequest) error
	AddReaction(context.Context, ReactionRequest) error
	RemoveReaction(context.Context, ReactionRequest) error
}

// Socket opens independently injectable Socket Mode streams. A successful
// Open means the candidate transport is ready to receive envelopes.
type Socket interface {
	Open(context.Context) (EnvelopeStream, error)
}

// FileTransfer is the deliberately small file-byte boundary. It owns fresh
// Slack metadata lookup, bounded download redirects, and Slack's external
// three-step upload protocol. File bytes and authenticated URLs never cross
// this contract except as streaming readers/writers.
type FileTransfer interface {
	Download(context.Context, DownloadRequest, io.Writer) (File, error)
	Upload(context.Context, UploadRequest) (UploadResult, error)
}

// DownloadRequest identifies one admitted Slack file and its immutable byte
// limit. The writer is owned by the caller and receives at most limit bytes;
// implementations read one additional byte to classify a streamed overrun
// without unbounded allocation.
type DownloadRequest struct {
	FileID       string
	MaximumBytes int64
}

// UploadSource is one prepared local file. Validate is invoked after its bytes
// have staged and before completion; a validation failure must prevent any
// completion request. Reader is consumed exactly once.
type UploadSource struct {
	Name          string
	SizeBytes     int64
	Reader        io.Reader
	ValidateAfter func(context.Context) error
}

// UploadRequest is one ordered file operation for the active, already
// verified channel. It deliberately has no team or arbitrary destination.
type UploadRequest struct {
	ChannelID      string
	ThreadTS       Timestamp
	InitialComment string
	Files          []UploadSource
}

// UploadedFile is completion evidence for one reserved file.
type UploadedFile struct {
	ID         string
	Permalink  string
	Diagnostic DiagnosticCode
}

// UploadResult is conclusive only when Files exactly matches the reservation
// count and order.
type UploadResult struct {
	Files []UploadedFile
}

// EnvelopeStream emits normalized envelopes. Exactly one Receive may be active
// at a time. Canceling an active Receive closes the owned transport and returns
// the context error; callers reconnect rather than reuse that stream. A
// bounded, identifiable malformed frame may return both an envelope carrying
// its acknowledgement path and an error. Unidentifiable frames return no ack.
// Close stops future receives and is idempotent.
type EnvelopeStream interface {
	Receive(context.Context) (Envelope, error)
	Close() error
}

type PermalinkRequest struct {
	ChannelID string    `json:"channel_id"`
	MessageTS Timestamp `json:"message_ts"`
}

type HistoryRequest struct {
	ChannelID string `json:"channel_id"`
	Limit     int    `json:"limit"`
	Cursor    string `json:"cursor,omitempty"`
}

type RepliesRequest struct {
	ChannelID string    `json:"channel_id"`
	ThreadTS  Timestamp `json:"thread_ts"`
	Limit     int       `json:"limit"`
	Cursor    string    `json:"cursor,omitempty"`
}

type PostMessageRequest struct {
	ChannelID       string    `json:"channel_id"`
	ThreadTS        Timestamp `json:"thread_ts,omitempty"`
	Text            string    `json:"text"`
	ClientMessageID string    `json:"client_message_id"`
}

type UpdateMessageRequest struct {
	ChannelID string    `json:"channel_id"`
	MessageTS Timestamp `json:"message_ts"`
	Text      string    `json:"text"`
}

type DeleteMessageRequest struct {
	ChannelID string    `json:"channel_id"`
	MessageTS Timestamp `json:"message_ts"`
}

type ReactionRequest struct {
	ChannelID string    `json:"channel_id"`
	MessageTS Timestamp `json:"message_ts"`
	Emoji     string    `json:"emoji"`
}

// MessageResult is the conclusive identifier returned by post/update.
type MessageResult struct {
	ChannelID string    `json:"channel_id"`
	MessageTS Timestamp `json:"message_ts"`
}

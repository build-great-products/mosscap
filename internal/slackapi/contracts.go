package slackapi

import "context"

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

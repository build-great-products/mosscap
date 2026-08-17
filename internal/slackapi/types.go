// Package slackapi defines Mosscap-owned Slack contracts and values.
//
// SDK-specific types belong in adapter packages such as slackapi/slackgo. The
// rest of Mosscap depends only on this package.
package slackapi

// EventKind is a normalized Slack event understood by Slack ingestion.
type EventKind string

const (
	EventAppMention    EventKind = "app_mention"
	EventMessage       EventKind = "message"
	EventMessageChange EventKind = "message_changed"
	EventMessageDelete EventKind = "message_deleted"
)

// Valid reports whether k is a supported normalized event kind.
func (k EventKind) Valid() bool {
	return k == EventAppMention || k == EventMessage || k == EventMessageChange || k == EventMessageDelete
}

// Envelope is the bounded, SDK-free representation emitted by Socket Mode.
// EventTS orders edits and deletions. MessageTS remains the opaque source
// identity used by subsequent Slack operations.
type Envelope struct {
	EnvelopeID string           `json:"envelope_id"`
	EventID    string           `json:"event_id"`
	Kind       EventKind        `json:"event_kind"`
	TeamID     string           `json:"team_id"`
	ChannelID  string           `json:"channel_id"`
	UserID     string           `json:"user_id,omitempty"`
	BotID      string           `json:"bot_id,omitempty"`
	Subtype    string           `json:"subtype,omitempty"`
	MessageTS  Timestamp        `json:"message_ts"`
	ThreadTS   Timestamp        `json:"thread_ts,omitempty"`
	EventTS    Timestamp        `json:"event_ts"`
	Text       string           `json:"text,omitempty"`
	Permalink  string           `json:"permalink,omitempty"`
	Files      []File           `json:"files,omitempty"`
	Ack        *Acknowledgement `json:"-"`
}

// ThreadIdentity returns ThreadTS when present and MessageTS otherwise.
func (e Envelope) ThreadIdentity() Timestamp {
	if e.ThreadTS != "" {
		return e.ThreadTS
	}
	return e.MessageTS
}

// File is bounded, untrusted Slack file metadata. It never contains bytes or
// an authenticated download URL.
type File struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	MIMEType  string `json:"mime_type,omitempty"`
	Permalink string `json:"permalink,omitempty"`
	SizeBytes *int64 `json:"size_bytes,omitempty"`
}

// Reaction is a normalized reaction summary on a read result.
type Reaction struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
	Mine  bool   `json:"mine"`
}

// Message is a bounded Slack read result.
type Message struct {
	MessageTS Timestamp  `json:"message_ts"`
	ThreadTS  Timestamp  `json:"thread_ts"`
	AuthorID  string     `json:"author_user_id"`
	Text      string     `json:"text"`
	Reactions []Reaction `json:"reactions,omitempty"`
	Files     []File     `json:"files,omitempty"`
}

// MessagePage is one bounded history or replies page. NextCursor is opaque.
type MessagePage struct {
	Messages   []Message `json:"messages"`
	NextCursor string    `json:"next_cursor,omitempty"`
}

// Scope is one normalized Slack OAuth scope name.
type Scope string

// AuthIdentity is the immutable identity returned by Slack authentication.
type AuthIdentity struct {
	TeamID    string  `json:"team_id"`
	BotUserID string  `json:"bot_user_id"`
	BotID     string  `json:"bot_id,omitempty"`
	Scopes    []Scope `json:"scopes,omitempty"`
}

// Team contains only identity needed for configuration verification.
type Team struct {
	ID string `json:"id"`
}

// User contains immutable identity, the current Slack username, and
// bot/deactivation flags. Name is presentation metadata and is never authority.
type User struct {
	ID          string `json:"id"`
	TeamID      string `json:"team_id"`
	Name        string `json:"name,omitempty"`
	Bot         bool   `json:"bot"`
	Deactivated bool   `json:"deactivated"`
}

// Channel contains access facts needed to verify the configured channel.
type Channel struct {
	ID         string `json:"id"`
	TeamID     string `json:"team_id"`
	Private    bool   `json:"private"`
	Member     bool   `json:"member"`
	Accessible bool   `json:"accessible"`
	Archived   bool   `json:"archived"`
	ReadOnly   bool   `json:"read_only"`
	IM         bool   `json:"im"`
	MPIM       bool   `json:"mpim"`
}

// VerificationRequest is the immutable identity and authority expected by a
// prepared runtime. TrustedUserIDs may be empty; when supplied each user must
// be an active, non-bot member of the configured workspace.
type VerificationRequest struct {
	TeamID         string
	BotUserID      string
	ChannelID      string
	TrustedUserIDs []string
}

// VerificationResult contains only safe, proven configuration facts.
type VerificationResult struct {
	Identity AuthIdentity
	Team     Team
	Channel  Channel
	Scopes   []Scope
	Users    []User
}

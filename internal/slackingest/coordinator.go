// Package slackingest commits normalized Slack source events before acknowledging
// their Socket Mode envelopes.
package slackingest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/stayradiated/mosscap/internal/slackapi"
	"github.com/stayradiated/mosscap/internal/store"
)

// Store is the intentionally narrow durable admission boundary.
type Store interface {
	CommitSlackSourceEvent(context.Context, store.SlackAdmissionPolicy, store.SlackSourceEvent) (store.SlackEventResult, error)
}

// Clock supplies receipt time. It is injectable to keep ingestion tests
// deterministic; production callers may leave it nil.
type Clock interface{ Now() time.Time }

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

// Coordinator applies a receipt-time policy to one envelope at a time. It
// never retains an envelope or its message body after Handle returns.
type Coordinator struct {
	store             Store
	policy            store.SlackAdmissionPolicy
	clock             Clock
	beforeAcknowledge func(context.Context, store.SlackSourceEvent, store.SlackEventResult) error

	mu  sync.RWMutex
	obs Observations
}

// Observations contains only status-safe ingestion facts. Result counts are
// process-local and intentionally exclude event IDs, text, and file metadata.
type Observations struct {
	LastReceivedAt     time.Time                             `json:"last_received_at,omitempty"`
	LastCommittedAt    time.Time                             `json:"last_committed_at,omitempty"`
	LastDiagnosticCode string                                `json:"last_diagnostic_code,omitempty"`
	Results            map[store.SlackAdmissionResult]uint64 `json:"results"`
}

// Options adds work that must become durable after source commit but before
// the Socket envelope is acknowledged.
type Options struct {
	BeforeAcknowledge func(context.Context, store.SlackSourceEvent, store.SlackEventResult) error
}

// New creates a coordinator bound to an immutable runtime policy snapshot.
func New(s Store, policy store.SlackAdmissionPolicy, c Clock) *Coordinator {
	return NewWithOptions(s, policy, c, Options{})
}

// NewWithOptions creates a coordinator with an optional pre-acknowledgement
// durability hook. A hook failure deliberately leaves the envelope unacked.
func NewWithOptions(s Store, policy store.SlackAdmissionPolicy, c Clock, options Options) *Coordinator {
	if c == nil {
		c = systemClock{}
	}
	return &Coordinator{store: s, policy: policy, clock: c, beforeAcknowledge: options.BeforeAcknowledge, obs: Observations{Results: make(map[store.SlackAdmissionResult]uint64)}}
}

// Handle commits one envelope exactly once before consuming its acknowledgement.
// An unidentifiable malformed envelope is deliberately ignored without an ack.
// A store or canonical-result error leaves the acknowledgement untouched.
func (c *Coordinator) Handle(ctx context.Context, envelope slackapi.Envelope) (store.SlackEventResult, error) {
	if c == nil || c.store == nil {
		return store.SlackEventResult{}, errors.New("Slack admission coordinator is unavailable")
	}
	if !identifiable(envelope) {
		return store.SlackEventResult{}, nil
	}

	receivedAt := c.clock.Now().UTC()
	c.recordReceived(receivedAt)
	event := normalize(envelope, receivedAt)
	result, err := c.store.CommitSlackSourceEvent(ctx, c.policy, event)
	if err != nil {
		c.recordDiagnostic("commit_failed")
		return store.SlackEventResult{}, err
	}
	if !canonical(event, result) {
		c.recordDiagnostic("commit_failed")
		return store.SlackEventResult{}, errors.New("Slack admission store returned a non-canonical result")
	}
	c.recordCommitted(result)
	if c.beforeAcknowledge != nil {
		if err := c.beforeAcknowledge(ctx, event, result); err != nil {
			c.recordDiagnostic("before_ack_failed")
			return result, err
		}
	}
	if err := envelope.Ack.Acknowledge(ctx); err != nil {
		c.recordDiagnostic("ack_failed")
		return result, err
	}
	return result, nil
}

// Observations returns a detached, status-safe snapshot.
func (c *Coordinator) Observations() Observations {
	if c == nil {
		return Observations{Results: map[store.SlackAdmissionResult]uint64{}}
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := c.obs
	out.Results = make(map[store.SlackAdmissionResult]uint64, len(c.obs.Results))
	for code, count := range c.obs.Results {
		out.Results[code] = count
	}
	return out
}

func (c *Coordinator) recordReceived(at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if at.After(c.obs.LastReceivedAt) {
		c.obs.LastReceivedAt = at
	}
}
func (c *Coordinator) recordDiagnostic(code string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.obs.LastDiagnosticCode = code
}
func (c *Coordinator) recordCommitted(result store.SlackEventResult) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if result.CommittedAt.After(c.obs.LastCommittedAt) {
		c.obs.LastCommittedAt = result.CommittedAt
	}
	c.obs.Results[result.AdmissionResult]++
}

func identifiable(e slackapi.Envelope) bool { return e.Ack != nil && validOpaque(e.EnvelopeID) }

func normalize(e slackapi.Envelope, receivedAt time.Time) store.SlackSourceEvent {
	files := make([]store.SlackIncomingFile, len(e.Files))
	for i, file := range e.Files {
		files[i] = store.SlackIncomingFile{SlackFileID: file.ID, OriginalName: file.Name, DeclaredMIME: file.MIMEType, Permalink: file.Permalink}
		if file.SizeBytes != nil {
			size := *file.SizeBytes
			files[i].SizeBytes = &size
		}
	}
	return store.SlackSourceEvent{
		SourceEventID: sourceEventID(e), EnvelopeID: e.EnvelopeID, EventKind: store.SlackSourceEventKind(e.Kind),
		TeamID: e.TeamID, ChannelID: e.ChannelID, UserID: e.UserID, BotID: e.BotID, Subtype: e.Subtype,
		MessageTS: string(e.MessageTS), ThreadTS: string(e.ThreadIdentity()), Text: taskText(e), Permalink: e.Permalink,
		SlackEventAtUS: eventMicroseconds(e.EventTS), ReceivedAt: receivedAt, Files: files,
	}
}

// taskText keeps forwarded content in the durable task body, the same field
// delivered to Pi and returned by task context. Identifiers are rendered only
// as opaque provenance, never as URLs.
func taskText(e slackapi.Envelope) string {
	text := e.Text
	for _, forwarded := range e.Forwarded {
		provenance := "message " + string(forwarded.MessageTS)
		if forwarded.ChannelID != "" {
			provenance = "channel " + forwarded.ChannelID + ", " + provenance
		}
		addition := "\n\nForwarded message (" + provenance + "):\n" + forwarded.Text
		if len(text)+len(addition) > slackapi.MaxTextBytes {
			break
		}
		text += addition
	}
	return text
}

func sourceEventID(e slackapi.Envelope) string {
	if validOpaque(e.EventID) {
		return e.EventID
	}
	// An identifiable malformed frame has no Events API ID. Hash its transport
	// ID so the durable key remains bounded even at the adapter's ID limit.
	if !e.Kind.Valid() {
		sum := sha256.Sum256([]byte(e.EnvelopeID))
		return "malformed:" + hex.EncodeToString(sum[:])
	}
	h := sha256.New()
	for _, value := range []string{e.TeamID, string(e.Kind), e.ChannelID, string(e.MessageTS), string(e.EventTS), e.UserID, e.BotID, e.Subtype, string(e.ThreadTS), e.Text, e.Permalink} {
		_, _ = h.Write([]byte(value))
		_, _ = h.Write([]byte{0})
	}
	for _, forwarded := range e.Forwarded {
		for _, value := range []string{forwarded.Text, forwarded.ChannelID, string(forwarded.MessageTS)} {
			_, _ = h.Write([]byte(value))
			_, _ = h.Write([]byte{0})
		}
	}
	for _, file := range e.Files {
		for _, value := range []string{file.ID, file.Name, file.MIMEType, file.Permalink} {
			_, _ = h.Write([]byte(value))
			_, _ = h.Write([]byte{0})
		}
		if file.SizeBytes != nil {
			_, _ = h.Write([]byte(strconv.FormatInt(*file.SizeBytes, 10)))
			_, _ = h.Write([]byte{0})
		}
	}
	return "derived:" + hex.EncodeToString(h.Sum(nil))
}

func eventMicroseconds(ts slackapi.Timestamp) int64 {
	parsed, err := slackapi.ParseTimestamp(string(ts))
	if err != nil {
		return 0
	}
	micros, err := parsed.UnixMicro()
	if err != nil {
		return 0
	}
	return micros
}
func validOpaque(value string) bool {
	return value != "" && len(value) <= slackapi.MaxIDBytes && utf8.ValidString(value) && strings.IndexFunc(value, func(r rune) bool { return r <= 0x20 || r == 0x7f }) < 0
}
func canonical(event store.SlackSourceEvent, result store.SlackEventResult) bool {
	if result.SourceEventID != event.SourceEventID {
		return false
	}
	switch result.AdmissionResult {
	case store.SlackAdmitted, store.SlackUpdated, store.SlackDeleted, store.SlackDuplicate, store.SlackIgnoredWorkspace, store.SlackIgnoredChannel, store.SlackIgnoredUntrusted, store.SlackIgnoredUnmentioned, store.SlackIgnoredBot, store.SlackIgnoredStale, store.SlackIgnoredUntracked, store.SlackMalformed:
		return true
	default:
		return false
	}
}

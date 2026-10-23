package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/stayradiated/mosscap/internal/slackapi"
	"github.com/stayradiated/mosscap/internal/task"
)

const (
	slackDiagnosticInvalidShape = "invalid_event_shape"
	slackDiagnosticInvalidText  = "invalid_text"
	slackDiagnosticInvalidFile  = "invalid_file_metadata"
)

type normalizedSlackSourceEvent struct {
	SlackSourceEvent
	receivedAtUS      int64
	threadTS          string
	text              string
	diagnostic        string
	identityMalformed bool
}

// CommitSlackSourceEvent evaluates the admission policy, then commits an event and any task/source/file changes in one transaction. It
// never acknowledges Slack and never exposes the transaction to callers.
func (s *Store) CommitSlackSourceEvent(ctx context.Context, policy SlackAdmissionPolicy, event SlackSourceEvent) (SlackEventResult, error) {
	if err := validateSlackPolicy(policy); err != nil {
		return SlackEventResult{}, err
	}
	normalized, err := s.normalizeSlackSourceEvent(event)
	if err != nil {
		return SlackEventResult{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return SlackEventResult{}, err
	}
	defer tx.Rollback()

	canonical, err := lookupSlackEventResult(ctx, tx, event.SourceEventID)
	if err == nil {
		lastSeenAtUS := s.now()
		if lastSeenAtUS < canonical.LastSeenAt.UnixMicro() {
			lastSeenAtUS = canonical.LastSeenAt.UnixMicro()
		}
		updated, updateErr := tx.ExecContext(ctx, `UPDATE slack_source_events
			SET redelivery_count=redelivery_count+1,last_seen_at_us=?
			WHERE source_event_id=? AND redelivery_count < 9223372036854775807`, lastSeenAtUS, event.SourceEventID)
		if updateErr != nil {
			return SlackEventResult{}, updateErr
		}
		changed, updateErr := rowsChanged(updated)
		if updateErr != nil {
			return SlackEventResult{}, updateErr
		}
		if !changed {
			return SlackEventResult{}, fmt.Errorf("%w: Slack redelivery count exhausted", ErrSlackInvariant)
		}
		canonical, updateErr = lookupSlackEventResult(ctx, tx, event.SourceEventID)
		if updateErr != nil {
			return SlackEventResult{}, updateErr
		}
		if updateErr = tx.Commit(); updateErr != nil {
			return SlackEventResult{}, updateErr
		}
		canonical.DuplicateDelivery = true
		return canonical, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return SlackEventResult{}, err
	}

	committedAtUS := s.now()
	if committedAtUS < normalized.receivedAtUS {
		committedAtUS = normalized.receivedAtUS
	}
	if normalized.diagnostic != "" && normalized.identityMalformed {
		return s.commitSlackEventOnly(ctx, tx, normalized, SlackMalformed, "", normalized.diagnostic, committedAtUS)
	}
	if event.TeamID != policy.TeamID {
		return s.commitSlackEventOnly(ctx, tx, normalized, SlackIgnoredWorkspace, "", "", committedAtUS)
	}

	source, sourceErr := findSlackSourceTx(ctx, tx, event.TeamID, event.ChannelID, event.MessageTS)
	if sourceErr != nil && !errors.Is(sourceErr, ErrNotFound) {
		return SlackEventResult{}, sourceErr
	}
	if normalized.diagnostic != "" && (event.EventKind != SlackEventMessageChange || sourceErr != nil) {
		return s.commitSlackEventOnly(ctx, tx, normalized, SlackMalformed, "", normalized.diagnostic, committedAtUS)
	}

	switch event.EventKind {
	case SlackEventMessageDelete:
		return s.commitSlackDeletion(ctx, tx, normalized, source, sourceErr == nil, committedAtUS)
	case SlackEventMessageChange:
		if sourceErr == nil {
			return s.commitSlackEdit(ctx, tx, normalized, source, committedAtUS)
		}
		return s.commitSlackInitial(ctx, tx, policy, normalized, committedAtUS)
	case SlackEventAppMention, SlackEventMessage:
		if sourceErr == nil {
			return s.commitSlackEventOnly(ctx, tx, normalized, SlackDuplicate, source.TaskID, "", committedAtUS)
		}
		return s.commitSlackInitial(ctx, tx, policy, normalized, committedAtUS)
	default:
		// normalizeSlackSourceEvent classifies unsupported kinds as malformed.
		return SlackEventResult{}, fmt.Errorf("%w: unsupported normalized event kind", ErrSlackInvariant)
	}
}

func (s *Store) normalizeSlackSourceEvent(event SlackSourceEvent) (normalizedSlackSourceEvent, error) {
	event.Files = cloneIncomingFiles(event.Files)
	n := normalizedSlackSourceEvent{SlackSourceEvent: event}
	if !validOpaqueID(event.SourceEventID, true) {
		// Without a bounded stable identity no durable deduplication is possible.
		return n, invalidInput("invalid Slack source event ID")
	}
	n.receivedAtUS = s.now()
	if !event.ReceivedAt.IsZero() {
		n.receivedAtUS = event.ReceivedAt.UnixMicro()
		if n.receivedAtUS <= 0 {
			n.receivedAtUS = s.now()
			n.diagnostic = slackDiagnosticInvalidShape
			n.identityMalformed = true
		}
	}
	if event.EnvelopeID != "" && !validOpaqueID(event.EnvelopeID, false) {
		n.EnvelopeID = ""
		n.diagnostic = slackDiagnosticInvalidShape
		n.identityMalformed = true
	}
	if !validEventKind(event.EventKind) {
		n.EventKind = SlackSourceEventKind("unknown")
		n.diagnostic = slackDiagnosticInvalidShape
		n.identityMalformed = true
	}
	if slackapi.ValidateTeamID(event.TeamID) != nil {
		n.TeamID = ""
		n.diagnostic = slackDiagnosticInvalidShape
		n.identityMalformed = true
	}
	if slackapi.ValidateChannelID(event.ChannelID) != nil {
		n.ChannelID = ""
		n.diagnostic = slackDiagnosticInvalidShape
		n.identityMalformed = true
	}
	if event.UserID != "" && slackapi.ValidateUserID(event.UserID) != nil {
		n.UserID = ""
		n.diagnostic = slackDiagnosticInvalidShape
	}
	if event.BotID != "" && slackapi.ValidateBotID(event.BotID) != nil {
		n.BotID = ""
		n.diagnostic = slackDiagnosticInvalidShape
	}
	if !validSubtype(event.Subtype) {
		n.Subtype = ""
		n.diagnostic = slackDiagnosticInvalidShape
	}
	if _, err := slackapi.ParseTimestamp(event.MessageTS); err != nil {
		n.MessageTS = ""
		n.diagnostic = slackDiagnosticInvalidShape
		n.identityMalformed = true
	}
	if event.ThreadTS != "" {
		if _, err := slackapi.ParseTimestamp(event.ThreadTS); err != nil {
			n.ThreadTS = ""
			n.diagnostic = slackDiagnosticInvalidShape
		}
	}
	n.threadTS = n.ThreadTS
	if n.threadTS == "" {
		n.threadTS = n.MessageTS
	}
	if event.SlackEventAtUS <= 0 {
		n.SlackEventAtUS = 0
		n.diagnostic = slackDiagnosticInvalidShape
		n.identityMalformed = true
	}
	if event.EventKind != SlackEventMessageDelete {
		if len(event.Text) > slackapi.MaxTextBytes || !utf8.ValidString(event.Text) || strings.IndexByte(event.Text, 0) >= 0 {
			n.diagnostic = slackDiagnosticInvalidText
		} else {
			// Empty/all-whitespace text is a valid bounded Slack payload. It is
			// ignored unless this is an edit of an admitted source, where the
			// task's non-empty-current-text invariant makes it malformed.
			n.text, _ = task.NormalizeText(event.Text)
		}
		if err := validateSlackPermalink(event.Permalink); err != nil {
			n.Permalink = ""
			n.diagnostic = slackDiagnosticInvalidShape
		}
		if len(event.Files) > slackapi.MaxFiles || !validIncomingFiles(event.Files) {
			n.Files = nil
			n.diagnostic = slackDiagnosticInvalidFile
		}
	}
	if n.UserID == "" && n.BotID == "" && n.Subtype == "" && event.EventKind != SlackEventMessageDelete {
		n.diagnostic = slackDiagnosticInvalidShape
	}
	return n, nil
}

func validateSlackPolicy(policy SlackAdmissionPolicy) error {
	if slackapi.ValidateTeamID(policy.TeamID) != nil || slackapi.ValidateUserID(policy.BotUserID) != nil {
		return invalidInput("invalid Slack admission policy")
	}
	return nil
}

func validOpaqueID(value string, required bool) bool {
	if value == "" {
		return !required
	}
	if len(value) > slackapi.MaxIDBytes || !utf8.ValidString(value) || strings.IndexByte(value, 0) >= 0 {
		return false
	}
	for _, r := range value {
		if r <= 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func validEventKind(value SlackSourceEventKind) bool {
	if value != SlackEventAppMention && value != SlackEventMessage && value != SlackEventMessageChange && value != SlackEventMessageDelete {
		return false
	}
	return len(value) <= 64
}

func validSubtype(value string) bool {
	if len(value) > slackapi.MaxSubtypeBytes {
		return false
	}
	for _, r := range value {
		if r != '_' && (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

func validateSlackPermalink(value string) error {
	if value == "" {
		return nil
	}
	if len(value) > slackapi.MaxPermalinkBytes || !utf8.ValidString(value) || strings.IndexByte(value, 0) >= 0 {
		return errors.New("invalid permalink")
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil {
		return errors.New("invalid permalink")
	}
	return nil
}

func cloneIncomingFiles(files []SlackIncomingFile) []SlackIncomingFile {
	cloned := append([]SlackIncomingFile(nil), files...)
	for i := range cloned {
		if cloned[i].SizeBytes != nil {
			size := *cloned[i].SizeBytes
			cloned[i].SizeBytes = &size
		}
	}
	return cloned
}

func validIncomingFiles(files []SlackIncomingFile) bool {
	seen := make(map[string]struct{}, len(files))
	for _, file := range files {
		if slackapi.ValidateFileID(file.SlackFileID) != nil || file.OriginalName == "" || len(file.OriginalName) > slackapi.MaxFileNameBytes || !utf8.ValidString(file.OriginalName) || strings.IndexByte(file.OriginalName, 0) >= 0 {
			return false
		}
		if file.DeclaredMIME != "" && (len(file.DeclaredMIME) > slackapi.MaxMIMETypeBytes || !utf8.ValidString(file.DeclaredMIME) || strings.IndexByte(file.DeclaredMIME, 0) >= 0) {
			return false
		}
		if validateSlackPermalink(file.Permalink) != nil || file.SizeBytes != nil && *file.SizeBytes < 0 {
			return false
		}
		if _, duplicate := seen[file.SlackFileID]; duplicate {
			return false
		}
		seen[file.SlackFileID] = struct{}{}
	}
	return true
}

// humanSubtype reports whether a message subtype is still an ordinary human
// message: a reply also sent to the channel, or a message sharing files.
func humanSubtype(subtype string) bool {
	return subtype == "" || subtype == "thread_broadcast" || subtype == "file_share"
}

// attachmentOnlyText is the task text of an admitted follow-up that carries
// files but no message text, because task text must not be empty.
const attachmentOnlyText = "(attachment without message text)"

func (s *Store) commitSlackInitial(ctx context.Context, tx *sql.Tx, policy SlackAdmissionPolicy, event normalizedSlackSourceEvent, committedAtUS int64) (SlackEventResult, error) {
	if event.BotID != "" || !humanSubtype(event.Subtype) || event.UserID == policy.BotUserID {
		return s.commitSlackEventOnly(ctx, tx, event, SlackIgnoredBot, "", "", committedAtUS)
	}
	// Only members of the authenticated workspace may send work. An author
	// from another workspace (a Slack Connect external user) is ignored like
	// an event from another workspace, whatever the message says.
	if event.UserTeamID != "" && event.UserTeamID != policy.TeamID {
		return s.commitSlackEventOnly(ctx, tx, event, SlackIgnoredWorkspace, "", "", committedAtUS)
	}
	reply := event.ThreadTS != "" && event.ThreadTS != event.MessageTS
	var thread slackThread
	if reply {
		var err error
		if thread, err = lookupSlackThreadTx(ctx, tx, event.TeamID, event.ChannelID, event.ThreadTS); err != nil {
			return SlackEventResult{}, err
		}
	}
	// Only a message addressed to the bot is a request: a direct mention, or a
	// new reply in a tracked thread (a follow-up). Ordinary channel chatter is
	// ignored_unmentioned. Every human member of the workspace may send work.
	mentioned := strings.Contains(event.Text, "<@"+policy.BotUserID+">")
	followUp := !mentioned && reply && thread.tracked && event.EventKind == SlackEventMessage
	if !mentioned && !followUp {
		return s.commitSlackEventOnly(ctx, tx, event, SlackIgnoredUnmentioned, "", "", committedAtUS)
	}
	text := event.text
	if text == "" && len(event.Files) != 0 {
		text = attachmentOnlyText
	}
	if text == "" {
		return s.commitSlackEventOnly(ctx, tx, event, SlackIgnoredUnmentioned, "", "", committedAtUS)
	}
	taskID, err := s.ids.NewTaskID()
	if err != nil {
		return SlackEventResult{}, err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO tasks(task_id,version,state,text,source_kind,created_at,updated_at,done_at) VALUES(?,1,'open',?,'slack',?,?,NULL)", taskID, text, committedAtUS, committedAtUS); err != nil {
		return SlackEventResult{}, err
	}
	if err := insertSlackEvent(ctx, tx, event, SlackAdmitted, taskID, "", 1, true, committedAtUS); err != nil {
		return SlackEventResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO slack_task_sources(task_id,initial_source_event_id,team_id,channel_id,author_user_id,message_ts,thread_ts,permalink,latest_event_at_us,source_deleted_at_us,created_at_us,updated_at_us,parent_task_id)
		VALUES(?,?,?,?,?,?,?,?,?,NULL,?,?,?)`, taskID, event.SourceEventID, event.TeamID, event.ChannelID, event.UserID, event.MessageTS, event.threadTS, nullable(event.Permalink), event.SlackEventAtUS, committedAtUS, committedAtUS, nullable(thread.parentTaskID)); err != nil {
		return SlackEventResult{}, err
	}
	if err := s.reconcileIncomingFiles(ctx, tx, taskID, event.Files, committedAtUS); err != nil {
		return SlackEventResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return SlackEventResult{}, err
	}
	return newSlackEventResult(event, SlackAdmitted, taskID, "", 1, true, committedAtUS), nil
}

// slackThread is what admission knows about a Slack thread. A thread is
// tracked when it already holds an admitted task, or when the bot posted its
// root or a reply in it. parentTaskID is the thread's original task: the task
// admitted from the root message, else the task the bot's root post was
// related to, else the earliest task admitted in the thread, else the task
// related to the bot's earliest post in it. It may be empty.
type slackThread struct {
	tracked      bool
	parentTaskID string
}

func lookupSlackThreadTx(ctx context.Context, tx *sql.Tx, teamID, channelID, threadTS string) (slackThread, error) {
	var taskCount, postCount int64
	var rootTask, rootPostTask, earliestTask, earliestPostTask sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT
		(SELECT count(*) FROM slack_task_sources WHERE team_id=?1 AND channel_id=?2 AND thread_ts=?3),
		(SELECT count(*) FROM slack_effects WHERE team_id=?1 AND channel_id=?2 AND state='confirmed' AND kind IN ('send','file_upload') AND (thread_ts=?3 OR response_message_ts=?3)),
		(SELECT task_id FROM slack_task_sources WHERE team_id=?1 AND channel_id=?2 AND message_ts=?3),
		(SELECT related_task_id FROM slack_effects WHERE team_id=?1 AND channel_id=?2 AND state='confirmed' AND kind='send' AND response_message_ts=?3 AND thread_ts IS NULL ORDER BY created_at_us,effect_id LIMIT 1),
		(SELECT task_id FROM slack_task_sources WHERE team_id=?1 AND channel_id=?2 AND thread_ts=?3 ORDER BY created_at_us,task_id LIMIT 1),
		(SELECT related_task_id FROM slack_effects WHERE team_id=?1 AND channel_id=?2 AND state='confirmed' AND kind IN ('send','file_upload') AND thread_ts=?3 AND related_task_id IS NOT NULL ORDER BY created_at_us,effect_id LIMIT 1)`,
		teamID, channelID, threadTS).Scan(&taskCount, &postCount, &rootTask, &rootPostTask, &earliestTask, &earliestPostTask)
	if err != nil {
		return slackThread{}, err
	}
	thread := slackThread{tracked: taskCount != 0 || postCount != 0}
	for _, candidate := range []sql.NullString{rootTask, rootPostTask, earliestTask, earliestPostTask} {
		if candidate.Valid && candidate.String != "" {
			thread.parentTaskID = candidate.String
			break
		}
	}
	return thread, nil
}

func (s *Store) commitSlackEdit(ctx context.Context, tx *sql.Tx, event normalizedSlackSourceEvent, source SlackSource, committedAtUS int64) (SlackEventResult, error) {
	if committedAtUS < source.UpdatedAt.UnixMicro() {
		committedAtUS = source.UpdatedAt.UnixMicro()
	}
	// Source ordering belongs immediately after immutable identity resolution:
	// stale tracked edits must not be reclassified by mutable text, files, or
	// sender fields that may differ across Slack retries.
	if event.SlackEventAtUS <= source.LatestEventAtUS {
		return s.commitSlackEventOnly(ctx, tx, event, SlackIgnoredStale, source.TaskID, "", committedAtUS)
	}
	if event.diagnostic != "" {
		return s.commitSlackEventOnly(ctx, tx, event, SlackMalformed, source.TaskID, event.diagnostic, committedAtUS)
	}
	if event.BotID != "" {
		return s.commitSlackEventOnly(ctx, tx, event, SlackIgnoredBot, source.TaskID, "", committedAtUS)
	}
	if event.UserID != source.AuthorUserID {
		return s.commitSlackEventOnly(ctx, tx, event, SlackIgnoredNotAuthor, source.TaskID, "", committedAtUS)
	}
	if event.text == "" {
		return s.commitSlackEventOnly(ctx, tx, event, SlackMalformed, source.TaskID, slackDiagnosticInvalidText, committedAtUS)
	}
	files, err := listSlackFilesForTaskTx(ctx, tx, source.TaskID)
	if err != nil {
		return SlackEventResult{}, err
	}
	filesChanged := !sameIncomingFiles(files, event.Files)
	changed := source.TaskText != event.text || source.Permalink != event.Permalink || source.SourceDeletedAt != nil || filesChanged
	version := source.TaskVersion
	if changed {
		version++
		if _, err := tx.ExecContext(ctx, "UPDATE tasks SET text=?,version=?,updated_at=? WHERE task_id=? AND source_kind='slack'", event.text, version, committedAtUS, source.TaskID); err != nil {
			return SlackEventResult{}, err
		}
		if filesChanged {
			if err := s.reconcileIncomingFiles(ctx, tx, source.TaskID, event.Files, committedAtUS); err != nil {
				return SlackEventResult{}, err
			}
		}
	}
	if _, err := tx.ExecContext(ctx, "UPDATE slack_task_sources SET permalink=?,latest_event_at_us=?,source_deleted_at_us=NULL,updated_at_us=? WHERE task_id=?", nullable(event.Permalink), event.SlackEventAtUS, committedAtUS, source.TaskID); err != nil {
		return SlackEventResult{}, err
	}
	if err := insertSlackEvent(ctx, tx, event, SlackUpdated, source.TaskID, "", version, changed, committedAtUS); err != nil {
		return SlackEventResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return SlackEventResult{}, err
	}
	return newSlackEventResult(event, SlackUpdated, source.TaskID, "", version, changed, committedAtUS), nil
}

func (s *Store) commitSlackDeletion(ctx context.Context, tx *sql.Tx, event normalizedSlackSourceEvent, source SlackSource, tracked bool, committedAtUS int64) (SlackEventResult, error) {
	if !tracked {
		return s.commitSlackEventOnly(ctx, tx, event, SlackIgnoredUntracked, "", "", committedAtUS)
	}
	if committedAtUS < source.UpdatedAt.UnixMicro() {
		committedAtUS = source.UpdatedAt.UnixMicro()
	}
	if event.SlackEventAtUS <= source.LatestEventAtUS || source.SourceDeletedAt != nil {
		return s.commitSlackEventOnly(ctx, tx, event, SlackIgnoredStale, source.TaskID, "", committedAtUS)
	}
	version := source.TaskVersion + 1
	if _, err := tx.ExecContext(ctx, "UPDATE tasks SET version=?,updated_at=? WHERE task_id=? AND source_kind='slack'", version, committedAtUS, source.TaskID); err != nil {
		return SlackEventResult{}, err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE slack_task_sources SET latest_event_at_us=?,source_deleted_at_us=?,updated_at_us=? WHERE task_id=?", event.SlackEventAtUS, event.SlackEventAtUS, committedAtUS, source.TaskID); err != nil {
		return SlackEventResult{}, err
	}
	if err := insertSlackEvent(ctx, tx, event, SlackDeleted, source.TaskID, "", version, true, committedAtUS); err != nil {
		return SlackEventResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return SlackEventResult{}, err
	}
	return newSlackEventResult(event, SlackDeleted, source.TaskID, "", version, true, committedAtUS), nil
}

func (s *Store) commitSlackEventOnly(ctx context.Context, tx *sql.Tx, event normalizedSlackSourceEvent, result SlackAdmissionResult, taskID, diagnostic string, committedAtUS int64) (SlackEventResult, error) {
	version := int64(0)
	if taskID != "" {
		if err := tx.QueryRowContext(ctx, "SELECT version FROM tasks WHERE task_id=?", taskID).Scan(&version); err != nil {
			return SlackEventResult{}, fmt.Errorf("%w: event task: %v", ErrSlackInvariant, err)
		}
	}
	if err := insertSlackEvent(ctx, tx, event, result, taskID, diagnostic, version, false, committedAtUS); err != nil {
		return SlackEventResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return SlackEventResult{}, err
	}
	return newSlackEventResult(event, result, taskID, diagnostic, version, false, committedAtUS), nil
}

func newSlackEventResult(event normalizedSlackSourceEvent, result SlackAdmissionResult, taskID, diagnostic string, version int64, changed bool, committedAtUS int64) SlackEventResult {
	committedAt := time.UnixMicro(committedAtUS).UTC()
	return SlackEventResult{
		SourceEventID: event.SourceEventID, AdmissionResult: result, TaskID: taskID,
		DiagnosticCode: diagnostic, SlackEventAtUS: event.SlackEventAtUS,
		ReceivedAt: time.UnixMicro(event.receivedAtUS).UTC(), CommittedAt: committedAt,
		TaskVersion: version, Changed: changed, LastSeenAt: committedAt,
	}
}

func insertSlackEvent(ctx context.Context, tx *sql.Tx, event normalizedSlackSourceEvent, result SlackAdmissionResult, taskID, diagnostic string, version int64, changed bool, committedAtUS int64) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO slack_source_events(source_event_id,envelope_id,event_kind,team_id,channel_id,message_ts,thread_ts,user_id,slack_event_at_us,received_at_us,committed_at_us,admission_result,task_id,diagnostic_code,task_version,changed,redelivery_count,last_seen_at_us)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,0,?)`, event.SourceEventID, nullable(event.EnvelopeID), event.EventKind, nullable(event.TeamID), nullable(event.ChannelID), nullable(event.MessageTS), nullable(event.ThreadTS), nullable(event.UserID), nullableInt(event.SlackEventAtUS), event.receivedAtUS, committedAtUS, result, nullable(taskID), nullable(diagnostic), version, changed, committedAtUS)
	return err
}

// LookupSlackEventResult returns an exact event-ID match; event IDs are never
// treated as prefixes because Slack does not define a typed prefix for them.
func (s *Store) LookupSlackEventResult(ctx context.Context, sourceEventID string) (SlackEventResult, error) {
	if !validOpaqueID(sourceEventID, true) {
		return SlackEventResult{}, ErrNotFound
	}
	return lookupSlackEventResult(ctx, s.db, sourceEventID)
}

func lookupSlackEventResult(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, sourceEventID string) (SlackEventResult, error) {
	var result SlackEventResult
	var taskID, diagnostic sql.NullString
	var eventAt sql.NullInt64
	var received, committed, lastSeen int64
	err := q.QueryRowContext(ctx, `SELECT source_event_id,admission_result,task_id,diagnostic_code,slack_event_at_us,
		received_at_us,committed_at_us,task_version,changed,redelivery_count,last_seen_at_us
		FROM slack_source_events WHERE source_event_id=?`, sourceEventID).Scan(&result.SourceEventID, &result.AdmissionResult, &taskID, &diagnostic, &eventAt, &received, &committed, &result.TaskVersion, &result.Changed, &result.RedeliveryCount, &lastSeen)
	if errors.Is(err, sql.ErrNoRows) {
		return SlackEventResult{}, ErrNotFound
	}
	if err != nil {
		return SlackEventResult{}, err
	}
	result.TaskID = taskID.String
	result.DiagnosticCode = diagnostic.String
	result.SlackEventAtUS = eventAt.Int64
	result.ReceivedAt = time.UnixMicro(received).UTC()
	result.CommittedAt = time.UnixMicro(committed).UTC()
	result.LastSeenAt = time.UnixMicro(lastSeen).UTC()
	return result, nil
}

// GetSlackSource resolves a typed or bare task selector and requires both the
// task and source row to agree that this is a Slack-origin task.
func (s *Store) GetSlackSource(ctx context.Context, taskSelector string) (SlackSource, error) {
	taskID, err := s.resolveSlackTaskSelector(ctx, taskSelector)
	if err != nil {
		return SlackSource{}, err
	}
	return scanSlackSource(s.db.QueryRowContext(ctx, slackSourceSelect+" WHERE s.task_id=?", taskID))
}

func (s *Store) resolveSlackTaskSelector(ctx context.Context, selector string) (string, error) {
	canonical, ok := canonicalTaskSelector(selector)
	if !ok {
		return "", ErrNotFound
	}
	rows, err := s.db.QueryContext(ctx, `SELECT s.task_id,t.source_kind FROM slack_task_sources s JOIN tasks t ON t.task_id=s.task_id
		WHERE s.task_id=? OR substr(s.task_id,1,length(?))=? ORDER BY s.task_id LIMIT 2`, canonical, canonical, canonical)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id, sourceKind string
		if err := rows.Scan(&id, &sourceKind); err != nil {
			return "", err
		}
		if sourceKind != "slack" {
			return "", ErrSlackInvariant
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if len(ids) == 0 {
		return "", ErrNotFound
	}
	if len(ids) > 1 {
		return "", ErrAmbiguous
	}
	return ids[0], nil
}

// validTypedSelector accepts unprefixed IDs and durable IDs from before the
// prefix-free migration.
func validTypedSelector(selector, legacyPrefix string) bool {
	if strings.HasPrefix(selector, legacyPrefix) {
		selector = selector[len(legacyPrefix):]
	}
	return len(selector) > 0 && len(selector) <= 22 && validOpaqueIDSuffix(selector)
}

// validFullOpaqueID accepts current unprefixed IDs and durable legacy IDs.
func validFullOpaqueID(value, legacyPrefix string) bool {
	if len(value) == 22 {
		return validOpaqueIDSuffix(value)
	}
	return strings.HasPrefix(value, legacyPrefix) && len(value) == len(legacyPrefix)+22 && validOpaqueIDSuffix(value[len(legacyPrefix):])
}

// FindSlackSource performs an exact immutable source-identity lookup.
func (s *Store) FindSlackSource(ctx context.Context, teamID, channelID, messageTS string) (SlackSource, error) {
	if slackapi.ValidateTeamID(teamID) != nil || slackapi.ValidateChannelID(channelID) != nil {
		return SlackSource{}, ErrNotFound
	}
	if _, err := slackapi.ParseTimestamp(messageTS); err != nil {
		return SlackSource{}, ErrNotFound
	}
	return findSlackSourceTx(ctx, s.db, teamID, channelID, messageTS)
}

const slackSourceSelect = `SELECT s.task_id,s.initial_source_event_id,s.team_id,s.channel_id,s.author_user_id,s.message_ts,s.thread_ts,s.permalink,s.parent_task_id,s.latest_event_at_us,s.source_deleted_at_us,s.created_at_us,s.updated_at_us,t.version,t.state,t.text,t.source_kind
	FROM slack_task_sources s JOIN tasks t ON t.task_id=s.task_id`

func findSlackSourceTx(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, teamID, channelID, messageTS string) (SlackSource, error) {
	return scanSlackSource(q.QueryRowContext(ctx, slackSourceSelect+" WHERE s.team_id=? AND s.channel_id=? AND s.message_ts=?", teamID, channelID, messageTS))
}

func scanSlackSource(row *sql.Row) (SlackSource, error) {
	var source SlackSource
	var permalink, parentTaskID sql.NullString
	var deleted sql.NullInt64
	var created, updated int64
	var sourceKind string
	err := row.Scan(&source.TaskID, &source.InitialSourceEventID, &source.TeamID, &source.ChannelID, &source.AuthorUserID, &source.MessageTS, &source.ThreadTS, &permalink, &parentTaskID, &source.LatestEventAtUS, &deleted, &created, &updated, &source.TaskVersion, &source.TaskState, &source.TaskText, &sourceKind)
	if errors.Is(err, sql.ErrNoRows) {
		return SlackSource{}, ErrNotFound
	}
	if err != nil {
		return SlackSource{}, err
	}
	if sourceKind != "slack" {
		return SlackSource{}, ErrSlackInvariant
	}
	source.Permalink = permalink.String
	source.ParentTaskID = parentTaskID.String
	source.CreatedAt = time.UnixMicro(created).UTC()
	source.UpdatedAt = time.UnixMicro(updated).UTC()
	if deleted.Valid {
		value := time.UnixMicro(deleted.Int64).UTC()
		source.SourceDeletedAt = &value
	}
	return source, nil
}

// SlackSourceEventCounts returns bounded counts without event payload fields.
func (s *Store) SlackSourceEventCounts(ctx context.Context) (SlackSourceEventCounts, error) {
	counts := SlackSourceEventCounts{ByResult: make(map[SlackAdmissionResult]int64)}
	for _, result := range []SlackAdmissionResult{
		SlackAdmitted, SlackUpdated, SlackDeleted, SlackDuplicate,
		SlackIgnoredWorkspace, SlackIgnoredChannel, SlackIgnoredNotAuthor,
		SlackIgnoredUnmentioned, SlackIgnoredBot, SlackIgnoredStale,
		SlackIgnoredUntracked, SlackMalformed,
	} {
		counts.ByResult[result] = 0
	}
	rows, err := s.db.QueryContext(ctx, "SELECT admission_result,count(*),sum(redelivery_count) FROM slack_source_events GROUP BY admission_result")
	if err != nil {
		return counts, err
	}
	for rows.Next() {
		var result SlackAdmissionResult
		var count, redeliveries int64
		if err := rows.Scan(&result, &count, &redeliveries); err != nil {
			_ = rows.Close()
			return counts, err
		}
		counts.ByResult[result] = count
		counts.Total += count
		counts.Redeliveries += redeliveries
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return counts, err
	}
	if err := rows.Close(); err != nil {
		return counts, err
	}
	if err := s.db.QueryRowContext(ctx, "SELECT (SELECT count(*) FROM slack_task_sources),(SELECT count(*) FROM files WHERE direction='from_slack')").Scan(&counts.Sources, &counts.Files); err != nil {
		return counts, err
	}
	return counts, nil
}

func nullable(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func nullableInt(value int64) any {
	if value == 0 {
		return nil
	}
	return value
}

func sortedIncomingFiles(files []SlackIncomingFile) []SlackIncomingFile {
	copyFiles := append([]SlackIncomingFile(nil), files...)
	sort.Slice(copyFiles, func(i, j int) bool { return copyFiles[i].SlackFileID < copyFiles[j].SlackFileID })
	return copyFiles
}

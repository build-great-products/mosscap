package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/stayradiated/mosscap/internal/identity"
	"github.com/stayradiated/mosscap/internal/task"
)

// SlackEffectKind identifies one durable Slack mutation intent.
type SlackEffectKind string

const (
	SlackEffectSend       SlackEffectKind = "send"
	SlackEffectEdit       SlackEffectKind = "edit"
	SlackEffectDelete     SlackEffectKind = "delete"
	SlackEffectReact      SlackEffectKind = "react"
	SlackEffectUnreact    SlackEffectKind = "unreact"
	SlackEffectFileUpload SlackEffectKind = "file_upload"
)

// SlackEffectState is the durable lifecycle state of a Slack effect.
type SlackEffectState string

const (
	SlackEffectPrepared  SlackEffectState = "prepared"
	SlackEffectSubmitted SlackEffectState = "submitted"
	SlackEffectConfirmed SlackEffectState = "confirmed"
	SlackEffectFailed    SlackEffectState = "failed"
	SlackEffectAmbiguous SlackEffectState = "ambiguous"
)

// SlackEffectDiagnosticCode is bounded, token-free outcome metadata.
type SlackEffectDiagnosticCode string

const (
	SlackEffectDiagnosticInvalidAuth           SlackEffectDiagnosticCode = "invalid_auth"
	SlackEffectDiagnosticTokenRevoked          SlackEffectDiagnosticCode = "token_revoked"
	SlackEffectDiagnosticMissingScope          SlackEffectDiagnosticCode = "missing_scope"
	SlackEffectDiagnosticTeamMismatch          SlackEffectDiagnosticCode = "team_mismatch"
	SlackEffectDiagnosticBotMismatch           SlackEffectDiagnosticCode = "bot_mismatch"
	SlackEffectDiagnosticChannelNotFound       SlackEffectDiagnosticCode = "channel_not_found"
	SlackEffectDiagnosticNotInChannel          SlackEffectDiagnosticCode = "not_in_channel"
	SlackEffectDiagnosticUserNotFound          SlackEffectDiagnosticCode = "user_not_found"
	SlackEffectDiagnosticMessageNotFound       SlackEffectDiagnosticCode = "message_not_found"
	SlackEffectDiagnosticExternalRejected      SlackEffectDiagnosticCode = "external_rejected"
	SlackEffectDiagnosticAPIRejected                                     = SlackEffectDiagnosticExternalRejected
	SlackEffectDiagnosticRateLimited           SlackEffectDiagnosticCode = "rate_limited"
	SlackEffectDiagnosticTimeout               SlackEffectDiagnosticCode = "timeout"
	SlackEffectDiagnosticCanceled              SlackEffectDiagnosticCode = "canceled"
	SlackEffectDiagnosticDisconnected          SlackEffectDiagnosticCode = "disconnected"
	SlackEffectDiagnosticMalformedResponse     SlackEffectDiagnosticCode = "malformed_response"
	SlackEffectDiagnosticUnavailable           SlackEffectDiagnosticCode = "unavailable"
	SlackEffectDiagnosticInvalidRequest        SlackEffectDiagnosticCode = "invalid_request"
	SlackEffectDiagnosticAccessDenied          SlackEffectDiagnosticCode = "access_denied"
	SlackEffectDiagnosticInvalidTarget         SlackEffectDiagnosticCode = "invalid_target"
	SlackEffectDiagnosticUnknownSubmission     SlackEffectDiagnosticCode = "unknown_submission"
	SlackEffectDiagnosticStartupReconciliation SlackEffectDiagnosticCode = "startup_reconciliation"
	SlackEffectDiagnosticFileChanged           SlackEffectDiagnosticCode = "file_changed"
)

var (
	ErrSlackEffectNotFound          = errors.New("Slack effect not found")
	ErrSlackEffectSelectorAmbiguous = errors.New("Slack effect selector is ambiguous")
	ErrSlackEffectTransition        = errors.New("illegal Slack effect state transition")
	ErrSlackEffectRetryNotAllowed   = errors.New("Slack effect cannot be retried in its current state")
	ErrSlackEffectDuplicateRisk     = errors.New("ambiguous Slack effect retry requires duplicate-risk confirmation")
	ErrSlackEffectUnsupported       = errors.New("unsupported Slack effect kind")
)

const (
	maxSlackEffectTextBytes   = 40_000
	maxSlackEffectEmojiBytes  = 100
	maxSlackEffectReasonBytes = 4096
	maxSlackEffectIDBytes     = 128
)

// SlackEffectTarget is the active configured workspace/channel snapshot. The
// command payload does not contain selectors, preventing selector injection.
type SlackEffectTarget struct {
	TeamID    string
	ChannelID string
}

// SlackEffectIntent is an SDK-free outbound mutation. Empty optional fields
// are stored as SQL NULL. RelatedTaskID is an audit relation only.
type SlackEffectIntent struct {
	Kind            SlackEffectKind
	TargetMessageTS string
	ThreadTS        string
	Text            string
	Emoji           string
	OperatorReason  string
	RelatedTaskID   string
}

// SlackEffect is one exact durable effect record.
type SlackEffect struct {
	ID                  string
	IdempotencyKey      string
	PredecessorEffectID string
	RelatedTaskID       string
	Kind                SlackEffectKind
	TeamID              string
	ChannelID           string
	TargetMessageTS     string
	ThreadTS            string
	Text                string
	Emoji               string
	OperatorReason      string
	PayloadHash         string
	State               SlackEffectState
	AttemptCount        int
	PreparedAt          time.Time
	SubmittedAt         *time.Time
	CompletedAt         *time.Time
	ResponseMessageTS   string
	ResponseFileID      string
	DiagnosticCode      SlackEffectDiagnosticCode
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

// SlackEffectMutationResult is deliberately compact and contains no message
// text, operator reason, token, or external response body.
type SlackEffectMutationResult struct {
	EffectID        string           `json:"effect_id"`
	State           SlackEffectState `json:"state"`
	Kind            SlackEffectKind  `json:"kind"`
	TeamID          string           `json:"team_id"`
	ChannelID       string           `json:"channel_id"`
	TargetMessageTS string           `json:"target_message_ts,omitempty"`
}

// SlackEffectOutcome is a terminal result for one already-submitted effect.
type SlackEffectOutcome struct {
	State             SlackEffectState
	ResponseMessageTS string
	ResponseFileID    string
	DiagnosticCode    SlackEffectDiagnosticCode
}

// PrepareSlackEffect atomically inserts a prepared effect and its request
// replay result. No external operation may occur before this returns success.
func (s *Store) PrepareSlackEffect(ctx context.Context, requestID string, target SlackEffectTarget, intent SlackEffectIntent) (SlackEffectMutationResult, error) {
	canonical, validationErr := canonicalSlackEffect(target, intent)
	inputHash := hashSlackEffectInput(target, intent)
	if validationErr == nil {
		inputHash = hashCanonicalSlackEffect("slack.effect.prepare.v1", canonical.payloadHash, canonical.intent.RelatedTaskID, canonical.intent.OperatorReason)
	}
	req := request{operation: "slack.effect.prepare", hash: inputHash}
	return s.mutateSlackEffect(ctx, req, requestID, func(tx *sql.Tx, now int64) (SlackEffectMutationResult, error) {
		if validationErr != nil {
			return SlackEffectMutationResult{}, validationErr
		}
		resolvedTaskID, err := resolveOptionalTaskID(ctx, tx, canonical.intent.RelatedTaskID)
		if err != nil {
			return SlackEffectMutationResult{}, err
		}
		effectID, err := s.ids.NewEffectID()
		if err != nil {
			return SlackEffectMutationResult{}, err
		}
		idempotencyKey := hashCanonicalSlackEffect("slack.effect.idempotency.v1", requestID, canonical.payloadHash)
		_, err = tx.ExecContext(ctx, `INSERT INTO slack_effects(
			effect_id,idempotency_key,related_task_id,kind,team_id,channel_id,
			target_message_ts,thread_ts,text,emoji,operator_reason,payload_hash,
			state,attempt_count,prepared_at_us,created_at_us,updated_at_us
		) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,'prepared',0,?,?,?)`,
			effectID, idempotencyKey, nullString(resolvedTaskID), canonical.intent.Kind,
			canonical.target.TeamID, canonical.target.ChannelID,
			nullString(canonical.intent.TargetMessageTS), nullString(canonical.intent.ThreadTS),
			nullString(canonical.intent.Text), nullString(canonical.intent.Emoji),
			nullString(canonical.intent.OperatorReason), canonical.payloadHash, now, now, now)
		if err != nil {
			return SlackEffectMutationResult{}, err
		}
		return slackEffectMutationResult(effectID, canonical.target, canonical.intent), nil
	})
}

// GetSlackEffect resolves an exact ID or an unambiguous opaque prefix.
func (s *Store) GetSlackEffect(ctx context.Context, selector string) (SlackEffect, error) {
	return getSlackEffect(ctx, s.db, selector)
}

// ListSlackEffects returns a bounded stable update-time/ID ordered page.
func (s *Store) ListSlackEffects(ctx context.Context, state *SlackEffectState, limit int) ([]SlackEffect, error) {
	if limit == 0 {
		limit = 100
	}
	if limit < 1 || limit > 1000 {
		return nil, invalidInput("invalid Slack effect list limit")
	}
	query := slackEffectSelect
	args := make([]any, 0, 2)
	if state != nil {
		if !state.valid() {
			return nil, invalidInput("invalid Slack effect state")
		}
		query += " WHERE state=?"
		args = append(args, *state)
	}
	query += " ORDER BY updated_at_us,effect_id LIMIT ?"
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var effects []SlackEffect
	for rows.Next() {
		effect, err := scanSlackEffect(rows)
		if err != nil {
			return nil, err
		}
		effects = append(effects, effect)
	}
	return effects, rows.Err()
}

// ListSlackEffectsPage returns a newest-prepared/ID page, optionally narrowed
// to one related task. Cursor IDs are exact anchors, never short selectors.
func (s *Store) ListSlackEffectsPage(ctx context.Context, state *SlackEffectState, taskSelector string, limit int, cursor string) ([]SlackEffect, string, error) {
	if limit == 0 {
		limit = 50
	}
	if limit < 1 || limit > 100 {
		return nil, "", invalidInput("invalid Slack effect list limit")
	}
	if state != nil && !state.valid() {
		return nil, "", invalidInput("invalid Slack effect state")
	}
	var taskID string
	if taskSelector != "" {
		var err error
		resolved, err := getTx(ctx, s.db, taskSelector)
		taskID = resolved.ID
		if err != nil {
			return nil, "", err
		}
	}
	var preparedAt int64
	if cursor != "" {
		if !validFullSlackEffectID(cursor) {
			return nil, "", invalidInput("invalid Slack effect list cursor")
		}
		if err := s.db.QueryRowContext(ctx, "SELECT prepared_at_us FROM slack_effects WHERE effect_id=?", cursor).Scan(&preparedAt); errors.Is(err, sql.ErrNoRows) {
			return nil, "", ErrCursorExpired
		} else if err != nil {
			return nil, "", err
		}
	}
	q, args := slackEffectSelect, []any{}
	conditions := []string{}
	if state != nil {
		conditions = append(conditions, "state=?")
		args = append(args, *state)
	}
	if taskID != "" {
		conditions = append(conditions, "related_task_id=?")
		args = append(args, taskID)
	}
	if cursor != "" {
		conditions = append(conditions, "(prepared_at_us < ? OR (prepared_at_us = ? AND effect_id > ?))")
		args = append(args, preparedAt, preparedAt, cursor)
	}
	if len(conditions) != 0 {
		q += " WHERE " + strings.Join(conditions, " AND ")
	}
	q += " ORDER BY prepared_at_us DESC,effect_id LIMIT ?"
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	var effects []SlackEffect
	for rows.Next() {
		effect, err := scanSlackEffect(rows)
		if err != nil {
			return nil, "", err
		}
		effects = append(effects, effect)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	next := ""
	if len(effects) > limit {
		next = effects[limit-1].ID
		effects = effects[:limit]
	}
	return effects, next, nil
}

// ClaimSlackEffect atomically performs the only legal submission claim:
// prepared -> submitted, with exactly one attempt. It requires a full ID.
func (s *Store) ClaimSlackEffect(ctx context.Context, effectID string) (SlackEffect, error) {
	if !validFullSlackEffectID(effectID) {
		return SlackEffect{}, ErrSlackEffectNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return SlackEffect{}, err
	}
	defer tx.Rollback()
	now := s.now()
	result, err := tx.ExecContext(ctx, `UPDATE slack_effects
		SET state='submitted',attempt_count=1,
			submitted_at_us=max(?,updated_at_us),updated_at_us=max(?,updated_at_us)
		WHERE effect_id=? AND state='prepared' AND attempt_count=0`, now, now, effectID)
	if err != nil {
		return SlackEffect{}, err
	}
	changed, err := rowsChanged(result)
	if err != nil {
		return SlackEffect{}, err
	}
	if !changed {
		if _, err := getSlackEffectExact(ctx, tx, effectID); err != nil {
			return SlackEffect{}, err
		}
		return SlackEffect{}, ErrSlackEffectTransition
	}
	if _, err := tx.ExecContext(ctx, `UPDATE files SET transfer_state='submitted',updated_at_us=? WHERE related_effect_id=? AND direction='to_slack' AND transfer_state='prepared'`, now, effectID); err != nil {
		return SlackEffect{}, err
	}
	effect, err := getSlackEffectExact(ctx, tx, effectID)
	if err != nil {
		return SlackEffect{}, err
	}
	if err := tx.Commit(); err != nil {
		return SlackEffect{}, err
	}
	return effect, nil
}

// CommitSlackEffectOutcome atomically makes a submitted effect terminal. An
// exact replay of an already committed outcome is idempotent; all other
// terminal rewrites and non-submitted transitions are rejected.
func (s *Store) CommitSlackEffectOutcome(ctx context.Context, effectID string, outcome SlackEffectOutcome) (SlackEffect, error) {
	if !validFullSlackEffectID(effectID) {
		return SlackEffect{}, ErrSlackEffectNotFound
	}
	if err := validateSlackEffectOutcome(outcome); err != nil {
		return SlackEffect{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return SlackEffect{}, err
	}
	defer tx.Rollback()
	current, err := getSlackEffectExact(ctx, tx, effectID)
	if err != nil {
		return SlackEffect{}, err
	}
	if current.Kind == SlackEffectFileUpload {
		return SlackEffect{}, ErrSlackEffectTransition
	}
	if current.State != SlackEffectSubmitted {
		if slackEffectOutcomeMatches(current, outcome) {
			return current, nil
		}
		return SlackEffect{}, ErrSlackEffectTransition
	}
	if err := validateSlackEffectOutcomeForKind(current.Kind, outcome); err != nil {
		return SlackEffect{}, err
	}
	now := s.now()
	result, err := tx.ExecContext(ctx, `UPDATE slack_effects SET
		state=?,completed_at_us=max(?,submitted_at_us,updated_at_us),
		response_message_ts=?,response_file_id=?,diagnostic_code=?,
		updated_at_us=max(?,submitted_at_us,updated_at_us)
		WHERE effect_id=? AND state='submitted' AND attempt_count=1`,
		outcome.State, now, nullString(outcome.ResponseMessageTS), nullString(outcome.ResponseFileID),
		nullString(string(outcome.DiagnosticCode)), now, effectID)
	if err != nil {
		return SlackEffect{}, err
	}
	changed, err := rowsChanged(result)
	if err != nil {
		return SlackEffect{}, err
	}
	if !changed {
		return SlackEffect{}, ErrSlackEffectTransition
	}
	completed, err := getSlackEffectExact(ctx, tx, effectID)
	if err != nil {
		return SlackEffect{}, err
	}
	if err := tx.Commit(); err != nil {
		return SlackEffect{}, err
	}
	return completed, nil
}

// ReconcileSubmittedSlackEffects marks every unresolved submitted effect
// ambiguous before startup execution begins.
func (s *Store) ReconcileSubmittedSlackEffects(ctx context.Context) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	now := s.now()
	result, err := tx.ExecContext(ctx, `UPDATE slack_effects SET
		state='ambiguous',diagnostic_code=?,
		completed_at_us=max(?,submitted_at_us,updated_at_us),
		updated_at_us=max(?,submitted_at_us,updated_at_us)
		WHERE state='submitted' AND attempt_count=1`,
		SlackEffectDiagnosticStartupReconciliation, now, now)
	if err != nil {
		return 0, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	if count != 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE files SET transfer_state='ambiguous',diagnostic_code=?,updated_at_us=max(?,updated_at_us) WHERE direction='to_slack' AND transfer_state='submitted' AND related_effect_id IN (SELECT effect_id FROM slack_effects WHERE state='ambiguous' AND diagnostic_code=?)`, SlackEffectDiagnosticStartupReconciliation, now, SlackEffectDiagnosticStartupReconciliation); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return count, nil
}

// RetrySlackEffect creates a new prepared effect linked to an unmodified
// failed or ambiguous predecessor. It preserves the exact external payload.
func (s *Store) RetrySlackEffect(ctx context.Context, requestID, predecessorSelector, reason string, confirmDuplicateRisk bool) (SlackEffectMutationResult, error) {
	validationErr := validateOperatorReason(reason)
	inputHash := hashSlackEffectRetryInput(predecessorSelector, reason, confirmDuplicateRisk, validationErr == nil)
	req := request{operation: "slack.effect.retry", hash: inputHash}
	return s.mutateSlackEffect(ctx, req, requestID, func(tx *sql.Tx, now int64) (SlackEffectMutationResult, error) {
		if validationErr != nil {
			return SlackEffectMutationResult{}, validationErr
		}
		predecessor, err := getSlackEffect(ctx, tx, predecessorSelector)
		if err != nil {
			return SlackEffectMutationResult{}, err
		}
		switch predecessor.State {
		case SlackEffectFailed:
		case SlackEffectAmbiguous:
			if !confirmDuplicateRisk {
				return SlackEffectMutationResult{}, ErrSlackEffectDuplicateRisk
			}
		default:
			return SlackEffectMutationResult{}, ErrSlackEffectRetryNotAllowed
		}
		effectID, err := s.ids.NewEffectID()
		if err != nil {
			return SlackEffectMutationResult{}, err
		}
		idempotencyKey := hashCanonicalSlackEffect("slack.effect.idempotency.v1", requestID, predecessor.PayloadHash)
		_, err = tx.ExecContext(ctx, `INSERT INTO slack_effects(
			effect_id,idempotency_key,predecessor_effect_id,related_task_id,kind,
			team_id,channel_id,target_message_ts,thread_ts,text,emoji,operator_reason,
			payload_hash,state,attempt_count,prepared_at_us,created_at_us,updated_at_us
		) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,'prepared',0,?,?,?)`,
			effectID, idempotencyKey, predecessor.ID, nullString(predecessor.RelatedTaskID),
			predecessor.Kind, predecessor.TeamID, predecessor.ChannelID,
			nullString(predecessor.TargetMessageTS), nullString(predecessor.ThreadTS),
			nullString(predecessor.Text), nullString(predecessor.Emoji), reason,
			predecessor.PayloadHash, now, now, now)
		if err != nil {
			return SlackEffectMutationResult{}, err
		}
		if predecessor.Kind == SlackEffectFileUpload {
			files, err := listOutgoingFilesForEffectTx(ctx, tx, predecessor.ID)
			if err != nil {
				return SlackEffectMutationResult{}, err
			}
			if len(files) == 0 {
				return SlackEffectMutationResult{}, ErrSlackInvariant
			}
			for _, file := range files {
				fileID, err := s.ids.NewFileID()
				if err != nil {
					return SlackEffectMutationResult{}, err
				}
				if _, err := tx.ExecContext(ctx, `INSERT INTO files(file_id,direction,original_name,validated_size_bytes,sha256,related_effect_id,effect_ordinal,source_attached,source_path,source_device,source_inode,source_mtime_ns,transfer_state,created_at_us,updated_at_us) VALUES(?,'to_slack',?,?,?,?,?,0,?,?,?,?, 'prepared',?,?)`, fileID, file.OriginalName, dereferenceInt(file.ValidatedSizeBytes), file.SHA256, effectID, dereferenceInt(file.EffectOrdinal), file.SourcePath, file.SourceDevice, file.SourceInode, file.SourceMTimeNS, now, now); err != nil {
					return SlackEffectMutationResult{}, err
				}
			}
		}
		return SlackEffectMutationResult{
			EffectID: effectID, State: SlackEffectPrepared, Kind: predecessor.Kind,
			TeamID: predecessor.TeamID, ChannelID: predecessor.ChannelID,
			TargetMessageTS: predecessor.TargetMessageTS,
		}, nil
	})
}

type canonicalEffect struct {
	target      SlackEffectTarget
	intent      SlackEffectIntent
	payloadHash string
}

func canonicalSlackEffect(target SlackEffectTarget, intent SlackEffectIntent) (canonicalEffect, error) {
	if !validSlackID(target.TeamID, "T") {
		return canonicalEffect{}, invalidInput("invalid configured Slack team ID")
	}
	if !validSlackID(target.ChannelID, "CG") {
		return canonicalEffect{}, invalidInput("invalid configured Slack channel ID")
	}
	if intent.Kind == SlackEffectFileUpload {
		return canonicalEffect{}, ErrSlackEffectUnsupported
	}
	if !intent.Kind.public() {
		return canonicalEffect{}, invalidInput("invalid Slack effect kind")
	}
	if intent.RelatedTaskID != "" && !validTaskSelector(intent.RelatedTaskID) {
		return canonicalEffect{}, invalidInput("invalid related task selector")
	}
	if intent.TargetMessageTS != "" && !validSlackTimestamp(intent.TargetMessageTS) {
		return canonicalEffect{}, invalidInput("invalid Slack target message timestamp")
	}
	if intent.ThreadTS != "" && !validSlackTimestamp(intent.ThreadTS) {
		return canonicalEffect{}, invalidInput("invalid Slack thread timestamp")
	}

	normalized := intent
	switch intent.Kind {
	case SlackEffectSend:
		if intent.TargetMessageTS != "" || intent.Emoji != "" || intent.OperatorReason != "" {
			return canonicalEffect{}, invalidInput("invalid send effect shape")
		}
		text, err := normalizeSlackEffectText(intent.Text)
		if err != nil {
			return canonicalEffect{}, err
		}
		normalized.Text = text
	case SlackEffectEdit:
		if intent.TargetMessageTS == "" || intent.ThreadTS != "" || intent.Emoji != "" || intent.OperatorReason != "" {
			return canonicalEffect{}, invalidInput("invalid edit effect shape")
		}
		text, err := normalizeSlackEffectText(intent.Text)
		if err != nil {
			return canonicalEffect{}, err
		}
		normalized.Text = text
	case SlackEffectDelete:
		if intent.TargetMessageTS == "" || intent.ThreadTS != "" || intent.Text != "" || intent.Emoji != "" {
			return canonicalEffect{}, invalidInput("invalid delete effect shape")
		}
		if err := validateOperatorReason(intent.OperatorReason); err != nil {
			return canonicalEffect{}, err
		}
	case SlackEffectReact, SlackEffectUnreact:
		if intent.TargetMessageTS == "" || intent.ThreadTS != "" || intent.Text != "" || intent.OperatorReason != "" {
			return canonicalEffect{}, invalidInput("invalid reaction effect shape")
		}
		emoji, err := normalizeSlackEffectEmoji(intent.Emoji)
		if err != nil {
			return canonicalEffect{}, err
		}
		normalized.Emoji = emoji
	}
	payloadHash := hashCanonicalSlackEffect("slack.effect.payload.v1", string(normalized.Kind), target.TeamID,
		target.ChannelID, normalized.TargetMessageTS, normalized.ThreadTS, normalized.Text, normalized.Emoji)
	return canonicalEffect{target: target, intent: normalized, payloadHash: payloadHash}, nil
}

// hashCanonicalSlackEffect retains the established hashes for canonical valid
// payloads and idempotency keys. Every caller supplies only fields whose
// validation excludes NUL, so hash's separator encoding is unambiguous here.
func hashCanonicalSlackEffect(operation string, values ...string) string {
	return hash(operation, values...)
}

// hashSlackEffectFields encodes both lengths and bytes. Unlike JSON, it neither
// repairs invalid UTF-8 nor loses field boundaries when a string contains NUL.
func hashSlackEffectFields(operation string, values ...string) string {
	h := sha256.New()
	var encodedLength [8]byte
	writeField := func(value string) {
		binary.BigEndian.PutUint64(encodedLength[:], uint64(len(value)))
		_, _ = h.Write(encodedLength[:])
		_, _ = h.Write([]byte(value))
	}
	binary.BigEndian.PutUint64(encodedLength[:], uint64(len(values)+1))
	_, _ = h.Write(encodedLength[:])
	writeField(operation)
	for _, value := range values {
		writeField(value)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func hashSlackEffectInput(target SlackEffectTarget, intent SlackEffectIntent) string {
	return hashSlackEffectFields("slack.effect.prepare.invalid.v2",
		target.TeamID, target.ChannelID, string(intent.Kind), intent.TargetMessageTS,
		intent.ThreadTS, intent.Text, intent.Emoji, intent.OperatorReason, intent.RelatedTaskID)
}

func hashSlackEffectRetryInput(predecessorSelector, reason string, confirmDuplicateRisk, reasonValid bool) string {
	confirmation := fmt.Sprint(confirmDuplicateRisk)
	if reasonValid && validSlackEffectSelector(predecessorSelector) {
		return hashCanonicalSlackEffect("slack.effect.retry.v1", predecessorSelector, reason, confirmation)
	}
	return hashSlackEffectFields("slack.effect.retry.invalid.v2", predecessorSelector, reason, confirmation)
}

func normalizeSlackEffectText(text string) (string, error) {
	normalized, err := task.NormalizeText(text)
	if err != nil {
		return "", invalidInput("invalid Slack effect text")
	}
	if len(normalized) > maxSlackEffectTextBytes {
		return "", invalidInput("Slack effect text exceeds 40000 bytes")
	}
	return normalized, nil
}

func normalizeSlackEffectEmoji(emoji string) (string, error) {
	if strings.HasPrefix(emoji, ":") && strings.HasSuffix(emoji, ":") && len(emoji) >= 3 {
		emoji = emoji[1 : len(emoji)-1]
	}
	if emoji == "" || len(emoji) > maxSlackEffectEmojiBytes || !utf8.ValidString(emoji) {
		return "", invalidInput("invalid Slack emoji")
	}
	base := emoji
	if index := strings.Index(emoji, "::"); index >= 0 {
		suffix := emoji[index:]
		if index == 0 || len(suffix) != len("::skin-tone-2") || !strings.HasPrefix(suffix, "::skin-tone-") || suffix[len(suffix)-1] < '2' || suffix[len(suffix)-1] > '6' {
			return "", invalidInput("invalid Slack emoji")
		}
		base = emoji[:index]
	}
	for _, r := range base {
		if !(r >= 'A' && r <= 'Z') && !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9') && r != '_' && r != '+' && r != '-' {
			return "", invalidInput("invalid Slack emoji")
		}
	}
	return emoji, nil
}

func validateOperatorReason(reason string) error {
	if reason == "" || strings.TrimSpace(reason) == "" || len(reason) > maxSlackEffectReasonBytes || !utf8.ValidString(reason) || strings.IndexByte(reason, 0) >= 0 {
		return invalidInput("invalid Slack effect operator reason")
	}
	return nil
}

func validateSlackEffectOutcome(outcome SlackEffectOutcome) error {
	if !outcome.State.terminal() {
		return invalidInput("Slack effect outcome must be terminal")
	}
	if outcome.ResponseMessageTS != "" && !validSlackTimestamp(outcome.ResponseMessageTS) {
		return invalidInput("invalid Slack response message timestamp")
	}
	if outcome.ResponseFileID != "" && !validSlackID(outcome.ResponseFileID, "F") {
		return invalidInput("invalid Slack response file ID")
	}
	if outcome.State == SlackEffectConfirmed {
		if outcome.DiagnosticCode != "" {
			return invalidInput("confirmed Slack effect cannot have a diagnostic")
		}
	} else {
		if !outcome.DiagnosticCode.valid() {
			return invalidInput("invalid Slack effect diagnostic code")
		}
		if outcome.ResponseMessageTS != "" || outcome.ResponseFileID != "" {
			return invalidInput("unsuccessful Slack effect cannot have response identifiers")
		}
	}
	return nil
}

func validateSlackEffectOutcomeForKind(kind SlackEffectKind, outcome SlackEffectOutcome) error {
	if outcome.ResponseFileID != "" && kind != SlackEffectFileUpload {
		return invalidInput("response file ID is forbidden for this Slack effect")
	}
	if outcome.ResponseMessageTS != "" && kind != SlackEffectSend && kind != SlackEffectEdit {
		return invalidInput("response message timestamp is forbidden for this Slack effect")
	}
	if outcome.State == SlackEffectConfirmed && kind == SlackEffectSend && outcome.ResponseMessageTS == "" {
		return invalidInput("confirmed send requires a response message timestamp")
	}
	return nil
}

func (code SlackEffectDiagnosticCode) valid() bool {
	switch code {
	case SlackEffectDiagnosticInvalidAuth, SlackEffectDiagnosticTokenRevoked,
		SlackEffectDiagnosticMissingScope, SlackEffectDiagnosticTeamMismatch,
		SlackEffectDiagnosticBotMismatch, SlackEffectDiagnosticChannelNotFound,
		SlackEffectDiagnosticNotInChannel, SlackEffectDiagnosticUserNotFound,
		SlackEffectDiagnosticMessageNotFound, SlackEffectDiagnosticExternalRejected,
		SlackEffectDiagnosticRateLimited, SlackEffectDiagnosticTimeout,
		SlackEffectDiagnosticCanceled, SlackEffectDiagnosticDisconnected,
		SlackEffectDiagnosticMalformedResponse, SlackEffectDiagnosticUnavailable,
		SlackEffectDiagnosticInvalidRequest, SlackEffectDiagnosticAccessDenied,
		SlackEffectDiagnosticInvalidTarget, SlackEffectDiagnosticUnknownSubmission,
		SlackEffectDiagnosticStartupReconciliation, SlackEffectDiagnosticFileChanged:
		return true
	default:
		return false
	}
}

func (kind SlackEffectKind) public() bool {
	return kind == SlackEffectSend || kind == SlackEffectEdit || kind == SlackEffectDelete || kind == SlackEffectReact || kind == SlackEffectUnreact
}

func (state SlackEffectState) valid() bool {
	return state == SlackEffectPrepared || state == SlackEffectSubmitted || state.terminal()
}

func (state SlackEffectState) terminal() bool {
	return state == SlackEffectConfirmed || state == SlackEffectFailed || state == SlackEffectAmbiguous
}

func validSlackID(value, prefixes string) bool {
	if len(value) < 2 || len(value) > maxSlackEffectIDBytes || strings.IndexByte(prefixes, value[0]) < 0 {
		return false
	}
	for i := range value {
		if (value[i] < 'A' || value[i] > 'Z') && (value[i] < '0' || value[i] > '9') {
			return false
		}
	}
	return true
}

func validSlackTimestamp(value string) bool {
	if len(value) < 8 || len(value) > 32 {
		return false
	}
	dot := strings.IndexByte(value, '.')
	if dot < 1 || len(value)-dot-1 != 6 || strings.IndexByte(value[dot+1:], '.') >= 0 {
		return false
	}
	for i := range value {
		if i == dot {
			continue
		}
		if value[i] < '0' || value[i] > '9' {
			return false
		}
	}
	return true
}

func canonicalTaskSelector(selector string) (string, bool) {
	// Task IDs created before the unprefixed base62 migration used the tsk_
	// prefix. They remain durable references, so selectors must continue to
	// resolve both forms rather than making existing Slack-task records
	// unreachable after an upgrade.
	if strings.HasPrefix(selector, "tsk_") {
		suffix := selector[len("tsk_"):]
		if len(suffix) == 0 || len(suffix) > 22 || !validOpaqueIDSuffix(suffix) {
			return "", false
		}
		return selector, true
	}
	if len(selector) == 0 || len(selector) > 22 || !validOpaqueIDSuffix(selector) {
		return "", false
	}
	return selector, true
}

func validFullTaskID(id string) bool {
	if strings.HasPrefix(id, "tsk_") {
		return len(id) == 26 && validOpaqueIDSuffix(id[len("tsk_"):])
	}
	return len(id) == 22 && validOpaqueIDSuffix(id)
}

func validTaskSelector(selector string) bool {
	_, ok := canonicalTaskSelector(selector)
	return ok
}

func validSlackEffectSelector(selector string) bool {
	return validTypedSelector(selector, "eff_")
}

func validFullSlackEffectID(effectID string) bool {
	return validFullOpaqueID(effectID, "eff_")
}

func validOpaqueIDSuffix(suffix string) bool {
	for i := range suffix {
		c := suffix[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_' || c == '-' {
			continue
		}
		return false
	}
	return true
}

func resolveOptionalTaskID(ctx context.Context, tx *sql.Tx, selector string) (string, error) {
	if selector == "" {
		return "", nil
	}
	canonical, ok := canonicalTaskSelector(selector)
	if !ok {
		return "", ErrNotFound
	}
	// Do not use LIKE for opaque prefixes: '_' is valid base64url data and is
	// a SQL wildcard. substr compares the caller's selector literally.
	rows, err := tx.QueryContext(ctx, `SELECT task_id FROM tasks
		WHERE task_id=? OR substr(task_id,1,length(?))=? ORDER BY task_id LIMIT 2`,
		canonical, canonical, canonical)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return "", err
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

func nullString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func slackEffectMutationResult(effectID string, target SlackEffectTarget, intent SlackEffectIntent) SlackEffectMutationResult {
	return SlackEffectMutationResult{
		EffectID: effectID, State: SlackEffectPrepared, Kind: intent.Kind,
		TeamID: target.TeamID, ChannelID: target.ChannelID,
		TargetMessageTS: intent.TargetMessageTS,
	}
}

const slackEffectSelect = `SELECT
	effect_id,idempotency_key,coalesce(predecessor_effect_id,''),coalesce(related_task_id,''),
	kind,team_id,channel_id,coalesce(target_message_ts,''),coalesce(thread_ts,''),
	coalesce(text,''),coalesce(emoji,''),coalesce(operator_reason,''),payload_hash,
	state,attempt_count,prepared_at_us,submitted_at_us,completed_at_us,
	coalesce(response_message_ts,''),coalesce(response_file_id,''),coalesce(diagnostic_code,''),
	created_at_us,updated_at_us
FROM slack_effects`

type slackEffectScanner interface {
	Scan(...any) error
}

func scanSlackEffect(scanner slackEffectScanner) (SlackEffect, error) {
	var effect SlackEffect
	var prepared, created, updated int64
	var submitted, completed sql.NullInt64
	err := scanner.Scan(
		&effect.ID, &effect.IdempotencyKey, &effect.PredecessorEffectID, &effect.RelatedTaskID,
		&effect.Kind, &effect.TeamID, &effect.ChannelID, &effect.TargetMessageTS, &effect.ThreadTS,
		&effect.Text, &effect.Emoji, &effect.OperatorReason, &effect.PayloadHash,
		&effect.State, &effect.AttemptCount, &prepared, &submitted, &completed,
		&effect.ResponseMessageTS, &effect.ResponseFileID, &effect.DiagnosticCode,
		&created, &updated,
	)
	if err != nil {
		return SlackEffect{}, err
	}
	effect.PreparedAt = time.UnixMicro(prepared).UTC()
	effect.CreatedAt = time.UnixMicro(created).UTC()
	effect.UpdatedAt = time.UnixMicro(updated).UTC()
	if submitted.Valid {
		t := time.UnixMicro(submitted.Int64).UTC()
		effect.SubmittedAt = &t
	}
	if completed.Valid {
		t := time.UnixMicro(completed.Int64).UTC()
		effect.CompletedAt = &t
	}
	return effect, nil
}

func getSlackEffect(ctx context.Context, q rowQueryer, selector string) (SlackEffect, error) {
	if !validSlackEffectSelector(selector) {
		return SlackEffect{}, ErrSlackEffectNotFound
	}
	rows, err := q.QueryContext(ctx, slackEffectSelect+" WHERE effect_id=? OR substr(effect_id,1,length(?))=? ORDER BY effect_id LIMIT 2", selector, selector, selector)
	if err != nil {
		return SlackEffect{}, err
	}
	defer rows.Close()
	var effects []SlackEffect
	for rows.Next() {
		effect, err := scanSlackEffect(rows)
		if err != nil {
			return SlackEffect{}, err
		}
		effects = append(effects, effect)
	}
	if err := rows.Err(); err != nil {
		return SlackEffect{}, err
	}
	if len(effects) == 0 {
		return SlackEffect{}, ErrSlackEffectNotFound
	}
	if len(effects) > 1 {
		return SlackEffect{}, ErrSlackEffectSelectorAmbiguous
	}
	return effects[0], nil
}

func getSlackEffectExact(ctx context.Context, q rowQueryer, effectID string) (SlackEffect, error) {
	if !validFullSlackEffectID(effectID) {
		return SlackEffect{}, ErrSlackEffectNotFound
	}
	effect, err := scanSlackEffect(q.QueryRowContext(ctx, slackEffectSelect+" WHERE effect_id=?", effectID))
	if errors.Is(err, sql.ErrNoRows) {
		return SlackEffect{}, ErrSlackEffectNotFound
	}
	return effect, err
}

func slackEffectOutcomeMatches(effect SlackEffect, outcome SlackEffectOutcome) bool {
	return effect.State == outcome.State && effect.ResponseMessageTS == outcome.ResponseMessageTS &&
		effect.ResponseFileID == outcome.ResponseFileID && effect.DiagnosticCode == outcome.DiagnosticCode
}

func (s *Store) mutateSlackEffect(ctx context.Context, req request, requestID string, fn func(*sql.Tx, int64) (SlackEffectMutationResult, error)) (SlackEffectMutationResult, error) {
	if !identity.ValidRequestID(requestID) {
		return SlackEffectMutationResult{}, invalidInput("invalid request ID")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return SlackEffectMutationResult{}, err
	}
	defer tx.Rollback()

	var operation, inputHash, outcome, resultType, resultJSON, errorCode, errorMessage string
	err = tx.QueryRowContext(ctx, `SELECT operation,input_hash,outcome,
		coalesce(result_type,''),coalesce(result_json,''),coalesce(error_code,''),coalesce(error_message,'')
		FROM mutation_requests WHERE request_id=?`, requestID).Scan(
		&operation, &inputHash, &outcome, &resultType, &resultJSON, &errorCode, &errorMessage)
	if err == nil {
		if operation != req.operation || inputHash != req.hash {
			return SlackEffectMutationResult{}, ErrRequestConflict
		}
		if outcome != "success" {
			return SlackEffectMutationResult{}, slackEffectErrorForCode(errorCode, errorMessage)
		}
		if resultType != "slack_effect_mutation" {
			return SlackEffectMutationResult{}, errors.New("prior mutation result type mismatch")
		}
		var result SlackEffectMutationResult
		if err := json.Unmarshal([]byte(resultJSON), &result); err != nil {
			return SlackEffectMutationResult{}, err
		}
		return result, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return SlackEffectMutationResult{}, err
	}

	now := s.now()
	result, mutationErr := fn(tx, now)
	if mutationErr != nil {
		code, deterministic := slackEffectErrorCode(mutationErr)
		if !deterministic {
			return SlackEffectMutationResult{}, mutationErr
		}
		expiry := now + RequestRetention.Microseconds()
		_, err = tx.ExecContext(ctx, `INSERT INTO mutation_requests(
			request_id,operation,input_hash,outcome,error_code,error_message,
			created_at,completed_at,expiry_eligible_at
		) VALUES(?,?,?,'error',?,?,?,?,?)`, requestID, req.operation, req.hash,
			code, mutationErr.Error(), now, now, expiry)
		if err != nil {
			return SlackEffectMutationResult{}, err
		}
		if err := tx.Commit(); err != nil {
			return SlackEffectMutationResult{}, err
		}
		return SlackEffectMutationResult{}, mutationErr
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return SlackEffectMutationResult{}, err
	}
	expiry := now + RequestRetention.Microseconds()
	_, err = tx.ExecContext(ctx, `INSERT INTO mutation_requests(
		request_id,operation,input_hash,outcome,result_type,result_json,
		created_at,completed_at,expiry_eligible_at
	) VALUES(?,?,?,'success','slack_effect_mutation',?,?,?,?)`,
		requestID, req.operation, req.hash, string(encoded), now, now, expiry)
	if err != nil {
		return SlackEffectMutationResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return SlackEffectMutationResult{}, err
	}
	return result, nil
}

func slackEffectErrorCode(err error) (string, bool) {
	if code, ok := deterministicCode(err); ok {
		return code, true
	}
	switch {
	case errors.Is(err, ErrSlackEffectNotFound):
		return "slack_effect_not_found", true
	case errors.Is(err, ErrSlackEffectSelectorAmbiguous):
		return "slack_effect_selector_ambiguous", true
	case errors.Is(err, ErrSlackEffectTransition):
		return "slack_effect_transition", true
	case errors.Is(err, ErrSlackEffectRetryNotAllowed):
		return "slack_effect_retry_not_allowed", true
	case errors.Is(err, ErrSlackEffectDuplicateRisk):
		return "slack_effect_duplicate_risk", true
	case errors.Is(err, ErrSlackEffectUnsupported):
		return "slack_effect_unsupported", true
	default:
		return "", false
	}
}

func slackEffectErrorForCode(code, message string) error {
	switch code {
	case "slack_effect_not_found":
		return ErrSlackEffectNotFound
	case "slack_effect_selector_ambiguous":
		return ErrSlackEffectSelectorAmbiguous
	case "slack_effect_transition":
		return ErrSlackEffectTransition
	case "slack_effect_retry_not_allowed":
		return ErrSlackEffectRetryNotAllowed
	case "slack_effect_duplicate_risk":
		return ErrSlackEffectDuplicateRisk
	case "slack_effect_unsupported":
		return ErrSlackEffectUnsupported
	default:
		return errorForCode(code, message)
	}
}

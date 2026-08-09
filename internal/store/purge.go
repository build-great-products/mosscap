package store

// This store-only increment deliberately does not remove temporary bytes or
// their rows.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/stayradiated/mosscap/internal/identity"
)

const (
	// PurgeBatchSize is the total root and dependent-row deletion budget for
	// one confirmed invocation. It is deliberately not a per-table limit.
	PurgeBatchSize = 100
	// PurgeRootLimit independently caps the number of root candidates, even
	// when their dependent graphs are tiny.
	PurgeRootLimit = 100
)

// PurgeCounts is deliberately keyed to the durable tables in schema version 5.
type PurgeCounts struct {
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

// PurgeResult is the replayable result of one bounded confirmed batch. A
// caller is responsible for iteration and for all temporary-byte handling.
type PurgeResult struct {
	Cutoff            time.Time   `json:"cutoff"`
	Eligible          PurgeCounts `json:"eligible"`
	SkippedLive       PurgeCounts `json:"skipped_live"`
	SkippedReferenced PurgeCounts `json:"skipped_referenced"`
	Removed           PurgeCounts `json:"removed"`
	// Skipped is retained internally for compatibility with the store-only
	// increment. New callers must use the typed skip groups above.
	Skipped PurgeCounts `json:"-"`
}

// PreviewMetadataPurge is an advisory, strictly read-only bounded preview. It
// never enters a write transaction, records no mutation request, and does not
// touch temporary-byte or SQLite-maintenance state.
func (s *Store) PreviewMetadataPurge(ctx context.Context, cutoff time.Time) (PurgeResult, error) {
	if cutoff.Location() == nil {
		return PurgeResult{}, invalidInput("invalid purge cutoff")
	}
	cutoff = cutoff.UTC().Truncate(time.Microsecond)
	cutoffUS := cutoff.UnixMicro()
	if cutoffUS <= 0 {
		return PurgeResult{}, invalidInput("invalid purge cutoff")
	}
	out := PurgeResult{Cutoff: cutoff}
	// The preview is deliberately bounded by the same root limit. It reports
	// roots which meet the timestamp/state rule; graph-sensitive confirmation
	// remains authoritative and can skip rows changed by concurrent writers.
	queries := []struct {
		query string
		count *int64
	}{
		{"SELECT count(*) FROM (SELECT 1 FROM tasks WHERE state='done' AND done_at < ? ORDER BY done_at,task_id LIMIT ?)", &out.Eligible.Tasks},
		{"SELECT count(*) FROM (SELECT 1 FROM slack_effects WHERE related_task_id IS NULL AND state='confirmed' AND completed_at_us < ? ORDER BY completed_at_us,effect_id LIMIT ?)", &out.Eligible.SlackEffects},
		{"SELECT count(*) FROM (SELECT 1 FROM slack_source_events WHERE task_id IS NULL AND admission_result IN ('duplicate','ignored_workspace','ignored_channel','ignored_untrusted','ignored_unmentioned','ignored_bot','ignored_stale','ignored_untracked','malformed') AND committed_at_us < ? ORDER BY committed_at_us,source_event_id LIMIT ?)", &out.Eligible.SlackSourceEvents},
		{"SELECT count(*) FROM (SELECT 1 FROM mutation_requests WHERE expiry_eligible_at < ? ORDER BY expiry_eligible_at,request_id LIMIT ?)", &out.Eligible.MutationRequests},
	}
	remaining := int64(PurgeRootLimit)
	for _, q := range queries {
		if remaining == 0 {
			break
		}
		if err := s.db.QueryRowContext(ctx, q.query, cutoffUS, remaining).Scan(q.count); err != nil {
			return PurgeResult{}, err
		}
		remaining -= *q.count
	}
	return out, nil
}

// ConfirmMetadataPurge removes one bounded, FK-safe batch. The cutoff is
// strict: values equal to it remain. Persisted deliveries require a non-null
// completed_at_us strictly before cutoff; updated_at_us is intentionally not a
// fallback because it could make a delivery with unknown completion removable.
func (s *Store) ConfirmMetadataPurge(ctx context.Context, requestID string, cutoff time.Time) (PurgeResult, error) {
	if !identity.ValidRequestID(requestID) {
		return PurgeResult{}, errors.New("invalid request ID")
	}
	if cutoff.Location() == nil {
		return PurgeResult{}, invalidInput("invalid purge cutoff")
	}
	cutoff = cutoff.UTC().Truncate(time.Microsecond)
	cutoffUS := cutoff.UnixMicro()
	if cutoffUS <= 0 {
		return PurgeResult{}, invalidInput("invalid purge cutoff")
	}
	req := request{"metadata.purge", hash("metadata.purge", fmt.Sprint(cutoffUS))}

	s.mu.Lock()
	defer s.mu.Unlock()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return PurgeResult{}, err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return PurgeResult{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	var operation, inputHash, outcome, resultType, resultJSON string
	err = conn.QueryRowContext(ctx, "SELECT operation,input_hash,outcome,coalesce(result_type,''),coalesce(result_json,'') FROM mutation_requests WHERE request_id=?", requestID).Scan(&operation, &inputHash, &outcome, &resultType, &resultJSON)
	if err == nil {
		if operation != req.operation || inputHash != req.hash {
			return PurgeResult{}, ErrRequestConflict
		}
		if outcome != "success" || resultType != "metadata_purge" {
			return PurgeResult{}, errors.New("prior purge result mismatch")
		}
		var replay PurgeResult
		if err := json.Unmarshal([]byte(resultJSON), &replay); err != nil {
			return PurgeResult{}, err
		}
		if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
			return PurgeResult{}, err
		}
		committed = true
		return replay, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return PurgeResult{}, err
	}

	out := PurgeResult{Cutoff: cutoff}
	budget := purgeBudget{rows: PurgeBatchSize, roots: PurgeRootLimit}
	if err := purgeTasks(ctx, conn, cutoffUS, &budget, &out); err != nil {
		return PurgeResult{}, err
	}
	if err := purgeStandaloneEffects(ctx, conn, cutoffUS, &budget, &out); err != nil {
		return PurgeResult{}, err
	}
	if err := purgeUnlinkedSources(ctx, conn, cutoffUS, &budget, &out); err != nil {
		return PurgeResult{}, err
	}
	if err := purgeRequests(ctx, conn, cutoffUS, requestID, &budget, &out); err != nil {
		return PurgeResult{}, err
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return PurgeResult{}, err
	}
	now := s.now()
	if _, err := conn.ExecContext(ctx, "INSERT INTO mutation_requests(request_id,operation,input_hash,outcome,result_type,result_json,created_at,completed_at,expiry_eligible_at) VALUES(?,?,?,'success','metadata_purge',?,?,?,?)", requestID, req.operation, req.hash, string(raw), now, now, now+RequestRetention.Microseconds()); err != nil {
		return PurgeResult{}, err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return PurgeResult{}, err
	}
	committed = true
	return out, nil
}

type purgeConn interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

type purgeBudget struct{ rows, roots int64 }

// takeCandidate reserves the global root-candidate budget. It is charged
// before graph preflight, so skipped roots cannot let later root families scan
// past PurgeRootLimit.
func (b *purgeBudget) takeCandidate() bool {
	if b.roots < 1 {
		return false
	}
	b.roots--
	return true
}

// take reserves deletion rows only after a candidate graph is eligible.
func (b *purgeBudget) take(rows int64) bool {
	if rows < 1 || rows > b.rows {
		return false
	}
	b.rows -= rows
	return true
}
func (b purgeBudget) candidates() int64 {
	if b.roots < b.rows {
		return b.roots
	}
	return b.rows
}

func purgeTasks(ctx context.Context, q purgeConn, cutoff int64, budget *purgeBudget, out *PurgeResult) error {
	if budget.candidates() == 0 {
		return nil
	}
	rows, err := q.QueryContext(ctx, "SELECT task_id FROM tasks WHERE state='done' AND done_at < ? ORDER BY done_at,task_id LIMIT ?", cutoff, budget.candidates())
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		if !budget.takeCandidate() {
			break
		}
		if err := purgeTask(ctx, q, id, cutoff, budget, out); err != nil {
			return err
		}
	}
	return rows.Err()
}

func purgeTask(ctx context.Context, q purgeConn, taskID string, cutoff int64, budget *purgeBudget, out *PurgeResult) error {
	var eligible bool
	if err := q.QueryRowContext(ctx, "SELECT state='done' AND done_at < ? FROM tasks WHERE task_id=?", cutoff, taskID).Scan(&eligible); errors.Is(err, sql.ErrNoRows) {
		return nil
	} else if err != nil {
		return err
	}
	if !eligible {
		out.Skipped.Tasks++
		return nil
	}
	// Every task-owned delivery must be finalized using the deliberately
	// conservative completed timestamp policy, and no retained delivery may
	// depend on one selected for this task.
	var live int64
	if err := q.QueryRowContext(ctx, `SELECT count(*) FROM pi_deliveries d WHERE d.task_id=? AND (d.state <> 'persisted' OR d.completed_at_us IS NULL OR d.completed_at_us >= ? OR EXISTS (SELECT 1 FROM pi_deliveries r WHERE r.predecessor_delivery_id=d.delivery_id AND r.task_id<>?))`, taskID, cutoff, taskID).Scan(&live); err != nil {
		return err
	}
	if live != 0 {
		out.Skipped.Tasks++
		return nil
	}
	var effects int64
	// A successor is selected only when it belongs to this task. In particular,
	// a NULL related_task_id is retained; SQLite's <> would silently miss it.
	if err := q.QueryRowContext(ctx, `SELECT count(*) FROM slack_effects e WHERE e.related_task_id=? AND (e.state <> 'confirmed' OR e.completed_at_us IS NULL OR e.completed_at_us >= ? OR EXISTS (SELECT 1 FROM slack_effects r WHERE r.predecessor_effect_id=e.effect_id AND (r.related_task_id IS NULL OR r.related_task_id <> ?)))`, taskID, cutoff, taskID).Scan(&effects); err != nil {
		return err
	}
	if effects != 0 {
		out.Skipped.Tasks++
		return nil
	}
	// Files may be directly task-owned or owned through an outgoing upload
	// effect. Temporary transfer rows represent private bytes managed outside
	// this metadata-only operation, so retaining either owner is required.
	var files int64
	if err := q.QueryRowContext(ctx, `SELECT count(*) FROM files f WHERE (f.related_task_id=? OR EXISTS (SELECT 1 FROM slack_effects e WHERE e.effect_id=f.related_effect_id AND e.related_task_id=?)) AND EXISTS (SELECT 1 FROM file_temporary_transfers x WHERE x.file_id=f.file_id)`, taskID, taskID).Scan(&files); err != nil {
		return err
	}
	if files != 0 {
		out.Skipped.Tasks++
		return nil
	}
	rows, err := taskGraphRows(ctx, q, taskID)
	if err != nil {
		return err
	}
	if !budget.take(rows) {
		out.Skipped.Tasks++
		return nil
	}
	return deleteTaskGraph(ctx, q, taskID, out)
}

func taskGraphRows(ctx context.Context, q purgeConn, taskID string) (int64, error) {
	var rows int64
	err := q.QueryRowContext(ctx, `SELECT
		1 +
		(SELECT count(*) FROM files f WHERE f.related_task_id=? OR EXISTS (SELECT 1 FROM slack_effects e WHERE e.effect_id=f.related_effect_id AND e.related_task_id=?)) +
		(SELECT count(*) FROM slack_task_sources WHERE task_id=?) +
		(SELECT count(*) FROM task_state_events WHERE task_id=?) +
		(SELECT count(*) FROM slack_source_events WHERE task_id=?) +
		(SELECT count(*) FROM pi_deliveries WHERE task_id=?) +
		(SELECT count(*) FROM slack_effects WHERE related_task_id=?)`, taskID, taskID, taskID, taskID, taskID, taskID, taskID).Scan(&rows)
	return rows, err
}

func deleteTaskGraph(ctx context.Context, q purgeConn, taskID string, out *PurgeResult) error {
	// The child-before-parent order follows every schema-v5 RESTRICT edge.
	// Outgoing upload files are owned through their task-owned effect, not
	// directly through the task. Preflight retains any file with temporary-byte
	// metadata, so every selected file can be deleted before its effect.
	fileOwner := `related_task_id=? OR EXISTS (SELECT 1 FROM slack_effects e WHERE e.effect_id=files.related_effect_id AND e.related_task_id=?)`
	if n, err := deleteCount(ctx, q, "DELETE FROM files WHERE "+fileOwner, taskID, taskID); err != nil {
		return err
	} else {
		out.Removed.Files += n
	}
	if n, err := deleteCount(ctx, q, "DELETE FROM slack_task_sources WHERE task_id=?", taskID); err != nil {
		return err
	} else {
		out.Removed.SlackTaskSources += n
	}
	if n, err := deleteCount(ctx, q, "DELETE FROM task_state_events WHERE task_id=?", taskID); err != nil {
		return err
	} else {
		out.Removed.TaskStateEvents += n
	}
	if n, err := deleteCount(ctx, q, "DELETE FROM slack_source_events WHERE task_id=?", taskID); err != nil {
		return err
	} else {
		out.Removed.SlackSourceEvents += n
	}
	if err := deleteDeliveryLeaves(ctx, q, taskID, &out.Removed.PiDeliveries); err != nil {
		return err
	}
	if err := deleteEffectLeaves(ctx, q, "related_task_id=?", []any{taskID}, &out.Removed.SlackEffects); err != nil {
		return err
	}
	if n, err := deleteCount(ctx, q, "DELETE FROM tasks WHERE task_id=?", taskID); err != nil {
		return err
	} else {
		out.Removed.Tasks += n
	}
	return nil
}

func purgeStandaloneEffects(ctx context.Context, q purgeConn, cutoff int64, budget *purgeBudget, out *PurgeResult) error {
	if budget.candidates() == 0 {
		return nil
	}
	rows, err := q.QueryContext(ctx, "SELECT effect_id FROM slack_effects WHERE related_task_id IS NULL AND state='confirmed' AND completed_at_us < ? ORDER BY completed_at_us,effect_id LIMIT ?", cutoff, budget.candidates())
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		if !budget.takeCandidate() {
			break
		}
		var blocked int64
		err := q.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM slack_effects r WHERE r.predecessor_effect_id=?) + (SELECT count(*) FROM files f WHERE f.related_effect_id=? AND (f.related_task_id IS NOT NULL OR EXISTS (SELECT 1 FROM file_temporary_transfers x WHERE x.file_id=f.file_id)))`, id, id).Scan(&blocked)
		if err != nil {
			return err
		}
		if blocked != 0 {
			out.Skipped.SlackEffects++
			continue
		}
		var fileRows int64
		if err := q.QueryRowContext(ctx, "SELECT count(*) FROM files WHERE related_effect_id=?", id).Scan(&fileRows); err != nil {
			return err
		}
		if !budget.take(1 + fileRows) {
			out.Skipped.SlackEffects++
			continue
		}
		if n, err := deleteCount(ctx, q, "DELETE FROM files WHERE related_effect_id=?", id); err != nil {
			return err
		} else {
			out.Removed.Files += n
		}
		if n, err := deleteCount(ctx, q, "DELETE FROM slack_effects WHERE effect_id=?", id); err != nil {
			return err
		} else {
			out.Removed.SlackEffects += n
		}
	}
	return rows.Err()
}

func purgeUnlinkedSources(ctx context.Context, q purgeConn, cutoff int64, budget *purgeBudget, out *PurgeResult) error {
	const results = "'duplicate','ignored_workspace','ignored_channel','ignored_untrusted','ignored_unmentioned','ignored_bot','ignored_stale','ignored_untracked','malformed'"
	if budget.candidates() == 0 {
		return nil
	}
	rows, err := q.QueryContext(ctx, "SELECT source_event_id FROM slack_source_events WHERE task_id IS NULL AND admission_result IN ("+results+") AND committed_at_us < ? ORDER BY committed_at_us,source_event_id LIMIT ?", cutoff, budget.candidates())
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		if !budget.takeCandidate() {
			break
		}
		if !budget.take(1) {
			break
		}
		n, err := deleteCount(ctx, q, "DELETE FROM slack_source_events WHERE source_event_id=?", id)
		if err != nil {
			return err
		}
		out.Removed.SlackSourceEvents += n
	}
	return rows.Err()
}
func purgeRequests(ctx context.Context, q purgeConn, cutoff int64, requestID string, budget *purgeBudget, out *PurgeResult) error {
	if budget.candidates() == 0 {
		return nil
	}
	rows, err := q.QueryContext(ctx, "SELECT request_id FROM mutation_requests WHERE request_id<>? AND expiry_eligible_at < ? ORDER BY expiry_eligible_at,request_id LIMIT ?", requestID, cutoff, budget.candidates())
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		if !budget.takeCandidate() {
			break
		}
		if !budget.take(1) {
			break
		}
		n, err := deleteCount(ctx, q, "DELETE FROM mutation_requests WHERE request_id=?", id)
		if err != nil {
			return err
		}
		out.Removed.MutationRequests += n
	}
	return rows.Err()
}
func deleteCount(ctx context.Context, q purgeConn, query string, args ...any) (int64, error) {
	r, err := q.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	return r.RowsAffected()
}
func deleteDeliveryLeaves(ctx context.Context, q purgeConn, taskID string, count *int64) error {
	for {
		n, err := deleteCount(ctx, q, "DELETE FROM pi_deliveries WHERE task_id=? AND NOT EXISTS (SELECT 1 FROM pi_deliveries child WHERE child.predecessor_delivery_id=pi_deliveries.delivery_id)", taskID)
		if err != nil {
			return err
		}
		*count += n
		if n == 0 {
			break
		}
	}
	var remain int64
	if err := q.QueryRowContext(ctx, "SELECT count(*) FROM pi_deliveries WHERE task_id=?", taskID).Scan(&remain); err != nil {
		return err
	}
	if remain != 0 {
		return errors.New("purge delivery predecessor cycle")
	}
	return nil
}
func deleteEffectLeaves(ctx context.Context, q purgeConn, where string, args []any, count *int64) error {
	for {
		n, err := deleteCount(ctx, q, "DELETE FROM slack_effects WHERE "+where+" AND NOT EXISTS (SELECT 1 FROM slack_effects child WHERE child.predecessor_effect_id=slack_effects.effect_id)", args...)
		if err != nil {
			return err
		}
		*count += n
		if n == 0 {
			break
		}
	}
	var remain int64
	if err := q.QueryRowContext(ctx, "SELECT count(*) FROM slack_effects WHERE "+where, args...).Scan(&remain); err != nil {
		return err
	}
	if remain != 0 {
		return errors.New("purge effect predecessor cycle")
	}
	return nil
}

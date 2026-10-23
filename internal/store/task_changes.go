package store

import (
	"context"
	"database/sql"
	"errors"
)

// TaskChangeKind names the most recent change recorded on a task row.
type TaskChangeKind string

const (
	TaskChangeCreated TaskChangeKind = "created"
	TaskChangeEdited  TaskChangeKind = "edited"
	TaskChangeState   TaskChangeKind = "state"
)

// ErrTaskChangeCursorAhead means a change cursor is newer than this ledger's
// sequence, normally because it came from another or a restored database.
var ErrTaskChangeCursorAhead = errors.New("task change cursor is ahead of this ledger; restart without a cursor")

// TaskChange is a current task snapshot keyed by its latest change sequence.
// Triggers assign the sequence inside the writing transaction, so commit order
// and sequence order agree. Only the latest change of a task is retained:
// earlier changes since a cursor are coalesced into the current snapshot.
// SlackSource is present for Slack-origin tasks and names their channel.
type TaskChange struct {
	Seq         int64
	Kind        TaskChangeKind
	Task        Task
	SlackSource *SlackSource
}

// prefixedScanner scans leading columns before handing the remaining columns
// to a shared row scanner.
type prefixedScanner struct {
	rows   *sql.Rows
	prefix []any
}

func (p prefixedScanner) Scan(dest ...any) error {
	return p.rows.Scan(append(append([]any(nil), p.prefix...), dest...)...)
}

// TaskChanges returns tasks whose latest change sequence is greater than after,
// oldest change first, plus the ledger head read from the same snapshot.
// Purge can remove done tasks but never reuses a sequence value.
func (s *Store) TaskChanges(ctx context.Context, after int64, limit int) ([]TaskChange, int64, error) {
	if limit == 0 {
		limit = 20
	}
	if limit < 1 || limit > 100 || after < 0 {
		return nil, 0, invalidInput("invalid task change page")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, 0, err
	}
	defer tx.Rollback()
	var head int64
	if err := tx.QueryRowContext(ctx, "SELECT last_seq FROM task_change_sequence WHERE singleton=1").Scan(&head); err != nil {
		return nil, 0, err
	}
	if after > head {
		return nil, 0, ErrTaskChangeCursorAhead
	}
	rows, err := tx.QueryContext(ctx, `SELECT t.change_seq,t.change_kind,t.task_id,t.version,t.state,t.text,t.source_kind,t.created_at,t.updated_at,t.done_at,
		s.initial_source_event_id,s.team_id,s.channel_id,s.author_user_id,s.message_ts,s.thread_ts,s.permalink,s.latest_event_at_us,s.source_deleted_at_us,s.created_at_us,s.updated_at_us
		FROM tasks t LEFT JOIN slack_task_sources s ON s.task_id=t.task_id
		WHERE t.change_seq>? ORDER BY t.change_seq LIMIT ?`, after, limit)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var changes []TaskChange
	for rows.Next() {
		var c TaskChange
		model, err := scanTaskReadModel(prefixedScanner{rows: rows, prefix: []any{&c.Seq, &c.Kind}})
		if err != nil {
			return nil, 0, err
		}
		c.Task, c.SlackSource = model.Task, model.SlackSource
		changes = append(changes, c)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return changes, head, nil
}

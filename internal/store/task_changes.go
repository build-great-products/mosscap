package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
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
type TaskChange struct {
	Seq  int64
	Kind TaskChangeKind
	Task Task
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
	rows, err := tx.QueryContext(ctx, "SELECT change_seq,change_kind,task_id,version,state,text,source_kind,created_at,updated_at,done_at FROM tasks WHERE change_seq>? ORDER BY change_seq LIMIT ?", after, limit)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var changes []TaskChange
	for rows.Next() {
		var c TaskChange
		var created, updated int64
		var done sql.NullInt64
		if err := rows.Scan(&c.Seq, &c.Kind, &c.Task.ID, &c.Task.Version, &c.Task.State, &c.Task.Text, &c.Task.SourceKind, &created, &updated, &done); err != nil {
			return nil, 0, err
		}
		c.Task.CreatedAt = time.UnixMicro(created).UTC()
		c.Task.UpdatedAt = time.UnixMicro(updated).UTC()
		if done.Valid {
			d := time.UnixMicro(done.Int64).UTC()
			c.Task.DoneAt = &d
		}
		changes = append(changes, c)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return changes, head, nil
}

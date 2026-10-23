package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stayradiated/mosscap/internal/task"
)

func taskChangeSeqs(t *testing.T, s *Store, after int64) ([]TaskChange, int64) {
	t.Helper()
	changes, head, err := s.TaskChanges(context.Background(), after, 100)
	if err != nil {
		t.Fatal(err)
	}
	return changes, head
}

// The fixed test clock gives every write the same timestamp, so this proves
// ordering comes from the trigger-maintained sequence rather than wall time.
func TestTaskChangesFollowCommitOrderAndCoalesceToCurrentSnapshot(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if changes, head := taskChangeSeqs(t, s, 0); len(changes) != 0 || head != 0 {
		t.Fatalf("empty ledger changes=%+v head=%d", changes, head)
	}
	first, err := s.Create(ctx, "change-create-1", "first")
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Create(ctx, "change-create-2", "second")
	if err != nil {
		t.Fatal(err)
	}
	changes, head := taskChangeSeqs(t, s, 0)
	if head != 2 || len(changes) != 2 || changes[0].Task.ID != first.TaskID || changes[0].Seq != 1 || changes[0].Kind != TaskChangeCreated || changes[1].Task.ID != second.TaskID || changes[1].Seq != 2 {
		t.Fatalf("created changes=%+v head=%d", changes, head)
	}

	if _, err := s.Update(ctx, "change-update-1", first.TaskID, "first\nedited"); err != nil {
		t.Fatal(err)
	}
	changes, head = taskChangeSeqs(t, s, 2)
	if head != 3 || len(changes) != 1 || changes[0].Seq != 3 || changes[0].Kind != TaskChangeEdited || changes[0].Task.Version != 2 || changes[0].Task.Text != "first\nedited" {
		t.Fatalf("edit changes=%+v head=%d", changes, head)
	}
	// Unchanged text and same-state transitions do not advance the sequence.
	if _, err := s.Update(ctx, "change-update-noop", first.TaskID, "first\nedited"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Transition(ctx, "change-open-noop", first.TaskID, task.Open, nil); err != nil {
		t.Fatal(err)
	}
	if _, head = taskChangeSeqs(t, s, 3); head != 3 {
		t.Fatalf("no-op writes advanced head to %d", head)
	}

	if _, err := s.Transition(ctx, "change-working", second.TaskID, task.Working, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Transition(ctx, "change-done", second.TaskID, task.Done, nil); err != nil {
		t.Fatal(err)
	}
	// A cursor taken before both transitions sees one coalesced current row.
	changes, head = taskChangeSeqs(t, s, 3)
	if head != 5 || len(changes) != 1 || changes[0].Seq != 5 || changes[0].Kind != TaskChangeState || changes[0].Task.State != task.Done || changes[0].Task.DoneAt == nil {
		t.Fatalf("state changes=%+v head=%d", changes, head)
	}
	changes, _ = taskChangeSeqs(t, s, 0)
	if len(changes) != 2 || changes[0].Task.ID != first.TaskID || changes[1].Task.ID != second.TaskID {
		t.Fatalf("full feed=%+v", changes)
	}
	if page, _, err := s.TaskChanges(ctx, 0, 1); err != nil || len(page) != 1 || page[0].Seq != 3 {
		t.Fatalf("bounded page=%+v err=%v", page, err)
	}
	if _, _, err := s.TaskChanges(ctx, head+1, 10); !errors.Is(err, ErrTaskChangeCursorAhead) {
		t.Fatalf("cursor ahead err=%v", err)
	}
	for _, limit := range []int{-1, 101} {
		if _, _, err := s.TaskChanges(ctx, 0, limit); err == nil {
			t.Fatalf("limit %d accepted", limit)
		}
	}
}

func TestTaskChangesRecordSlackEditsAsEdited(t *testing.T) {
	s := testStore(t)
	trustSourceTestUser(t, s)
	ctx := context.Background()
	initial := sourceTestEvent("Ev-change-initial", SlackEventAppMention, 1_000)
	admitted, err := s.CommitSlackSourceEvent(ctx, sourceTestPolicy, initial)
	if err != nil {
		t.Fatal(err)
	}
	changes, head := taskChangeSeqs(t, s, 0)
	if len(changes) != 1 || changes[0].Kind != TaskChangeCreated || changes[0].Task.SourceKind != "slack" || changes[0].Task.ID != admitted.TaskID {
		t.Fatalf("admitted changes=%+v", changes)
	}
	edit := initial
	edit.SourceEventID = "Ev-change-edit"
	edit.EventKind = SlackEventMessageChange
	edit.SlackEventAtUS = 1_001
	edit.Text = "edited <@U99999999>"
	if result, err := s.CommitSlackSourceEvent(ctx, sourceTestPolicy, edit); err != nil || result.TaskVersion != 2 {
		t.Fatalf("edit=%+v err=%v", result, err)
	}
	changes, _ = taskChangeSeqs(t, s, head)
	if len(changes) != 1 || changes[0].Kind != TaskChangeEdited || changes[0].Task.Version != 2 || changes[0].Task.Text != "edited <@U99999999>" {
		t.Fatalf("edited changes=%+v", changes)
	}
}

func TestTaskChangeSequenceIsNotReusedAfterPurge(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	old := int64(1_700_000_000_000_000)
	if _, err := s.DB().Exec(`INSERT INTO tasks(task_id,version,state,text,source_kind,created_at,updated_at,done_at) VALUES('tsk_change_purged',1,'done','done','local',?,?,?)`, old, old, old); err != nil {
		t.Fatal(err)
	}
	_, head := taskChangeSeqs(t, s, 0)
	if _, err := s.ConfirmMetadataPurge(ctx, "change-purge", time.UnixMicro(old+10).UTC()); err != nil {
		t.Fatal(err)
	}
	created, err := s.Create(ctx, "change-after-purge", "after purge")
	if err != nil {
		t.Fatal(err)
	}
	changes, next := taskChangeSeqs(t, s, head)
	if next != head+1 || len(changes) != 1 || changes[0].Seq != head+1 || changes[0].Task.ID != created.TaskID {
		t.Fatalf("post-purge changes=%+v head=%d previous=%d", changes, next, head)
	}
}

func TestOpenUpgradeV6BackfillsTaskChangesInUpdateOrder(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ledger.sqlite3")
	db, err := sql.Open("sqlite", sqliteDSN(path, time.Second))
	if err != nil {
		t.Fatal(err)
	}
	for version, migration := range [][]byte{migration0001, migration0002, migration0003, migration0004, migration0005, migration0006} {
		if _, err := db.Exec(string(migration)); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec("INSERT INTO schema_migrations(version,checksum,applied_at) VALUES(?,?,1)", version+1, migrationChecksum(version+1)); err != nil {
			t.Fatal(err)
		}
	}
	for _, statement := range []string{
		fmt.Sprintf("PRAGMA application_id=%d", ApplicationID),
		"PRAGMA user_version=6",
		`INSERT INTO tasks(task_id,version,state,text,source_kind,created_at,updated_at,done_at) VALUES('tsk_v6_later',1,'open','later','local',1,30,NULL)`,
		`INSERT INTO tasks(task_id,version,state,text,source_kind,created_at,updated_at,done_at) VALUES('tsk_v6_edited',2,'open','edited','local',2,10,NULL)`,
		`INSERT INTO tasks(task_id,version,state,text,source_kind,created_at,updated_at,done_at) VALUES('tsk_v6_done',1,'done','done','local',3,20,20)`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatalf("Open v6: %v", err)
	}
	defer s.Close()
	changes, head := taskChangeSeqs(t, s, 0)
	if head != 3 || len(changes) != 3 {
		t.Fatalf("backfill changes=%+v head=%d", changes, head)
	}
	want := []struct {
		id   string
		kind TaskChangeKind
	}{{"tsk_v6_edited", TaskChangeEdited}, {"tsk_v6_done", TaskChangeState}, {"tsk_v6_later", TaskChangeCreated}}
	for i, w := range want {
		if changes[i].Seq != int64(i+1) || changes[i].Task.ID != w.id || changes[i].Kind != w.kind {
			t.Fatalf("backfill[%d]=%+v want %+v", i, changes[i], w)
		}
	}
	created, err := s.Create(ctx, "after-upgrade", "after upgrade")
	if err != nil {
		t.Fatal(err)
	}
	if changes, head := taskChangeSeqs(t, s, 3); head != 4 || len(changes) != 1 || changes[0].Task.ID != created.TaskID {
		t.Fatalf("post-upgrade changes=%+v head=%d", changes, head)
	}
}

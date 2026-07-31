package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stayradiated/mosscap/internal/clock"
	"github.com/stayradiated/mosscap/internal/task"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ledger.sqlite3")
	s, err := Initialize(context.Background(), path, Options{Clock: clock.Fixed{Time: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}
func TestInitializeAndOpenCompatibility(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ledger.sqlite3")
	s, err := Initialize(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	s, err = Open(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	if err := os.WriteFile(filepath.Join(filepath.Dir(path), "foreign.sqlite3"), []byte("not sqlite"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(ctx, filepath.Join(filepath.Dir(path), "foreign.sqlite3"), Options{}); err == nil {
		t.Fatal("foreign database opened")
	}
}
func TestMutationsAreAtomicAndDoNotKeepOldText(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	created, err := s.Create(ctx, "create-1", "old text  \r\nline\t")
	if err != nil {
		t.Fatal(err)
	}
	if created.Version != 1 || created.State != task.Open || !created.Changed {
		t.Fatalf("create=%+v", created)
	}
	again, err := s.Create(ctx, "create-1", "old text  \r\nline\t")
	if err != nil || again != created {
		t.Fatalf("replay=%+v %v", again, err)
	}
	if _, err := s.Create(ctx, "create-1", "other"); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("conflict=%v", err)
	}
	updated, err := s.Update(ctx, "update-1", created.TaskID, "new text")
	if err != nil || updated.Version != 2 || !updated.Changed {
		t.Fatalf("update=%+v %v", updated, err)
	}
	noop, err := s.Update(ctx, "update-2", created.TaskID, "new text")
	if err != nil || noop.Changed || noop.Version != 2 || !noop.UpdatedAt.Equal(updated.UpdatedAt) {
		t.Fatalf("noop=%+v %v", noop, err)
	}
	var n int
	if err := s.DB().QueryRow("SELECT count(*) FROM mutation_requests WHERE result_json LIKE '%old text%'").Scan(&n); err != nil || n != 0 {
		t.Fatalf("old request text count=%d err=%v", n, err)
	}
	if err := s.DB().QueryRow("SELECT count(*) FROM task_state_events WHERE note LIKE '%old text%'").Scan(&n); err != nil || n != 0 {
		t.Fatalf("old audit text count=%d err=%v", n, err)
	}
}
func TestTransitionsAndRestart(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	c, err := s.Create(ctx, "create", "task")
	if err != nil {
		t.Fatal(err)
	}
	done, err := s.Transition(ctx, "done", c.TaskID, task.Done, nil)
	if err != nil || done.Version != 1 {
		t.Fatalf("done=%+v %v", done, err)
	}
	noop, err := s.Transition(ctx, "done-noop", c.TaskID, task.Done, nil)
	if err != nil || noop.Changed || !noop.UpdatedAt.Equal(done.UpdatedAt) {
		t.Fatalf("noop=%+v %v", noop, err)
	}
	back, err := s.Transition(ctx, "open", c.TaskID, task.Open, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ctx, c.TaskID)
	if err != nil || got.DoneAt != nil || got.State != task.Open || back.Version != 1 {
		t.Fatalf("get=%+v %v", got, err)
	}
	var events int
	if err := s.DB().QueryRow("SELECT count(*) FROM task_state_events").Scan(&events); err != nil || events != 2 {
		t.Fatalf("events=%d err=%v", events, err)
	}
}
func TestConcurrentDuplicateCreateProducesOneTask(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const workers = 8
	results := make(chan MutationResult, workers)
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() { defer wg.Done(); r, err := s.Create(ctx, "duplicate", "only once"); results <- r; errs <- err }()
	}
	wg.Wait()
	close(results)
	close(errs)
	var id string
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	for r := range results {
		if id == "" {
			id = r.TaskID
		}
		if r.TaskID != id {
			t.Fatalf("IDs differ: %q %q", id, r.TaskID)
		}
	}
	var n int
	if err := s.DB().QueryRow("SELECT count(*) FROM tasks").Scan(&n); err != nil || n != 1 {
		t.Fatalf("tasks=%d err=%v", n, err)
	}
}
func TestPragmasAndConstraints(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	for range 2 {
		conn, err := s.DB().Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var fk int
		if err = conn.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fk); err != nil || fk != 1 {
			t.Fatalf("foreign_keys=%d err=%v", fk, err)
		}
		_ = conn.Close()
	}
	if _, err := s.DB().ExecContext(ctx, "INSERT INTO task_state_events(event_id,task_id,prior_state,new_state,actor,request_id,committed_at) VALUES('ste_x','tsk_missing','open','done','a','r',1)"); err == nil {
		t.Fatal("foreign key constraint absent")
	}
}
func TestDefaultGenerator(t *testing.T) {
	s := testStore(t)
	r, err := s.Create(context.Background(), "generated", "text")
	if err != nil || r.TaskID == "" {
		t.Fatalf("result=%+v err=%v", r, err)
	}
}

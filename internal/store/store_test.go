package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
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
func TestDatabasePathWithSQLiteQuerySyntaxIsLiteral(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ledger?mode=memory")
	s, err := Initialize(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(ctx, "literal-path", "durable"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
		t.Fatalf("literal database path = %v, %v", info, err)
	}
	s, err = Open(ctx, path, Options{})
	if err != nil {
		t.Fatalf("reopen literal database: %v", err)
	}
	defer s.Close()
	tasks, _, err := s.List(ctx, nil, true, 10, "")
	if err != nil || len(tasks) != 1 || tasks[0].Text != "durable" {
		t.Fatalf("literal database contents = %#v, %v", tasks, err)
	}
}

func TestOpenRejectsGappedMigrationHistory(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ledger.sqlite3")
	s, err := Initialize(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec("UPDATE schema_migrations SET version=8 WHERE version=2"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(ctx, path, Options{}); !errors.Is(err, ErrIncompatibleSchema) {
		t.Fatalf("Open gapped migration: %v", err)
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
func TestMutationSemanticErrorsAreDurableAndReplayExactly(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	_, first := s.Create(ctx, "bad-create", "\x00")
	_, replay := s.Create(ctx, "bad-create", "\x00")
	if first == nil || replay == nil || first.Error() != replay.Error() {
		t.Fatalf("invalid create replay: first=%v replay=%v", first, replay)
	}
	var outcome, code, message string
	if err := s.DB().QueryRow("SELECT outcome, error_code, error_message FROM mutation_requests WHERE request_id='bad-create'").Scan(&outcome, &code, &message); err != nil {
		t.Fatal(err)
	}
	if outcome != "error" || code != "invalid_input" || message != first.Error() {
		t.Fatalf("stored error = %q %q %q", outcome, code, message)
	}
	_, err := s.Update(ctx, "missing-update", "tsk_missing", "text")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing update = %v", err)
	}
	_, err = s.Update(ctx, "missing-update", "tsk_missing", "text")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing update replay = %v", err)
	}
}

func TestListPaginationUsesStableCursor(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	for _, id := range []string{"page-one", "page-two", "page-three"} {
		if _, err := s.Create(ctx, id, id); err != nil {
			t.Fatal(err)
		}
	}
	first, cursor, err := s.List(ctx, nil, true, 2, "")
	if err != nil || len(first) != 2 || cursor == "" {
		t.Fatalf("first page = %#v cursor=%q err=%v", first, cursor, err)
	}
	second, next, err := s.List(ctx, nil, true, 2, cursor)
	if err != nil || len(second) != 1 || next != "" || second[0].ID == first[0].ID || second[0].ID == first[1].ID {
		t.Fatalf("second page = %#v next=%q err=%v", second, next, err)
	}
	if _, _, err := s.List(ctx, nil, true, 2, "not-a-cursor"); err == nil {
		t.Fatal("accepted invalid cursor")
	}
}

func TestLegacyPrefixedTaskIDsRemainResolvableAndPageable(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const legacyID = "tsk_abcdefghijklmnopqrstuv"
	if _, err := s.DB().ExecContext(ctx, "INSERT INTO tasks(task_id,version,state,text,source_kind,created_at,updated_at) VALUES(?,1,'open','preserved','slack',1,1)", legacyID); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ctx, legacyID)
	if err != nil || got.ID != legacyID {
		t.Fatalf("Get legacy task = %#v, %v", got, err)
	}
	if _, _, err := s.List(ctx, nil, true, 1, legacyID); err != nil {
		t.Fatalf("List with legacy cursor: %v", err)
	}
}

func TestListCursorExpiredAndTrustCursorSurvivesRemoval(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	created, err := s.Create(ctx, "cursor-expired", "task")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.List(ctx, nil, true, 1, created.TaskID); err != nil {
		t.Fatalf("present cursor: %v", err)
	}
	if _, err := s.DB().Exec("DELETE FROM tasks WHERE task_id=?", created.TaskID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.List(ctx, nil, true, 1, created.TaskID); !errors.Is(err, ErrCursorExpired) {
		t.Fatalf("expired cursor = %v", err)
	}
	for _, id := range []string{"U00000001", "U00000002", "U00000003"} {
		if _, err := s.AddTrustedUser(ctx, "trust-"+id, id); err != nil {
			t.Fatal(err)
		}
	}
	users, next, err := s.ListTrustedUsersPage(ctx, 2, "")
	if err != nil || len(users) != 2 || next == "" {
		t.Fatalf("trust first = %#v %q %v", users, next, err)
	}
	if _, err := s.RemoveTrustedUser(ctx, "remove-anchor", next); err != nil {
		t.Fatal(err)
	}
	users, next, err = s.ListTrustedUsersPage(ctx, 2, next)
	if err != nil || len(users) != 1 || next != "" {
		t.Fatalf("trust resume = %#v %q %v", users, next, err)
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

func initialSchemaDatabase(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(string(migration0001)); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		"PRAGMA application_id=1296257072", // ApplicationID cannot be a SQL parameter.
		"PRAGMA user_version=1",
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec("INSERT INTO schema_migrations(version,checksum,applied_at) VALUES(1,?,1)", Checksum()); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestOpenMigrationFailureRollsBackV1Upgrade(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ledger.sqlite3")
	db := initialSchemaDatabase(t, path)
	if _, err := db.Exec("INSERT INTO tasks(task_id,version,state,text,source_kind,created_at,updated_at) VALUES('tsk_before_failed_upgrade',1,'open','preserved','local',1,1)"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	breakMigration3(t)
	if opened, err := Open(ctx, path, Options{}); err == nil {
		_ = opened.Close()
		t.Fatal("upgrade with failing v3 migration succeeded")
	}
	assertFailedUpgradeRolledBack(t, path, 1, false)
}

func breakMigration3(t *testing.T) {
	t.Helper()
	original := migration0003
	migration0003 = append(bytes.Clone(original), []byte("\nCREATE TABLE migration_failure_probe(value INTEGER);\nSELECT FROM;\n")...)
	t.Cleanup(func() { migration0003 = original })
}

func TestOpenMigrationFailureRollsBackV3ToV4Upgrade(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ledger.sqlite3")
	db := initialSchemaDatabase(t, path)
	if _, err := db.Exec(string(migration0002)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(string(migration0003)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO schema_migrations(version,checksum,applied_at) VALUES(2,?,2),(3,?,3)", MigrationChecksum(2), MigrationChecksum(3)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("PRAGMA user_version=3"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO tasks(task_id,version,state,text,source_kind,created_at,updated_at) VALUES('tsk_before_v4_failure',1,'open','preserved','local',1,1)"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	original := migration0004
	migration0004 = append(bytes.Clone(original), []byte("\nCREATE TABLE migration_failure_probe(value INTEGER);\nSELECT FROM;\n")...)
	t.Cleanup(func() { migration0004 = original })
	if opened, err := Open(ctx, path, Options{}); err == nil {
		_ = opened.Close()
		t.Fatal("upgrade with failing v4 migration succeeded")
	}
	check, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	var version, migrations, deliveries int
	if err := check.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := check.QueryRow("SELECT count(*) FROM schema_migrations").Scan(&migrations); err != nil {
		t.Fatal(err)
	}
	if err := check.QueryRow("SELECT count(*) FROM sqlite_master WHERE name='pi_deliveries'").Scan(&deliveries); err != nil {
		t.Fatal(err)
	}
	if version != 3 || migrations != 3 || deliveries != 0 {
		t.Fatalf("failed v4 upgrade version=%d migrations=%d deliveries=%d", version, migrations, deliveries)
	}
}

func assertFailedUpgradeRolledBack(t *testing.T, path string, wantVersion int, wantTrustedUsers bool) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version, migrations, taskCount int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT count(*) FROM schema_migrations").Scan(&migrations); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT count(*) FROM tasks").Scan(&taskCount); err != nil {
		t.Fatal(err)
	}
	if version != wantVersion || migrations != wantVersion || taskCount != 1 {
		t.Fatalf("failed upgrade changed ledger: version=%d migrations=%d tasks=%d", version, migrations, taskCount)
	}
	var trustedUsers, slackObjects, failureProbe int
	if err := db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name='trusted_users'").Scan(&trustedUsers); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT count(*) FROM sqlite_master WHERE name IN ('slack_source_events','slack_task_sources','slack_effects','files')").Scan(&slackObjects); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT count(*) FROM sqlite_master WHERE name='migration_failure_probe'").Scan(&failureProbe); err != nil {
		t.Fatal(err)
	}
	wantTrustTable := 0
	if wantTrustedUsers {
		wantTrustTable = 1
	}
	if trustedUsers != wantTrustTable || slackObjects != 0 || failureProbe != 0 {
		t.Fatalf("failed upgrade leaked objects: trust=%d Slack=%d probe=%d", trustedUsers, slackObjects, failureProbe)
	}
}

func TestOpenAtomicallyUpgradesIntactV1ThroughV2ToV3AndPreservesData(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ledger.sqlite3")
	db := initialSchemaDatabase(t, path)
	if _, err := db.Exec("INSERT INTO tasks(task_id,version,state,text,source_kind,created_at,updated_at,done_at) VALUES('tsk_preserved',1,'open','preserved task','local',1,1,NULL)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO mutation_requests(request_id,operation,input_hash,outcome,result_type,result_json,created_at,completed_at,expiry_eligible_at) VALUES('preserved-request','task.create',?,'success','task_mutation','{}',1,1,1)", hash("preserved")); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	var version, migrations, requests, slackTables int
	if err := s.DB().QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := s.DB().QueryRow("SELECT count(*) FROM schema_migrations").Scan(&migrations); err != nil {
		t.Fatal(err)
	}
	if err := s.DB().QueryRow("SELECT count(*) FROM mutation_requests WHERE request_id='preserved-request'").Scan(&requests); err != nil {
		t.Fatal(err)
	}
	if err := s.DB().QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name IN ('slack_task_sources','slack_source_events','slack_effects','files')").Scan(&slackTables); err != nil {
		t.Fatal(err)
	}
	if version != SchemaVersion || migrations != SchemaVersion || requests != 1 || slackTables != 4 {
		t.Fatalf("upgrade state version=%d migrations=%d requests=%d Slack tables=%d", version, migrations, requests, slackTables)
	}
	tasks, _, err := s.List(ctx, nil, true, 10, "")
	if err != nil || len(tasks) != 1 || tasks[0].Text != "preserved task" {
		t.Fatalf("preserved tasks=%#v err=%v", tasks, err)
	}
	if users, err := s.ListTrustedUsers(ctx); err != nil || len(users) != 0 {
		t.Fatalf("initial trusted users=%v err=%v", users, err)
	}
	if _, err := s.AddTrustedUser(ctx, "upgrade-trust", "U12345678"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, path, Options{})
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	defer s.Close()
	if users, err := s.ListTrustedUsers(ctx); err != nil || len(users) != 1 || users[0] != "U12345678" {
		t.Fatalf("restart trusted users=%v err=%v", users, err)
	}
}

func TestOpenRejectsUnrecognizedSchemasWithoutChangingThem(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ledger.sqlite3")
	db := initialSchemaDatabase(t, path)
	if _, err := db.Exec("UPDATE schema_migrations SET checksum='bad'"); err == nil {
		t.Fatal("accepted invalid checksum")
	}
	if _, err := db.Exec("UPDATE schema_migrations SET checksum=?", strings.Repeat("0", 64)); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(ctx, path, Options{}); !errors.Is(err, ErrIncompatibleSchema) {
		t.Fatalf("Open checksum mismatch: %v", err)
	}
	check, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	var version, migrations int
	if err := check.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := check.QueryRow("SELECT count(*) FROM schema_migrations").Scan(&migrations); err != nil {
		t.Fatal(err)
	}
	if version != 1 || migrations != 1 {
		t.Fatalf("rejected ledger changed: version=%d migrations=%d", version, migrations)
	}
}

func TestOpenRejectsUnknownAndNewerSchemas(t *testing.T) {
	ctx := context.Background()
	t.Run("unknown object", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "ledger.sqlite3")
		db := initialSchemaDatabase(t, path)
		if _, err := db.Exec("CREATE TABLE unknown_schema_object (id INTEGER)"); err != nil {
			t.Fatal(err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(ctx, path, Options{}); !errors.Is(err, ErrIncompatibleSchema) {
			t.Fatalf("Open unknown schema: %v", err)
		}
	})
	t.Run("newer version", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "ledger.sqlite3")
		s, err := Initialize(ctx, path, Options{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.DB().Exec("PRAGMA user_version=8"); err != nil {
			t.Fatal(err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(ctx, path, Options{}); !errors.Is(err, ErrIncompatibleSchema) {
			t.Fatalf("Open newer schema: %v", err)
		}
	})
}

func TestTrustedUsersMutationReplayConflictAndIdempotency(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if trusted, err := s.IsTrustedUser(ctx, "U12345678"); err != nil || trusted {
		t.Fatalf("empty authority=%t err=%v", trusted, err)
	}
	added, err := s.AddTrustedUser(ctx, "trust-add", "U12345678")
	if err != nil || added != (TrustMutationResult{SlackUserID: "U12345678", Trusted: true, Changed: true}) {
		t.Fatalf("add=%+v err=%v", added, err)
	}
	if trusted, err := s.IsTrustedUser(ctx, "U12345678"); err != nil || !trusted {
		t.Fatalf("committed add authority=%t err=%v", trusted, err)
	}
	replay, err := s.AddTrustedUser(ctx, "trust-add", "U12345678")
	if err != nil || replay != added {
		t.Fatalf("add replay=%+v err=%v", replay, err)
	}
	if _, err := s.AddTrustedUser(ctx, "trust-add", "U87654321"); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("add conflict=%v", err)
	}
	existing, err := s.AddTrustedUser(ctx, "trust-existing", "U12345678")
	if err != nil || existing.Changed || !existing.Trusted {
		t.Fatalf("add existing=%+v err=%v", existing, err)
	}
	removed, err := s.RemoveTrustedUser(ctx, "trust-remove", "U12345678")
	if err != nil || removed != (TrustMutationResult{SlackUserID: "U12345678", Trusted: false, Changed: true}) {
		t.Fatalf("remove=%+v err=%v", removed, err)
	}
	if trusted, err := s.IsTrustedUser(ctx, "U12345678"); err != nil || trusted {
		t.Fatalf("committed removal authority=%t err=%v", trusted, err)
	}
	absent, err := s.RemoveTrustedUser(ctx, "trust-absent", "U12345678")
	if err != nil || absent.Changed || absent.Trusted {
		t.Fatalf("remove absent=%+v err=%v", absent, err)
	}
	if users, err := s.ListTrustedUsers(ctx); err != nil || len(users) != 0 {
		t.Fatalf("zero trusted users=%v err=%v", users, err)
	}
	if _, err := s.AddTrustedUser(ctx, "bad-trust", "B12345678"); err == nil {
		t.Fatal("accepted bot ID as trusted user")
	}
	if _, err := s.AddTrustedUser(ctx, "bad-trust", "B12345678"); err == nil {
		t.Fatal("invalid ID replay did not preserve error")
	}
}

func TestTrustedUsersAreSortedAndSerializedWithTaskWrites(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	ids := []string{"U99999999", "W12345678", "U12345678"}
	var wg sync.WaitGroup
	errs := make(chan error, len(ids)*2)
	for i, userID := range ids {
		wg.Add(2)
		go func(i int, userID string) {
			defer wg.Done()
			_, err := s.AddTrustedUser(ctx, "trust-concurrent-"+strconv.Itoa(i), userID)
			errs <- err
		}(i, userID)
		go func(i int) {
			defer wg.Done()
			_, err := s.Create(ctx, "task-concurrent-"+strconv.Itoa(i), "task")
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	users, err := s.ListTrustedUsers(ctx)
	if err != nil || !slices.Equal(users, []string{"U12345678", "U99999999", "W12345678"}) {
		t.Fatalf("trusted users=%v err=%v", users, err)
	}
	var tasks int
	if err := s.DB().QueryRow("SELECT count(*) FROM tasks").Scan(&tasks); err != nil || tasks != len(ids) {
		t.Fatalf("tasks=%d err=%v", tasks, err)
	}
}

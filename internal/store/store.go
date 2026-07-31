// Package store owns Mosscap's SQLite ledger and durable mutation deduplication.
package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/stayradiated/mosscap/internal/clock"
	"github.com/stayradiated/mosscap/internal/doctor"
	"github.com/stayradiated/mosscap/internal/identity"
	"github.com/stayradiated/mosscap/internal/status"
	"github.com/stayradiated/mosscap/internal/task"
	_ "modernc.org/sqlite"
)

const (
	SchemaVersion      = 1
	ApplicationID      = 0x4d435030 // "MCP0"
	RequestRetention   = 30 * 24 * time.Hour
	defaultBusyTimeout = 5 * time.Second
	migrationFilename  = "0001_phase0.sql"
)

//go:embed migrations/0001_phase0.sql
var migration0001 []byte

var (
	ErrIncompatibleSchema = errors.New("incompatible SQLite schema")
	ErrRequestConflict    = errors.New("request ID conflicts with prior request")
	ErrNotFound           = errors.New("task not found")
	ErrAmbiguous          = errors.New("task selector is ambiguous")
)

type Options struct {
	BusyTimeout time.Duration
	Clock       clock.Clock
	IDs         identity.Generator
}

type Store struct {
	db    *sql.DB
	mu    sync.Mutex
	clock clock.Clock
	ids   identity.Generator
}

type Task struct {
	ID, Text, SourceKind string
	Version              int64
	State                task.State
	CreatedAt, UpdatedAt time.Time
	DoneAt               *time.Time
}

type MutationResult struct {
	TaskID    string     `json:"task_id"`
	Version   int64      `json:"version"`
	State     task.State `json:"state"`
	Changed   bool       `json:"changed"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
}

type StateEvent struct {
	ID, TaskID, Actor, RequestID string
	PriorState, NewState         task.State
	Note                         *string
	CommittedAt                  time.Time
}

type request struct{ operation, hash string }

func Checksum() string { h := sha256.Sum256(migration0001); return hex.EncodeToString(h[:]) }

// Initialize creates (if needed) and migrates only a new or empty database.
func Initialize(ctx context.Context, path string, opt Options) (*Store, error) {
	created, err := precreate(path)
	if err != nil {
		return nil, err
	}
	s, err := open(ctx, path, opt)
	if err != nil {
		return nil, err
	}
	if err := s.initialize(ctx, created); err != nil {
		_ = s.Close()
		return nil, err
	}
	return s, nil
}

// Open verifies an already initialized database; it never migrates it.
func Open(ctx context.Context, path string, opt Options) (*Store, error) {
	s, err := open(ctx, path, opt)
	if err != nil {
		return nil, err
	}
	if err := s.compatible(ctx); err != nil {
		_ = s.Close()
		return nil, err
	}
	return s, nil
}

func precreate(path string) (bool, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return false, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
	if err == nil {
		return true, f.Close()
	}
	if errors.Is(err, os.ErrExist) {
		return false, nil
	}
	return false, err
}
func open(ctx context.Context, path string, opt Options) (*Store, error) {
	if opt.BusyTimeout == 0 {
		opt.BusyTimeout = defaultBusyTimeout
	}
	if opt.BusyTimeout < 0 {
		return nil, errors.New("negative busy timeout")
	}
	if opt.Clock == nil {
		opt.Clock = clock.System{}
	}
	if opt.IDs.IsZero() {
		opt.IDs = identity.NewGenerator(nil)
	}
	dsn := "file:" + filepath.ToSlash(path) + "?_pragma=foreign_keys(ON)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(" + fmt.Sprint(opt.BusyTimeout.Milliseconds()) + ")"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	s := &Store{db: db, clock: opt.Clock, ids: opt.IDs}
	if err := s.pragmas(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}
func (s *Store) Close() error { return s.db.Close() }
func (s *Store) DB() *sql.DB  { return s.db } // for diagnostics and store tests.

// Counts returns current inventory without task contents.
func (s *Store) Counts(ctx context.Context) (status.Counts, error) {
	var c status.Counts
	if err := s.db.QueryRowContext(ctx, "SELECT count(*) FILTER (WHERE state='open'), count(*) FILTER (WHERE state='working'), count(*) FILTER (WHERE state='done') FROM tasks").Scan(&c.Open, &c.Working, &c.Done); err != nil {
		return c, err
	}
	err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM mutation_requests").Scan(&c.RequestRecords)
	return c, err
}

// Doctor runs read-only verification plus a deliberately rolled-back write
// probe. The probe only creates a temporary table, then rolls back, and cannot
// create durable domain rows.
func (s *Store) Doctor(ctx context.Context) []doctor.Check {
	checks := make([]doctor.Check, 0, 4)
	var integrity string
	if err := s.db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity); err != nil || integrity != "ok" {
		checks = append(checks, doctor.Check{Name: "sqlite_integrity", Status: doctor.Fail, Message: "SQLite integrity check failed", Action: "restore a known-good ledger; do not auto-repair"})
	} else {
		checks = append(checks, doctor.Check{Name: "sqlite_integrity", Status: doctor.Pass, Message: "SQLite integrity check passed"})
	}
	if err := s.pragmas(ctx); err != nil {
		checks = append(checks, doctor.Check{Name: "sqlite_pragmas", Status: doctor.Fail, Message: "SQLite required pragmas are not active", Action: "stop the gateway and inspect the ledger filesystem"})
	} else {
		checks = append(checks, doctor.Check{Name: "sqlite_pragmas", Status: doctor.Pass, Message: "SQLite required pragmas are active"})
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err == nil {
		_, err = tx.ExecContext(ctx, "CREATE TEMP TABLE mosscap_doctor_probe (value INTEGER)")
		if err == nil {
			_, err = tx.ExecContext(ctx, "INSERT INTO mosscap_doctor_probe(value) VALUES(1)")
		}
		rollErr := tx.Rollback()
		if err == nil {
			err = rollErr
		}
	}
	if err != nil {
		checks = append(checks, doctor.Check{Name: "sqlite_rollback_write", Status: doctor.Fail, Message: "SQLite rolled-back write probe failed", Action: "check free space and ledger write permissions"})
	} else {
		checks = append(checks, doctor.Check{Name: "sqlite_rollback_write", Status: doctor.Pass, Message: "SQLite rolled-back write probe passed"})
	}
	if err := s.compatible(ctx); err != nil {
		checks = append(checks, doctor.Check{Name: "schema_compatibility", Status: doctor.Fail, Message: "ledger schema is incompatible", Action: "run the matching Mosscap version or restore a compatible ledger"})
	} else {
		checks = append(checks, doctor.Check{Name: "schema_compatibility", Status: doctor.Pass, Message: "ledger schema is compatible"})
	}
	return checks
}

func (s *Store) pragmas(ctx context.Context) error {
	for i := 0; i < 4; i++ {
		conn, err := s.db.Conn(ctx)
		if err != nil {
			return err
		}
		var foreign int
		var journal string
		err = conn.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&foreign)
		if err == nil {
			err = conn.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journal)
		}
		conn.Close()
		if err != nil {
			return err
		}
		if foreign != 1 || !strings.EqualFold(journal, "wal") {
			return errors.New("SQLite pragma verification failed")
		}
	}
	return nil
}
func (s *Store) initialize(ctx context.Context, created bool) error {
	var count int
	if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type='table'").Scan(&count); err != nil {
		return err
	}
	if !created && count != 0 {
		return fmt.Errorf("%w: setup only migrates an empty database", ErrIncompatibleSchema)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, string(migration0001)); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, fmt.Sprintf("PRAGMA application_id=%d", ApplicationID)); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "PRAGMA user_version=1"); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO schema_migrations(version, checksum, applied_at) VALUES(1, ?, ?)", Checksum(), s.now()); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) compatible(ctx context.Context) error {
	var app, version int
	if err := s.db.QueryRowContext(ctx, "PRAGMA application_id").Scan(&app); err != nil {
		return err
	}
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if app != ApplicationID || version != SchemaVersion {
		return fmt.Errorf("%w: application_id or version", ErrIncompatibleSchema)
	}
	var migrationVersion, n int
	var checksum string
	if err := s.db.QueryRowContext(ctx, "SELECT count(*), coalesce(max(version), 0), coalesce(max(checksum), '') FROM schema_migrations").Scan(&n, &migrationVersion, &checksum); err != nil {
		return fmt.Errorf("%w: migration table: %v", ErrIncompatibleSchema, err)
	}
	if n != 1 || migrationVersion != SchemaVersion || checksum != Checksum() {
		return fmt.Errorf("%w: migration history", ErrIncompatibleSchema)
	}
	var check string
	if err := s.db.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&check); err != nil || check != "ok" {
		return fmt.Errorf("%w: quick_check", ErrIncompatibleSchema)
	}
	return nil
}
func (s *Store) now() int64 { return clock.UnixMicro(s.clock.Now()) }
func hash(operation string, values ...string) string {
	h := sha256.New()
	_, _ = h.Write([]byte(operation))
	for _, v := range values {
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(v))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (s *Store) Create(ctx context.Context, requestID, text string) (MutationResult, error) {
	text, err := task.NormalizeText(text)
	if err != nil {
		return MutationResult{}, err
	}
	return s.mutate(ctx, request{"task.create", hash("task.create", text)}, requestID, func(tx *sql.Tx, now int64) (MutationResult, error) {
		id, err := s.ids.NewTaskID()
		if err != nil {
			return MutationResult{}, err
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO tasks(task_id,version,state,text,source_kind,created_at,updated_at,done_at) VALUES(?,1,'open',?,'local',?,?,NULL)", id, text, now, now); err != nil {
			return MutationResult{}, err
		}
		return result(id, 1, task.Open, true, now, now), nil
	})
}
func (s *Store) Update(ctx context.Context, requestID, selector, text string) (MutationResult, error) {
	text, err := task.NormalizeText(text)
	if err != nil {
		return MutationResult{}, err
	}
	return s.mutate(ctx, request{"task.update", hash("task.update", selector, text)}, requestID, func(tx *sql.Tx, now int64) (MutationResult, error) {
		t, err := getTx(ctx, tx, selector)
		if err != nil {
			return MutationResult{}, err
		}
		if t.Text == text {
			return resultTask(t, false), nil
		}
		if _, err = tx.ExecContext(ctx, "UPDATE tasks SET text=?,version=version+1,updated_at=? WHERE task_id=?", text, now, t.ID); err != nil {
			return MutationResult{}, err
		}
		return result(t.ID, t.Version+1, t.State, true, unix(t.CreatedAt), now), nil
	})
}
func (s *Store) Transition(ctx context.Context, requestID, selector string, target task.State, note *string) (MutationResult, error) {
	if !target.Valid() {
		return MutationResult{}, errors.New("invalid target state")
	}
	if note != nil && len(*note) > 4096 {
		return MutationResult{}, errors.New("note exceeds 4096 characters")
	}
	return s.mutate(ctx, request{"task.transition", hash("task.transition", selector, string(target), value(note))}, requestID, func(tx *sql.Tx, now int64) (MutationResult, error) {
		t, err := getTx(ctx, tx, selector)
		if err != nil {
			return MutationResult{}, err
		}
		if t.State == target {
			return resultTask(t, false), nil
		}
		id, err := s.ids.NewStateEventID()
		if err != nil {
			return MutationResult{}, err
		}
		var done any
		if target == task.Done {
			done = now
		}
		if _, err = tx.ExecContext(ctx, "UPDATE tasks SET state=?,updated_at=?,done_at=? WHERE task_id=?", target, now, done, t.ID); err != nil {
			return MutationResult{}, err
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO task_state_events(event_id,task_id,prior_state,new_state,actor,note,request_id,committed_at) VALUES(?,?,?,?,?,?,?,?)", id, t.ID, t.State, target, "local", note, requestID, now); err != nil {
			return MutationResult{}, err
		}
		return result(t.ID, t.Version, target, true, unix(t.CreatedAt), now), nil
	})
}
func deterministicCode(err error) (string, bool) {
	if errors.Is(err, ErrNotFound) {
		return "not_found", true
	}
	if errors.Is(err, ErrAmbiguous) {
		return "ambiguous", true
	}
	return "", false
}
func errorForCode(code string) error {
	switch code {
	case "not_found":
		return ErrNotFound
	case "ambiguous":
		return ErrAmbiguous
	default:
		return errors.New("prior mutation error")
	}
}
func value(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}
func (s *Store) mutate(ctx context.Context, req request, requestID string, fn func(*sql.Tx, int64) (MutationResult, error)) (MutationResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !identity.ValidRequestID(requestID) {
		return MutationResult{}, errors.New("invalid request ID")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return MutationResult{}, err
	}
	defer tx.Rollback()
	var operation, inputHash, outcome, resultJSON, errorCode string
	err = tx.QueryRowContext(ctx, "SELECT operation,input_hash,outcome,coalesce(result_json,''),coalesce(error_code,'') FROM mutation_requests WHERE request_id=?", requestID).Scan(&operation, &inputHash, &outcome, &resultJSON, &errorCode)
	if err == nil {
		if operation != req.operation || inputHash != req.hash {
			return MutationResult{}, ErrRequestConflict
		}
		if outcome != "success" {
			return MutationResult{}, errorForCode(errorCode)
		}
		var r MutationResult
		if err := json.Unmarshal([]byte(resultJSON), &r); err != nil {
			return MutationResult{}, err
		}
		return r, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return MutationResult{}, err
	}
	now := s.now()
	r, err := fn(tx, now)
	if err != nil {
		if code, ok := deterministicCode(err); ok {
			expiry := now + RequestRetention.Microseconds()
			if _, insertErr := tx.ExecContext(ctx, "INSERT INTO mutation_requests(request_id,operation,input_hash,outcome,error_code,error_message,created_at,completed_at,expiry_eligible_at) VALUES(?,?,?,'error',?,?,?, ?,?)", requestID, req.operation, req.hash, code, err.Error(), now, now, expiry); insertErr != nil {
				return MutationResult{}, insertErr
			}
			if commitErr := tx.Commit(); commitErr != nil {
				return MutationResult{}, commitErr
			}
		}
		return MutationResult{}, err
	}
	b, err := json.Marshal(r)
	if err != nil {
		return MutationResult{}, err
	}
	expiry := now + RequestRetention.Microseconds()
	if _, err = tx.ExecContext(ctx, "INSERT INTO mutation_requests(request_id,operation,input_hash,outcome,result_type,result_json,created_at,completed_at,expiry_eligible_at) VALUES(?,?,?,'success','task_mutation',?,?,?,?)", requestID, req.operation, req.hash, string(b), now, now, expiry); err != nil {
		return MutationResult{}, err
	}
	if err = tx.Commit(); err != nil {
		return MutationResult{}, err
	}
	return r, nil
}
func (s *Store) Get(ctx context.Context, selector string) (Task, error) {
	return getTx(ctx, s.db, selector)
}

// List returns current tasks in stable updated-at/task-id order. By default done
// tasks are excluded; an explicit state always takes precedence.
func (s *Store) List(ctx context.Context, stateFilter *string, includeDone bool, limit int) ([]Task, error) {
	if limit == 0 {
		limit = 100
	}
	if limit < 1 || limit > 1000 {
		return nil, errors.New("invalid task list limit")
	}
	q := "SELECT task_id,version,state,text,source_kind,created_at,updated_at,done_at FROM tasks"
	args := []any{}
	if stateFilter != nil {
		q += " WHERE state=?"
		args = append(args, *stateFilter)
	} else if !includeDone {
		q += " WHERE state IN ('open','working')"
	}
	q += " ORDER BY updated_at,task_id LIMIT ?"
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tasks []Task
	for rows.Next() {
		var t Task
		var created, updated int64
		var done sql.NullInt64
		if err := rows.Scan(&t.ID, &t.Version, &t.State, &t.Text, &t.SourceKind, &created, &updated, &done); err != nil {
			return nil, err
		}
		t.CreatedAt = time.UnixMicro(created).UTC()
		t.UpdatedAt = time.UnixMicro(updated).UTC()
		if done.Valid {
			d := time.UnixMicro(done.Int64).UTC()
			t.DoneAt = &d
		}
		tasks = append(tasks, t)
	}
	return tasks, rows.Err()
}

type queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func getTx(ctx context.Context, q queryer, selector string) (Task, error) {
	if !strings.HasPrefix(selector, "tsk_") {
		return Task{}, ErrNotFound
	}
	rows, err := q.QueryContext(ctx, "SELECT task_id,version,state,text,source_kind,created_at,updated_at,done_at FROM tasks WHERE task_id=? OR task_id LIKE ?", selector, selector+"%")
	if err != nil {
		return Task{}, err
	}
	defer rows.Close()
	var got []Task
	for rows.Next() {
		var t Task
		var created, updated int64
		var done sql.NullInt64
		if err := rows.Scan(&t.ID, &t.Version, &t.State, &t.Text, &t.SourceKind, &created, &updated, &done); err != nil {
			return Task{}, err
		}
		t.CreatedAt = time.UnixMicro(created).UTC()
		t.UpdatedAt = time.UnixMicro(updated).UTC()
		if done.Valid {
			x := time.UnixMicro(done.Int64).UTC()
			t.DoneAt = &x
		}
		got = append(got, t)
	}
	if err := rows.Err(); err != nil {
		return Task{}, err
	}
	if len(got) == 0 {
		return Task{}, ErrNotFound
	}
	if len(got) > 1 {
		return Task{}, ErrAmbiguous
	}
	return got[0], nil
}
func result(id string, version int64, state task.State, changed bool, created, updated int64) MutationResult {
	return MutationResult{TaskID: id, Version: version, State: state, Changed: changed, CreatedAt: time.UnixMicro(created).UTC(), UpdatedAt: time.UnixMicro(updated).UTC()}
}
func resultTask(t Task, changed bool) MutationResult {
	return MutationResult{TaskID: t.ID, Version: t.Version, State: t.State, Changed: changed, CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt}
}
func unix(t time.Time) int64 { return t.UnixMicro() }

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
	"net/url"
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
	SchemaVersion      = 8
	ApplicationID      = 0x4d435030 // "MCP0"
	RequestRetention   = 30 * 24 * time.Hour
	defaultBusyTimeout = 5 * time.Second
)

//go:embed migrations/0001_initial.sql
var migration0001 []byte

//go:embed migrations/0002_trusted_users.sql
var migration0002 []byte

//go:embed migrations/0003_slack.sql
var migration0003 []byte

//go:embed migrations/0004_pi_delivery.sql
var migration0004 []byte

//go:embed migrations/0005_file_transfer.sql
var migration0005 []byte

//go:embed migrations/0006_prefix_free_ids.sql
var migration0006 []byte

//go:embed migrations/0007_task_changes.sql
var migration0007 []byte

//go:embed migrations/0008_drop_pi_delivery.sql
var migration0008 []byte

var (
	ErrIncompatibleSchema = errors.New("incompatible SQLite schema")
	ErrRequestConflict    = errors.New("request ID conflicts with prior request")
	ErrNotFound           = errors.New("task not found")
	ErrAmbiguous          = errors.New("task selector is ambiguous")
	// ErrCursorExpired means a syntactically valid immutable cursor anchor was
	// removed (normally by metadata purge). Restart the listing without it.
	ErrCursorExpired = errors.New("list cursor has expired; restart without --cursor")
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

	// purgeDeleteHook is an unexported deterministic test seam. It is called
	// after each successful purge DELETE, while the containing transaction is
	// still open, so rollback/error boundaries can be proved without altering
	// production behavior.
	purgeDeleteHook func() error
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

// TrustMutationResult is the current authorization state after a trust mutation.
// It intentionally contains no trust-change history.
type TrustMutationResult struct {
	SlackUserID string `json:"slack_user_id"`
	Trusted     bool   `json:"trusted"`
	Changed     bool   `json:"changed"`
}

type StateEvent struct {
	ID, TaskID, Actor, RequestID string
	PriorState, NewState         task.State
	Note                         *string
	CommittedAt                  time.Time
}

type request struct{ operation, hash string }

type mutationError struct {
	code    string
	message string
}

func (e *mutationError) Error() string { return e.message }

func invalidInput(message string) error {
	return &mutationError{code: "invalid_input", message: message}
}

// Checksum returns the initial migration checksum for compatibility with v1 callers.
func Checksum() string { return migrationChecksum(1) }

// MigrationChecksum returns the checksum for a known embedded migration.
func MigrationChecksum(version int) string { return migrationChecksum(version) }

func migrationChecksum(version int) string {
	var migration []byte
	switch version {
	case 1:
		migration = migration0001
	case 2:
		migration = migration0002
	case 3:
		migration = migration0003
	case 4:
		migration = migration0004
	case 5:
		migration = migration0005
	case 6:
		migration = migration0006
	case 7:
		migration = migration0007
	case 8:
		migration = migration0008
	default:
		return ""
	}
	h := sha256.Sum256(migration)
	return hex.EncodeToString(h[:])
}

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

// Open verifies an initialized database and upgrades only an intact older ledger.
// A direct v1 upgrade applies v2 and v3 atomically.
func Open(ctx context.Context, path string, opt Options) (*Store, error) {
	s, err := open(ctx, path, opt)
	if err != nil {
		return nil, err
	}
	if err := s.openOrUpgrade(ctx); err != nil {
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
	db, err := sql.Open("sqlite", sqliteDSN(path, opt.BusyTimeout))
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

// sqliteDSN encodes path as a URI path, so URI delimiters in a configured
// filesystem name cannot alter SQLite's connection options.
func sqliteDSN(path string, busyTimeout time.Duration) string {
	u := &url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
	q := url.Values{}
	q.Add("_pragma", "foreign_keys(ON)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(NORMAL)")
	q.Add("_pragma", "busy_timeout("+fmt.Sprint(busyTimeout.Milliseconds())+")")
	u.RawQuery = q.Encode()
	return u.String()
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
	for _, migration := range []struct {
		version int
		sql     []byte
	}{{1, migration0001}, {2, migration0002}, {3, migration0003}, {4, migration0004}, {5, migration0005}, {6, migration0006}, {7, migration0007}, {8, migration0008}} {
		if _, err = tx.ExecContext(ctx, string(migration.sql)); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO schema_migrations(version, checksum, applied_at) VALUES(?, ?, ?)", migration.version, migrationChecksum(migration.version), s.now()); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, fmt.Sprintf("PRAGMA application_id=%d", ApplicationID)); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version=%d", SchemaVersion)); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) openOrUpgrade(ctx context.Context) error {
	if err := s.compatible(ctx); err == nil {
		return nil
	}
	for version := SchemaVersion - 1; version >= 1; version-- {
		if err := s.compatibleVersion(ctx, version); err == nil {
			return s.upgrade(ctx, version)
		}
	}
	return fmt.Errorf("%w: application_id, version, or migration history", ErrIncompatibleSchema)
}

func (s *Store) upgrade(ctx context.Context, fromVersion int) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := compatibleVersion(ctx, tx, fromVersion); err != nil {
		return err
	}
	migrations := map[int][]byte{2: migration0002, 3: migration0003, 4: migration0004, 5: migration0005, 6: migration0006, 7: migration0007, 8: migration0008}
	for version := fromVersion + 1; version <= SchemaVersion; version++ {
		migration, ok := migrations[version]
		if !ok {
			return fmt.Errorf("%w: missing migration %d", ErrIncompatibleSchema, version)
		}
		if _, err := tx.ExecContext(ctx, string(migration)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO schema_migrations(version, checksum, applied_at) VALUES(?, ?, ?)", version, migrationChecksum(version), s.now()); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version=%d", version)); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return s.compatible(ctx)
}

func (s *Store) compatible(ctx context.Context) error {
	return s.compatibleVersion(ctx, SchemaVersion)
}

type rowQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func (s *Store) compatibleVersion(ctx context.Context, version int) error {
	return compatibleVersion(ctx, s.db, version)
}

func compatibleVersion(ctx context.Context, q rowQueryer, version int) error {
	var app, actualVersion int
	if err := q.QueryRowContext(ctx, "PRAGMA application_id").Scan(&app); err != nil {
		return fmt.Errorf("%w: application_id: %v", ErrIncompatibleSchema, err)
	}
	if err := q.QueryRowContext(ctx, "PRAGMA user_version").Scan(&actualVersion); err != nil {
		return fmt.Errorf("%w: user_version: %v", ErrIncompatibleSchema, err)
	}
	if app != ApplicationID || actualVersion != version {
		return fmt.Errorf("%w: application_id or version", ErrIncompatibleSchema)
	}
	rows, err := q.QueryContext(ctx, "SELECT version, checksum FROM schema_migrations ORDER BY version")
	if err != nil {
		return fmt.Errorf("%w: migration table: %v", ErrIncompatibleSchema, err)
	}
	for expected := 1; expected <= version; expected++ {
		if !rows.Next() {
			_ = rows.Close()
			return fmt.Errorf("%w: migration history", ErrIncompatibleSchema)
		}
		var got int
		var checksum string
		if err := rows.Scan(&got, &checksum); err != nil || got != expected || checksum != migrationChecksum(expected) {
			_ = rows.Close()
			return fmt.Errorf("%w: migration history", ErrIncompatibleSchema)
		}
	}
	if rows.Next() || rows.Err() != nil {
		_ = rows.Close()
		return fmt.Errorf("%w: migration history", ErrIncompatibleSchema)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("%w: migration history", ErrIncompatibleSchema)
	}
	if err := expectedSchemaObjects(ctx, q, version); err != nil {
		return err
	}
	var check string
	if err := q.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&check); err != nil || check != "ok" {
		return fmt.Errorf("%w: quick_check", ErrIncompatibleSchema)
	}
	return nil
}

func expectedSchemaObjects(ctx context.Context, q rowQueryer, version int) error {
	expected, err := expectedSchemaFingerprints(ctx, version)
	if err != nil {
		return fmt.Errorf("%w: expected schema: %v", ErrIncompatibleSchema, err)
	}
	rows, err := q.QueryContext(ctx, "SELECT type,name,sql FROM sqlite_master WHERE name NOT LIKE 'sqlite_%'")
	if err != nil {
		return fmt.Errorf("%w: schema objects: %v", ErrIncompatibleSchema, err)
	}
	defer rows.Close()
	for rows.Next() {
		var typ, name string
		var ddl sql.NullString
		if err := rows.Scan(&typ, &name, &ddl); err != nil {
			return fmt.Errorf("%w: schema objects", ErrIncompatibleSchema)
		}
		key := typ + "/" + name
		fingerprint, ok := expected[key]
		if !ok {
			return fmt.Errorf("%w: unknown schema object", ErrIncompatibleSchema)
		}
		if !ddl.Valid || schemaFingerprint(ddl.String) != fingerprint {
			return fmt.Errorf("%w: altered schema object %s", ErrIncompatibleSchema, key)
		}
		delete(expected, key)
	}
	if err := rows.Err(); err != nil || len(expected) != 0 {
		return fmt.Errorf("%w: schema objects", ErrIncompatibleSchema)
	}
	return nil
}

// expectedSchemaFingerprints builds SQLite's canonical sqlite_master DDL from
// the embedded migrations themselves. Comparing fingerprints, rather than
// names alone, rejects same-name tables and indexes with altered semantics.
func expectedSchemaFingerprints(ctx context.Context, version int) (map[string]string, error) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	migrations := [][]byte{migration0001, migration0002, migration0003, migration0004, migration0005, migration0006, migration0007, migration0008}
	if version < 1 || version > len(migrations) {
		return nil, errors.New("unknown schema version")
	}
	for _, migration := range migrations[:version] {
		if _, err := db.ExecContext(ctx, string(migration)); err != nil {
			return nil, err
		}
	}
	rows, err := db.QueryContext(ctx, "SELECT type,name,sql FROM sqlite_master WHERE name NOT LIKE 'sqlite_%'")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	fingerprints := make(map[string]string)
	for rows.Next() {
		var typ, name, ddl string
		if err := rows.Scan(&typ, &name, &ddl); err != nil {
			return nil, err
		}
		fingerprints[typ+"/"+name] = schemaFingerprint(ddl)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return fingerprints, nil
}

func schemaFingerprint(ddl string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(strings.ReplaceAll(ddl, "\r\n", "\n"))))
	return hex.EncodeToString(sum[:])
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
	normalized, normalizeErr := task.NormalizeText(text)
	canonical := normalized
	if normalizeErr != nil {
		canonical = text
	}
	return s.mutate(ctx, request{"task.create", hash("task.create", canonical)}, requestID, func(tx *sql.Tx, now int64) (MutationResult, error) {
		if normalizeErr != nil {
			return MutationResult{}, invalidInput(normalizeErr.Error())
		}
		text := normalized
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
	normalized, normalizeErr := task.NormalizeText(text)
	canonical := normalized
	if normalizeErr != nil {
		canonical = text
	}
	return s.mutate(ctx, request{"task.update", hash("task.update", selector, canonical)}, requestID, func(tx *sql.Tx, now int64) (MutationResult, error) {
		if normalizeErr != nil {
			return MutationResult{}, invalidInput(normalizeErr.Error())
		}
		text := normalized
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
	return s.mutate(ctx, request{"task.transition", hash("task.transition", selector, string(target), value(note))}, requestID, func(tx *sql.Tx, now int64) (MutationResult, error) {
		if !target.Valid() {
			return MutationResult{}, invalidInput("invalid target state")
		}
		if note != nil && len(*note) > 4096 {
			return MutationResult{}, invalidInput("note exceeds 4096 characters")
		}
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
	var semantic *mutationError
	if errors.As(err, &semantic) {
		return semantic.code, true
	}
	if errors.Is(err, ErrNotFound) {
		return "not_found", true
	}
	if errors.Is(err, ErrAmbiguous) {
		return "ambiguous", true
	}
	return "", false
}
func errorForCode(code, message string) error {
	switch code {
	case "not_found":
		return ErrNotFound
	case "ambiguous":
		return ErrAmbiguous
	case "invalid_input":
		return invalidInput(message)
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
	var operation, inputHash, outcome, resultType, resultJSON, errorCode, errorMessage string
	err = tx.QueryRowContext(ctx, "SELECT operation,input_hash,outcome,coalesce(result_type,''),coalesce(result_json,''),coalesce(error_code,''),coalesce(error_message,'') FROM mutation_requests WHERE request_id=?", requestID).Scan(&operation, &inputHash, &outcome, &resultType, &resultJSON, &errorCode, &errorMessage)
	if err == nil {
		if operation != req.operation || inputHash != req.hash {
			return MutationResult{}, ErrRequestConflict
		}
		if outcome != "success" {
			return MutationResult{}, errorForCode(errorCode, errorMessage)
		}
		if resultType != "task_mutation" {
			return MutationResult{}, errors.New("prior mutation result type mismatch")
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

// ListTrustedUsers returns the committed Slack user-ID allowlist in lexical order.
// Deprecated callers retain this unpaged view; gateway-facing reads use
// ListTrustedUsersPage so no protocol response is unbounded.
func (s *Store) ListTrustedUsers(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT slack_user_id FROM trusted_users ORDER BY slack_user_id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var users []string
	for rows.Next() {
		var userID string
		if err := rows.Scan(&userID); err != nil {
			return nil, err
		}
		users = append(users, userID)
	}
	return users, rows.Err()
}

// IsTrustedUser reads current committed authorization directly from SQLite;
// callers must not replace this authority check with a configuration cache.
// ListTrustedUsersPage returns a lexical ID-cursor page. Unlike resource
// cursors, a removed trust anchor remains a valid point after which to resume.
func (s *Store) ListTrustedUsersPage(ctx context.Context, limit int, cursor string) ([]string, string, error) {
	if limit == 0 {
		limit = 50
	}
	if limit < 1 || limit > 100 {
		return nil, "", invalidInput("invalid trusted-user list limit")
	}
	if cursor != "" && !validSlackUserID(cursor) {
		return nil, "", invalidInput("invalid trusted-user list cursor")
	}
	q, args := "SELECT slack_user_id FROM trusted_users", []any{}
	if cursor != "" {
		q += " WHERE slack_user_id>?"
		args = append(args, cursor)
	}
	q += " ORDER BY slack_user_id LIMIT ?"
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	var users []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, "", err
		}
		users = append(users, id)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	next := ""
	if len(users) > limit {
		next = users[limit-1]
		users = users[:limit]
	}
	return users, next, nil
}

func (s *Store) IsTrustedUser(ctx context.Context, userID string) (bool, error) {
	if !validSlackUserID(userID) {
		return false, invalidInput("invalid Slack user ID")
	}
	var trusted bool
	if err := s.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM trusted_users WHERE slack_user_id=?)", userID).Scan(&trusted); err != nil {
		return false, err
	}
	return trusted, nil
}

// AddTrustedUser records a Slack user ID in the current allowlist.
func (s *Store) AddTrustedUser(ctx context.Context, requestID, userID string) (TrustMutationResult, error) {
	return s.mutateTrust(ctx, request{"trusted_user.add", hash("trusted_user.add", userID)}, requestID, userID, true)
}

// RemoveTrustedUser removes a Slack user ID from the current allowlist.
func (s *Store) RemoveTrustedUser(ctx context.Context, requestID, userID string) (TrustMutationResult, error) {
	return s.mutateTrust(ctx, request{"trusted_user.remove", hash("trusted_user.remove", userID)}, requestID, userID, false)
}

func validSlackUserID(userID string) bool {
	if len(userID) < 9 || len(userID) > 64 || (userID[0] != 'U' && userID[0] != 'W') {
		return false
	}
	for _, c := range userID {
		if c < 'A' || c > 'Z' {
			if c < '0' || c > '9' {
				return false
			}
		}
	}
	return true
}

func (s *Store) mutateTrust(ctx context.Context, req request, requestID, userID string, trusted bool) (TrustMutationResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !identity.ValidRequestID(requestID) {
		return TrustMutationResult{}, errors.New("invalid request ID")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return TrustMutationResult{}, err
	}
	defer tx.Rollback()
	var operation, inputHash, outcome, resultType, resultJSON, errorCode, errorMessage string
	err = tx.QueryRowContext(ctx, "SELECT operation,input_hash,outcome,coalesce(result_type,''),coalesce(result_json,''),coalesce(error_code,''),coalesce(error_message,'') FROM mutation_requests WHERE request_id=?", requestID).Scan(&operation, &inputHash, &outcome, &resultType, &resultJSON, &errorCode, &errorMessage)
	if err == nil {
		if operation != req.operation || inputHash != req.hash {
			return TrustMutationResult{}, ErrRequestConflict
		}
		if outcome != "success" {
			return TrustMutationResult{}, errorForCode(errorCode, errorMessage)
		}
		if resultType != "trust_mutation" {
			return TrustMutationResult{}, errors.New("prior mutation result type mismatch")
		}
		var result TrustMutationResult
		if err := json.Unmarshal([]byte(resultJSON), &result); err != nil {
			return TrustMutationResult{}, err
		}
		return result, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return TrustMutationResult{}, err
	}
	now := s.now()
	if !validSlackUserID(userID) {
		err := invalidInput("invalid Slack user ID")
		return TrustMutationResult{}, s.storeTrustError(ctx, tx, req, requestID, now, err)
	}
	var changed bool
	if trusted {
		result, err := tx.ExecContext(ctx, "INSERT INTO trusted_users(slack_user_id) VALUES(?) ON CONFLICT(slack_user_id) DO NOTHING", userID)
		if err != nil {
			return TrustMutationResult{}, err
		}
		changed, err = rowsChanged(result)
		if err != nil {
			return TrustMutationResult{}, err
		}
	} else {
		result, err := tx.ExecContext(ctx, "DELETE FROM trusted_users WHERE slack_user_id=?", userID)
		if err != nil {
			return TrustMutationResult{}, err
		}
		changed, err = rowsChanged(result)
		if err != nil {
			return TrustMutationResult{}, err
		}
	}
	response := TrustMutationResult{SlackUserID: userID, Trusted: trusted, Changed: changed}
	encoded, err := json.Marshal(response)
	if err != nil {
		return TrustMutationResult{}, err
	}
	expiry := now + RequestRetention.Microseconds()
	if _, err := tx.ExecContext(ctx, "INSERT INTO mutation_requests(request_id,operation,input_hash,outcome,result_type,result_json,created_at,completed_at,expiry_eligible_at) VALUES(?,?,?,'success','trust_mutation',?,?,?,?)", requestID, req.operation, req.hash, string(encoded), now, now, expiry); err != nil {
		return TrustMutationResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return TrustMutationResult{}, err
	}
	return response, nil
}

func rowsChanged(result sql.Result) (bool, error) {
	n, err := result.RowsAffected()
	return n != 0, err
}

func (s *Store) storeTrustError(ctx context.Context, tx *sql.Tx, req request, requestID string, now int64, mutationErr error) error {
	code, ok := deterministicCode(mutationErr)
	if !ok {
		return mutationErr
	}
	expiry := now + RequestRetention.Microseconds()
	if _, err := tx.ExecContext(ctx, "INSERT INTO mutation_requests(request_id,operation,input_hash,outcome,error_code,error_message,created_at,completed_at,expiry_eligible_at) VALUES(?,?,?,'error',?,?,?, ?,?)", requestID, req.operation, req.hash, code, mutationErr.Error(), now, now, expiry); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return mutationErr
}

func (s *Store) Get(ctx context.Context, selector string) (Task, error) {
	return getTx(ctx, s.db, selector)
}

// LatestStateEvent returns the newest task transition matching the optional
// prior/new-state filters. The boolean is false when no matching transition
// exists; this is expected for tasks that have never entered or left done.
func (s *Store) LatestStateEvent(ctx context.Context, taskID string, priorState, newState *task.State) (StateEvent, bool, error) {
	query := "SELECT event_id,task_id,prior_state,new_state,actor,note,request_id,committed_at FROM task_state_events WHERE task_id=?"
	args := []any{taskID}
	if priorState != nil {
		query += " AND prior_state=?"
		args = append(args, *priorState)
	}
	if newState != nil {
		query += " AND new_state=?"
		args = append(args, *newState)
	}
	// rowid preserves commit insertion order when a clock yields the same
	// microsecond for consecutive transitions; event IDs are random.
	query += " ORDER BY committed_at DESC,rowid DESC LIMIT 1"
	var event StateEvent
	var note sql.NullString
	var committedAt int64
	err := s.db.QueryRowContext(ctx, query, args...).Scan(&event.ID, &event.TaskID, &event.PriorState, &event.NewState, &event.Actor, &note, &event.RequestID, &committedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return StateEvent{}, false, nil
	}
	if err != nil {
		return StateEvent{}, false, err
	}
	if note.Valid {
		event.Note = &note.String
	}
	event.CommittedAt = time.UnixMicro(committedAt).UTC()
	return event, true, nil
}

// List returns a stable creation-time/task-ID page. A cursor is the full ID
// of its last task; its immutable creation key is reloaded before continuation.
func (s *Store) List(ctx context.Context, stateFilter *string, includeDone bool, limit int, cursor string) ([]Task, string, error) {
	if limit == 0 {
		limit = 20
	}
	if limit < 1 || limit > 100 {
		return nil, "", invalidInput("invalid task list limit")
	}
	var anchorCreated int64
	if cursor != "" {
		if !validFullTaskID(cursor) {
			return nil, "", invalidInput("invalid task list cursor")
		}
		if err := s.db.QueryRowContext(ctx, "SELECT created_at FROM tasks WHERE task_id=?", cursor).Scan(&anchorCreated); errors.Is(err, sql.ErrNoRows) {
			return nil, "", ErrCursorExpired
		} else if err != nil {
			return nil, "", err
		}
	}
	q := "SELECT task_id,version,state,text,source_kind,created_at,updated_at,done_at FROM tasks"
	args := []any{}
	conditions := []string{}
	if stateFilter != nil {
		conditions = append(conditions, "state=?")
		args = append(args, *stateFilter)
	} else if !includeDone {
		conditions = append(conditions, "state IN ('open','working')")
	}
	if cursor != "" {
		conditions = append(conditions, "(created_at > ? OR (created_at = ? AND task_id > ?))")
		args = append(args, anchorCreated, anchorCreated, cursor)
	}
	if len(conditions) > 0 {
		q += " WHERE " + strings.Join(conditions, " AND ")
	}
	q += " ORDER BY created_at,task_id LIMIT ?"
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	var tasks []Task
	for rows.Next() {
		var t Task
		var created, updated int64
		var done sql.NullInt64
		if err := rows.Scan(&t.ID, &t.Version, &t.State, &t.Text, &t.SourceKind, &created, &updated, &done); err != nil {
			return nil, "", err
		}
		t.CreatedAt = time.UnixMicro(created).UTC()
		t.UpdatedAt = time.UnixMicro(updated).UTC()
		if done.Valid {
			d := time.UnixMicro(done.Int64).UTC()
			t.DoneAt = &d
		}
		tasks = append(tasks, t)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	var next string
	if len(tasks) > limit {
		last := tasks[limit-1]
		next = last.ID
		tasks = tasks[:limit]
	}
	return tasks, next, nil
}

type queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func getTx(ctx context.Context, q queryer, selector string) (Task, error) {
	canonical, ok := canonicalTaskSelector(selector)
	if !ok {
		return Task{}, ErrNotFound
	}
	rows, err := q.QueryContext(ctx, "SELECT task_id,version,state,text,source_kind,created_at,updated_at,done_at FROM tasks WHERE task_id=? OR substr(task_id,1,length(?))=? ORDER BY task_id LIMIT 2", canonical, canonical, canonical)
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

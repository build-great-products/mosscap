package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
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

const (
	maxPiSessionID  = 256
	maxObservations = 256
)

type DeliveryState string

const (
	DeliveryPending   DeliveryState = "pending"
	DeliveryInserting DeliveryState = "inserting"
	DeliveryPersisted DeliveryState = "persisted"
	DeliveryUncertain DeliveryState = "uncertain"
	DeliveryRejected  DeliveryState = "rejected"
)

var (
	ErrDeliveryNotFound      = errors.New("delivery not found")
	ErrDeliveryTransition    = errors.New("delivery transition is not allowed")
	ErrDeliveryRequeueDenied = errors.New("delivery is not eligible for requeue")
	ErrDeliveryDuplicateRisk = errors.New("delivery requeue may duplicate an insertion")
	ErrDeliveryOwnership     = errors.New("delivery is not owned by this connection")
)

// Delivery is deliberately safe for operator output. It has no task body,
// source body, session path, transcript, provider, or credential field.
type Delivery struct {
	ID                    string        `json:"delivery_id"`
	TaskID                string        `json:"task_id"`
	TaskVersion           int64         `json:"task_version"`
	PiSessionID           string        `json:"pi_session_id"`
	DeliveryKey           string        `json:"delivery_key"`
	PredecessorDeliveryID string        `json:"predecessor_delivery_id,omitempty"`
	State                 DeliveryState `json:"state"`
	DiagnosticCode        string        `json:"diagnostic_code,omitempty"`
	RequeueReason         string        `json:"requeue_reason,omitempty"`
	OfferedAt             *time.Time    `json:"offered_at,omitempty"`
	InsertingAt           *time.Time    `json:"inserting_at,omitempty"`
	CompletedAt           *time.Time    `json:"completed_at,omitempty"`
	CreatedAt             time.Time     `json:"created_at"`
	UpdatedAt             time.Time     `json:"updated_at"`
}

type DeliveryCounts struct {
	Eligible  int64 `json:"eligible_backlog_count"`
	Uncertain int64 `json:"uncertain_count"`
	Rejected  int64 `json:"rejected_count"`
}

type DeliveryObservation struct {
	DeliveryID  string `json:"delivery_id"`
	DeliveryKey string `json:"delivery_key"`
}

// DeliveryWork is sent only to an opted-in extension. Unlike Delivery it has
// current task text and immutable Slack metadata needed for the visible message.
type DeliveryWork struct {
	Delivery     Delivery
	Task         Task
	TaskSelector string
	Slack        *SlackSource
}

// DeliveryKey is stable across requeues and state-only task transitions.
func DeliveryKey(taskID string, version int64, piSessionID string) string {
	h := sha256.New()
	_, _ = h.Write([]byte("mosscap-pi-delivery-v1\x00"))
	_, _ = h.Write([]byte(taskID))
	_, _ = h.Write([]byte("\x00"))
	_, _ = h.Write([]byte(fmt.Sprintf("%d", version)))
	_, _ = h.Write([]byte("\x00"))
	_, _ = h.Write([]byte(piSessionID))
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

func validPiSessionID(value string) bool {
	return value != "" && len(value) <= maxPiSessionID && utf8.ValidString(value) && !strings.ContainsRune(value, 0)
}
func validDeliveryID(value string) bool { return validFullOpaqueID(value, "dlv_") }
func validDeliverySelector(value string) bool {
	if strings.HasPrefix(value, "dlv_") {
		return len(value) > 4 && len(value) <= 26 && validOpaqueIDSuffix(value[4:])
	}
	return len(value) > 0 && len(value) <= 22 && validOpaqueIDSuffix(value)
}
func validDeliveryKey(value string) bool {
	if len(value) != 71 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	for _, c := range value[7:] {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
func validDeliveryDiagnostic(value string) bool {
	// Extension-originated rejection is deliberately a small vocabulary, not an
	// arbitrary exception channel. Recovery diagnostics written by the gateway
	// are not accepted through this method.
	switch value {
	case "oversized_task", "invalid_delivery_work", "invalid_visible_message":
		return true
	}
	return false
}

// validFullOpaqueID accepts current unprefixed IDs and durable legacy IDs.
func validFullOpaqueID(value, legacyPrefix string) bool {
	if len(value) == 22 {
		return validOpaqueIDSuffix(value)
	}
	return strings.HasPrefix(value, legacyPrefix) && len(value) == len(legacyPrefix)+22 && validOpaqueIDSuffix(value[len(legacyPrefix):])
}

// ReconcileDeliveryObservations promotes exact durable Pi evidence. Unknown or
// cross-session observations fail closed; callers must validate the cap before
// opening a connection.
func (s *Store) ReconcileDeliveryObservations(ctx context.Context, requestID, piSessionID string, observed []DeliveryObservation) (int64, error) {
	if !validPiSessionID(piSessionID) || len(observed) > maxObservations {
		return 0, invalidInput("invalid delivery observations")
	}
	for _, o := range observed {
		if !validDeliveryID(o.DeliveryID) || !validDeliveryKey(o.DeliveryKey) {
			return 0, invalidInput("invalid delivery observation")
		}
	}
	canonical, _ := json.Marshal(observed)
	return s.mutateDeliveryCount(ctx, request{"pi.delivery.reconcile", hash("pi.delivery.reconcile", piSessionID, string(canonical))}, requestID, func(tx *sql.Tx, now int64) (int64, error) {
		var changed int64
		for _, o := range observed {
			result, err := tx.ExecContext(ctx, `UPDATE pi_deliveries SET state='persisted',connection_id=NULL,completed_at_us=max(?,coalesce(completed_at_us,0),updated_at_us),updated_at_us=max(?,updated_at_us)
				WHERE delivery_id=? AND delivery_key=? AND pi_session_id=? AND state IN ('pending','inserting','uncertain')`, now, now, o.DeliveryID, o.DeliveryKey, piSessionID)
			if err != nil {
				return 0, err
			}
			n, err := result.RowsAffected()
			if err != nil {
				return 0, err
			}
			changed += n
			if n == 0 {
				var count int
				if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM pi_deliveries WHERE delivery_id=? AND delivery_key=? AND pi_session_id=?", o.DeliveryID, o.DeliveryKey, piSessionID).Scan(&count); err != nil {
					return 0, err
				}
				if count == 0 {
					return 0, ErrDeliveryNotFound
				}
			}
		}
		return changed, nil
	})
}

// PollDelivery replays a pending row first; only then it creates a pending row
// for the oldest current open/working task version with no row for this session.
func (s *Store) PollDelivery(ctx context.Context, piSessionID, connectionID string) (*DeliveryWork, DeliveryCounts, error) {
	if !validPiSessionID(piSessionID) || !validConnectionID(connectionID) {
		return nil, DeliveryCounts{}, invalidInput("invalid delivery session or connection")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, DeliveryCounts{}, err
	}
	defer tx.Rollback()
	pending, err := getPendingDeliveryWork(ctx, tx, piSessionID)
	if errors.Is(err, ErrDeliveryNotFound) {
		candidate, candidateErr := eligibleDeliveryTask(ctx, tx, piSessionID)
		if candidateErr == nil {
			id, idErr := s.ids.NewDeliveryID()
			if idErr != nil {
				return nil, DeliveryCounts{}, idErr
			}
			now := s.now()
			key := DeliveryKey(candidate.Task.ID, candidate.Task.Version, piSessionID)
			_, candidateErr = tx.ExecContext(ctx, `INSERT INTO pi_deliveries(delivery_id,task_id,task_version,pi_session_id,delivery_key,connection_id,state,offered_at_us,created_at_us,updated_at_us) VALUES(?,?,?,?,?,?, 'pending',?,?,?)`, id, candidate.Task.ID, candidate.Task.Version, piSessionID, key, connectionID, now, now, now)
			if candidateErr == nil {
				pending, err = getPendingDeliveryWork(ctx, tx, piSessionID)
			} else if isConstraint(candidateErr) {
				pending, err = getPendingDeliveryWork(ctx, tx, piSessionID)
			} else {
				return nil, DeliveryCounts{}, candidateErr
			}
		} else if !errors.Is(candidateErr, ErrDeliveryNotFound) {
			return nil, DeliveryCounts{}, candidateErr
		}
	} else if err != nil {
		return nil, DeliveryCounts{}, err
	}
	if pending != nil && pending.Delivery.State == DeliveryPending {
		// Pending attempts may be rebound after socket loss, but never to a new session.
		if _, err := tx.ExecContext(ctx, "UPDATE pi_deliveries SET connection_id=? WHERE delivery_id=?", connectionID, pending.Delivery.ID); err != nil {
			return nil, DeliveryCounts{}, err
		}
	}
	counts, err := deliveryCountsTx(ctx, tx, piSessionID)
	if err != nil {
		return nil, DeliveryCounts{}, err
	}
	if err := tx.Commit(); err != nil {
		return nil, DeliveryCounts{}, err
	}
	return pending, counts, nil
}

func (s *Store) MarkDeliveryInserting(ctx context.Context, requestID, connectionID, deliveryID, deliveryKey string) (Delivery, error) {
	return s.deliveryTransition(ctx, requestID, "pi.delivery.inserting", connectionID, deliveryID, deliveryKey, "inserting", "")
}
func (s *Store) MarkDeliveryPersisted(ctx context.Context, requestID, connectionID, deliveryID, deliveryKey string) (Delivery, error) {
	return s.deliveryTransition(ctx, requestID, "pi.delivery.persisted", connectionID, deliveryID, deliveryKey, "persisted", "")
}
func (s *Store) MarkDeliveryRejected(ctx context.Context, requestID, connectionID, deliveryID, deliveryKey, diagnostic string) (Delivery, error) {
	if !validDeliveryDiagnostic(diagnostic) {
		return Delivery{}, invalidInput("invalid delivery diagnostic code")
	}
	return s.deliveryTransition(ctx, requestID, "pi.delivery.rejected", connectionID, deliveryID, deliveryKey, "rejected", diagnostic)
}

func (s *Store) deliveryTransition(ctx context.Context, requestID, operation, connectionID, deliveryID, deliveryKey, target, diagnostic string) (Delivery, error) {
	if !validConnectionID(connectionID) || !validDeliveryID(deliveryID) || !validDeliveryKey(deliveryKey) {
		return Delivery{}, invalidInput("invalid delivery identity")
	}
	return s.mutateDelivery(ctx, request{operation, hash(operation, connectionID, deliveryID, deliveryKey, diagnostic)}, requestID, func(tx *sql.Tx, now int64) (Delivery, error) {
		current, err := getDeliveryExact(ctx, tx, deliveryID)
		if err != nil {
			return Delivery{}, err
		}
		if current.DeliveryKey != deliveryKey || current.connectionID != connectionID {
			return Delivery{}, ErrDeliveryOwnership
		}
		if deliveryTransitionMatches(current, target, diagnostic) {
			return current.Delivery, nil
		}
		var result sql.Result
		switch target {
		case "inserting":
			if current.State != DeliveryPending {
				return Delivery{}, ErrDeliveryTransition
			}
			result, err = tx.ExecContext(ctx, "UPDATE pi_deliveries SET state='inserting',inserting_at_us=max(?,updated_at_us),updated_at_us=max(?,updated_at_us) WHERE delivery_id=? AND state='pending'", now, now, deliveryID)
		case "persisted":
			if current.State != DeliveryPending && current.State != DeliveryInserting && current.State != DeliveryUncertain {
				return Delivery{}, ErrDeliveryTransition
			}
			result, err = tx.ExecContext(ctx, "UPDATE pi_deliveries SET state='persisted',connection_id=NULL,completed_at_us=max(?,updated_at_us),updated_at_us=max(?,updated_at_us) WHERE delivery_id=?", now, now, deliveryID)
		case "rejected":
			if current.State != DeliveryPending && current.State != DeliveryInserting {
				return Delivery{}, ErrDeliveryTransition
			}
			result, err = tx.ExecContext(ctx, "UPDATE pi_deliveries SET state='rejected',connection_id=NULL,diagnostic_code=?,completed_at_us=max(?,updated_at_us),updated_at_us=max(?,updated_at_us) WHERE delivery_id=?", diagnostic, now, now, deliveryID)
		}
		if err != nil {
			return Delivery{}, err
		}
		changed, err := rowsChanged(result)
		if err != nil || !changed {
			return Delivery{}, ErrDeliveryTransition
		}
		updated, err := getDeliveryExact(ctx, tx, deliveryID)
		return updated.Delivery, err
	})
}

// ReconcileDeliveryConnection marks only inserting work uncertain; pending rows
// are intentionally left replayable for the same session.
func (s *Store) ReconcileDeliveryConnection(ctx context.Context, connectionID string) (int64, error) {
	if !validConnectionID(connectionID) {
		return 0, invalidInput("invalid connection ID")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	result, err := s.db.ExecContext(ctx, "UPDATE pi_deliveries SET state='uncertain',connection_id=NULL,diagnostic_code='connection_lost',completed_at_us=max(?,updated_at_us),updated_at_us=max(?,updated_at_us) WHERE connection_id=? AND state='inserting'", now, now, connectionID)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}
func (s *Store) ReconcileInsertingDeliveries(ctx context.Context) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	result, err := s.db.ExecContext(ctx, "UPDATE pi_deliveries SET state='uncertain',connection_id=NULL,diagnostic_code='startup_reconciliation',completed_at_us=max(?,updated_at_us),updated_at_us=max(?,updated_at_us) WHERE state='inserting'", now, now)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func (s *Store) ListDeliveries(ctx context.Context, state *DeliveryState, limit int) ([]Delivery, error) {
	if limit == 0 {
		limit = 100
	}
	if limit < 1 || limit > 1000 {
		return nil, invalidInput("invalid delivery list limit")
	}
	q, args := deliverySelect, []any{}
	if state != nil {
		if !state.valid() {
			return nil, invalidInput("invalid delivery state")
		}
		q += " WHERE state=?"
		args = append(args, *state)
	}
	q += " ORDER BY updated_at_us,delivery_id LIMIT ?"
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Delivery
	for rows.Next() {
		x, err := scanDelivery(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, x.Delivery)
	}
	return out, rows.Err()
}

// ListDeliveriesPage returns a newest-created/ID page, optionally for one
// task. Cursor anchors are immutable and must still be present.
func (s *Store) ListDeliveriesPage(ctx context.Context, state *DeliveryState, taskSelector string, limit int, cursor string) ([]Delivery, string, error) {
	if limit == 0 {
		limit = 50
	}
	if limit < 1 || limit > 100 {
		return nil, "", invalidInput("invalid delivery list limit")
	}
	if state != nil && !state.valid() {
		return nil, "", invalidInput("invalid delivery state")
	}
	var taskID string
	if taskSelector != "" {
		t, err := getTx(ctx, s.db, taskSelector)
		if err != nil {
			return nil, "", err
		}
		taskID = t.ID
	}
	var createdAt int64
	if cursor != "" {
		if !validDeliveryID(cursor) {
			return nil, "", invalidInput("invalid delivery list cursor")
		}
		if err := s.db.QueryRowContext(ctx, "SELECT created_at_us FROM pi_deliveries WHERE delivery_id=?", cursor).Scan(&createdAt); errors.Is(err, sql.ErrNoRows) {
			return nil, "", ErrCursorExpired
		} else if err != nil {
			return nil, "", err
		}
	}
	q, args := deliverySelect, []any{}
	conditions := []string{}
	if state != nil {
		conditions = append(conditions, "state=?")
		args = append(args, *state)
	}
	if taskID != "" {
		conditions = append(conditions, "task_id=?")
		args = append(args, taskID)
	}
	if cursor != "" {
		conditions = append(conditions, "(created_at_us < ? OR (created_at_us = ? AND delivery_id > ?))")
		args = append(args, createdAt, createdAt, cursor)
	}
	if len(conditions) != 0 {
		q += " WHERE " + strings.Join(conditions, " AND ")
	}
	q += " ORDER BY created_at_us DESC,delivery_id LIMIT ?"
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	var out []Delivery
	for rows.Next() {
		x, err := scanDelivery(rows)
		if err != nil {
			return nil, "", err
		}
		out = append(out, x.Delivery)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	next := ""
	if len(out) > limit {
		next = out[limit-1].ID
		out = out[:limit]
	}
	return out, next, nil
}

// HasPersistedDelivery answers the exact lifecycle question without depending
// on the bounded operator-facing delivery listing.
func (s *Store) HasPersistedDelivery(ctx context.Context, taskID string, version int64) (bool, error) {
	var found int
	err := s.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM pi_deliveries WHERE task_id=? AND task_version=? AND state='persisted')", taskID, version).Scan(&found)
	return found != 0, err
}

func (s *Store) GetDelivery(ctx context.Context, selector string) (Delivery, error) {
	x, err := getDelivery(ctx, s.db, selector)
	return x.Delivery, err
}
func (s *Store) RequeueDelivery(ctx context.Context, requestID, selector, reason string) (Delivery, error) {
	if !validReason(reason) {
		return Delivery{}, invalidInput("invalid delivery requeue reason")
	}
	return s.mutateDelivery(ctx, request{"pi.delivery.requeue", hash("pi.delivery.requeue", selector, reason)}, requestID, func(tx *sql.Tx, now int64) (Delivery, error) {
		previous, err := getDelivery(ctx, tx, selector)
		if err != nil {
			return Delivery{}, err
		}
		if previous.State != DeliveryUncertain && previous.State != DeliveryRejected {
			return Delivery{}, ErrDeliveryRequeueDenied
		}
		id, err := s.ids.NewDeliveryID()
		if err != nil {
			return Delivery{}, err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO pi_deliveries(delivery_id,task_id,task_version,pi_session_id,delivery_key,predecessor_delivery_id,state,requeue_reason,created_at_us,updated_at_us) VALUES(?,?,?,?,?,?,'pending',?,?,?)`, id, previous.TaskID, previous.TaskVersion, previous.PiSessionID, previous.DeliveryKey, previous.ID, reason, now, now)
		if err != nil {
			return Delivery{}, err
		}
		next, err := getDeliveryExact(ctx, tx, id)
		return next.Delivery, err
	})
}

const deliverySelect = `SELECT delivery_id,task_id,task_version,pi_session_id,delivery_key,predecessor_delivery_id,connection_id,state,diagnostic_code,requeue_reason,offered_at_us,inserting_at_us,completed_at_us,created_at_us,updated_at_us FROM pi_deliveries`

type storedDelivery struct {
	Delivery
	connectionID string
}
type deliveryScanner interface{ Scan(...any) error }

func scanDelivery(row deliveryScanner) (storedDelivery, error) {
	var x storedDelivery
	var predecessor, connection, diagnostic, reason sql.NullString
	var offered, inserting, completed sql.NullInt64
	var created, updated int64
	err := row.Scan(&x.ID, &x.TaskID, &x.TaskVersion, &x.PiSessionID, &x.DeliveryKey, &predecessor, &connection, &x.State, &diagnostic, &reason, &offered, &inserting, &completed, &created, &updated)
	if err != nil {
		return x, err
	}
	x.PredecessorDeliveryID = predecessor.String
	x.connectionID = connection.String
	x.DiagnosticCode = diagnostic.String
	x.RequeueReason = reason.String
	x.CreatedAt = time.UnixMicro(created).UTC()
	x.UpdatedAt = time.UnixMicro(updated).UTC()
	for _, pair := range []struct {
		src sql.NullInt64
		dst **time.Time
	}{{offered, &x.OfferedAt}, {inserting, &x.InsertingAt}, {completed, &x.CompletedAt}} {
		if pair.src.Valid {
			v := time.UnixMicro(pair.src.Int64).UTC()
			*pair.dst = &v
		}
	}
	return x, nil
}
func getDeliveryExact(ctx context.Context, q rowQueryer, id string) (storedDelivery, error) {
	if !validDeliveryID(id) {
		return storedDelivery{}, ErrDeliveryNotFound
	}
	x, err := scanDelivery(q.QueryRowContext(ctx, deliverySelect+" WHERE delivery_id=?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return x, ErrDeliveryNotFound
	}
	return x, err
}
func getDelivery(ctx context.Context, q queryer, selector string) (storedDelivery, error) {
	if !validDeliverySelector(selector) {
		return storedDelivery{}, ErrDeliveryNotFound
	}
	// A selector is never interpolated into SQL: '_' is valid opaque ID data.
	rows, err := q.QueryContext(ctx, deliverySelect+" WHERE delivery_id=? OR substr(delivery_id,1,length(?))=? ORDER BY delivery_id LIMIT 2", selector, selector, selector)
	if err != nil {
		return storedDelivery{}, err
	}
	defer rows.Close()
	var got []storedDelivery
	for rows.Next() {
		x, e := scanDelivery(rows)
		if e != nil {
			return storedDelivery{}, e
		}
		got = append(got, x)
	}
	if err = rows.Err(); err != nil {
		return storedDelivery{}, err
	}
	if len(got) == 0 {
		return storedDelivery{}, ErrDeliveryNotFound
	}
	if len(got) > 1 {
		return storedDelivery{}, ErrAmbiguous
	}
	return got[0], nil
}
func getPendingDeliveryWork(ctx context.Context, tx *sql.Tx, session string) (*DeliveryWork, error) {
	rows, err := tx.QueryContext(ctx, deliverySelect+" WHERE pi_session_id=? AND state='pending' ORDER BY updated_at_us,delivery_id LIMIT 1", session)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, ErrDeliveryNotFound
	}
	x, err := scanDelivery(rows)
	if err != nil {
		return nil, err
	}
	w, err := deliveryWorkFor(ctx, tx, x)
	return &w, err
}
func eligibleDeliveryTask(ctx context.Context, tx *sql.Tx, session string) (TaskReadModel, error) {
	rows, err := tx.QueryContext(ctx, `SELECT t.task_id,t.version,t.state,t.text,t.source_kind,t.created_at,t.updated_at,t.done_at,s.initial_source_event_id,s.team_id,s.channel_id,s.author_user_id,s.message_ts,s.thread_ts,s.permalink,s.latest_event_at_us,s.source_deleted_at_us,s.created_at_us,s.updated_at_us FROM tasks t LEFT JOIN slack_task_sources s ON s.task_id=t.task_id WHERE t.state IN ('open','working') AND NOT EXISTS (SELECT 1 FROM pi_deliveries d WHERE d.task_id=t.task_id AND d.task_version=t.version AND d.pi_session_id=?) ORDER BY t.updated_at,t.task_id LIMIT 1`, session)
	if err != nil {
		return TaskReadModel{}, err
	}
	defer rows.Close()
	if !rows.Next() {
		return TaskReadModel{}, ErrDeliveryNotFound
	}
	return scanTaskReadModel(rows)
}
func deliveryWorkFor(ctx context.Context, q rowQueryer, x storedDelivery) (DeliveryWork, error) {
	model, err := getTaskReadModelExact(ctx, q, x.TaskID)
	if err != nil {
		return DeliveryWork{}, err
	}
	if model.Task.Version != x.TaskVersion {
		return DeliveryWork{}, ErrDeliveryNotFound
	}
	selector, err := shortestTaskSelector(ctx, q, model.Task.ID)
	if err != nil {
		return DeliveryWork{}, err
	}
	return DeliveryWork{Delivery: x.Delivery, Task: model.Task, TaskSelector: selector, Slack: model.SlackSource}, nil
}

// shortestTaskSelector returns a bare selector with a readable minimum length.
// It grows only when needed to distinguish tasks currently in the ledger.
func shortestTaskSelector(ctx context.Context, q rowQueryer, taskID string) (string, error) {
	canonical, ok := canonicalTaskSelector(taskID)
	if !ok || canonical != taskID || len(taskID) != 22 {
		return "", ErrNotFound
	}
	for length := 8; length <= len(taskID); length++ {
		selector := taskID[:length]
		var count int
		if err := q.QueryRowContext(ctx, `SELECT count(*) FROM tasks WHERE substr(task_id,1,?)=?`, length, selector).Scan(&count); err != nil {
			return "", err
		}
		if count == 1 {
			return selector, nil
		}
	}
	return taskID, nil
}
func getTaskReadModelExact(ctx context.Context, q rowQueryer, id string) (TaskReadModel, error) {
	row := q.QueryRowContext(ctx, `SELECT t.task_id,t.version,t.state,t.text,t.source_kind,t.created_at,t.updated_at,t.done_at,s.initial_source_event_id,s.team_id,s.channel_id,s.author_user_id,s.message_ts,s.thread_ts,s.permalink,s.latest_event_at_us,s.source_deleted_at_us,s.created_at_us,s.updated_at_us FROM tasks t LEFT JOIN slack_task_sources s ON s.task_id=t.task_id WHERE t.task_id=?`, id)
	model, err := scanTaskReadModel(row)
	if errors.Is(err, sql.ErrNoRows) {
		return model, ErrNotFound
	}
	return model, err
}
func deliveryCountsTx(ctx context.Context, q rowQueryer, session string) (DeliveryCounts, error) {
	var c DeliveryCounts
	err := q.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM tasks t WHERE t.state IN ('open','working') AND NOT EXISTS (SELECT 1 FROM pi_deliveries d WHERE d.task_id=t.task_id AND d.task_version=t.version AND d.pi_session_id=?)) + (SELECT count(*) FROM pi_deliveries WHERE pi_session_id=? AND state='pending'), (SELECT count(*) FROM pi_deliveries WHERE pi_session_id=? AND state='uncertain'), (SELECT count(*) FROM pi_deliveries WHERE pi_session_id=? AND state='rejected')`, session, session, session, session).Scan(&c.Eligible, &c.Uncertain, &c.Rejected)
	return c, err
}
func (s *Store) mutateDelivery(ctx context.Context, req request, requestID string, fn func(*sql.Tx, int64) (Delivery, error)) (Delivery, error) {
	return mutateDeliveryValue(s, ctx, req, requestID, "delivery", fn)
}
func (s *Store) mutateDeliveryCount(ctx context.Context, req request, requestID string, fn func(*sql.Tx, int64) (int64, error)) (int64, error) {
	return mutateDeliveryValue(s, ctx, req, requestID, "delivery_count", fn)
}
func mutateDeliveryValue[T any](s *Store, ctx context.Context, req request, requestID, resultType string, fn func(*sql.Tx, int64) (T, error)) (T, error) {
	var zero T
	s.mu.Lock()
	defer s.mu.Unlock()
	if !identity.ValidRequestID(requestID) {
		return zero, errors.New("invalid request ID")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return zero, err
	}
	defer tx.Rollback()
	var operation, inputHash, outcome, storedType, resultJSON, errorCode, errorMessage string
	err = tx.QueryRowContext(ctx, "SELECT operation,input_hash,outcome,coalesce(result_type,''),coalesce(result_json,''),coalesce(error_code,''),coalesce(error_message,'') FROM mutation_requests WHERE request_id=?", requestID).Scan(&operation, &inputHash, &outcome, &storedType, &resultJSON, &errorCode, &errorMessage)
	if err == nil {
		if operation != req.operation || inputHash != req.hash {
			return zero, ErrRequestConflict
		}
		if outcome != "success" {
			return zero, errorForCode(errorCode, errorMessage)
		}
		if storedType != resultType {
			return zero, errors.New("prior mutation result type mismatch")
		}
		var replay T
		if err = json.Unmarshal([]byte(resultJSON), &replay); err != nil {
			return zero, err
		}
		return replay, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return zero, err
	}
	now := s.now()
	out, callErr := fn(tx, now)
	if callErr != nil {
		if code, ok := deliveryDeterministicCode(callErr); ok {
			_, err = tx.ExecContext(ctx, "INSERT INTO mutation_requests(request_id,operation,input_hash,outcome,error_code,error_message,created_at,completed_at,expiry_eligible_at) VALUES(?,?,?,'error',?,?,?, ?,?)", requestID, req.operation, req.hash, code, callErr.Error(), now, now, now+RequestRetention.Microseconds())
			if err != nil {
				return zero, err
			}
			if err = tx.Commit(); err != nil {
				return zero, err
			}
		}
		return zero, callErr
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return zero, err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO mutation_requests(request_id,operation,input_hash,outcome,result_type,result_json,created_at,completed_at,expiry_eligible_at) VALUES(?,?,?,'success',?,?,?, ?,?)", requestID, req.operation, req.hash, resultType, string(raw), now, now, now+RequestRetention.Microseconds())
	if err != nil {
		return zero, err
	}
	if err = tx.Commit(); err != nil {
		return zero, err
	}
	return out, nil
}
func deliveryDeterministicCode(err error) (string, bool) {
	if code, ok := deterministicCode(err); ok {
		return code, true
	}
	switch {
	case errors.Is(err, ErrDeliveryNotFound):
		return "not_found", true
	case errors.Is(err, ErrDeliveryOwnership), errors.Is(err, ErrDeliveryTransition), errors.Is(err, ErrDeliveryRequeueDenied):
		return "conflict", true
	}
	return "", false
}
func (s DeliveryState) valid() bool {
	return s == DeliveryPending || s == DeliveryInserting || s == DeliveryPersisted || s == DeliveryUncertain || s == DeliveryRejected
}
func deliveryTransitionMatches(x storedDelivery, target, diagnostic string) bool {
	return string(x.State) == target && (target != "rejected" || x.DiagnosticCode == diagnostic)
}
func validConnectionID(v string) bool {
	return v != "" && len(v) <= 128 && utf8.ValidString(v) && !strings.ContainsRune(v, 0)
}
func isConstraint(err error) bool {
	return strings.Contains(strings.ToLower(err.Error()), "constraint")
}
func validReason(v string) bool {
	return v != "" && len(v) <= 4096 && utf8.ValidString(v) && !strings.ContainsRune(v, 0)
}

var _ = task.Open

package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stayradiated/mosscap/internal/task"
)

func TestDeliveryKeyAndRecoveryLifecycle(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	created, err := s.Create(ctx, "delivery-task", "canary task text")
	if err != nil {
		t.Fatal(err)
	}
	key := DeliveryKey(created.TaskID, created.Version, "pi-session")
	if !strings.HasPrefix(key, "sha256:") || len(key) != 71 || key != DeliveryKey(created.TaskID, created.Version, "pi-session") || key == DeliveryKey(created.TaskID, created.Version+1, "pi-session") {
		t.Fatalf("key=%q", key)
	}
	work, counts, err := s.PollDelivery(ctx, "pi-session", "connection-one")
	if err != nil || work == nil || work.Task.Text != "canary task text" || work.Delivery.State != DeliveryPending || counts.Eligible != 1 {
		t.Fatalf("poll=%+v counts=%+v err=%v", work, counts, err)
	}
	if work.Delivery.DeliveryKey != key {
		t.Fatalf("delivery key=%q", work.Delivery.DeliveryKey)
	}
	inserting, err := s.MarkDeliveryInserting(ctx, "delivery-inserting", "connection-one", work.Delivery.ID, key)
	if err != nil || inserting.State != DeliveryInserting {
		t.Fatalf("inserting=%+v err=%v", inserting, err)
	}
	if n, err := s.ReconcileDeliveryConnection(ctx, "connection-one"); err != nil || n != 1 {
		t.Fatalf("disconnect n=%d err=%v", n, err)
	}
	uncertain, err := s.GetDelivery(ctx, work.Delivery.ID)
	if err != nil || uncertain.State != DeliveryUncertain {
		t.Fatalf("uncertain=%+v err=%v", uncertain, err)
	}
	persisted, err := s.ReconcileDeliveryObservations(ctx, "delivery-evidence", "pi-session", []DeliveryObservation{{DeliveryID: work.Delivery.ID, DeliveryKey: key}})
	if err != nil || persisted != 1 {
		t.Fatalf("evidence=%d err=%v", persisted, err)
	}
	got, err := s.GetDelivery(ctx, work.Delivery.ID)
	if err != nil || got.State != DeliveryPersisted {
		t.Fatalf("persisted=%+v err=%v", got, err)
	}
	rows, err := s.DB().Query("PRAGMA table_info(pi_deliveries)")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notNull, pk int
		var name, typ string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(strings.ToLower(name), "text") || strings.Contains(strings.ToLower(name), "path") || strings.Contains(strings.ToLower(name), "transcript") {
			t.Fatalf("unsafe durable delivery column %q", name)
		}
	}
}

func TestDeliveryOrderRequeueAndVersionEligibility(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	first, err := s.Create(ctx, "first", "first")
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Create(ctx, "second", "second")
	if err != nil {
		t.Fatal(err)
	}
	// Fixed clocks make create timestamps equal; make oldest-first explicit.
	if _, err := s.DB().Exec("UPDATE tasks SET updated_at=created_at+1 WHERE task_id=?", first.TaskID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec("UPDATE tasks SET updated_at=created_at+2 WHERE task_id=?", second.TaskID); err != nil {
		t.Fatal(err)
	}
	w, _, err := s.PollDelivery(ctx, "session", "one")
	if err != nil || w == nil || w.Task.ID != first.TaskID {
		t.Fatalf("first poll=%+v err=%v", w, err)
	}
	if _, err := s.MarkDeliveryRejected(ctx, "reject", "one", w.Delivery.ID, w.Delivery.DeliveryKey, "oversized_task"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RequeueDelivery(ctx, "requeue", w.Delivery.ID, "operator accepts duplicate risk"); err != nil {
		t.Fatal(err)
	}
	// The linked pending successor replays before a different task.
	next, _, err := s.PollDelivery(ctx, "session", "two")
	if err != nil || next == nil || next.Delivery.PredecessorDeliveryID != w.Delivery.ID {
		t.Fatalf("requeue poll=%+v err=%v", next, err)
	}
	if _, err := s.MarkDeliveryRejected(ctx, "reject-next", "two", next.Delivery.ID, next.Delivery.DeliveryKey, "oversized_task"); err != nil {
		t.Fatal(err)
	}
	other, _, err := s.PollDelivery(ctx, "session", "three")
	if err != nil || other == nil || other.Task.ID != second.TaskID {
		t.Fatalf("second poll=%+v err=%v", other, err)
	}
	if _, err := s.MarkDeliveryRejected(ctx, "reject-other", "three", other.Delivery.ID, other.Delivery.DeliveryKey, "oversized_task"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Update(ctx, "new-version", first.TaskID, "new first"); err != nil {
		t.Fatal(err)
	}
	versioned, _, err := s.PollDelivery(ctx, "session", "four")
	if err != nil || versioned == nil || versioned.Task.ID != first.TaskID || versioned.Task.Version != 2 {
		t.Fatalf("version poll=%+v err=%v", versioned, err)
	}
	if _, err := s.Transition(ctx, "done", first.TaskID, task.Done, nil); err != nil {
		t.Fatal(err)
	}
}

func TestDeliveryRequestReplayAndOwnership(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	c, err := s.Create(ctx, "task", "body")
	if err != nil {
		t.Fatal(err)
	}
	w, _, err := s.PollDelivery(ctx, "session", "connection")
	if err != nil || w == nil {
		t.Fatal(err)
	}
	first, err := s.MarkDeliveryInserting(ctx, "same-request", "connection", w.Delivery.ID, w.Delivery.DeliveryKey)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := s.MarkDeliveryInserting(ctx, "same-request", "connection", w.Delivery.ID, w.Delivery.DeliveryKey)
	if err != nil || replay.ID != first.ID || replay.State != DeliveryInserting {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	if _, err := s.MarkDeliveryPersisted(ctx, "wrong-owner", "other", w.Delivery.ID, w.Delivery.DeliveryKey); !errors.Is(err, ErrDeliveryOwnership) {
		t.Fatalf("wrong owner=%v", err)
	}
	if c.TaskID != w.Task.ID {
		t.Fatal("wrong task")
	}
}

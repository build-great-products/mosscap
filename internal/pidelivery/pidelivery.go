// Package pidelivery coordinates the one in-memory opted-in Pi extension.
package pidelivery

import (
	"context"
	"errors"
	"sync"

	"github.com/stayradiated/mosscap/internal/store"
)

var (
	ErrNotOpen      = errors.New("Pi delivery session is not open")
	ErrSessionBound = errors.New("Pi delivery session does not match connection")
)

// ActiveSessionError is returned when another extension already owns the
// gateway's single active slot. It contains only the session identity.
type ActiveSessionError struct{ PiSessionID string }

func (e *ActiveSessionError) Error() string { return "another Pi delivery session is active" }

type binding struct {
	connectionID string
	piSessionID  string
}

// Coordinator owns ephemeral connection/session ownership. The ledger holds
// recovery evidence; this type intentionally has no lease, heartbeat, or
// process-control state.
type Coordinator struct {
	mu     sync.Mutex
	ledger *store.Store
	active *binding
}

func New(ledger *store.Store) *Coordinator { return &Coordinator{ledger: ledger} }

// ReplaceStore is called while the gateway has drained dispatches, before the
// old SQLite handle is closed. It keeps long-lived connection coordination from
// retaining a closed store after config reload.
func (c *Coordinator) ReplaceStore(ledger *store.Store) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ledger = ledger
}

// Recover marks any incomplete pre-start insertion uncertain before the
// endpoint becomes reachable.
func (c *Coordinator) Recover(ctx context.Context) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ledger.ReconcileInsertingDeliveries(ctx)
}

// Open claims the single slot and reconciles persisted-session evidence before
// a later poll can offer backlog. Repeating the same connection/session is
// idempotent.
func (c *Coordinator) Open(ctx context.Context, requestID, connectionID, piSessionID string, observed []store.DeliveryObservation) (store.DeliveryCounts, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	claimed := false
	if c.active != nil {
		if c.active.connectionID != connectionID {
			return store.DeliveryCounts{}, &ActiveSessionError{PiSessionID: c.active.piSessionID}
		}
		if c.active.piSessionID != piSessionID {
			return store.DeliveryCounts{}, ErrSessionBound
		}
	} else {
		c.active = &binding{connectionID: connectionID, piSessionID: piSessionID}
		claimed = true
	}
	if _, err := c.ledger.ReconcileDeliveryObservations(ctx, requestID, piSessionID, observed); err != nil {
		if claimed {
			c.active = nil
		}
		return store.DeliveryCounts{}, err
	}
	// Poll has the authoritative bounded counts and is deliberately deferred
	// until after exact observed evidence reconciliation.
	_, counts, err := c.ledger.PollDelivery(ctx, piSessionID, connectionID)
	if err != nil {
		return store.DeliveryCounts{}, err
	}
	return counts, nil
}

func (c *Coordinator) Poll(ctx context.Context, connectionID string) (*store.DeliveryWork, store.DeliveryCounts, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	b, err := c.bound(connectionID)
	if err != nil {
		return nil, store.DeliveryCounts{}, err
	}
	for i := 0; i < 1000; i++ {
		work, counts, err := c.ledger.PollDelivery(ctx, b.piSessionID, connectionID)
		if err != nil || work == nil || len([]byte(work.Task.Text)) <= 40000 {
			return work, counts, err
		}
		// The task cannot fit in the extension contract. This is a conclusive
		// pre-insertion rejection, never an oversized protocol frame.
		if _, err := c.ledger.MarkDeliveryRejected(ctx, "gateway-oversized-"+work.Delivery.ID, connectionID, work.Delivery.ID, work.Delivery.DeliveryKey, "oversized_task"); err != nil {
			return nil, store.DeliveryCounts{}, err
		}
	}
	return nil, store.DeliveryCounts{}, errors.New("too many oversized delivery tasks")
}

func (c *Coordinator) Inserting(ctx context.Context, requestID, connectionID, deliveryID, deliveryKey string) (store.Delivery, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, err := c.bound(connectionID); err != nil {
		return store.Delivery{}, err
	}
	return c.ledger.MarkDeliveryInserting(ctx, requestID, connectionID, deliveryID, deliveryKey)
}
func (c *Coordinator) Persisted(ctx context.Context, requestID, connectionID, deliveryID, deliveryKey string) (store.Delivery, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, err := c.bound(connectionID); err != nil {
		return store.Delivery{}, err
	}
	return c.ledger.MarkDeliveryPersisted(ctx, requestID, connectionID, deliveryID, deliveryKey)
}
func (c *Coordinator) Rejected(ctx context.Context, requestID, connectionID, deliveryID, deliveryKey, diagnostic string) (store.Delivery, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, err := c.bound(connectionID); err != nil {
		return store.Delivery{}, err
	}
	return c.ledger.MarkDeliveryRejected(ctx, requestID, connectionID, deliveryID, deliveryKey, diagnostic)
}

// Close reconciles inserting work before releasing the active slot. It is
// idempotent for a connection which is already gone.
// ReconcileActive is used by gateway reload before retiring its old ledger.
// The successor connection must explicitly open again after reload.
func (c *Coordinator) ReconcileActive(ctx context.Context) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active == nil {
		return 0, nil
	}
	n, err := c.ledger.ReconcileDeliveryConnection(ctx, c.active.connectionID)
	if err != nil {
		return n, err
	}
	c.active = nil
	return n, nil
}

func (c *Coordinator) Close(ctx context.Context, connectionID string) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active == nil || c.active.connectionID != connectionID {
		return 0, nil
	}
	n, err := c.ledger.ReconcileDeliveryConnection(ctx, connectionID)
	if err != nil {
		return n, err
	}
	c.active = nil
	return n, nil
}

type Status struct {
	ActivePiSessionID string
	Pending           int64
	Inserting         int64
	Persisted         int64
	Uncertain         int64
	Rejected          int64
}

// Status exposes only the active session identity and bounded delivery state
// counts. Delivery metadata, task text, and diagnostics do not escape.
func (c *Coordinator) Status(ctx context.Context) (Status, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := Status{}
	if c.active != nil {
		out.ActivePiSessionID = c.active.piSessionID
	}
	rows, err := c.ledger.ListDeliveries(ctx, nil, 1000)
	if err != nil {
		return out, err
	}
	for _, row := range rows {
		switch row.State {
		case store.DeliveryPending:
			out.Pending++
		case store.DeliveryInserting:
			out.Inserting++
		case store.DeliveryPersisted:
			out.Persisted++
		case store.DeliveryUncertain:
			out.Uncertain++
		case store.DeliveryRejected:
			out.Rejected++
		}
	}
	return out, nil
}

func (c *Coordinator) bound(connectionID string) (*binding, error) {
	if c.active == nil {
		return nil, ErrNotOpen
	}
	if c.active.connectionID != connectionID {
		return nil, ErrSessionBound
	}
	return c.active, nil
}

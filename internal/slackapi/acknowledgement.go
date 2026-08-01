package slackapi

import (
	"context"
	"errors"
	"sync"
)

var (
	// ErrAlreadyAcknowledged is returned when an envelope acknowledgement is
	// attempted more than once.
	ErrAlreadyAcknowledged = errors.New("slack envelope acknowledgement already attempted")
	// ErrAcknowledgementUnavailable prevents a missing transport callback from
	// being mistaken for a successful acknowledgement.
	ErrAcknowledgementUnavailable = errors.New("slack envelope acknowledgement callback is unavailable")
	// ErrConcurrentReceive documents the Socket contract: exactly one caller
	// may receive from a stream at a time.
	ErrConcurrentReceive = errors.New("slack envelope stream already has an active receiver")
)

// Acknowledgement is a one-shot callback supplied by the Socket adapter. The
// first call consumes it even when the transport callback returns an error;
// retrying an acknowledgement could otherwise emit multiple protocol frames.
type Acknowledgement struct {
	mu     sync.Mutex
	called bool
	fn     func(context.Context) error
}

// NewAcknowledgement wraps fn in a concurrency-safe one-shot callback. A nil
// callback remains unavailable and can never report false success.
func NewAcknowledgement(fn func(context.Context) error) *Acknowledgement {
	return &Acknowledgement{fn: fn}
}

// Acknowledge invokes the callback at most once.
func (a *Acknowledgement) Acknowledge(ctx context.Context) error {
	if a == nil {
		return ErrAcknowledgementUnavailable
	}
	a.mu.Lock()
	if a.called {
		a.mu.Unlock()
		return ErrAlreadyAcknowledged
	}
	a.called = true
	fn := a.fn
	a.mu.Unlock()
	if fn == nil {
		return ErrAcknowledgementUnavailable
	}
	return fn(ctx)
}

func (a *Acknowledgement) ready() bool {
	if a == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.fn != nil && !a.called
}

// Called reports whether the callback has been consumed.
func (a *Acknowledgement) Called() bool {
	if a == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.called
}

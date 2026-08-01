// Package clock provides UTC time helpers with injectable clocks.
package clock

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Clock supplies wall-clock time.
type Clock interface {
	Now() time.Time
}

// Sleeper provides cancellation-aware waits for reconnect and retry loops.
type Sleeper interface {
	Sleep(context.Context, time.Duration) error
}

// System is the production clock and sleeper.
type System struct{}

// Now returns the current UTC time truncated to microsecond precision.
func (System) Now() time.Time { return UTC(time.Now()) }

// Sleep waits for d or context cancellation without leaking a timer.
func (System) Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Fixed is an injectable clock for tests.
type Fixed struct {
	Time time.Time
}

// Now returns the fixed UTC time truncated to microsecond precision.
func (f Fixed) Now() time.Time { return UTC(f.Time) }

// Manual is a deterministic clock and sleeper for concurrent tests. Sleepers
// wake only when Advance or Set reaches their deadline. It keeps exact virtual
// time internally so repeated sub-microsecond advances are not discarded;
// Now still exposes Mosscap's microsecond precision.
type Manual struct {
	mu       sync.Mutex
	now      time.Time
	nextID   uint64
	sleepers map[uint64]manualSleeper
}

type manualSleeper struct {
	deadline time.Time
	ready    chan struct{}
}

// NewManual creates a manual clock at start.
func NewManual(start time.Time) *Manual {
	return &Manual{now: start.UTC(), sleepers: make(map[uint64]manualSleeper)}
}

// Now returns the current manual time at Mosscap's microsecond precision.
func (m *Manual) Now() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return UTC(m.now)
}

// Sleep blocks until the manual clock advances by d or ctx is canceled.
func (m *Manual) Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	m.mu.Lock()
	m.nextID++
	id := m.nextID
	ready := make(chan struct{})
	m.sleepers[id] = manualSleeper{deadline: m.now.Add(d), ready: ready}
	m.mu.Unlock()

	select {
	case <-ready:
		return nil
	case <-ctx.Done():
		m.mu.Lock()
		if _, waiting := m.sleepers[id]; waiting {
			delete(m.sleepers, id)
		}
		m.mu.Unlock()
		return ctx.Err()
	}
}

// Advance moves the clock forward and wakes every due sleeper.
func (m *Manual) Advance(d time.Duration) error {
	if d < 0 {
		return errors.New("manual clock cannot advance backward")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.setLocked(m.now.Add(d))
	return nil
}

// Set moves the clock to t. Moving backward is rejected.
func (m *Manual) Set(t time.Time) error {
	t = t.UTC()
	m.mu.Lock()
	defer m.mu.Unlock()
	if t.Before(m.now) {
		return errors.New("manual clock cannot move backward")
	}
	m.setLocked(t)
	return nil
}

func (m *Manual) setLocked(t time.Time) {
	m.now = t.UTC()
	for id, sleeper := range m.sleepers {
		if !sleeper.deadline.After(m.now) {
			close(sleeper.ready)
			delete(m.sleepers, id)
		}
	}
}

// PendingSleepers reports how many goroutines are waiting. It is intended for
// deterministic test synchronization.
func (m *Manual) PendingSleepers() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sleepers)
}

// UTC converts t to UTC and truncates it to the precision stored by Mosscap.
func UTC(t time.Time) time.Time { return t.UTC().Truncate(time.Microsecond) }

// UnixMicro returns t as UTC Unix microseconds.
func UnixMicro(t time.Time) int64 { return UTC(t).UnixMicro() }

// RFC3339Micro returns t in Mosscap's wire timestamp format.
func RFC3339Micro(t time.Time) string { return UTC(t).Format("2006-01-02T15:04:05.000000Z07:00") }

// Package clock provides UTC time helpers with injectable clocks.
package clock

import "time"

// Clock supplies wall-clock time.
type Clock interface {
	Now() time.Time
}

// System is the production clock.
type System struct{}

// Now returns the current UTC time truncated to microsecond precision.
func (System) Now() time.Time { return UTC(time.Now()) }

// Fixed is an injectable clock for tests.
type Fixed struct {
	Time time.Time
}

// Now returns the fixed UTC time truncated to microsecond precision.
func (f Fixed) Now() time.Time { return UTC(f.Time) }

// UTC converts t to UTC and truncates it to the precision stored by Mosscap.
func UTC(t time.Time) time.Time { return t.UTC().Truncate(time.Microsecond) }

// UnixMicro returns t as UTC Unix microseconds.
func UnixMicro(t time.Time) int64 { return UTC(t).UnixMicro() }

// RFC3339Micro returns t in Mosscap's wire timestamp format.
func RFC3339Micro(t time.Time) string { return UTC(t).Format("2006-01-02T15:04:05.000000Z07:00") }

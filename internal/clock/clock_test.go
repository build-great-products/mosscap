package clock

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"
)

func TestFixedClockAndFormatting(t *testing.T) {
	input := time.Date(2026, 7, 31, 22, 44, 55, 123456789, time.FixedZone("offset", -7*60*60))
	fixed := Fixed{Time: input}
	want := time.Date(2026, 8, 1, 5, 44, 55, 123456000, time.UTC)
	if got := fixed.Now(); !got.Equal(want) || got.Location() != time.UTC {
		t.Fatalf("Now() = %s (%s), want %s UTC", got, got.Location(), want)
	}
	if got, want := UnixMicro(input), want.UnixMicro(); got != want {
		t.Fatalf("UnixMicro() = %d, want %d", got, want)
	}
	if got, want := RFC3339Micro(input), "2026-08-01T05:44:55.123456Z"; got != want {
		t.Fatalf("RFC3339Micro() = %q, want %q", got, want)
	}
}

func TestManualClockDrivesSleepers(t *testing.T) {
	start := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	manual := NewManual(start)
	done := make(chan error, 1)
	go func() { done <- manual.Sleep(context.Background(), 5*time.Second) }()
	for manual.PendingSleepers() != 1 {
		runtime.Gosched()
	}
	if err := manual.Advance(4 * time.Second); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		t.Fatalf("sleeper woke early: %v", err)
	default:
	}
	if err := manual.Advance(time.Second); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("Sleep: %v", err)
	}
	if got := manual.Now(); !got.Equal(start.Add(5 * time.Second)) {
		t.Fatalf("Now = %v", got)
	}
	if err := manual.Advance(-time.Second); err == nil {
		t.Fatal("backward Advance succeeded")
	}
}

func TestManualClockPreservesSubMicrosecondScheduling(t *testing.T) {
	start := time.Date(2026, 1, 2, 3, 4, 5, 123456100, time.FixedZone("offset", int(time.Hour/time.Second)))
	manual := NewManual(start)
	done := make(chan error, 1)
	go func() { done <- manual.Sleep(context.Background(), 500*time.Nanosecond) }()
	for manual.PendingSleepers() != 1 {
		runtime.Gosched()
	}

	if err := manual.Advance(499 * time.Nanosecond); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		t.Fatalf("sub-microsecond sleeper woke early: %v", err)
	default:
	}
	if err := manual.Advance(time.Nanosecond); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("Sleep: %v", err)
	}
	if got, want := manual.Now(), start.UTC().Truncate(time.Microsecond); !got.Equal(want) || got.Location() != time.UTC {
		t.Fatalf("Now = %v (%v), want %v UTC", got, got.Location(), want)
	}

	// A subsequent sub-microsecond advance starts from the exact internal
	// instant rather than losing the previous 500 nanoseconds.
	if err := manual.Set(start.UTC().Add(999 * time.Nanosecond)); err != nil {
		t.Fatal(err)
	}
	if err := manual.Advance(time.Nanosecond); err != nil {
		t.Fatal(err)
	}
	if got, want := manual.Now(), start.UTC().Add(time.Microsecond).Truncate(time.Microsecond); !got.Equal(want) {
		t.Fatalf("Now after cumulative advance = %v, want %v", got, want)
	}
}

func TestManualAndSystemSleepCancellation(t *testing.T) {
	for name, sleeper := range map[string]Sleeper{
		"manual": NewManual(time.Now()),
		"system": System{},
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if err := sleeper.Sleep(ctx, time.Hour); !errors.Is(err, context.Canceled) {
				t.Fatalf("Sleep error = %v", err)
			}
		})
	}
}

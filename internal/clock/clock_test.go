package clock

import (
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

package slackapi

import (
	"math"
	"testing"
	"time"
)

func TestTimestampValidationAndConversion(t *testing.T) {
	valid := []struct {
		value string
		want  int64
	}{
		{"0.000000", 0},
		{"1.000005", 1_000_005},
		{"1712345678.123456", 1_712_345_678_123_456},
		{"9223372036854.775807", math.MaxInt64},
	}
	for _, test := range valid {
		timestamp, err := ParseTimestamp(test.value)
		if err != nil {
			t.Fatalf("ParseTimestamp(%q): %v", test.value, err)
		}
		got, err := timestamp.UnixMicro()
		if err != nil || got != test.want {
			t.Fatalf("UnixMicro(%q) = %d, %v; want %d", test.value, got, err, test.want)
		}
	}

	for _, value := range []string{"", "1", ".000000", "1.0", "1.0000000", "-1.000000", "1x000000", "9223372036854.775808"} {
		if timestamp, err := ParseTimestamp(value); err == nil || timestamp != "" {
			t.Errorf("ParseTimestamp(%q) = %q, %v; want error", value, timestamp, err)
		}
	}
}

func TestTimestampFromTime(t *testing.T) {
	instant := time.Date(2026, 4, 5, 6, 7, 8, 123456789, time.FixedZone("offset", -2*60*60))
	timestamp, err := TimestampFromTime(instant)
	if err != nil {
		t.Fatal(err)
	}
	if want := "1775376428.123456"; string(timestamp) != want {
		t.Fatalf("TimestampFromTime = %q, want %q", timestamp, want)
	}
	got, err := timestamp.Time()
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(instant.Truncate(time.Microsecond)) || got.Location() != time.UTC {
		t.Fatalf("Time = %v, want %v in UTC", got, instant.Truncate(time.Microsecond))
	}
}

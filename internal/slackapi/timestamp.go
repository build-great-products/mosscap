package slackapi

import (
	"errors"
	"math"
	"strconv"
	"time"
)

// Timestamp is Slack's opaque seconds.microseconds message/event identifier.
// Its lexical form is preserved for API calls and durable source identity.
type Timestamp string

// ParseTimestamp validates a Slack timestamp without echoing the supplied
// value in errors.
func ParseTimestamp(value string) (Timestamp, error) {
	if len(value) < 8 {
		return "", errors.New("invalid Slack timestamp")
	}
	dot := len(value) - 7
	if dot < 1 || value[dot] != '.' {
		return "", errors.New("invalid Slack timestamp")
	}
	for i := range value {
		if i == dot {
			continue
		}
		if value[i] < '0' || value[i] > '9' {
			return "", errors.New("invalid Slack timestamp")
		}
	}
	seconds, err := strconv.ParseUint(value[:dot], 10, 64)
	if err != nil {
		return "", errors.New("invalid Slack timestamp")
	}
	fraction, err := strconv.ParseUint(value[dot+1:], 10, 32)
	if err != nil || seconds > uint64((math.MaxInt64-int64(fraction))/1_000_000) {
		return "", errors.New("Slack timestamp is out of range")
	}
	return Timestamp(value), nil
}

// Valid reports whether t has canonical Slack timestamp syntax and fits in
// Mosscap's integer-microsecond storage representation.
func (t Timestamp) Valid() bool {
	_, err := ParseTimestamp(string(t))
	return err == nil
}

// UnixMicro converts a validated timestamp to integer microseconds.
func (t Timestamp) UnixMicro() (int64, error) {
	parsed, err := ParseTimestamp(string(t))
	if err != nil {
		return 0, err
	}
	value := string(parsed)
	dot := len(value) - 7
	seconds, _ := strconv.ParseInt(value[:dot], 10, 64)
	fraction, _ := strconv.ParseInt(value[dot+1:], 10, 64)
	return seconds*1_000_000 + fraction, nil
}

// Time converts a validated Slack timestamp to UTC.
func (t Timestamp) Time() (time.Time, error) {
	micros, err := t.UnixMicro()
	if err != nil {
		return time.Time{}, err
	}
	return time.UnixMicro(micros).UTC(), nil
}

// TimestampFromTime creates a canonical Slack timestamp from a non-negative
// instant at microsecond precision.
func TimestampFromTime(t time.Time) (Timestamp, error) {
	micros := t.UTC().UnixMicro()
	if micros < 0 {
		return "", errors.New("Slack timestamp precedes the Unix epoch")
	}
	seconds, fraction := micros/1_000_000, micros%1_000_000
	return Timestamp(strconv.FormatInt(seconds, 10) + "." + leftPadMicros(fraction)), nil
}

func leftPadMicros(value int64) string {
	text := strconv.FormatInt(value, 10)
	for len(text) < 6 {
		text = "0" + text
	}
	return text
}

package slackapi

import "time"

// JitterSource is the injectable random boundary used by reconnect backoff.
// Jitter must return a duration in [0, maximum]. Keeping randomness behind this
// tiny interface lets runtime tests pair a scripted source with clock.Manual.
type JitterSource interface {
	Jitter(maximum time.Duration) time.Duration
}

// NoJitter is useful when jitter is disabled by configuration.
type NoJitter struct{}

func (NoJitter) Jitter(time.Duration) time.Duration { return 0 }

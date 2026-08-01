package fake

import (
	"fmt"
	"sync"
	"time"
)

// Jitter is a strict FIFO deterministic jitter source. It panics on script
// exhaustion or an out-of-range value because either condition is a test bug.
type Jitter struct {
	mu      sync.Mutex
	values  []time.Duration
	maximum []time.Duration
}

func NewJitter(values ...time.Duration) *Jitter {
	return &Jitter{values: append([]time.Duration(nil), values...)}
}

func (j *Jitter) Jitter(maximum time.Duration) time.Duration {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.maximum = append(j.maximum, maximum)
	if len(j.values) == 0 {
		panic("fake Slack jitter: script exhausted")
	}
	value := j.values[0]
	j.values = j.values[1:]
	if value < 0 || value > maximum {
		panic(fmt.Sprintf("fake Slack jitter: scripted duration outside [0, %s]", maximum))
	}
	return value
}

func (j *Jitter) Maximums() []time.Duration {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]time.Duration(nil), j.maximum...)
}

func (j *Jitter) Remaining() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return len(j.values)
}

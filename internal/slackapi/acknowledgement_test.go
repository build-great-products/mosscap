package slackapi

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

func TestAcknowledgementIsOneShotUnderConcurrency(t *testing.T) {
	var calls atomic.Int64
	ack := NewAcknowledgement(func(context.Context) error {
		calls.Add(1)
		return nil
	})

	const attempts = 32
	var wg sync.WaitGroup
	results := make(chan error, attempts)
	for range attempts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- ack.Acknowledge(context.Background())
		}()
	}
	wg.Wait()
	close(results)

	successes := 0
	for err := range results {
		if err == nil {
			successes++
		} else if !errors.Is(err, ErrAlreadyAcknowledged) {
			t.Fatalf("unexpected acknowledgement error: %v", err)
		}
	}
	if successes != 1 || calls.Load() != 1 || !ack.Called() {
		t.Fatalf("successes=%d calls=%d called=%t, want 1, 1, true", successes, calls.Load(), ack.Called())
	}
}

func TestAcknowledgementConsumesFailingCallback(t *testing.T) {
	boom := errors.New("transport failed")
	ack := NewAcknowledgement(func(context.Context) error { return boom })
	if err := ack.Acknowledge(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("first Acknowledge error = %v", err)
	}
	if err := ack.Acknowledge(context.Background()); !errors.Is(err, ErrAlreadyAcknowledged) {
		t.Fatalf("second Acknowledge error = %v", err)
	}
}

func TestAcknowledgementFailsClosedWithoutCallback(t *testing.T) {
	for name, ack := range map[string]*Acknowledgement{
		"nil receiver": nil,
		"nil callback": NewAcknowledgement(nil),
	} {
		t.Run(name, func(t *testing.T) {
			if err := ack.Acknowledge(context.Background()); !errors.Is(err, ErrAcknowledgementUnavailable) {
				t.Fatalf("Acknowledge error = %v", err)
			}
		})
	}
	ack := NewAcknowledgement(nil)
	if err := ack.Acknowledge(context.Background()); !errors.Is(err, ErrAcknowledgementUnavailable) {
		t.Fatalf("first Acknowledge error = %v", err)
	}
	if !ack.Called() {
		t.Fatal("unavailable callback attempt was not consumed")
	}
	if err := ack.Acknowledge(context.Background()); !errors.Is(err, ErrAlreadyAcknowledged) {
		t.Fatalf("second Acknowledge error = %v", err)
	}
}

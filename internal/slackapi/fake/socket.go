package fake

import (
	"context"
	"errors"
	"io"
	"sync"

	"github.com/stayradiated/mosscap/internal/slackapi"
)

// Barrier is a context-aware, reusable test gate. Wait reports that the gate
// was reached and blocks until Release or context cancellation.
type Barrier struct {
	reached     chan struct{}
	release     chan struct{}
	reachedOnce sync.Once
	releaseOnce sync.Once
}

func NewBarrier() *Barrier {
	return &Barrier{reached: make(chan struct{}), release: make(chan struct{})}
}

// Reached closes when the first caller starts waiting at the barrier.
func (b *Barrier) Reached() <-chan struct{} {
	if b == nil {
		closed := make(chan struct{})
		close(closed)
		return closed
	}
	return b.reached
}

// Release unblocks every current and future waiter. It is idempotent.
func (b *Barrier) Release() {
	if b == nil {
		return
	}
	b.releaseOnce.Do(func() { close(b.release) })
}

// Wait blocks at the barrier until release or context cancellation.
func (b *Barrier) Wait(ctx context.Context) error {
	return b.wait(ctx, nil)
}

func (b *Barrier) wait(ctx context.Context, stopped <-chan struct{}) error {
	if b == nil {
		return nil
	}
	b.reachedOnce.Do(func() { close(b.reached) })
	if stopped == nil {
		select {
		case <-b.release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	select {
	case <-b.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-stopped:
		return io.ErrClosedPipe
	}
}

// ReceiveStep is one scripted stream receive. Barrier, when present, reports
// that Receive reached this step and delays its result context-safely.
type ReceiveStep struct {
	Envelope slackapi.Envelope
	Err      error
	Barrier  *Barrier
}

// SocketSession scripts one Open call and its resulting stream. OpenBarrier,
// when present, delays both a successful open and an OpenError.
type SocketSession struct {
	OpenError   error
	OpenBarrier *Barrier
	Receives    []ReceiveStep
}

// Socket returns strictly ordered sessions, allowing deterministic reconnect
// tests without a network.
type Socket struct {
	mu       sync.Mutex
	sessions []SocketSession
	opens    int
	streams  []*Stream
	changed  chan struct{}
}

var _ slackapi.Socket = (*Socket)(nil)

func NewSocket(sessions ...SocketSession) *Socket {
	return &Socket{
		sessions: append([]SocketSession(nil), sessions...),
		changed:  make(chan struct{}),
	}
}

func (s *Socket) Open(ctx context.Context) (slackapi.EnvelopeStream, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.opens++
	s.notifyLocked()
	if len(s.sessions) == 0 {
		s.mu.Unlock()
		return nil, errors.New("fake Slack Socket: unexpected open")
	}
	session := s.sessions[0]
	s.sessions = s.sessions[1:]
	s.mu.Unlock()

	if err := session.OpenBarrier.Wait(ctx); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if session.OpenError != nil {
		return nil, session.OpenError
	}
	stream := newStream(session.Receives)
	s.mu.Lock()
	s.streams = append(s.streams, stream)
	s.notifyLocked()
	s.mu.Unlock()
	return stream, nil
}

func (s *Socket) notifyLocked() {
	close(s.changed)
	s.changed = make(chan struct{})
}

// Opens returns the number of attempted connections.
func (s *Socket) Opens() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.opens
}

// WaitForOpens blocks until at least want connection attempts have started.
func (s *Socket) WaitForOpens(ctx context.Context, want int) error {
	for {
		s.mu.Lock()
		if s.opens >= want {
			s.mu.Unlock()
			return nil
		}
		changed := s.changed
		s.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// OpenedStreams returns successful streams in connection order.
func (s *Socket) OpenedStreams() []*Stream {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*Stream(nil), s.streams...)
}

// WaitForOpenedStreams blocks until at least want sessions opened successfully.
func (s *Socket) WaitForOpenedStreams(ctx context.Context, want int) error {
	for {
		s.mu.Lock()
		if len(s.streams) >= want {
			s.mu.Unlock()
			return nil
		}
		changed := s.changed
		s.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Remaining returns the number of unconsumed connection sessions.
func (s *Socket) Remaining() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sessions)
}

// Stream is a deterministic EnvelopeStream. Once scripted steps are exhausted,
// Receive blocks like a live transport until its context is canceled or Close
// is called; tests that need EOF should script it explicitly.
type Stream struct {
	mu           sync.Mutex
	receives     []ReceiveStep
	receiveCalls int
	closeCalls   int
	closed       bool
	done         chan struct{}
	changed      chan struct{}
}

var _ slackapi.EnvelopeStream = (*Stream)(nil)

func newStream(receives []ReceiveStep) *Stream {
	return &Stream{
		receives: append([]ReceiveStep(nil), receives...),
		done:     make(chan struct{}),
		changed:  make(chan struct{}),
	}
}

func (s *Stream) Receive(ctx context.Context) (slackapi.Envelope, error) {
	if err := ctx.Err(); err != nil {
		return slackapi.Envelope{}, err
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return slackapi.Envelope{}, io.ErrClosedPipe
	}
	s.receiveCalls++
	s.notifyLocked()
	if len(s.receives) == 0 {
		done := s.done
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return slackapi.Envelope{}, ctx.Err()
		case <-done:
			return slackapi.Envelope{}, io.ErrClosedPipe
		}
	}
	step := s.receives[0]
	s.receives = s.receives[1:]
	done := s.done
	s.mu.Unlock()

	if err := step.Barrier.wait(ctx, done); err != nil {
		return slackapi.Envelope{}, err
	}
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return slackapi.Envelope{}, io.ErrClosedPipe
	}
	return step.Envelope, step.Err
}

func (s *Stream) notifyLocked() {
	close(s.changed)
	s.changed = make(chan struct{})
}

func (s *Stream) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closeCalls++
	if !s.closed {
		s.closed = true
		close(s.done)
	}
	s.notifyLocked()
	return nil
}

// Remaining returns the number of unconsumed receive steps.
func (s *Stream) Remaining() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.receives)
}

// ReceiveCalls returns the number of receive attempts accepted by the stream.
func (s *Stream) ReceiveCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.receiveCalls
}

// WaitForReceiveCalls blocks until at least want receive attempts have started.
func (s *Stream) WaitForReceiveCalls(ctx context.Context, want int) error {
	for {
		s.mu.Lock()
		if s.receiveCalls >= want {
			s.mu.Unlock()
			return nil
		}
		changed := s.changed
		s.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Closed reports whether Close has stopped the stream.
func (s *Stream) Closed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// CloseCalls reports all close attempts, including idempotent repeats.
func (s *Stream) CloseCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closeCalls
}

// WaitForClose blocks until Close is first called or ctx is canceled.
func (s *Stream) WaitForClose(ctx context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	done := s.done
	s.mu.Unlock()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// AckRecorder produces a one-shot acknowledgement and a concurrency-safe call
// count. The callback error is scripted without retaining envelope contents.
type AckRecorder struct {
	mu    sync.Mutex
	calls int
	err   error
}

func NewAckRecorder(err error) (*slackapi.Acknowledgement, *AckRecorder) {
	recorder := &AckRecorder{err: err}
	ack := slackapi.NewAcknowledgement(func(context.Context) error {
		recorder.mu.Lock()
		defer recorder.mu.Unlock()
		recorder.calls++
		return recorder.err
	})
	return ack, recorder
}

func (r *AckRecorder) Calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

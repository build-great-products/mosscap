// Package slackruntime owns verified, swappable Slack Socket Mode runtimes.
package slackruntime

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/stayradiated/mosscap/internal/config"
	"github.com/stayradiated/mosscap/internal/slackapi"
	"github.com/stayradiated/mosscap/internal/slackapi/slackgo"
)

var ErrClosed = errors.New("Slack runtime is closed")

// Factory constructs fresh, independently closable Slack clients for a
// candidate. Factories must not retain or expose token values.
type Factory interface {
	Build(config.Config) (slackapi.Web, slackapi.Socket, error)
}

// FactoryFunc adapts a constructor for deterministic fake or production use.
type FactoryFunc func(config.Config) (slackapi.Web, slackapi.Socket, error)

func (f FactoryFunc) Build(c config.Config) (slackapi.Web, slackapi.Socket, error) { return f(c) }

type RealFactory struct{}

func (RealFactory) Build(c config.Config) (slackapi.Web, slackapi.Socket, error) {
	return slackgo.NewWeb(c.SlackBotToken, slackgo.WebMaxFiles(c.MaximumFilesPerSlackOperation)), slackgo.NewSocket(c.SlackAppToken, slackgo.SocketMaxFiles(c.MaximumFilesPerSlackOperation)), nil
}

// Handler synchronously commits and acknowledges an envelope. Returning an
// error intentionally causes reconnect; it never invents an offline receipt.
type Handler func(context.Context, slackapi.Envelope) error

type Options struct {
	Factory        Factory
	Handler        Handler
	TrustedUserIDs []string
	Jitter         slackapi.JitterSource
	Sleep          func(context.Context, time.Duration) error
}

type State string

const (
	StateConnecting   State = "connecting"
	StateConnected    State = "connected"
	StateReconnecting State = "reconnecting"
	StateDisabled     State = "disabled"
	StateClosed       State = "closed"
)

type Observations struct {
	State            State
	ReconnectAttempt int
	Backoff          time.Duration
	LastDiagnostic   slackapi.DiagnosticCode
}

// Snapshot is immutable except for its private drain accounting. Hold it for
// the entire operation attempt, then Release exactly once.
type Snapshot struct {
	Config                       config.Config
	TeamID, BotUserID, ChannelID string
	Web                          slackapi.Web
	stream                       slackapi.EnvelopeStream
	done                         chan struct{}
	mu                           sync.Mutex
	refs                         int
	retiring                     bool
	closed                       bool
}

func (s *Snapshot) Release() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.refs > 0 {
		s.refs--
		if s.refs == 0 && s.retiring {
			close(s.done)
		}
	}
	s.mu.Unlock()
}
func (s *Snapshot) closeStream() error {
	s.mu.Lock()
	stream := s.stream
	s.mu.Unlock()
	if stream == nil {
		return nil
	}
	return stream.Close()
}
func (s *Snapshot) receive(ctx context.Context) (slackapi.Envelope, error) {
	s.mu.Lock()
	stream := s.stream
	s.mu.Unlock()
	if stream == nil {
		return slackapi.Envelope{}, ErrClosed
	}
	return stream.Receive(ctx)
}
func (s *Snapshot) replaceStream(stream slackapi.EnvelopeStream) error {
	s.mu.Lock()
	old := s.stream
	s.stream = stream
	s.mu.Unlock()
	if old == nil {
		return nil
	}
	return old.Close()
}
func (s *Snapshot) stopping() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.retiring || s.closed
}
func (s *Snapshot) retain() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.retiring || s.closed {
		return false
	}
	s.refs++
	return true
}
func (s *Snapshot) retire() {
	s.mu.Lock()
	if !s.retiring {
		s.retiring = true
		if s.refs == 0 {
			close(s.done)
		}
	}
	s.mu.Unlock()
	_ = s.closeStream()
	<-s.done
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
}

type Runtime struct {
	options   Options
	ctx       context.Context
	cancel    context.CancelFunc
	mu        sync.RWMutex
	snapshot  *Snapshot
	activated bool
	closed    bool
	obs       Observations
	wg        sync.WaitGroup
}

func New(options Options) (*Runtime, error) {
	if options.Factory == nil {
		return nil, errors.New("Slack runtime factory is required")
	}
	if options.Handler == nil {
		return nil, errors.New("Slack runtime handler is required")
	}
	if options.Sleep == nil {
		options.Sleep = sleep
	}
	if options.Jitter == nil {
		options.Jitter = slackapi.NoJitter{}
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Runtime{options: options, ctx: ctx, cancel: cancel, obs: Observations{State: StateConnecting}}, nil
}
func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Start verifies auth/access/scopes and Socket readiness before publishing an
// inactive candidate. Call Activate only after its durable activation decision.
// A failed start leaves no active runtime.
func (r *Runtime) Start(ctx context.Context, c config.Config) error {
	candidate, err := r.prepare(ctx, c)
	if err != nil {
		return err
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		candidate.retire()
		return ErrClosed
	}
	if r.snapshot != nil {
		r.mu.Unlock()
		candidate.retire()
		return errors.New("Slack runtime already started")
	}
	r.snapshot = candidate
	r.obs = Observations{State: StateConnected}
	r.mu.Unlock()
	return nil
}

// Activate begins intake for the verified candidate published by Start. It is
// deliberately separate from readiness so callers can commit their durable
// activation decision before an envelope can reach Handler.
func (r *Runtime) Activate() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return ErrClosed
	}
	if r.snapshot == nil {
		r.mu.Unlock()
		return errors.New("Slack runtime is not started")
	}
	if r.activated {
		r.mu.Unlock()
		return nil
	}
	snapshot := r.snapshot
	r.activated = true
	// Register the receiver while Close is excluded, so it cannot return from
	// Wait before this activated intake is accounted for.
	r.run(snapshot)
	r.mu.Unlock()
	return nil
}

// Reload is failure-atomic: the old snapshot remains published until a fully
// verified, ready candidate has been prepared.
func (r *Runtime) Reload(ctx context.Context, c config.Config) error {
	candidate, err := r.prepare(ctx, c)
	if err != nil {
		return err
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		candidate.retire()
		return ErrClosed
	}
	old := r.snapshot
	active := r.activated
	r.snapshot = candidate
	r.obs = Observations{State: StateConnected}
	if active {
		r.run(candidate)
	}
	r.mu.Unlock()
	if old != nil {
		go old.retire()
	}
	return nil
}
func (r *Runtime) prepare(ctx context.Context, c config.Config) (*Snapshot, error) {
	if err := config.ValidateSlackProduction(c); err != nil {
		return nil, err
	}
	web, socket, err := r.options.Factory.Build(c)
	if err != nil {
		return nil, safeError(err)
	}
	if web == nil || socket == nil {
		return nil, errors.New("Slack runtime factory returned incomplete clients")
	}
	verified, err := slackapi.Verify(ctx, web, slackapi.VerificationRequest{ChannelID: c.SlackChannelID, TrustedUserIDs: append([]string(nil), r.options.TrustedUserIDs...)})
	if err != nil {
		return nil, safeError(err)
	}
	stream, err := socket.Open(ctx)
	if err != nil {
		return nil, safeError(err)
	}
	return &Snapshot{Config: c, TeamID: verified.Identity.TeamID, BotUserID: verified.Identity.BotUserID, ChannelID: c.SlackChannelID, Web: web, stream: stream, done: make(chan struct{})}, nil
}
func safeError(err error) error {
	failure := slackapi.Classify(err, slackapi.FailureSocket)
	if failure.Diagnostic.Valid() {
		return errors.New("Slack runtime verification failed (" + string(failure.Diagnostic) + ")")
	}
	return errors.New("Slack runtime verification failed")
}

// Acquire returns the active immutable snapshot. The caller must Release it.
func (r *Runtime) Acquire() (*Snapshot, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		return nil, ErrClosed
	}
	if r.snapshot == nil || !r.snapshot.retain() {
		return nil, errors.New("Slack runtime is unavailable")
	}
	return r.snapshot, nil
}
func (r *Runtime) Observations() Observations { r.mu.RLock(); defer r.mu.RUnlock(); return r.obs }
func (r *Runtime) run(s *Snapshot)            { r.wg.Add(1); go func() { defer r.wg.Done(); r.receive(s) }() }
func (r *Runtime) receive(s *Snapshot) {
	attempts := 0
	for {
		envelope, err := s.receive(r.ctx)
		if err == nil {
			attempts = 0
			r.observe(StateConnected, 0, 0, "")
			if !s.retain() {
				return
			}
			handleErr := r.options.Handler(r.ctx, envelope)
			s.Release()
			if handleErr == nil {
				continue
			}
			err = handleErr
		}
		if r.ctx.Err() != nil || s.stopping() {
			return
		}
		failure := slackapi.Classify(err, slackapi.FailureSocket)
		attempts++
		backoff := r.backoff(s.Config, attempts)
		r.observe(StateReconnecting, attempts, backoff, failure.Diagnostic)
		if r.options.Sleep(r.ctx, backoff) != nil {
			return
		}
		stream, openErr := r.open(r.ctx, s)
		if openErr != nil {
			continue
		}
		_ = s.replaceStream(stream)
		r.observe(StateConnected, 0, 0, "")
		attempts = 0
	}
}
func (r *Runtime) open(ctx context.Context, s *Snapshot) (slackapi.EnvelopeStream, error) {
	_, socket, err := r.options.Factory.Build(s.Config)
	if err != nil || socket == nil {
		return nil, errors.New("Slack runtime reconnect unavailable")
	}
	return socket.Open(ctx)
}
func (r *Runtime) backoff(c config.Config, attempt int) time.Duration {
	initial, _ := time.ParseDuration(c.SlackReconnectInitialBackoff)
	maximum, _ := time.ParseDuration(c.SlackReconnectMaxBackoff)
	d := initial
	for i := 1; i < attempt && d < maximum; i++ {
		d *= 2
		if d > maximum {
			d = maximum
		}
	}
	if c.SlackReconnectJitter {
		d += r.options.Jitter.Jitter(d)
	}
	if d > maximum {
		d = maximum
	}
	return d
}
func (r *Runtime) observe(state State, attempt int, backoff time.Duration, diagnostic slackapi.DiagnosticCode) {
	r.mu.Lock()
	if !r.closed {
		r.obs = Observations{State: state, ReconnectAttempt: attempt, Backoff: backoff, LastDiagnostic: diagnostic}
	}
	r.mu.Unlock()
}
func (r *Runtime) Close() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	r.activated = false
	old := r.snapshot
	r.snapshot = nil
	r.obs.State = StateClosed
	r.mu.Unlock()
	r.cancel()
	if old != nil {
		old.retire()
	}
	r.wg.Wait()
	return nil
}

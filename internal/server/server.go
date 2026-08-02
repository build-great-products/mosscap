// Package server serves the private local Mosscap protocol.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/stayradiated/mosscap/internal/config"
	"github.com/stayradiated/mosscap/internal/doctor"
	"github.com/stayradiated/mosscap/internal/protocol"
	"github.com/stayradiated/mosscap/internal/securepath"
	"github.com/stayradiated/mosscap/internal/slackapi"
	"github.com/stayradiated/mosscap/internal/slackingest"
	"github.com/stayradiated/mosscap/internal/slackops"
	"github.com/stayradiated/mosscap/internal/slackruntime"
	"github.com/stayradiated/mosscap/internal/status"
	"github.com/stayradiated/mosscap/internal/store"
	"github.com/stayradiated/mosscap/internal/task"
	"golang.org/x/sys/unix"
)

// Server owns one open ledger and its Unix endpoint.
// ConfigurationVerifier is the Phase 1 seam for authenticating proposed Slack
// credentials and checking workspace/channel access before activation. The
// Phase 0 gateway has no Slack network integration, so a nil verifier performs
// no network claims and reload reports that verification as deferred.
type ConfigurationVerifier interface {
	Verify(context.Context, config.Config) error
}

type ConfigurationVerifierFunc func(context.Context, config.Config) error

type reloadError struct{ message string }

func (e *reloadError) Error() string { return e.message }

func (f ConfigurationVerifierFunc) Verify(ctx context.Context, c config.Config) error {
	return f(ctx, c)
}

type Server struct {
	Config         config.Config // immutable startup fallback for direct tests
	ConfigLocation config.Location
	Version        string
	Log            *slog.Logger
	Verifier       ConfigurationVerifier
	store          *store.Store
	listener       *net.UnixListener
	lock           *os.File
	endpoint       string
	socketDev      uint64
	socketIno      uint64
	wg             sync.WaitGroup
	closing        chan struct{}
	closeOnce      sync.Once
	slots          chan struct{}
	admissionMu    sync.Mutex
	stopping       bool
	activeMu       sync.Mutex
	active         map[*net.UnixConn]context.CancelFunc
	started        time.Time
	connections    atomic.Int32
	activeConfig   atomic.Pointer[config.Config]
	generation     atomic.Int64
	reloadMu       sync.Mutex
	operationMu    sync.RWMutex
	activeDigest   string // guarded by operationMu; SHA-256 only, never TOML bytes
	runtimeFactory slackruntime.Factory
	runtime        *slackruntime.Runtime
	ingest         *slackingest.Coordinator
	slackDisabled  bool
	slackState     slackruntime.State
}

func New(c config.Config, loc config.Location, version string, log *slog.Logger) *Server {
	if version == "" {
		version = "devel"
	}
	if log == nil {
		log = slog.Default()
	}
	s := &Server{Config: c, ConfigLocation: loc, Version: version, Log: log, closing: make(chan struct{}), slots: make(chan struct{}, 64), active: make(map[*net.UnixConn]context.CancelFunc), slackDisabled: true, slackState: slackruntime.StateDisabled}
	s.activeConfig.Store(&c)
	s.generation.Store(1)
	return s
}

func (s *Server) snapshot() config.Config {
	if c := s.activeConfig.Load(); c != nil {
		return *c
	}
	return s.Config
}

// Serve validates state, obtains the singleton lock, and runs until ctx is cancelled.
func (s *Server) Serve(ctx context.Context) error {
	if err := s.open(ctx); err != nil {
		s.shutdown()
		return err
	}
	defer s.shutdown()
	go func() { <-ctx.Done(); s.shutdown() }()
	for {
		conn, err := s.listener.AcceptUnix()
		if err != nil {
			select {
			case <-s.closing:
				return nil
			default:
			}
			if ne, ok := err.(net.Error); ok && ne.Temporary() {
				continue
			}
			return err
		}
		if !sameEUID(conn) {
			_ = conn.Close()
			continue
		}
		select {
		case s.slots <- struct{}{}:
		case <-s.closing:
			_ = conn.Close()
			return nil
		}
		s.admissionMu.Lock()
		if s.stopping {
			s.admissionMu.Unlock()
			<-s.slots
			_ = conn.Close()
			return nil
		}
		s.wg.Add(1)
		s.admissionMu.Unlock()
		go func() {
			defer s.wg.Done()
			defer func() { <-s.slots }()
			s.handle(conn)
		}()
	}
}
func (s *Server) open(ctx context.Context) error {
	c := s.snapshot()
	if err := config.Validate(c); err != nil {
		return err
	}
	if err := s.validateStartupPaths(c); err != nil {
		return err
	}
	lockPath := filepath.Join(c.Paths.StateDir, "gateway.lock")
	f, err := securepath.OpenFileNoFollow(lockPath, os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return err
	}
	if err := f.Chmod(0600); err != nil {
		_ = f.Close()
		return err
	}
	if err := securepath.CheckOpenFile(lockPath, f); err != nil {
		_ = f.Close()
		return err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return fmt.Errorf("gateway is already running: %w", err)
	}
	s.lock = f
	busy, _ := time.ParseDuration(c.SQLiteBusyTimeout)
	db, err := store.Open(ctx, c.Paths.Database, store.Options{BusyTimeout: busy})
	if err != nil {
		return err
	}
	// SQLite opens by pathname, so repeat the complete check after open to
	// detect a replacement between validation and database initialization.
	if err := s.validateStartupPaths(c); err != nil {
		_ = db.Close()
		return err
	}
	s.store = db
	// Reconcile uncertainty before any prepared effect can be submitted, then
	// verify Socket readiness before making the local endpoint reachable.
	if err := s.startSlack(ctx, c); err != nil {
		return err
	}
	if s.runtime != nil {
		op, release, err := s.ops()
		if err != nil {
			return err
		}
		_, err = op.Recover(ctx)
		release()
		if err != nil {
			return err
		}
	}
	if err := s.prepareEndpoint(c.Paths.Endpoint); err != nil {
		return err
	}
	addr := &net.UnixAddr{Name: c.Paths.Endpoint, Net: "unix"}
	l, err := net.ListenUnix("unix", addr)
	if err != nil {
		return err
	}
	// Closing a UnixListener otherwise unlinks its current pathname, which could
	// be a socket another process installed after ours was removed.
	l.SetUnlinkOnClose(false)
	if err := os.Chmod(c.Paths.Endpoint, 0600); err != nil {
		_ = l.Close()
		return err
	}
	info, err := os.Lstat(c.Paths.Endpoint)
	if err != nil {
		_ = l.Close()
		return err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || info.Mode()&os.ModeSocket == 0 {
		_ = l.Close()
		return errors.New("gateway endpoint changed while binding")
	}
	s.listener, s.endpoint, s.socketDev, s.socketIno, s.started = l, c.Paths.Endpoint, uint64(st.Dev), st.Ino, time.Now()
	// The endpoint is reachable and recovery has completed; only now may the
	// verified initial Socket candidate dispatch envelopes.
	if s.runtime != nil {
		if err := s.runtime.Activate(); err != nil {
			return err
		}
	}
	s.Log.Info("gateway listening", "endpoint", s.endpoint)
	return nil
}

// validateStartupPaths repeats setup's path checks immediately before and
// after opening state. This closes the configuration-to-gateway race.
func (s *Server) validateStartupPaths(c config.Config) error {
	configPath := s.ConfigLocation.Path
	if configPath == "" {
		configPath = filepath.Join(c.Paths.ConfigDir, config.FileName)
	}
	if err := securepath.CheckAbsoluteComponents(configPath); err != nil {
		return fmt.Errorf("config path: %w", err)
	}
	if err := securepath.CheckFile(configPath); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	for _, path := range []string{c.Paths.StateDir, c.Paths.Database, c.Paths.Endpoint, c.Paths.TmpDir} {
		if err := securepath.CheckAbsoluteComponents(path); err != nil {
			return fmt.Errorf("state path: %w", err)
		}
	}
	if err := securepath.CheckDir(c.Paths.StateDir); err != nil {
		return fmt.Errorf("state directory: %w", err)
	}
	if err := securepath.CheckDir(c.Paths.TmpDir); err != nil {
		return fmt.Errorf("temporary directory: %w", err)
	}
	if err := securepath.CheckFile(c.Paths.Database); err != nil {
		return fmt.Errorf("database: %w", err)
	}
	return nil
}

func (s *Server) prepareEndpoint(endpoint string) error {
	info, err := os.Lstat(endpoint)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || info.Mode()&os.ModeSocket == 0 {
		return errors.New("unsafe existing gateway endpoint")
	}
	if c, err := net.DialTimeout("unix", endpoint, 150*time.Millisecond); err == nil {
		_ = c.Close()
		return errors.New("gateway endpoint is already live")
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != uint32(os.Geteuid()) {
		return errors.New("unsafe stale gateway endpoint")
	}
	return os.Remove(endpoint)
}
func (s *Server) shutdown() {
	s.closeOnce.Do(func() {
		close(s.closing)
		s.admissionMu.Lock()
		s.stopping = true
		if s.listener != nil {
			_ = s.listener.Close()
		}
		s.admissionMu.Unlock()
		done := make(chan struct{})
		go func() { s.wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			s.closeActive()
			<-done
		}
		if s.endpoint != "" {
			// Only remove the socket inode this process bound. A replacement must
			// remain untouched even though the containing directory is private.
			if info, err := os.Lstat(s.endpoint); err == nil {
				if st, ok := info.Sys().(*syscall.Stat_t); ok && info.Mode()&os.ModeSocket != 0 && uint64(st.Dev) == s.socketDev && st.Ino == s.socketIno {
					_ = os.Remove(s.endpoint)
				}
			}
		}
		// Socket intake is stopped and snapshots drained before SQLite closes.
		s.closeSlack()
		if s.store != nil {
			_, _ = s.store.DB().Exec("PRAGMA wal_checkpoint(PASSIVE)")
			_ = s.store.Close()
		}
		if s.lock != nil {
			_ = syscall.Flock(int(s.lock.Fd()), syscall.LOCK_UN)
			_ = s.lock.Close()
		}
	})
}
func sameEUID(c *net.UnixConn) bool {
	raw, err := c.SyscallConn()
	if err != nil {
		return false
	}
	ok := false
	_ = raw.Control(func(fd uintptr) {
		cred, e := unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
		ok = e == nil && cred.Uid == uint32(os.Geteuid())
	})
	return ok
}
func (s *Server) closeActive() {
	s.activeMu.Lock()
	defer s.activeMu.Unlock()
	for c, cancel := range s.active {
		cancel()
		_ = c.Close()
	}
}

func (s *Server) handle(c *net.UnixConn) {
	s.connections.Add(1)
	defer s.connections.Add(-1)
	defer c.Close()
	ctx, cancel := context.WithCancel(context.Background())
	s.activeMu.Lock()
	s.active[c] = cancel
	s.activeMu.Unlock()
	defer func() {
		s.activeMu.Lock()
		delete(s.active, c)
		s.activeMu.Unlock()
		cancel()
	}()
	// A connection retains the immutable configuration snapshot under which it
	// started; connections accepted after reload use the newly active frame bound.
	active := s.snapshot()
	codec := protocol.NewCodec(c, c, uint32(active.LocalProtocolMaximumFrame))
	hello := false
	for {
		_ = c.SetDeadline(time.Now().Add(15 * time.Second))
		payload, err := codec.ReadFrame()
		if err != nil {
			return
		}
		r, err := protocol.DecodeRequest(payload)
		if err != nil {
			// A version mismatch is the one malformed-request case for which the
			// peer needs a typed response. It can then map the incompatibility to
			// exit 9 rather than treating it as a transport outage.
			if r.ProtocolVersion != protocol.Version {
				response := protocol.NewErrorResponse(r.RequestID, s.Version, "unsupported_protocol_version", "unsupported", "protocol version is unsupported", false)
				if out, encodeErr := protocol.EncodeResponse(response); encodeErr == nil {
					_ = codec.WriteFrame(out)
				}
			}
			return
		}
		var response protocol.Response
		if !hello {
			if r.Command != "hello" {
				response = protocol.NewErrorResponse(r.RequestID, s.Version, "hello_required", "usage", "hello is required", false)
			} else {
				response = s.dispatch(ctx, r)
				hello = response.Error == nil
			}
		} else {
			response = s.dispatch(ctx, r)
		}
		out, err := protocol.EncodeResponse(response)
		if err != nil {
			return
		}
		if codec.WriteFrame(out) != nil {
			return
		}
	}
}
func (s *Server) dispatch(ctx context.Context, r protocol.Request) protocol.Response {
	// Reload needs exclusive ownership while it replaces the live SQLite pool.
	// Every other dispatch holds a read lock so it cannot observe a half-swapped
	// store or configuration snapshot.
	if r.Command == "config.reload" {
		result, err := s.reload(ctx, r.RequestID)
		if err != nil {
			return errorResponse(r.RequestID, s.Version, err)
		}
		b, err := json.Marshal(result)
		if err != nil {
			return errorResponse(r.RequestID, s.Version, err)
		}
		return protocol.Response{ProtocolVersion: protocol.Version, RequestID: r.RequestID, ServerVersion: s.Version, ResultType: "config_reload", Result: b}
	}
	s.operationMu.RLock()
	defer s.operationMu.RUnlock()
	resultType := ""
	var result any
	var err error
	switch r.Command {
	case "hello":
		resultType = "hello"
		result = protocol.HelloResult{SelectedVersion: protocol.Version, Capabilities: protocol.ServerCapabilities}
	case "status.get":
		resultType = "gateway_status"
		result, err = s.status(ctx)
	case "doctor.run":
		resultType = "doctor_report"
		checks := s.store.Doctor(ctx)
		checks = append(checks, doctor.Check{Name: "endpoint_peer_compatibility", Status: doctor.Pass, Message: "Unix endpoint accepts same-EUID peers"})
		checks = append(checks, s.phase1Doctor(ctx)...)
		result = doctor.Report{Overall: doctor.Overall(checks), Checks: checks}
	case "trust.list":
		resultType = "trusted_user_list"
		var users []string
		users, err = s.store.ListTrustedUsers(ctx)
		result = protocol.TrustedUserListDTO{SlackUserIDs: users}
	case "trust.add", "trust.remove":
		var p protocol.TrustMutationParams
		_ = json.Unmarshal(r.Params, &p)
		resultType = "trust_mutation"
		var mutation store.TrustMutationResult
		if r.Command == "trust.add" {
			mutation, err = s.store.AddTrustedUser(ctx, r.RequestID, p.SlackUserID)
		} else {
			mutation, err = s.store.RemoveTrustedUser(ctx, r.RequestID, p.SlackUserID)
		}
		result = protocol.TrustMutationDTO{SlackUserID: mutation.SlackUserID, Trusted: mutation.Trusted, Changed: mutation.Changed}
	case "task.create":
		var p protocol.TaskCreateParams
		_ = json.Unmarshal(r.Params, &p)
		resultType = "task_mutation"
		result, err = s.store.Create(ctx, r.RequestID, p.Text)
	case "task.get":
		var p protocol.TaskSelector
		_ = json.Unmarshal(r.Params, &p)
		resultType = "task"
		result, err = s.store.GetTaskReadModel(ctx, p.Task)
	case "file.show":
		var p protocol.FileSelector
		_ = json.Unmarshal(r.Params, &p)
		resultType = "slack_file"
		result, err = s.store.GetSlackFile(ctx, p.File)
	case "slack.history", "slack.thread":
		var op interface {
			History(context.Context, int) (slackapi.MessagePage, error)
			Thread(context.Context, string) (slackapi.MessagePage, error)
		}
		var release func()
		op, release, err = s.ops()
		if err == nil {
			defer release()
		}
		resultType = "slack_message_page"
		if err == nil && r.Command == "slack.history" {
			var p protocol.SlackHistoryParams
			_ = json.Unmarshal(r.Params, &p)
			result, err = op.History(ctx, p.Limit)
		}
		if err == nil && r.Command == "slack.thread" {
			var p protocol.SlackThreadParams
			_ = json.Unmarshal(r.Params, &p)
			result, err = op.Thread(ctx, p.ThreadTS)
		}
	case "slack.send", "slack.edit", "slack.delete", "slack.react", "slack.unreact":
		var op *slackops.Service
		var release func()
		op, release, err = s.ops()
		if err == nil {
			defer release()
			var intent store.SlackEffectIntent
			intent, err = effectIntent(r.Command, r)
			if err == nil {
				result, err = op.Execute(ctx, r.RequestID, intent)
			}
		}
		resultType = "slack_effect"
	case "effect.list":
		var p protocol.EffectListParams
		_ = json.Unmarshal(r.Params, &p)
		resultType = "slack_effect_list"
		var state *store.SlackEffectState
		if p.State != nil {
			x := store.SlackEffectState(*p.State)
			state = &x
		}
		result, err = s.store.ListSlackEffects(ctx, state, p.Limit)
	case "effect.inspect":
		var p protocol.EffectSelector
		_ = json.Unmarshal(r.Params, &p)
		resultType = "effect_inspect"
		var effect store.SlackEffect
		effect, err = s.store.GetSlackEffect(ctx, p.Effect)
		if err == nil {
			result = protocol.EffectInspectDTO{Effect: slackEffectDTO(effect), Intent: protocol.SlackEffectIntentDTO{Text: effect.Text, Emoji: effect.Emoji, OperatorReason: effect.OperatorReason}}
		}
	case "effect.retry":
		var p protocol.EffectRetryParams
		_ = json.Unmarshal(r.Params, &p)
		var op *slackops.Service
		var release func()
		op, release, err = s.ops()
		if err == nil {
			defer release()
			result, err = op.Retry(ctx, r.RequestID, p.Effect, p.Reason, p.ConfirmDuplicateRisk)
		}
		resultType = "slack_effect"
	case "task.list":
		var p protocol.TaskListParams
		_ = json.Unmarshal(r.Params, &p)
		resultType = "task_list"
		var tasks []store.Task
		var next string
		tasks, next, err = s.store.List(ctx, p.State, p.IncludeDone, p.Limit, p.Cursor)
		result = protocol.TaskListDTO{Tasks: taskDTOs(tasks), NextCursor: next}
	case "task.update":
		var p protocol.TaskUpdateParams
		_ = json.Unmarshal(r.Params, &p)
		resultType = "task_mutation"
		result, err = s.store.Update(ctx, r.RequestID, p.Task, p.Text)
	case "task.transition":
		var p protocol.TaskTransitionParams
		_ = json.Unmarshal(r.Params, &p)
		resultType = "task_mutation"
		result, err = s.store.Transition(ctx, r.RequestID, p.Task, task.State(p.State), p.Note)
	default:
		err = errors.New("unsupported command")
	}
	if err != nil {
		return errorResponse(r.RequestID, s.Version, err)
	}
	b, err := json.Marshal(dto(result))
	if err != nil {
		return errorResponse(r.RequestID, s.Version, err)
	}
	return protocol.Response{ProtocolVersion: protocol.Version, RequestID: r.RequestID, ServerVersion: s.Version, ResultType: resultType, Result: b}
}
func (s *Server) reload(ctx context.Context, requestID string) (protocol.ConfigReloadDTO, error) {
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()
	loc := s.ConfigLocation
	if loc.Path == "" {
		var err error
		loc, err = config.DefaultLocation()
		if err != nil {
			return protocol.ConfigReloadDTO{}, &reloadError{message: "reload location is invalid"}
		}
	}
	candidate, digest, loadErr := config.LoadWithDigest(loc)
	if digest == "" {
		// An unreadable or unsafe file has no stable candidate identity to
		// deduplicate. Do not persist a path error that might change underneath us.
		return protocol.ConfigReloadDTO{}, &reloadError{message: "reload candidate cannot be read safely; repair config.toml"}
	}

	s.operationMu.Lock()
	defer s.operationMu.Unlock()

	// Construct and verify every replacement before recording the activation
	// decision. Nothing below publishes these candidates until ReloadConfig has
	// committed its durable success result.
	var replacement *store.Store
	var candidateRuntime *slackruntime.Runtime
	var candidateIngest *slackingest.Coordinator
	candidateState := slackruntime.StateDisabled
	var prepareErr error
	if loadErr != nil {
		prepareErr = &store.ConfigReloadError{Message: "reload candidate is invalid; repair config.toml"}
	} else if err := s.validateStartupPaths(candidate); err != nil {
		prepareErr = &store.ConfigReloadError{Message: "reload candidate paths are unsafe; repair private XDG paths"}
	} else {
		if s.Verifier != nil {
			if err := s.Verifier.Verify(ctx, candidate); err != nil {
				// External verifier errors may contain credentials or Slack responses.
				prepareErr = &store.ConfigReloadError{Message: "reload candidate Slack verification failed; check credentials, workspace, bot, and channel access"}
			}
		}
		if prepareErr == nil && candidate.SQLiteBusyTimeout != s.snapshot().SQLiteBusyTimeout {
			busy, err := time.ParseDuration(candidate.SQLiteBusyTimeout)
			if err != nil {
				prepareErr = &store.ConfigReloadError{Message: "reload candidate has an invalid SQLite busy timeout"}
			} else if replacement, err = store.Open(ctx, candidate.Paths.Database, store.Options{BusyTimeout: busy}); err != nil {
				prepareErr = &store.ConfigReloadError{Message: "reload candidate SQLite busy timeout could not be activated"}
			}
		}
		if prepareErr == nil {
			ledger := s.store
			if replacement != nil {
				ledger = replacement
			}
			candidateRuntime, candidateIngest, candidateState, err = s.prepareSlack(ctx, candidate, ledger)
			if err != nil {
				// Runtime errors are already token-safe, but do not expose adapters'
				// implementation details through the durable reload record.
				prepareErr = &store.ConfigReloadError{Message: "reload candidate Slack verification failed; check credentials, workspace, bot, and channel access"}
			}
		}
	}
	cleanupCandidate := func() {
		if candidateRuntime != nil {
			_ = candidateRuntime.Close()
		}
		if replacement != nil {
			_ = replacement.Close()
		}
	}
	result, replay, err := s.store.ReloadConfig(ctx, requestID, digest, func() (store.ConfigReloadResult, error) {
		if prepareErr != nil {
			return store.ConfigReloadResult{}, prepareErr
		}
		return store.ConfigReloadResult{Activated: true, SlackNetworkVerificationDeferred: s.slackDisabled, ActiveConfigGeneration: s.generation.Load() + 1, ActiveConfigDigest: digest}, nil
	})
	if err != nil {
		cleanupCandidate()
		return protocol.ConfigReloadDTO{}, err
	}
	if replay {
		cleanupCandidate()
		return configReloadDTO(result), nil
	}
	// Publish the complete runtime/config/store frame only after the durable
	// activation decision. operationMu has drained all server operations using
	// the old frame; closing it afterwards drains retained Slack snapshots.
	oldStore, oldRuntime := s.store, s.runtime
	s.store, s.runtime, s.ingest, s.slackState = replacement, candidateRuntime, candidateIngest, candidateState
	if replacement == nil {
		s.store = oldStore
	}
	s.activeConfig.Store(&candidate)
	s.activeDigest = digest
	s.generation.Store(result.ActiveConfigGeneration)
	// The durable decision and complete replacement frame are now visible.
	// Start the candidate intake only after both, before retiring the old one.
	if candidateRuntime != nil {
		if err := candidateRuntime.Activate(); err != nil {
			return protocol.ConfigReloadDTO{}, err
		}
	}
	if oldRuntime != nil {
		_ = oldRuntime.Close()
	}
	if replacement != nil {
		_ = oldStore.Close()
	}
	return configReloadDTO(result), nil
}

func configReloadDTO(result store.ConfigReloadResult) protocol.ConfigReloadDTO {
	return protocol.ConfigReloadDTO{Activated: result.Activated, SlackNetworkVerificationDeferred: result.SlackNetworkVerificationDeferred, ActiveConfigGeneration: result.ActiveConfigGeneration, ActiveConfigDigest: result.ActiveConfigDigest}
}

func (s *Server) status(ctx context.Context) (status.Snapshot, error) {
	counts, err := s.store.Counts(ctx)
	if err != nil {
		return status.Snapshot{}, err
	}
	c := s.snapshot()
	dbSize, err := fileSize(c.Paths.Database)
	if err != nil {
		return status.Snapshot{}, err
	}
	walSize, err := fileSize(c.Paths.Database + "-wal")
	if err != nil {
		return status.Snapshot{}, err
	}
	slack, err := s.phase1Status(ctx)
	if err != nil {
		return status.Snapshot{}, err
	}
	return status.Snapshot{UptimeSeconds: int64(time.Since(s.started).Seconds()), ServerVersion: s.Version, ProtocolVersion: protocol.Version, SchemaVersion: store.SchemaVersion, ActiveConfigGeneration: s.generation.Load(), ActiveConfigDigest: s.activeDigest, PID: os.Getpid(), Database: c.Paths.Database, Endpoint: s.endpoint, TaskCounts: counts, DatabaseBytes: dbSize, WALBytes: walSize, Connections: int(s.connections.Load()), Health: "healthy", EffectsCapability: "available", SlackCapability: string(s.slackState), PiCapability: "not_implemented", Slack: slack}, nil
}
func fileSize(path string) (int64, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

func errorResponse(id, version string, err error) protocol.Response {
	code, category := "invalid_input", "usage"
	switch {
	case errors.Is(err, store.ErrNotFound):
		code, category = "not_found", "not_found"
	case errors.Is(err, store.ErrAmbiguous):
		code, category = "ambiguous", "ambiguous"
	case errors.Is(err, store.ErrRequestConflict):
		code, category = "request_id_conflict", "conflict"
	case errors.Is(err, slackops.ErrUnavailable), errors.Is(err, slackruntime.ErrClosed):
		code, category = "slack_unavailable", "unavailable"
	default:
		var failure *slackops.FailureError
		var reload *reloadError
		var durableReload *store.ConfigReloadError
		if errors.As(err, &failure) {
			if failure.State == store.SlackEffectAmbiguous {
				code, category = "ambiguous_external_outcome", "ambiguous"
			} else {
				code, category = "external_operation_failed", "external_failed"
			}
		} else if errors.As(err, &reload) || errors.As(err, &durableReload) {
			code, category = "config_reload_failed", "unsafe"
		} else if err.Error() == "unsupported command" {
			code, category = "unsupported", "unsupported"
		}
	}
	return protocol.NewErrorResponse(id, version, code, category, err.Error(), false)
}
func dto(v any) any {
	switch x := v.(type) {
	case store.Task:
		return taskDTO(x)
	case store.MutationResult:
		return mutationDTO(x)
	case store.TaskReadModel:
		out := taskDTO(x.Task)
		if x.SlackSource != nil {
			out.SlackSource = slackSourceDTO(*x.SlackSource)
		}
		out.Files = slackFileDTOs(x.Files)
		return out
	case store.SlackFile:
		return slackFileDTO(x)
	case store.SlackEffect:
		return slackEffectDTO(x)
	case []store.SlackEffect:
		out := make([]protocol.SlackEffectDTO, len(x))
		for i := range x {
			out[i] = slackEffectDTO(x[i])
		}
		return protocol.EffectListDTO{Effects: out}
	case slackapi.MessagePage:
		return messagePageDTO(x)
	case []store.Task:
		return protocol.TaskListDTO{Tasks: taskDTOs(x)}
	default:
		return v
	}
}
func taskDTOs(tasks []store.Task) []protocol.TaskDTO {
	a := make([]protocol.TaskDTO, len(tasks))
	for i := range tasks {
		a[i] = taskDTO(tasks[i])
	}
	return a
}
func stamp(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000000Z") }
func taskDTO(t store.Task) protocol.TaskDTO {
	var done *string
	if t.DoneAt != nil {
		d := stamp(*t.DoneAt)
		done = &d
	}
	return protocol.TaskDTO{TaskID: t.ID, Version: t.Version, State: string(t.State), Text: t.Text, SourceKind: t.SourceKind, CreatedAt: stamp(t.CreatedAt), UpdatedAt: stamp(t.UpdatedAt), DoneAt: done}
}
func mutationDTO(m store.MutationResult) protocol.TaskMutationDTO {
	return protocol.TaskMutationDTO{TaskID: m.TaskID, Version: m.Version, State: string(m.State), Changed: m.Changed, CreatedAt: stamp(m.CreatedAt), UpdatedAt: stamp(m.UpdatedAt)}
}
func slackSourceDTO(s store.SlackSource) *protocol.SlackSourceDTO {
	out := &protocol.SlackSourceDTO{InitialSourceEventID: s.InitialSourceEventID, TeamID: s.TeamID, ChannelID: s.ChannelID, AuthorUserID: s.AuthorUserID, MessageTS: s.MessageTS, ThreadTS: s.ThreadTS, Permalink: s.Permalink, LatestEventAt: stamp(time.UnixMicro(s.LatestEventAtUS))}
	if s.SourceDeletedAt != nil {
		v := stamp(*s.SourceDeletedAt)
		out.SourceDeletedAt = &v
	}
	return out
}
func slackFileDTO(f store.SlackFile) protocol.SlackFileDTO {
	return protocol.SlackFileDTO{FileID: f.FileID, SlackFileID: f.SlackFileID, Permalink: f.Permalink, OriginalName: f.OriginalName, DeclaredMIME: f.DeclaredMIME, SizeBytes: f.SizeBytes, RelatedTaskID: f.RelatedTaskID, TransferState: f.TransferState, CreatedAt: stamp(f.CreatedAt), UpdatedAt: stamp(f.UpdatedAt)}
}
func slackFileDTOs(files []store.SlackFile) []protocol.SlackFileDTO {
	out := make([]protocol.SlackFileDTO, len(files))
	for i := range files {
		out[i] = slackFileDTO(files[i])
	}
	return out
}
func slackEffectDTO(e store.SlackEffect) protocol.SlackEffectDTO {
	out := protocol.SlackEffectDTO{EffectID: e.ID, PredecessorEffectID: e.PredecessorEffectID, RelatedTaskID: e.RelatedTaskID, Kind: string(e.Kind), TargetMessageTS: e.TargetMessageTS, ThreadTS: e.ThreadTS, State: string(e.State), AttemptCount: e.AttemptCount, PreparedAt: stamp(e.PreparedAt), ResponseMessageTS: e.ResponseMessageTS, DiagnosticCode: string(e.DiagnosticCode)}
	if e.SubmittedAt != nil {
		v := stamp(*e.SubmittedAt)
		out.SubmittedAt = &v
	}
	if e.CompletedAt != nil {
		v := stamp(*e.CompletedAt)
		out.CompletedAt = &v
	}
	return out
}
func messagePageDTO(p slackapi.MessagePage) protocol.SlackMessagePageDTO {
	out := protocol.SlackMessagePageDTO{NextCursor: p.NextCursor, Messages: make([]protocol.SlackMessageDTO, len(p.Messages))}
	for i, m := range p.Messages {
		d := protocol.SlackMessageDTO{MessageTS: string(m.MessageTS), ThreadTS: string(m.ThreadTS), AuthorUserID: m.AuthorID, Text: m.Text}
		for _, r := range m.Reactions {
			d.Reactions = append(d.Reactions, protocol.SlackReactionDTO{Name: r.Name, Count: r.Count, Mine: r.Mine})
		}
		out.Messages[i] = d
	}
	return out
}

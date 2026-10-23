// Package server serves the private local Mosscap protocol.
package server

import (
	"context"
	"crypto/sha256"
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
	"github.com/stayradiated/mosscap/internal/fileio"
	"github.com/stayradiated/mosscap/internal/filetransfer"
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
// ConfigurationVerifier authenticates proposed Slack credentials and checks
// workspace/channel access before activation. A local-only test gateway may
// omit it; reload then reports network verification as deferred.
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
	temporaryDir   *fileio.TempDir
	fileTransfer   *filetransfer.Service
	// slackDiagnostic records only bounded candidate-verification evidence.
	// It is independent of runtime observations so a failed reload can report
	// that the already-published runtime was retained.
	slackDiagnosticMu     sync.RWMutex
	slackVerificationCode slackapi.DiagnosticCode
	slackVerificationHeld bool
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
		if err := s.startFileTransfer(ctx, s.runtime, c); err != nil {
			return err
		}
		op, release, err := s.ops()
		if err != nil {
			return err
		}
		// The first pass recreates interrupted add intents and recovers them.
		// The second can then prepare and recover dependent removals, but only
		// where the add was confirmed; failed/ambiguous adds remain blocking.
		for pass := 0; pass < 2; pass++ {
			if err = s.reconcileTaskReactions(ctx); err != nil {
				break
			}
			_, err = op.Recover(ctx)
			if err != nil {
				break
			}
		}
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
		// Close active sockets now so in-flight requests finish before the
		// ledger is retired, rather than waiting for the normal grace period.
		s.closeActive()
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
		// The transfer sweeper owns SQLite and its directory descriptor, so it
		// must stop before the Slack runtime and ledger are retired.
		s.closeFileTransfer()
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

type connectionState struct {
	clientKind string
}

func (s *Server) handle(c *net.UnixConn) {
	s.connections.Add(1)
	defer s.connections.Add(-1)
	defer c.Close()
	ctx, cancel := context.WithCancel(context.Background())
	s.activeMu.Lock()
	s.active[c] = cancel
	s.activeMu.Unlock()
	state := &connectionState{}
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
		// A client closing its socket while a request is running must stop
		// cancellable pre-boundary work. Slack post-boundary commits detach
		// cancellation themselves so their durable uncertainty evidence survives.
		requestCtx, requestCancel := context.WithCancel(ctx)
		watchDone := make(chan struct{})
		go watchClientDisconnect(c, watchDone, requestCancel)
		var response protocol.Response
		if !hello {
			if r.Command != "hello" {
				response = protocol.NewErrorResponse(r.RequestID, s.Version, "hello_required", "usage", "hello is required", false)
			} else {
				response = s.dispatchConnection(requestCtx, r, state)
				hello = response.Error == nil
				if hello {
					state.clientKind = r.ClientKind
				}
			}
		} else if r.Command == "hello" {
			response = protocol.NewErrorResponse(r.RequestID, s.Version, "hello_already_completed", "conflict", "hello already completed", false)
		} else if r.ClientKind != state.clientKind {
			response = protocol.NewErrorResponse(r.RequestID, s.Version, "client_kind_changed", "conflict", "client kind is bound to this connection", false)
		} else {
			response = s.dispatchConnection(requestCtx, r, state)
		}
		close(watchDone)
		requestCancel()
		out, err := protocol.EncodeResponse(response)
		if err != nil {
			return
		}
		if codec.WriteFrame(out) != nil {
			return
		}
	}
}

// watchClientDisconnect polls only hangup/error bits, never reads protocol
// bytes, so it can cancel an in-flight dispatch without racing frame parsing.
func watchClientDisconnect(c *net.UnixConn, done <-chan struct{}, cancel context.CancelFunc) {
	raw, err := c.SyscallConn()
	if err != nil {
		return
	}
	var fd int
	if raw.Control(func(value uintptr) { fd = int(value) }) != nil || fd < 0 {
		return
	}
	for {
		select {
		case <-done:
			return
		default:
		}
		poll := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLHUP | unix.POLLERR | unix.POLLRDHUP}}
		if _, err := unix.Poll(poll, 25); err != nil {
			return
		}
		if poll[0].Revents&(unix.POLLHUP|unix.POLLERR|unix.POLLRDHUP) != 0 {
			cancel()
			return
		}
	}
}

// dispatch supports direct unit tests. Socket connections use dispatchConnection
// so their hello/client/session ownership is enforced.
func (s *Server) dispatch(ctx context.Context, r protocol.Request) protocol.Response {
	return s.dispatchConnection(ctx, r, nil)
}

func withoutCapability(capabilities []string, value string) []string {
	out := capabilities[:0]
	for _, capability := range capabilities {
		if capability != value {
			out = append(out, capability)
		}
	}
	return out
}

func (s *Server) dispatchConnection(ctx context.Context, r protocol.Request, connection *connectionState) protocol.Response {
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
		capabilities := append([]string(nil), protocol.ServerCapabilities...)
		if s.fileTransfer == nil {
			capabilities = withoutCapability(capabilities, protocol.CapabilityFileTransfer)
		}
		result = protocol.HelloResult{SelectedVersion: protocol.Version, Capabilities: capabilities}
	case "status.get":
		resultType = "gateway_status"
		result, err = s.status(ctx)
	case "doctor.run":
		resultType = "doctor_report"
		checks := s.store.Doctor(ctx)
		checks = append(checks, doctor.Check{Name: "endpoint_peer_compatibility", Status: doctor.Pass, Message: "Unix endpoint accepts same-EUID peers"})
		checks = append(checks, s.slackDoctor(ctx)...)
		checks = append(checks, s.fileTransferDoctor(ctx)...)
		result = doctor.Report{Overall: doctor.Overall(checks), Checks: checks}
	case "metadata.purge":
		var p protocol.MetadataPurgeParams
		_ = json.Unmarshal(r.Params, &p)
		resultType = "metadata_purge"
		var cutoff time.Time
		cutoff, err = protocol.ParsePurgeCutoff(p.Cutoff)
		if err == nil {
			var purge store.PurgeResult
			if p.Mode == "dry_run" {
				purge, err = s.store.PreviewMetadataPurge(ctx, cutoff)
			} else if p.Mode == "confirm" {
				purge, err = s.store.ConfirmMetadataPurge(ctx, r.RequestID, cutoff)
			} else {
				err = errors.New("invalid metadata purge mode")
			}
			if err == nil {
				result = metadataPurgeDTO(purge, p.Mode)
			}
		}
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
	case "task.context":
		var p protocol.TaskContextParams
		_ = json.Unmarshal(r.Params, &p)
		resultType = "task_context"
		result, err = s.taskContext(ctx, p)
	case "file.show":
		var p protocol.FileSelector
		_ = json.Unmarshal(r.Params, &p)
		resultType = "slack_file"
		result, err = s.store.GetSlackFile(ctx, p.File)
	case "file.get":
		var p protocol.FileGetParams
		_ = json.Unmarshal(r.Params, &p)
		resultType = "file_transfer"
		if s.fileTransfer == nil {
			err = slackops.ErrUnavailable
		} else {
			result, err = s.fileTransfer.Get(ctx, r.RequestID, p.File)
		}
	case "file.release":
		var p protocol.FileReleaseParams
		_ = json.Unmarshal(r.Params, &p)
		resultType = "file_release"
		if s.fileTransfer == nil {
			err = slackops.ErrUnavailable
		} else {
			err = s.fileTransfer.Release(ctx, p.TransferID)
			result = protocol.EmptyParams{}
		}
	case "slack.history", "slack.thread":
		var op *slackops.Service
		var release func()
		op, release, err = s.ops()
		if err == nil {
			defer release()
		}
		resultType = "slack_message_page"
		var channel string
		if err == nil && r.Command == "slack.history" {
			var p protocol.SlackHistoryParams
			_ = json.Unmarshal(r.Params, &p)
			if channel, err = s.resolveChannel(ctx, p.Channel, ""); err == nil {
				result, err = op.History(ctx, channel, p.Limit, p.Cursor)
			}
		}
		if err == nil && r.Command == "slack.thread" {
			var p protocol.SlackThreadParams
			_ = json.Unmarshal(r.Params, &p)
			if channel, err = s.resolveChannel(ctx, p.Channel, p.Task); err == nil {
				result, err = op.Thread(ctx, channel, p.ThreadTS, p.Limit, p.Cursor)
			}
		}
	case "slack.send", "slack.file", "slack.edit", "slack.delete", "slack.react", "slack.unreact":
		var op *slackops.Service
		var release func()
		op, release, err = s.ops()
		if err == nil {
			defer release()
			var channel string
			if r.Command == "slack.file" || r.Command == "slack.send" && hasUploadFiles(r.Params) {
				var intent store.SlackFileUploadIntent
				intent, channel, err = fileUploadIntent(r.Command, r)
				if err == nil {
					channel, err = s.resolveChannel(ctx, channel, intent.RelatedTaskID)
				}
				if err == nil {
					result, err = op.ExecuteFileUpload(ctx, r.RequestID, channel, intent)
				}
			} else {
				var intent store.SlackEffectIntent
				intent, channel, err = effectIntent(r.Command, r)
				if err == nil {
					channel, err = s.resolveChannel(ctx, channel, intent.RelatedTaskID)
				}
				if err == nil {
					result, err = op.Execute(ctx, r.RequestID, channel, intent)
				}
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
		var effects []store.SlackEffect
		var next string
		effects, next, err = s.store.ListSlackEffectsPage(ctx, state, p.Task, p.Limit, p.Cursor)
		if err == nil {
			result = protocol.EffectListDTO{Effects: slackEffectDTOs(effects), NextCursor: next}
		}
	case "effect.inspect":
		var p protocol.EffectSelector
		_ = json.Unmarshal(r.Params, &p)
		resultType = "effect_inspect"
		var effect store.SlackEffect
		effect, err = s.store.GetSlackEffect(ctx, p.Effect)
		if err == nil {
			inspection := protocol.EffectInspectDTO{Effect: slackEffectDTO(effect), Intent: protocol.SlackEffectIntentDTO{Text: effect.Text, Emoji: effect.Emoji, OperatorReason: effect.OperatorReason}}
			if effect.Kind == store.SlackEffectFileUpload {
				var files []store.SlackEffectOutgoingSource
				files, err = s.store.InspectSlackFileUploadSources(ctx, effect.ID)
				if err == nil {
					inspection.OutgoingFiles = fileUploadSourceDTOs(files)
				}
			}
			result = inspection
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
	case "task.changes":
		var p protocol.TaskChangesParams
		_ = json.Unmarshal(r.Params, &p)
		resultType = "task_changes"
		var changes []store.TaskChange
		var head int64
		changes, head, err = s.store.TaskChanges(ctx, p.After, p.Limit)
		if err == nil {
			result = taskChangesDTO(changes, p.After, head)
		}
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
		if err == nil {
			s.projectTransition(ctx, result.(store.MutationResult))
		}
	case "task.reply":
		var p protocol.TaskReplyParams
		_ = json.Unmarshal(r.Params, &p)
		resultType = "task_reply"
		result, err = s.taskReply(ctx, r.RequestID, p)
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
	// Context (and every other response) must fail explicitly rather than let
	// the frame codec silently strand an oversized result at the client.
	candidate := protocol.Response{ProtocolVersion: protocol.Version, RequestID: r.RequestID, ServerVersion: s.Version, ResultType: resultType, Result: b}
	encoded, encodeErr := protocol.EncodeResponse(candidate)
	if encodeErr != nil {
		return errorResponse(r.RequestID, s.Version, encodeErr)
	}
	if maximum := s.snapshot().LocalProtocolMaximumFrame; maximum > 0 && len(encoded) > maximum {
		return protocol.NewErrorResponse(r.RequestID, s.Version, "bounded_result_exceeded", "usage", "result exceeds the configured protocol frame limit", false)
	}
	return candidate
}

// taskReply is deliberately a resumable composition, not a cross-system
// transaction. A deterministic sub-request first durably prepares/executes the
// Slack effect; only its confirmed state permits the separate SQLite transition.
// taskContext combines an authoritative bounded SQLite read with one best-effort
// Slack thread read. Slack failure is represented in the successful component,
// never as a failure of the durable context read.
func (s *Server) taskContext(ctx context.Context, p protocol.TaskContextParams) (protocol.TaskContextDTO, error) {
	model, err := s.store.GetTaskContextReadModel(ctx, p.Task)
	if err != nil {
		return protocol.TaskContextDTO{}, err
	}
	taskValue := dto(model.TaskReadModel).(protocol.TaskDTO)
	out := protocol.TaskContextDTO{Task: taskValue, Thread: protocol.TaskContextThreadDTO{Status: "not_applicable", Messages: []protocol.SlackMessageDTO{}}, Effects: protocol.TaskContextEffectsDTO{NonConfirmed: slackEffectDTOs(model.NonConfirmed), NonConfirmedCount: model.NonConfirmedCount, Truncated: model.NonConfirmedCount > 10}}
	if model.LatestReplyEffect != nil {
		v := slackEffectDTO(*model.LatestReplyEffect)
		out.Effects.LatestReply = &v
	}
	if model.SlackSource == nil {
		return out, nil
	}
	limit := p.ThreadLimit
	if limit == 0 {
		limit = 20
	}
	op, release, opErr := s.ops()
	if opErr != nil {
		out.Thread.Status, out.Thread.DiagnosticCode = "unavailable", "slack_unavailable"
		out.Warnings = []string{"Slack thread was unavailable"}
		return out, nil
	}
	defer release()
	if !op.Allows(model.SlackSource.TeamID, model.SlackSource.ChannelID) {
		out.Thread.Status, out.Thread.DiagnosticCode = "unavailable", "slack_source_unavailable"
		out.Warnings = []string{"Slack thread was unavailable"}
		return out, nil
	}
	page, readErr := op.Thread(ctx, model.SlackSource.ChannelID, model.SlackSource.ThreadTS, limit, "")
	if readErr != nil {
		out.Thread.Status, out.Thread.DiagnosticCode = "unavailable", string(slackapi.Classify(readErr, slackapi.FailureRead).Diagnostic)
		out.Warnings = []string{"Slack thread was unavailable"}
		return out, nil
	}
	messages := messagePageDTO(page).Messages
	// Live reply files have not passed task-source admission. Keep their metadata
	// out of context so callers cannot mistake them for file-get resources.
	for i := range messages {
		messages[i].Files = nil
	}
	out.Thread = protocol.TaskContextThreadDTO{Status: "available", Messages: messages, NextCursor: page.NextCursor, FetchedAt: stamp(time.Now())}
	return out, nil
}

func (s *Server) taskReply(ctx context.Context, requestID string, p protocol.TaskReplyParams) (protocol.TaskReplyDTO, error) {
	source, err := s.store.GetSlackSource(ctx, p.Task)
	if err != nil {
		return protocol.TaskReplyDTO{}, err
	}
	intent := store.SlackEffectIntent{Kind: store.SlackEffectSend, ThreadTS: source.ThreadTS, Text: p.Text, RelatedTaskID: source.TaskID}
	prepared, err := s.store.PrepareSlackEffect(ctx, requestID, store.SlackEffectTarget{TeamID: source.TeamID, ChannelID: source.ChannelID}, intent)
	if err != nil {
		return protocol.TaskReplyDTO{}, err
	}
	effect, err := s.store.GetSlackEffect(ctx, prepared.EffectID)
	if err != nil {
		return protocol.TaskReplyDTO{}, err
	}
	// A prior confirmed effect is sufficient to recover the SQLite half even
	// when the gateway is now unavailable; do not re-preflight or re-send it.
	if effect.State == store.SlackEffectPrepared {
		op, release, opErr := s.ops()
		if opErr != nil {
			return protocol.TaskReplyDTO{}, opErr
		}
		defer release()
		if !op.Allows(source.TeamID, source.ChannelID) {
			return protocol.TaskReplyDTO{}, slackops.ErrUnavailable
		}
		effect, err = op.Execute(ctx, requestID, source.ChannelID, intent)
		if err != nil {
			return protocol.TaskReplyDTO{}, err
		}
	}
	if effect.State == store.SlackEffectFailed || effect.State == store.SlackEffectAmbiguous {
		return protocol.TaskReplyDTO{}, &slackops.FailureError{State: effect.State, Diagnostic: effect.DiagnosticCode}
	}
	out := protocol.TaskReplyDTO{Effect: slackEffectDTO(effect)}
	if !p.Done || effect.State != store.SlackEffectConfirmed {
		return out, nil
	}
	transition, err := s.store.Transition(ctx, taskReplyDoneRequestID(requestID), source.TaskID, task.Done, nil)
	if err != nil {
		return protocol.TaskReplyDTO{}, err
	}
	s.projectTransition(ctx, transition)
	m := mutationDTO(transition)
	out.Task = &m
	return out, nil
}

// taskReplyDoneRequestID maps a valid reply-intent ID to a distinct bounded
// local-completion ID. The caller ID is deliberately the Slack send ID, so it
// remains globally consumed and cannot be reused for another mutation.
func taskReplyDoneRequestID(requestID string) string {
	sum := sha256.Sum256([]byte("mosscap.task.reply.done.v1\x00" + requestID))
	return fmt.Sprintf("req_task_reply_done_%x", sum[:16])
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
	var candidateTemporaryDir *fileio.TempDir
	var candidateFileTransfer *filetransfer.Service
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
				s.recordSlackVerificationFailure(err, s.runtime != nil)
				prepareErr = &store.ConfigReloadError{Message: s.reloadSlackVerificationMessage(err)}
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
			if err == nil && candidateRuntime != nil {
				candidateTemporaryDir, candidateFileTransfer, err = s.prepareFileTransfer(ctx, candidateRuntime, ledger, candidate)
			}
			if err != nil {
				// Runtime errors are already token-safe, but do not expose adapters'
				// implementation details through the durable reload record.
				s.recordSlackVerificationFailure(err, s.runtime != nil)
				prepareErr = &store.ConfigReloadError{Message: s.reloadSlackVerificationMessage(err)}
			}
		}
	}
	cleanupCandidate := func() {
		if candidateFileTransfer != nil {
			candidateFileTransfer.Close()
		}
		if candidateTemporaryDir != nil {
			_ = candidateTemporaryDir.Close()
		}
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
	oldFileTransfer, oldTemporaryDir := s.fileTransfer, s.temporaryDir
	s.store, s.runtime, s.ingest, s.slackState = replacement, candidateRuntime, candidateIngest, candidateState
	s.fileTransfer, s.temporaryDir = candidateFileTransfer, candidateTemporaryDir
	if replacement == nil {
		s.store = oldStore
	}
	s.activeConfig.Store(&candidate)
	s.activeDigest = digest
	s.generation.Store(result.ActiveConfigGeneration)
	s.clearSlackVerificationFailure()
	// The durable decision and complete replacement frame are now visible.
	// Start the candidate intake only after both, before retiring the old one.
	if candidateRuntime != nil {
		if err := candidateRuntime.Activate(); err != nil {
			return protocol.ConfigReloadDTO{}, err
		}
	}
	if oldFileTransfer != nil {
		oldFileTransfer.Close()
	}
	if oldTemporaryDir != nil {
		_ = oldTemporaryDir.Close()
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
	slack, err := s.slackStatus(ctx)
	if err != nil {
		return status.Snapshot{}, err
	}
	transfer, err := s.fileTransferStatus(ctx)
	if err != nil {
		return status.Snapshot{}, err
	}
	return status.Snapshot{UptimeSeconds: int64(time.Since(s.started).Seconds()), ServerVersion: s.Version, ProtocolVersion: protocol.Version, SchemaVersion: store.SchemaVersion, ActiveConfigGeneration: s.generation.Load(), ActiveConfigDigest: s.activeDigest, PID: os.Getpid(), Database: c.Paths.Database, Endpoint: s.endpoint, TaskCounts: counts, DatabaseBytes: dbSize, WALBytes: walSize, Connections: int(s.connections.Load()), Health: "healthy", EffectsCapability: "available", SlackCapability: string(s.slackState), FileTransfer: transfer, Slack: slack}, nil
}
func (s *Server) fileTransferStatus(ctx context.Context) (status.FileTransferStatus, error) {
	out := status.FileTransferStatus{Capability: "unavailable", IncomingByState: map[string]int64{}, OutgoingByState: map[string]int64{}}
	if s.fileTransfer == nil || s.temporaryDir == nil {
		return out, nil
	}
	counts, err := s.store.SlackFileTransferCounts(ctx)
	if err != nil {
		return out, err
	}
	out.Capability, out.TemporaryCount, out.TemporaryBytes = "available", counts.TemporaryCount, counts.TemporaryBytes
	out.IncomingByState, out.OutgoingByState = counts.Incoming, counts.Outgoing
	entries, err := os.ReadDir(s.temporaryDir.Root())
	if err != nil {
		return out, err
	}
	live, err := s.store.ListFileTemporaryTransfers(ctx, 1000)
	if err != nil {
		return out, err
	}
	known := make(map[string]struct{}, len(live))
	var nearest time.Time
	for _, transfer := range live {
		known[transfer.GeneratedName] = struct{}{}
		if nearest.IsZero() || transfer.ExpiresAt.Before(nearest) {
			nearest = transfer.ExpiresAt
		}
	}
	for _, entry := range entries {
		name := entry.Name()
		if !fileio.IsGeneratedName(name) {
			out.UnsafeTemporaryEntries++
			continue
		}

		// Open through the retained directory descriptor rather than trusting a
		// pathname inspection. TempDir.Open refuses links and verifies regular
		// type, effective owner, exact 0600 mode, and a single link before status
		// or doctor reports this temporary entry as safe.
		file, err := s.temporaryDir.Open(name)
		if err != nil {
			out.UnsafeTemporaryEntries++
			continue
		}
		if err := file.Close(); err != nil {
			out.UnsafeTemporaryEntries++
			continue
		}
		if _, ok := known[name]; !ok {
			out.UnsafeTemporaryEntries++
		}
	}
	if !nearest.IsZero() {
		v := stamp(nearest)
		out.NearestExpiry = &v
	}
	return out, nil
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
	// Adapter diagnostics are bounded and secret-safe. Preserve the file
	// transfer diagnostics that have distinct CLI safety semantics instead of
	// flattening them into a generic external failure.
	if failure := slackapi.Classify(err, slackapi.FailureRead); failure.Diagnostic == slackapi.DiagnosticUnsafeFileRedirect {
		return protocol.NewErrorResponse(id, version, "unsafe_file_redirect", "unsafe", err.Error(), false)
	} else if failure.Diagnostic == slackapi.DiagnosticSlackFileUnavailable {
		return protocol.NewErrorResponse(id, version, "slack_file_unavailable", "external_failed", err.Error(), false)
	} else if failure.Diagnostic == slackapi.DiagnosticFileTooLarge {
		return protocol.NewErrorResponse(id, version, "file_too_large", "usage", err.Error(), false)
	}
	code, category := "invalid_input", "usage"
	switch {
	case errors.Is(err, store.ErrNotFound), errors.Is(err, store.ErrFileTransferNotFound):
		code, category = "not_found", "not_found"
	case errors.Is(err, store.ErrFileTransferExpired), errors.Is(err, filetransfer.ErrExpired):
		code, category = "file_transfer_expired", "not_found"
	case errors.Is(err, filetransfer.ErrCapacity):
		code, category = "temporary_capacity", "unavailable"
	case errors.Is(err, filetransfer.ErrTooLarge), errors.Is(err, fileio.ErrTooLarge):
		code, category = "file_too_large", "usage"
	case errors.Is(err, filetransfer.ErrContentChanged), errors.Is(err, fileio.ErrChanged):
		code, category = "file_content_changed", "conflict"
	case errors.Is(err, filetransfer.ErrNotIncoming):
		code, category = "invalid_input", "usage"
	case errors.Is(err, fileio.ErrUnsafeFileType):
		code, category = "unsafe_file_type", "unsafe"
	case errors.Is(err, filetransfer.ErrUnsafeTemporary), errors.Is(err, fileio.ErrUnsafePath), errors.Is(err, fileio.ErrInvalidTemporary):
		code, category = "unsafe_file_path", "unsafe"
	case errors.Is(err, fileio.ErrDestinationExists):
		code, category = "destination_exists", "conflict"
	case errors.Is(err, fileio.ErrUnsupported):
		code, category = "unsupported", "unsupported"
	case errors.Is(err, store.ErrAmbiguous):
		code, category = "ambiguous", "ambiguous"
	case errors.Is(err, store.ErrRequestConflict):
		code, category = "request_id_conflict", "conflict"
	case errors.Is(err, store.ErrCursorExpired):
		code, category = "cursor_expired", "conflict"
	case errors.Is(err, store.ErrTaskChangeCursorAhead):
		code, category = "cursor_ahead", "conflict"
	case func() bool { var purge *store.PurgeError; return errors.As(err, &purge) }():
		var purge *store.PurgeError
		_ = errors.As(err, &purge)
		return protocol.Response{ProtocolVersion: protocol.Version, RequestID: id, ServerVersion: version, Error: &protocol.Error{Code: "purge_interrupted", Category: "unavailable", Message: "metadata purge interrupted; retry safely", Retriable: true, Progress: &protocol.PurgeProgressDTO{Removed: purgeCountsDTO(purge.Progress.Removed)}}}
	case errors.Is(err, slackops.ErrInvalidTarget):
		code, category = "invalid_channel", "usage"
	case errors.Is(err, ErrChannelRequired):
		code, category = "channel_required", "usage"
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
		if x.SlackSource != nil {
			out.SlackActions = slackTaskActionsDTO(*x.SlackSource, x.LatestReplyEffect, x.Task.State)
		}
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
	case filetransfer.Result:
		return fileTransferDTO(x)
	case []store.Task:
		return protocol.TaskListDTO{Tasks: taskDTOs(x)}
	default:
		return v
	}
}
func metadataPurgeDTO(result store.PurgeResult, mode string) protocol.MetadataPurgeDTO {
	return protocol.MetadataPurgeDTO{
		Cutoff: stamp(result.Cutoff), Mode: mode,
		Eligible: purgeCountsDTO(result.Eligible), SkippedLive: purgeCountsDTO(result.SkippedLive),
		SkippedReferenced: purgeCountsDTO(result.SkippedReferenced), Removed: purgeCountsDTO(result.Removed),
	}
}
func purgeCountsDTO(c store.PurgeCounts) protocol.PurgeCountsDTO {
	return protocol.PurgeCountsDTO{Tasks: c.Tasks, TaskStateEvents: c.TaskStateEvents, SlackSourceEvents: c.SlackSourceEvents, SlackTaskSources: c.SlackTaskSources, SlackEffects: c.SlackEffects, Files: c.Files, FileTemporaryTransfers: c.FileTemporaryTransfers, MutationRequests: c.MutationRequests}
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
func taskChangesDTO(changes []store.TaskChange, after, head int64) protocol.TaskChangesDTO {
	out := protocol.TaskChangesDTO{Changes: make([]protocol.TaskChangeDTO, len(changes)), NextAfter: after, Head: head}
	for i, c := range changes {
		task := taskDTO(c.Task)
		if c.SlackSource != nil {
			task.SlackSource = slackSourceDTO(*c.SlackSource)
		}
		out.Changes[i] = protocol.TaskChangeDTO{ChangeSeq: c.Seq, Change: string(c.Kind), Task: task}
		out.NextAfter = c.Seq
	}
	return out
}
func mutationDTO(m store.MutationResult) protocol.TaskMutationDTO {
	return protocol.TaskMutationDTO{TaskID: m.TaskID, Version: m.Version, State: string(m.State), Changed: m.Changed, CreatedAt: stamp(m.CreatedAt), UpdatedAt: stamp(m.UpdatedAt)}
}
func slackTaskActionsDTO(source store.SlackSource, effect *store.SlackEffect, state task.State) *protocol.SlackTaskActionsDTO {
	out := &protocol.SlackTaskActionsDTO{
		ReplyTarget:      protocol.SlackReplyTargetDTO{TeamID: source.TeamID, ChannelID: source.ChannelID, ThreadTS: source.ThreadTS},
		ReplyStatus:      "none",
		CompletionStatus: "not_done",
	}
	if state == task.Done {
		out.CompletionStatus = "done"
	}
	if effect != nil {
		out.ReplyStatus = string(effect.State)
		out.ReplyEffectID = effect.ID
	}
	return out
}

func slackSourceDTO(s store.SlackSource) *protocol.SlackSourceDTO {
	out := &protocol.SlackSourceDTO{InitialSourceEventID: s.InitialSourceEventID, TeamID: s.TeamID, ChannelID: s.ChannelID, AuthorUserID: s.AuthorUserID, MessageTS: s.MessageTS, ThreadTS: s.ThreadTS, Permalink: s.Permalink, ParentTaskID: s.ParentTaskID, LatestEventAt: stamp(time.UnixMicro(s.LatestEventAtUS))}
	if s.SourceDeletedAt != nil {
		v := stamp(*s.SourceDeletedAt)
		out.SourceDeletedAt = &v
	}
	return out
}
func slackFileDTO(f store.SlackFile) protocol.SlackFileDTO {
	out := protocol.SlackFileDTO{FileID: f.FileID, Direction: f.Direction, SlackFileID: f.SlackFileID, Permalink: f.Permalink, OriginalName: f.OriginalName, DeclaredMIME: f.DeclaredMIME, DeclaredSizeBytes: f.DeclaredSizeBytes, ValidatedSizeBytes: f.ValidatedSizeBytes, SHA256: f.SHA256, RelatedTaskID: f.RelatedTaskID, RelatedEffectID: f.RelatedEffectID, EffectOrdinal: f.EffectOrdinal, SourceAttached: f.SourceAttached, TransferState: f.TransferState, DiagnosticCode: f.DiagnosticCode, CreatedAt: stamp(f.CreatedAt), UpdatedAt: stamp(f.UpdatedAt)}
	if f.SourceDetachedAt != nil {
		v := stamp(*f.SourceDetachedAt)
		out.SourceDetachedAt = &v
	}
	return out
}
func fileTransferDTO(result filetransfer.Result) protocol.FileGetDTO {
	return protocol.FileGetDTO{TransferID: result.TransferID, FileID: result.FileID, TemporaryPath: result.Path, ValidatedSizeBytes: result.SizeBytes, SHA256: result.SHA256, ExpiresAt: stamp(result.ExpiresAt)}
}
func fileUploadSourceDTOs(files []store.SlackEffectOutgoingSource) []protocol.FileUploadSourceDTO {
	out := make([]protocol.FileUploadSourceDTO, len(files))
	for i, file := range files {
		out[i] = protocol.FileUploadSourceDTO{FileID: file.FileID, SourcePath: file.SourcePath, OriginalName: file.OriginalName, SizeBytes: file.SizeBytes, SHA256: file.SHA256}
	}
	return out
}

// slackFileDTOs is the metadata-list projection for task/read results. Hashes,
// output paths, and outgoing intent details remain available only through the
// explicit file/effect inspection routes.
func slackFileDTOs(files []store.SlackFile) []protocol.SlackFileDTO {
	out := make([]protocol.SlackFileDTO, len(files))
	for i := range files {
		out[i] = slackFileDTO(files[i])
		out[i].SHA256, out[i].RelatedEffectID, out[i].EffectOrdinal = "", "", nil
		out[i].ValidatedSizeBytes = nil
	}
	return out
}
func slackEffectDTOs(effects []store.SlackEffect) []protocol.SlackEffectDTO {
	out := make([]protocol.SlackEffectDTO, len(effects))
	for i := range effects {
		out[i] = slackEffectDTO(effects[i])
	}
	return out
}

func slackEffectDTO(e store.SlackEffect) protocol.SlackEffectDTO {
	out := protocol.SlackEffectDTO{EffectID: e.ID, PredecessorEffectID: e.PredecessorEffectID, RelatedTaskID: e.RelatedTaskID, ChannelID: e.ChannelID, Kind: string(e.Kind), TargetMessageTS: e.TargetMessageTS, ThreadTS: e.ThreadTS, State: string(e.State), AttemptCount: e.AttemptCount, PreparedAt: stamp(e.PreparedAt), ResponseMessageTS: e.ResponseMessageTS, DiagnosticCode: string(e.DiagnosticCode)}
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
		for _, forwarded := range m.Forwarded {
			d.Forwarded = append(d.Forwarded, protocol.SlackForwardedMessageDTO{Text: forwarded.Text, ChannelID: forwarded.ChannelID, MessageTS: string(forwarded.MessageTS)})
		}
		out.Messages[i] = d
	}
	return out
}

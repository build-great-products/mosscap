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
	"unicode/utf8"

	"github.com/stayradiated/mosscap/internal/config"
	"github.com/stayradiated/mosscap/internal/doctor"
	"github.com/stayradiated/mosscap/internal/fileio"
	"github.com/stayradiated/mosscap/internal/filetransfer"
	"github.com/stayradiated/mosscap/internal/pidelivery"
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
	connectionIDs  atomic.Int64
	piDelivery     *pidelivery.Coordinator
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
	s.piDelivery = pidelivery.New(db)
	// Delivery uncertainty is reconciled before the endpoint is reachable.
	if _, err := s.piDelivery.Recover(ctx); err != nil {
		return err
	}
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
		// An opened extension intentionally has no idle deadline. Close active
		// sockets now so their deferred delivery reconciliation completes before
		// the ledger is retired, rather than waiting for the normal grace period.
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
	connectionID string
	clientKind   string
	opened       bool
}

func (s *Server) handle(c *net.UnixConn) {
	s.connections.Add(1)
	defer s.connections.Add(-1)
	defer c.Close()
	ctx, cancel := context.WithCancel(context.Background())
	s.activeMu.Lock()
	s.active[c] = cancel
	s.activeMu.Unlock()
	state := &connectionState{connectionID: fmt.Sprintf("connection-%d", s.connectionIDs.Add(1))}
	defer func() {
		if s.piDelivery != nil {
			_, _ = s.piDelivery.Close(context.Background(), state.connectionID)
		}
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
		if state.opened {
			_ = c.SetDeadline(time.Time{})
		} else {
			_ = c.SetDeadline(time.Now().Add(15 * time.Second))
		}
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
				response = s.dispatchConnection(ctx, r, state)
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
			response = s.dispatchConnection(ctx, r, state)
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
	if connection != nil {
		extensionCommand := r.Command == "delivery.open" || r.Command == "delivery.poll" || r.Command == "delivery.inserting" || r.Command == "delivery.persisted" || r.Command == "delivery.rejected" || r.Command == "delivery.close"
		operatorCommand := r.Command == "delivery.list" || r.Command == "delivery.inspect" || r.Command == "delivery.requeue"
		if extensionCommand && connection.clientKind != "extension" {
			return protocol.NewErrorResponse(r.RequestID, s.Version, "extension_client_required", "unsafe", "delivery extension command requires extension client", false)
		}
		if operatorCommand && connection.clientKind != "cli" {
			return protocol.NewErrorResponse(r.RequestID, s.Version, "operator_client_required", "unsafe", "delivery operator command requires CLI client", false)
		}
		if extensionCommand && r.Command != "delivery.open" && !connection.opened {
			return protocol.NewErrorResponse(r.RequestID, s.Version, "delivery_open_required", "conflict", "delivery open is required", false)
		}
		if connection.clientKind == "extension" && !extensionCommand && r.Command != "status.get" {
			return protocol.NewErrorResponse(r.RequestID, s.Version, "operator_client_required", "unsafe", "operator command requires CLI client", false)
		}
	}
	switch r.Command {
	case "hello":
		resultType = "hello"
		capabilities := append([]string(nil), protocol.ServerCapabilities...)
		if s.fileTransfer == nil {
			capabilities = withoutCapability(capabilities, protocol.CapabilityFileTransfer)
		}
		capabilities = append(capabilities, protocol.CapabilityPiDeliveryMVP)
		result = protocol.HelloResult{SelectedVersion: protocol.Version, Capabilities: capabilities}
	case "status.get":
		resultType = "gateway_status"
		result, err = s.status(ctx)
	case "delivery.open":
		var p protocol.DeliveryOpenParams
		_ = json.Unmarshal(r.Params, &p)
		resultType = "delivery_open"
		if s.piDelivery == nil || connection == nil {
			err = errors.New("Pi delivery coordinator is unavailable")
			break
		}
		observed := make([]store.DeliveryObservation, len(p.Observed))
		for i, item := range p.Observed {
			observed[i] = store.DeliveryObservation{DeliveryID: item.DeliveryID, DeliveryKey: item.DeliveryKey}
		}
		var counts store.DeliveryCounts
		counts, err = s.piDelivery.Open(ctx, r.RequestID, connection.connectionID, p.PiSessionID, observed)
		if err == nil {
			connection.opened = true
			result = protocol.DeliveryOpenDTO{PiSessionID: p.PiSessionID, DeliveryCountsDTO: deliveryCountsDTO(counts)}
		}
	case "delivery.poll":
		resultType = "delivery_poll"
		var work *store.DeliveryWork
		var counts store.DeliveryCounts
		if s.piDelivery == nil || connection == nil {
			err = errors.New("Pi delivery coordinator is unavailable")
		} else {
			work, counts, err = s.piDelivery.Poll(ctx, connection.connectionID)
			if err == nil {
				var attached []store.SlackFile
				if work != nil && work.Task.SourceKind == "slack" {
					attached, err = s.store.ListSlackFiles(ctx, work.Task.ID)
				}
				if err == nil {
					result = protocol.DeliveryPollDTO{Work: deliveryWorkDTO(work, attached), DeliveryCountsDTO: deliveryCountsDTO(counts)}
				}
			}
		}
	case "delivery.inserting", "delivery.persisted", "delivery.rejected":
		resultType = "delivery"
		var id, key, diagnostic string
		switch r.Command {
		case "delivery.inserting", "delivery.persisted":
			var p protocol.DeliveryInsertingParams
			_ = json.Unmarshal(r.Params, &p)
			id, key = p.DeliveryID, p.DeliveryKey
		case "delivery.rejected":
			var p protocol.DeliveryRejectedParams
			_ = json.Unmarshal(r.Params, &p)
			id, key, diagnostic = p.DeliveryID, p.DeliveryKey, p.DiagnosticCode
		}
		var delivery store.Delivery
		if s.piDelivery == nil || connection == nil {
			err = errors.New("Pi delivery coordinator is unavailable")
		} else if r.Command == "delivery.inserting" {
			delivery, err = s.piDelivery.Inserting(ctx, r.RequestID, connection.connectionID, id, key)
		} else if r.Command == "delivery.persisted" {
			delivery, err = s.piDelivery.Persisted(ctx, r.RequestID, connection.connectionID, id, key)
		} else {
			delivery, err = s.piDelivery.Rejected(ctx, r.RequestID, connection.connectionID, id, key, diagnostic)
		}
		if err == nil {
			result = deliveryDTO(delivery)
		}
	case "delivery.close":
		resultType = "delivery_close"
		if s.piDelivery == nil || connection == nil {
			err = errors.New("Pi delivery coordinator is unavailable")
		} else {
			_, err = s.piDelivery.Close(ctx, connection.connectionID)
			if err == nil {
				connection.opened = false
				result = protocol.EmptyParams{}
			}
		}
	case "delivery.list":
		var p protocol.DeliveryListParams
		_ = json.Unmarshal(r.Params, &p)
		resultType = "delivery_list"
		var state *store.DeliveryState
		if p.State != nil {
			v := store.DeliveryState(*p.State)
			state = &v
		}
		var deliveries []store.Delivery
		deliveries, err = s.store.ListDeliveries(ctx, state, p.Limit)
		if err == nil {
			result = protocol.DeliveryListDTO{Deliveries: deliveryDTOs(deliveries)}
		}
	case "delivery.inspect":
		var p protocol.DeliverySelector
		_ = json.Unmarshal(r.Params, &p)
		resultType = "delivery"
		var delivery store.Delivery
		delivery, err = s.store.GetDelivery(ctx, p.Delivery)
		if err == nil {
			result = deliveryDTO(delivery)
		}
	case "delivery.requeue":
		var p protocol.DeliveryRequeueParams
		_ = json.Unmarshal(r.Params, &p)
		resultType = "delivery_requeue"
		var delivery store.Delivery
		delivery, err = s.store.RequeueDelivery(ctx, r.RequestID, p.Delivery, p.Reason)
		if err == nil {
			result = protocol.DeliveryRequeueDTO{Delivery: deliveryDTO(delivery), DuplicateInsertionPossible: true}
		}
	case "doctor.run":
		resultType = "doctor_report"
		checks := s.store.Doctor(ctx)
		checks = append(checks, doctor.Check{Name: "endpoint_peer_compatibility", Status: doctor.Pass, Message: "Unix endpoint accepts same-EUID peers"})
		checks = append(checks, s.phase1Doctor(ctx)...)
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
	case "trust.list":
		resultType = "trusted_user_list"
		var users []string
		users, err = s.store.ListTrustedUsers(ctx)
		result = protocol.TrustedUserListDTO{SlackUserIDs: users, SlackUserNames: s.slackUserNames(ctx, users)}
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
	case "slack.send", "slack.file", "slack.edit", "slack.delete", "slack.react", "slack.unreact":
		var op *slackops.Service
		var release func()
		op, release, err = s.ops()
		if err == nil {
			defer release()
			if r.Command == "slack.file" || r.Command == "slack.send" && hasUploadFiles(r.Params) {
				var intent store.SlackFileUploadIntent
				intent, err = fileUploadIntent(r.Command, r)
				if err == nil {
					result, err = op.ExecuteFileUpload(ctx, r.RequestID, intent)
				}
			} else {
				var intent store.SlackEffectIntent
				intent, err = effectIntent(r.Command, r)
				if err == nil {
					result, err = op.Execute(ctx, r.RequestID, intent)
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
		result, err = s.store.ListSlackEffects(ctx, state, p.Limit)
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
	return protocol.Response{ProtocolVersion: protocol.Version, RequestID: r.RequestID, ServerVersion: s.Version, ResultType: resultType, Result: b}
}

// taskReply is deliberately a resumable composition, not a cross-system
// transaction. A deterministic sub-request first durably prepares/executes the
// Slack effect; only its confirmed state permits the separate SQLite transition.
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
		if !op.MatchesTarget(source.TeamID, source.ChannelID) {
			return protocol.TaskReplyDTO{}, slackops.ErrUnavailable
		}
		effect, err = op.Execute(ctx, requestID, intent)
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
	if replacement != nil && s.piDelivery != nil && prepareErr == nil {
		// Reconcile against the old ledger before a replacement can be published
		// or the old SQLite handle is closed.
		if _, err := s.piDelivery.ReconcileActive(ctx); err != nil {
			cleanupCandidate()
			return protocol.ConfigReloadDTO{}, err
		}
		s.closeActive()
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
	if s.piDelivery != nil {
		s.piDelivery.ReplaceStore(s.store)
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
	slack, err := s.phase1Status(ctx)
	if err != nil {
		return status.Snapshot{}, err
	}
	transfer, err := s.fileTransferStatus(ctx)
	if err != nil {
		return status.Snapshot{}, err
	}
	piCapability := "not_implemented"
	pi := status.PiDeliveryStatus{}
	if s.piDelivery != nil {
		piCapability = "available"
		current, err := s.piDelivery.Status(ctx)
		if err != nil {
			return status.Snapshot{}, err
		}
		pi = status.PiDeliveryStatus{ActivePiSessionID: current.ActivePiSessionID, Pending: current.Pending, Inserting: current.Inserting, Persisted: current.Persisted, Uncertain: current.Uncertain, Rejected: current.Rejected}
	}
	return status.Snapshot{UptimeSeconds: int64(time.Since(s.started).Seconds()), ServerVersion: s.Version, ProtocolVersion: protocol.Version, SchemaVersion: store.SchemaVersion, ActiveConfigGeneration: s.generation.Load(), ActiveConfigDigest: s.activeDigest, PID: os.Getpid(), Database: c.Paths.Database, Endpoint: s.endpoint, TaskCounts: counts, DatabaseBytes: dbSize, WALBytes: walSize, Connections: int(s.connections.Load()), Health: "healthy", EffectsCapability: "available", SlackCapability: string(s.slackState), PiCapability: piCapability, PiDelivery: pi, FileTransfer: transfer, Slack: slack}, nil
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
	case func() bool { var purge *store.PurgeError; return errors.As(err, &purge) }():
		var purge *store.PurgeError
		_ = errors.As(err, &purge)
		return protocol.Response{ProtocolVersion: protocol.Version, RequestID: id, ServerVersion: version, Error: &protocol.Error{Code: "purge_interrupted", Category: "unavailable", Message: "metadata purge interrupted; retry safely", Retriable: true, Progress: &protocol.PurgeProgressDTO{Removed: purgeCountsDTO(purge.Progress.Removed)}}}
	case errors.Is(err, store.ErrDeliveryNotFound):
		code, category = "delivery_not_found", "not_found"
	case errors.Is(err, store.ErrDeliveryOwnership), errors.Is(err, store.ErrDeliveryTransition), errors.Is(err, store.ErrDeliveryRequeueDenied), errors.Is(err, pidelivery.ErrNotOpen), errors.Is(err, pidelivery.ErrSessionBound):
		code, category = "delivery_conflict", "conflict"
	case errors.Is(err, slackops.ErrUnavailable), errors.Is(err, slackruntime.ErrClosed):
		code, category = "slack_unavailable", "unavailable"
	default:
		var failure *slackops.FailureError
		var reload *reloadError
		var durableReload *store.ConfigReloadError
		var active *pidelivery.ActiveSessionError
		if errors.As(err, &active) {
			return protocol.Response{ProtocolVersion: protocol.Version, RequestID: id, ServerVersion: version, Error: &protocol.Error{Code: "pi_session_active", Category: "conflict", Message: "another Pi delivery session is active", Retriable: false, Details: map[string]string{"active_pi_session_id": active.PiSessionID}}}
		} else if errors.As(err, &failure) {
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
	return protocol.PurgeCountsDTO{Tasks: c.Tasks, TaskStateEvents: c.TaskStateEvents, SlackSourceEvents: c.SlackSourceEvents, SlackTaskSources: c.SlackTaskSources, PiDeliveries: c.PiDeliveries, SlackEffects: c.SlackEffects, Files: c.Files, FileTemporaryTransfers: c.FileTemporaryTransfers, MutationRequests: c.MutationRequests}
}
func deliveryCountsDTO(c store.DeliveryCounts) protocol.DeliveryCountsDTO {
	return protocol.DeliveryCountsDTO{EligibleBacklogCount: c.Eligible, UncertainCount: c.Uncertain, RejectedCount: c.Rejected}
}
func deliveryDTO(d store.Delivery) protocol.DeliveryDTO {
	out := protocol.DeliveryDTO{DeliveryID: d.ID, TaskID: d.TaskID, TaskVersion: d.TaskVersion, PiSessionID: d.PiSessionID, DeliveryKey: d.DeliveryKey, PredecessorDeliveryID: d.PredecessorDeliveryID, State: string(d.State), DiagnosticCode: d.DiagnosticCode, RequeueReason: d.RequeueReason, CreatedAt: stamp(d.CreatedAt), UpdatedAt: stamp(d.UpdatedAt)}
	if d.OfferedAt != nil {
		v := stamp(*d.OfferedAt)
		out.OfferedAt = &v
	}
	if d.InsertingAt != nil {
		v := stamp(*d.InsertingAt)
		out.InsertingAt = &v
	}
	if d.CompletedAt != nil {
		v := stamp(*d.CompletedAt)
		out.CompletedAt = &v
	}
	return out
}
func deliveryDTOs(deliveries []store.Delivery) []protocol.DeliveryDTO {
	out := make([]protocol.DeliveryDTO, len(deliveries))
	for i := range deliveries {
		out[i] = deliveryDTO(deliveries[i])
	}
	return out
}
func deliveryWorkDTO(work *store.DeliveryWork, files []store.SlackFile) *protocol.DeliveryWorkDTO {
	if work == nil {
		return nil
	}
	out := &protocol.DeliveryWorkDTO{DeliveryID: work.Delivery.ID, DeliveryKey: work.Delivery.DeliveryKey, TaskID: work.Task.ID, TaskVersion: work.Task.Version, TaskState: string(work.Task.State), TaskText: work.Task.Text, SourceKind: work.Task.SourceKind}
	if work.Slack != nil {
		out.SlackSource = &protocol.DeliverySlackSourceDTO{AuthorUserID: work.Slack.AuthorUserID, MessageTS: work.Slack.MessageTS, ThreadTS: work.Slack.ThreadTS}
	}
	for _, file := range files {
		// ListSlackFiles already returns attached incoming rows. Keep this guard at
		// the delivery boundary so a future repository query cannot leak detached
		// or outgoing intent metadata into a persisted Pi message.
		if len(out.AttachedFiles) == 20 || !validDeliveryAttachedFile(file) {
			continue
		}
		var mime *string
		if file.DeclaredMIME != "" {
			value := file.DeclaredMIME
			mime = &value
		}
		out.AttachedFiles = append(out.AttachedFiles, protocol.DeliveryAttachedFileDTO{FileID: file.FileID, OriginalName: file.OriginalName, DeclaredMIME: mime, DeclaredSizeBytes: file.DeclaredSizeBytes, TransferState: file.TransferState})
	}
	return out
}

func validDeliveryAttachedFile(file store.SlackFile) bool {
	return file.Direction == "from_slack" && file.SourceAttached && len(file.OriginalName) <= 1024 && utf8.ValidString(file.OriginalName) && len(file.DeclaredMIME) <= 255 && utf8.ValidString(file.DeclaredMIME) && (file.DeclaredSizeBytes == nil || *file.DeclaredSizeBytes >= 0) && deliveryIncomingTransferState(file.TransferState)
}

func deliveryIncomingTransferState(value string) bool {
	switch value {
	case "metadata_only", "validated", "unavailable", "oversized", "content_changed":
		return true
	default:
		return false
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

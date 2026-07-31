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
	"github.com/stayradiated/mosscap/internal/status"
	"github.com/stayradiated/mosscap/internal/store"
	"github.com/stayradiated/mosscap/internal/task"
	"golang.org/x/sys/unix"
)

// Server owns one open ledger and its Unix endpoint.
type Server struct {
	Config      config.Config
	Version     string
	Log         *slog.Logger
	store       *store.Store
	listener    *net.UnixListener
	lock        *os.File
	endpoint    string
	wg          sync.WaitGroup
	closing     chan struct{}
	closeOnce   sync.Once
	started     time.Time
	connections atomic.Int32
}

func New(c config.Config, version string, log *slog.Logger) *Server {
	if version == "" {
		version = "devel"
	}
	if log == nil {
		log = slog.Default()
	}
	return &Server{Config: c, Version: version, Log: log, closing: make(chan struct{})}
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
		s.wg.Add(1)
		go func() { defer s.wg.Done(); s.handle(conn) }()
	}
}
func (s *Server) open(ctx context.Context) error {
	c := s.Config
	if err := config.Validate(c); err != nil {
		return err
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
	lockPath := filepath.Join(c.Paths.StateDir, "gateway.lock")
	f, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return err
	}
	if err := f.Chmod(0600); err != nil {
		_ = f.Close()
		return err
	}
	if err := securepath.CheckFile(lockPath); err != nil {
		_ = f.Close()
		return err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return fmt.Errorf("gateway is already running: %w", err)
	}
	s.lock = f
	busy, _ := time.ParseDuration(c.Database.BusyTimeout)
	db, err := store.Open(ctx, c.Paths.Database, store.Options{BusyTimeout: busy})
	if err != nil {
		return err
	}
	s.store = db
	if err := s.prepareEndpoint(c.Paths.Endpoint); err != nil {
		return err
	}
	addr := &net.UnixAddr{Name: c.Paths.Endpoint, Net: "unix"}
	l, err := net.ListenUnix("unix", addr)
	if err != nil {
		return err
	}
	if err := os.Chmod(c.Paths.Endpoint, 0600); err != nil {
		_ = l.Close()
		return err
	}
	s.listener, s.endpoint, s.started = l, c.Paths.Endpoint, time.Now()
	s.Log.Info("gateway listening", "endpoint", s.endpoint)
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
		if s.listener != nil {
			_ = s.listener.Close()
		}
		done := make(chan struct{})
		go func() { s.wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
		if s.endpoint != "" {
			_ = os.Remove(s.endpoint)
		}
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
func (s *Server) handle(c *net.UnixConn) {
	s.connections.Add(1)
	defer s.connections.Add(-1)
	defer c.Close()
	codec := protocol.NewCodec(c, c, uint32(s.Config.Protocol.MaxFrameBytes))
	hello := false
	for {
		_ = c.SetDeadline(time.Now().Add(15 * time.Second))
		payload, err := codec.ReadFrame()
		if err != nil {
			return
		}
		r, err := protocol.DecodeRequest(payload)
		if err != nil {
			return
		}
		var response protocol.Response
		if !hello {
			if r.Command != "hello" {
				response = protocol.NewErrorResponse(r.RequestID, s.Version, "hello_required", "usage", "hello is required", false)
			} else {
				response = s.dispatch(r)
				hello = response.Error == nil
			}
		} else {
			response = s.dispatch(r)
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
func (s *Server) dispatch(r protocol.Request) protocol.Response {
	resultType := ""
	var result any
	var err error
	switch r.Command {
	case "hello":
		resultType = "hello"
		result = protocol.HelloResult{SelectedVersion: protocol.Version, Capabilities: protocol.ServerCapabilities}
	case "status.get":
		resultType = "gateway_status"
		result, err = s.status(context.Background())
	case "doctor.run":
		resultType = "doctor_report"
		checks := s.store.Doctor(context.Background())
		checks = append(checks, doctor.Check{Name: "endpoint_peer_compatibility", Status: doctor.Pass, Message: "Unix endpoint accepts same-EUID peers"})
		result = doctor.Report{Overall: doctor.Overall(checks), Checks: checks}
	case "task.create":
		var p protocol.TaskCreateParams
		_ = json.Unmarshal(r.Params, &p)
		resultType = "task_mutation"
		result, err = s.store.Create(context.Background(), r.RequestID, p.Text)
	case "task.get":
		var p protocol.TaskSelector
		_ = json.Unmarshal(r.Params, &p)
		resultType = "task"
		result, err = s.store.Get(context.Background(), p.Task)
	case "task.list":
		var p protocol.TaskListParams
		_ = json.Unmarshal(r.Params, &p)
		resultType = "task_list"
		result, err = s.store.List(context.Background(), p.State, p.IncludeDone, p.Limit)
	case "task.update":
		var p protocol.TaskUpdateParams
		_ = json.Unmarshal(r.Params, &p)
		resultType = "task_mutation"
		result, err = s.store.Update(context.Background(), r.RequestID, p.Task, p.Text)
	case "task.transition":
		var p protocol.TaskTransitionParams
		_ = json.Unmarshal(r.Params, &p)
		resultType = "task_mutation"
		result, err = s.store.Transition(context.Background(), r.RequestID, p.Task, task.State(p.State), p.Note)
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
func (s *Server) status(ctx context.Context) (status.Snapshot, error) {
	counts, err := s.store.Counts(ctx)
	if err != nil {
		return status.Snapshot{}, err
	}
	dbSize, err := fileSize(s.Config.Paths.Database)
	if err != nil {
		return status.Snapshot{}, err
	}
	walSize, err := fileSize(s.Config.Paths.Database + "-wal")
	if err != nil {
		return status.Snapshot{}, err
	}
	return status.Snapshot{UptimeSeconds: int64(time.Since(s.started).Seconds()), ServerVersion: s.Version, ProtocolVersion: protocol.Version, SchemaVersion: store.SchemaVersion, PID: os.Getpid(), Database: s.Config.Paths.Database, Endpoint: s.endpoint, TaskCounts: counts, DatabaseBytes: dbSize, WALBytes: walSize, Connections: int(s.connections.Load()), Health: "healthy", EffectsCapability: "not_implemented", SlackCapability: "not_implemented", PiCapability: "not_implemented"}, nil
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
	code, category := "unavailable", "unavailable"
	switch {
	case errors.Is(err, store.ErrNotFound):
		code, category = "not_found", "not_found"
	case errors.Is(err, store.ErrAmbiguous):
		code, category = "ambiguous", "ambiguous"
	case errors.Is(err, store.ErrRequestConflict):
		code, category = "request_id_conflict", "conflict"
	default:
		if err.Error() == "unsupported command" {
			code, category = "unsupported", "unsupported"
		} else {
			code, category = "invalid_input", "usage"
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
	case []store.Task:
		a := make([]protocol.TaskDTO, len(x))
		for i := range x {
			a[i] = taskDTO(x[i])
		}
		return protocol.TaskListDTO{Tasks: a}
	default:
		return v
	}
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

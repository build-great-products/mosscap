// Package filetransfer owns bounded, lazy incoming Slack file retrieval.
// It deliberately exposes interfaces: runtime composition belongs to the server
// increment, while this package owns no credentials, configuration reload, or
// output placement.
package filetransfer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/stayradiated/mosscap/internal/clock"
	"github.com/stayradiated/mosscap/internal/fileio"
	"github.com/stayradiated/mosscap/internal/identity"
	"github.com/stayradiated/mosscap/internal/slackapi"
	"github.com/stayradiated/mosscap/internal/store"
)

var (
	// ErrNotIncoming prevents outgoing file identities from reaching Slack reads.
	ErrNotIncoming = errors.New("file is not an incoming Slack attachment")
	// ErrCapacity means retaining another maximum-sized temporary file would
	// exceed the immutable attempt snapshot.
	ErrCapacity = errors.New("temporary transfer capacity exhausted")
	// ErrTooLarge is returned for a declared or streamed size over the limit.
	ErrTooLarge = errors.New("file too large")
	// ErrContentChanged means a later download differs from durable identity.
	ErrContentChanged = errors.New("file content changed")
	// ErrExpired means a replay/release refers to no safe live temporary file.
	ErrExpired = errors.New("file transfer expired")
	// ErrUnsafeTemporary means lifecycle authority could not safely verify bytes.
	ErrUnsafeTemporary = errors.New("unsafe temporary file")
)

// Settings is the immutable configuration snapshot for one operation.
type Settings struct {
	MaximumBytes     int64
	MaximumLiveFiles int
	Lifetime         time.Duration
}

func (s Settings) valid() bool {
	return s.MaximumBytes > 0 && s.MaximumLiveFiles > 0 && s.Lifetime > 0 && s.MaximumBytes <= math.MaxInt64/int64(s.MaximumLiveFiles)
}

// SettingsProvider obtains the active immutable settings snapshot. Reloaded
// values affect new attempts only; a committed transfer retains its expiry.
type SettingsProvider interface {
	Settings(context.Context) (Settings, error)
}

// Repository is the narrow durable authority needed by incoming lifecycle.
// Implementations must make CommitIncoming atomic with request replay evidence.
type Repository interface {
	GetSlackFile(context.Context, string) (store.SlackFile, error)
	ReplayIncomingFileTransfer(context.Context, string, string) (store.FileGetRequestResult, error)
	GetFileTemporaryTransfer(context.Context, string) (store.FileTemporaryTransfer, error)
	CommitIncomingFileTransfer(context.Context, string, store.FileTemporaryTransfer) (store.FileGetRequestResult, error)
	ListExpiredFileTemporaryTransfers(context.Context, time.Time, int) ([]store.FileTemporaryTransfer, error)
	DeleteFileTemporaryTransfer(context.Context, string) error
	SlackFileTransferCounts(context.Context) (store.SlackFileTransferCounts, error)
}

// TemporaryInventory is optional repository support for startup reconciliation.
// It is intentionally separate so the service can be introduced before server
// wiring adds an all-live-row store query.
type TemporaryInventory interface {
	ListFileTemporaryTransfers(context.Context, int) ([]store.FileTemporaryTransfer, error)
}

// IncomingFailureRecorder is optional durable safe diagnostic support.
type IncomingFailureRecorder interface {
	RecordIncomingFileFailure(context.Context, string, string) error
}

// IDSource creates opaque transfer IDs. It must return full xfr_ IDs.
type IDSource interface{ NewID(string) (string, error) }

// TemporaryFile is an unpublished owner-private file. Sync must durably flush
// bytes before its name is recorded in SQLite.
type TemporaryFile interface {
	io.Writer
	Sync() error
	Close() error
	Name() string
	Path() string
}

// TemporaryFiles is the private generated-file lifecycle boundary. Methods
// must operate through a directory descriptor and never follow child symlinks.
type TemporaryFiles interface {
	Root() string
	Create() (TemporaryFile, error)
	Remove(string) error
	Exists(string) error
	Reconcile(map[string]struct{}) (fileio.ReconcileReport, error)
}

// Result is the only successful incoming retrieval result. Its path is derived
// from a generated basename and never becomes durable request evidence.
type Result struct {
	TransferID string
	FileID     string
	Path       string
	SizeBytes  int64
	SHA256     string
	ExpiresAt  time.Time
}

// Service retrieves admitted incoming files and owns their private lifecycle.
type Service struct {
	repo     Repository
	files    TemporaryFiles
	slack    slackapi.FileTransfer
	settings SettingsProvider
	ids      IDSource
	clock    clock.Clock

	slots   chan struct{}
	stop    chan struct{}
	done    chan struct{}
	once    sync.Once
	startMu sync.Mutex
	started bool
}

// New constructs a service with one gateway-wide allocation slot. The single
// slot is intentional: it bounds unpublished and committed 100 MiB files even
// when callers race before capacity can be made durable.
func New(repo Repository, files TemporaryFiles, slack slackapi.FileTransfer, settings SettingsProvider, ids IDSource, c clock.Clock) (*Service, error) {
	if repo == nil || files == nil || slack == nil || settings == nil || ids == nil || c == nil {
		return nil, errors.New("file transfer service has nil dependency")
	}
	return &Service{repo: repo, files: files, slack: slack, settings: settings, ids: ids, clock: c, slots: make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{})}, nil
}

// Start performs an initial bounded cleanup and starts an unreferenced cleanup
// ticker. Close stops it before callers close the repository.
func (s *Service) Start(ctx context.Context) error {
	s.startMu.Lock()
	defer s.startMu.Unlock()
	if s.started {
		return errors.New("file transfer service already started")
	}
	if err := s.Cleanup(ctx); err != nil {
		return err
	}
	s.started = true
	go func() {
		defer close(s.done)
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-s.stop:
				return
			case <-ticker.C:
				// A later explicit get will surface cleanup errors. The ticker is
				// deliberately best-effort and must not retain error detail/paths.
				_ = s.Cleanup(context.Background())
			}
		}
	}()
	return nil
}

// Close is safe before Start and waits for the ticker before repository close.
func (s *Service) Close() {
	s.once.Do(func() { close(s.stop) })
	s.startMu.Lock()
	started := s.started
	s.startMu.Unlock()
	if started {
		<-s.done
	}
}

// Get lazily downloads one admitted incoming file. It never contacts Slack for
// a live request replay and removes every unpublished file on failure.
func (s *Service) Get(ctx context.Context, requestID, fileID string) (Result, error) {
	if !identity.ValidRequestID(requestID) {
		return Result{}, errors.New("invalid request ID")
	}
	file, err := s.repo.GetSlackFile(ctx, fileID)
	if err != nil {
		return Result{}, err
	}
	if file.Direction != "from_slack" || file.SlackFileID == "" {
		return Result{}, ErrNotIncoming
	}
	if replay, err := s.repo.ReplayIncomingFileTransfer(ctx, requestID, file.FileID); err == nil {
		return s.liveResult(ctx, replay)
	} else if !errors.Is(err, store.ErrFileTransferNotFound) {
		if errors.Is(err, store.ErrFileTransferExpired) {
			return Result{}, ErrExpired
		}
		return Result{}, err
	}

	cfg, err := s.settings.Settings(ctx)
	if err != nil {
		return Result{}, err
	}
	if !cfg.valid() {
		return Result{}, errors.New("invalid incoming transfer settings")
	}
	if file.DeclaredSizeBytes != nil && *file.DeclaredSizeBytes > cfg.MaximumBytes {
		s.recordFailure(ctx, file.FileID, "file_too_large")
		return Result{}, ErrTooLarge
	}
	if err := s.Cleanup(ctx); err != nil {
		return Result{}, err
	}
	if err := s.ensureCapacity(ctx, cfg); err != nil {
		return Result{}, err
	}
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}
	// Capacity is checked again inside the slot: concurrent callers may all
	// have passed the first check before the prior attempt committed.
	if err := s.Cleanup(ctx); err != nil {
		return Result{}, err
	}
	if err := s.ensureCapacity(ctx, cfg); err != nil {
		return Result{}, err
	}

	tmp, err := s.files.Create()
	if err != nil {
		return Result{}, err
	}
	published := false
	defer func() {
		_ = tmp.Close()
		if !published {
			_ = s.files.Remove(tmp.Name())
		}
	}()
	writer := &boundedHashWriter{dst: tmp, limit: cfg.MaximumBytes}
	_, err = s.slack.Download(ctx, slackapi.DownloadRequest{FileID: file.SlackFileID, MaximumBytes: cfg.MaximumBytes}, writer)
	if err != nil {
		s.recordFailure(ctx, file.FileID, diagnosticFor(err))
		return Result{}, err
	}
	if writer.tooLarge {
		s.recordFailure(ctx, file.FileID, "file_too_large")
		return Result{}, ErrTooLarge
	}
	if err := tmp.Sync(); err != nil {
		return Result{}, err
	}
	if err := tmp.Close(); err != nil {
		return Result{}, err
	}
	digest := writer.Digest()
	if file.SHA256 != "" && file.SHA256 != digest {
		s.recordFailure(ctx, file.FileID, "file_content_changed")
		return Result{}, ErrContentChanged
	}
	id, err := s.ids.NewID("xfr_")
	if err != nil {
		return Result{}, err
	}
	now := s.clock.Now()
	transfer := store.FileTemporaryTransfer{TransferID: id, FileID: file.FileID, GeneratedName: tmp.Name(), SizeBytes: writer.n, SHA256: digest, CreatedAt: now, ExpiresAt: now.Add(cfg.Lifetime)}
	committed, err := s.repo.CommitIncomingFileTransfer(ctx, requestID, transfer)
	if err != nil {
		return Result{}, err
	}
	published = true
	return Result{TransferID: committed.TransferID, FileID: file.FileID, Path: tmp.Path(), SizeBytes: writer.n, SHA256: digest, ExpiresAt: transfer.ExpiresAt}, nil
}

// Release unlinks the verified generated entry before deleting durable liveness.
// A missing entry is treated as expired and its stale live row is removed.
func (s *Service) Release(ctx context.Context, transferID string) error {
	transfer, err := s.repo.GetFileTemporaryTransfer(ctx, transferID)
	if err != nil {
		if errors.Is(err, store.ErrFileTransferNotFound) {
			return ErrExpired
		}
		return err
	}
	if err := s.files.Remove(transfer.GeneratedName); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: %v", ErrUnsafeTemporary, err)
	}
	if err := s.repo.DeleteFileTemporaryTransfer(ctx, transferID); err != nil && !errors.Is(err, store.ErrFileTransferNotFound) {
		return err
	}
	return nil
}

// Cleanup removes expired bytes before their rows, repairs missing live rows,
// and removes only recognized generated orphans through fileio's descriptor API.
func (s *Service) Cleanup(ctx context.Context) error {
	for {
		expired, err := s.repo.ListExpiredFileTemporaryTransfers(ctx, s.clock.Now(), 1000)
		if err != nil {
			return err
		}
		for _, transfer := range expired {
			err := s.files.Remove(transfer.GeneratedName)
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				continue // preserve row if a path is unsafe; never unlink blindly.
			}
			if err := s.repo.DeleteFileTemporaryTransfer(ctx, transfer.TransferID); err != nil && !errors.Is(err, store.ErrFileTransferNotFound) {
				return err
			}
		}
		if len(expired) < 1000 {
			break
		}
	}
	inventory, ok := s.repo.(TemporaryInventory)
	if !ok {
		return nil
	}
	live, err := inventory.ListFileTemporaryTransfers(ctx, 1000)
	if err != nil {
		return err
	}
	known := make(map[string]struct{}, len(live))
	for _, transfer := range live {
		known[transfer.GeneratedName] = struct{}{}
		if err := s.files.Exists(transfer.GeneratedName); errors.Is(err, os.ErrNotExist) {
			if err := s.repo.DeleteFileTemporaryTransfer(ctx, transfer.TransferID); err != nil && !errors.Is(err, store.ErrFileTransferNotFound) {
				return err
			}
		}
	}
	_, err = s.files.Reconcile(known)
	return err
}

func (s *Service) ensureCapacity(ctx context.Context, cfg Settings) error {
	counts, err := s.repo.SlackFileTransferCounts(ctx)
	if err != nil {
		return err
	}
	capacity := cfg.MaximumBytes * int64(cfg.MaximumLiveFiles)
	if counts.TemporaryCount >= int64(cfg.MaximumLiveFiles) || counts.TemporaryBytes > capacity-cfg.MaximumBytes {
		return ErrCapacity
	}
	return nil
}

func (s *Service) liveResult(ctx context.Context, replay store.FileGetRequestResult) (Result, error) {
	transfer, err := s.repo.GetFileTemporaryTransfer(ctx, replay.TransferID)
	if err != nil {
		if errors.Is(err, store.ErrFileTransferNotFound) {
			return Result{}, ErrExpired
		}
		return Result{}, err
	}
	if !transfer.ExpiresAt.After(s.clock.Now()) {
		_ = s.Release(ctx, transfer.TransferID)
		return Result{}, ErrExpired
	}
	if err := s.files.Exists(transfer.GeneratedName); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			_ = s.repo.DeleteFileTemporaryTransfer(ctx, transfer.TransferID)
			return Result{}, ErrExpired
		}
		return Result{}, fmt.Errorf("%w: %v", ErrUnsafeTemporary, err)
	}
	return Result{TransferID: transfer.TransferID, FileID: transfer.FileID, Path: filepath.Join(s.files.Root(), transfer.GeneratedName), SizeBytes: transfer.SizeBytes, SHA256: transfer.SHA256, ExpiresAt: transfer.ExpiresAt}, nil
}

func (s *Service) recordFailure(ctx context.Context, fileID, code string) {
	if recorder, ok := s.repo.(IncomingFailureRecorder); ok {
		_ = recorder.RecordIncomingFileFailure(ctx, fileID, code)
	}
}

func diagnosticFor(err error) string {
	if errors.Is(err, fileio.ErrTooLarge) || errors.Is(err, ErrTooLarge) {
		return "file_too_large"
	}
	failure := slackapi.Classify(err, slackapi.FailureRead)
	if failure.Diagnostic != "" {
		return string(failure.Diagnostic)
	}
	return "slack_file_unavailable"
}

type boundedHashWriter struct {
	dst      io.Writer
	limit, n int64
	hash     hash.Hash
	tooLarge bool
}

func (w *boundedHashWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.limit-w.n {
		w.tooLarge = true
		return 0, ErrTooLarge
	}
	if w.hash == nil {
		w.hash = sha256.New()
	}
	n, err := w.dst.Write(p)
	w.n += int64(n)
	if err != nil {
		return n, err
	}
	if n != len(p) {
		return n, io.ErrShortWrite
	}
	_, _ = w.hash.Write(p)
	return n, nil
}
func (w *boundedHashWriter) Digest() string {
	if w.hash == nil {
		w.hash = sha256.New()
	}
	return hex.EncodeToString(w.hash.Sum(nil))
}

package filetransfer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"sync"
	"testing"
	"time"

	"github.com/stayradiated/mosscap/internal/clock"
	"github.com/stayradiated/mosscap/internal/fileio"
	"github.com/stayradiated/mosscap/internal/slackapi"
	"github.com/stayradiated/mosscap/internal/store"
)

func TestGetCommitsValidatedBytesAndReplaysWithoutDownload(t *testing.T) {
	repo := newRepository("fil_abcdefghijklmnopqrstuv", "F12345678")
	files := newMemoryFiles()
	slack := &downloadSlack{bytes: []byte("verified bytes")}
	svc := newService(t, repo, files, slack, clock.Fixed{Time: time.Unix(100, 0)}, 100, 2)
	first, err := svc.Get(context.Background(), "get-1", repo.file.FileID)
	if err != nil {
		t.Fatal(err)
	}
	if first.Path == "" || first.SizeBytes != int64(len(slack.bytes)) || first.SHA256 != digest(slack.bytes) {
		t.Fatalf("result=%+v", first)
	}
	if slack.downloads != 1 || len(repo.transfers) != 1 || repo.file.TransferState != "validated" {
		t.Fatalf("downloads=%d transfers=%d file=%+v", slack.downloads, len(repo.transfers), repo.file)
	}
	second, err := svc.Get(context.Background(), "get-1", repo.file.FileID)
	if err != nil {
		t.Fatal(err)
	}
	if second != first || slack.downloads != 1 {
		t.Fatalf("second=%+v downloads=%d", second, slack.downloads)
	}
}

func TestGetRejectsDeclaredOversizeBeforeSlack(t *testing.T) {
	repo := newRepository("fil_abcdefghijklmnopqrstuv", "F12345678")
	declared := int64(11)
	repo.file.DeclaredSizeBytes = &declared
	slack := &downloadSlack{bytes: []byte("never")}
	svc := newService(t, repo, newMemoryFiles(), slack, clock.System{}, 10, 2)
	_, err := svc.Get(context.Background(), "get-2", repo.file.FileID)
	if !errors.Is(err, ErrTooLarge) || slack.downloads != 0 || repo.failure != "file_too_large" {
		t.Fatalf("err=%v downloads=%d failure=%q", err, slack.downloads, repo.failure)
	}
}

func TestGetRejectsChangedBytesAndUnlinksUnpublishedFile(t *testing.T) {
	repo := newRepository("fil_abcdefghijklmnopqrstuv", "F12345678")
	repo.file.SHA256 = digest([]byte("first download"))
	files := newMemoryFiles()
	svc := newService(t, repo, files, &downloadSlack{bytes: []byte("different download")}, clock.System{}, 100, 2)
	_, err := svc.Get(context.Background(), "get-3", repo.file.FileID)
	if !errors.Is(err, ErrContentChanged) || len(files.entries) != 0 || len(repo.transfers) != 0 || repo.failure != "file_content_changed" {
		t.Fatalf("err=%v entries=%d transfers=%d failure=%q", err, len(files.entries), len(repo.transfers), repo.failure)
	}
}

func TestGetEnforcesLiveCapacityAndExpiredCleanup(t *testing.T) {
	repo := newRepository("fil_abcdefghijklmnopqrstuv", "F12345678")
	files := newMemoryFiles()
	now := time.Unix(100, 0).UTC()
	repo.transfers["xfr_bcdefghijklmnopqrstuvw"] = store.FileTemporaryTransfer{TransferID: "xfr_bcdefghijklmnopqrstuvw", FileID: repo.file.FileID, GeneratedName: "mosscap_xfr_bcdefghijklmnopqrstuv", SizeBytes: 10, SHA256: digest(nil), CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
	files.entries["mosscap_xfr_bcdefghijklmnopqrstuv"] = &memoryTemp{}
	svc := newService(t, repo, files, &downloadSlack{bytes: []byte("a")}, clock.Fixed{Time: now}, 10, 1)
	if _, err := svc.Get(context.Background(), "get-4", repo.file.FileID); !errors.Is(err, ErrCapacity) {
		t.Fatalf("err=%v", err)
	}
	repo.transfers["xfr_bcdefghijklmnopqrstuvw"] = store.FileTemporaryTransfer{TransferID: "xfr_bcdefghijklmnopqrstuvw", FileID: repo.file.FileID, GeneratedName: "mosscap_xfr_bcdefghijklmnopqrstuv", SizeBytes: 10, SHA256: digest(nil), CreatedAt: now, ExpiresAt: now}
	if err := svc.Cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(repo.transfers) != 0 || len(files.entries) != 0 {
		t.Fatalf("cleanup transfers=%d entries=%d", len(repo.transfers), len(files.entries))
	}
}

func TestReleaseMissingFileDeletesStaleRow(t *testing.T) {
	repo := newRepository("fil_abcdefghijklmnopqrstuv", "F12345678")
	repo.transfers["xfr_bcdefghijklmnopqrstuvw"] = store.FileTemporaryTransfer{TransferID: "xfr_bcdefghijklmnopqrstuvw", FileID: repo.file.FileID, GeneratedName: "mosscap_xfr_bcdefghijklmnopqrstuv", SizeBytes: 0, SHA256: digest(nil), CreatedAt: time.Unix(1, 0), ExpiresAt: time.Unix(2, 0)}
	svc := newService(t, repo, newMemoryFiles(), &downloadSlack{}, clock.System{}, 10, 1)
	if err := svc.Release(context.Background(), "xfr_bcdefghijklmnopqrstuvw"); err != nil {
		t.Fatal(err)
	}
	if len(repo.transfers) != 0 {
		t.Fatal("stale row remains")
	}
}

func newService(t *testing.T, repo *memoryRepository, files *memoryFiles, slack *downloadSlack, c clock.Clock, max int64, count int) *Service {
	t.Helper()
	svc, err := New(repo, files, slack, fixedSettings{Settings{MaximumBytes: max, MaximumLiveFiles: count, Lifetime: time.Hour}}, fixedIDs{}, c)
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

type fixedSettings struct{ value Settings }

func (s fixedSettings) Settings(context.Context) (Settings, error) { return s.value, nil }

type fixedIDs struct{}

func (fixedIDs) NewID(string) (string, error) { return "xfr_abcdefghijklmnopqrstuv", nil }

type downloadSlack struct {
	bytes     []byte
	err       error
	downloads int
}

func (s *downloadSlack) Download(_ context.Context, _ slackapi.DownloadRequest, w io.Writer) (slackapi.File, error) {
	s.downloads++
	if s.err != nil {
		return slackapi.File{}, s.err
	}
	_, err := w.Write(s.bytes)
	return slackapi.File{}, err
}
func (*downloadSlack) Upload(context.Context, slackapi.UploadRequest) (slackapi.UploadResult, error) {
	return slackapi.UploadResult{}, errors.New("unused")
}

type memoryFiles struct {
	mu      sync.Mutex
	entries map[string]*memoryTemp
	n       int
}
type memoryTemp struct {
	data       []byte
	closed     bool
	name, path string
}

func newMemoryFiles() *memoryFiles  { return &memoryFiles{entries: map[string]*memoryTemp{}} }
func (f *memoryFiles) Root() string { return "/private/tmp" }
func (f *memoryFiles) Create() (TemporaryFile, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	name := "mosscap_xfr_abcdefghijklmnopqrstuv"
	if f.n > 1 {
		name = "mosscap_xfr_bcdefghijklmnopqrstuvw"
	}
	tmp := &memoryTemp{name: name, path: "/private/tmp/" + name}
	f.entries[name] = tmp
	return tmp, nil
}
func (f *memoryFiles) Remove(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.entries[name]; !ok {
		return fs.ErrNotExist
	}
	delete(f.entries, name)
	return nil
}
func (f *memoryFiles) Exists(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.entries[name]; !ok {
		return fs.ErrNotExist
	}
	return nil
}
func (f *memoryFiles) Reconcile(known map[string]struct{}) (fileio.ReconcileReport, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var r fileio.ReconcileReport
	for name := range f.entries {
		if _, ok := known[name]; ok {
			r.Live++
		} else {
			delete(f.entries, name)
			r.Removed++
		}
	}
	return r, nil
}
func (m *memoryTemp) Write(p []byte) (int, error) {
	if m.closed {
		return 0, fs.ErrClosed
	}
	m.data = append(m.data, p...)
	return len(p), nil
}
func (*memoryTemp) Sync() error    { return nil }
func (m *memoryTemp) Close() error { m.closed = true; return nil }
func (m *memoryTemp) Name() string { return m.name }
func (m *memoryTemp) Path() string { return m.path }

type memoryRepository struct {
	mu        sync.Mutex
	file      store.SlackFile
	transfers map[string]store.FileTemporaryTransfer
	replay    map[string]store.FileGetRequestResult
	failure   string
}

func newRepository(id, slackID string) *memoryRepository {
	return &memoryRepository{file: store.SlackFile{FileID: id, Direction: "from_slack", SlackFileID: slackID, TransferState: "metadata_only"}, transfers: map[string]store.FileTemporaryTransfer{}, replay: map[string]store.FileGetRequestResult{}}
}
func (r *memoryRepository) GetSlackFile(_ context.Context, id string) (store.SlackFile, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if id != r.file.FileID {
		return store.SlackFile{}, store.ErrNotFound
	}
	return r.file, nil
}
func (r *memoryRepository) ReplayIncomingFileTransfer(_ context.Context, request, file string) (store.FileGetRequestResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.replay[request]
	if !ok {
		return store.FileGetRequestResult{}, store.ErrFileTransferNotFound
	}
	if v.FileID != file {
		return store.FileGetRequestResult{}, store.ErrRequestConflict
	}
	if _, ok := r.transfers[v.TransferID]; !ok {
		return store.FileGetRequestResult{}, store.ErrFileTransferExpired
	}
	return v, nil
}
func (r *memoryRepository) GetFileTemporaryTransfer(_ context.Context, id string) (store.FileTemporaryTransfer, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.transfers[id]
	if !ok {
		return store.FileTemporaryTransfer{}, store.ErrFileTransferNotFound
	}
	return v, nil
}
func (r *memoryRepository) CommitIncomingFileTransfer(_ context.Context, request string, transfer store.FileTemporaryTransfer) (store.FileGetRequestResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if old, ok := r.replay[request]; ok {
		return old, nil
	}
	if r.file.SHA256 != "" && r.file.SHA256 != transfer.SHA256 {
		return store.FileGetRequestResult{}, ErrContentChanged
	}
	r.transfers[transfer.TransferID] = transfer
	r.file.SHA256, r.file.TransferState = transfer.SHA256, "validated"
	r.replay[request] = store.FileGetRequestResult{FileID: transfer.FileID, TransferID: transfer.TransferID}
	return r.replay[request], nil
}
func (r *memoryRepository) ListExpiredFileTemporaryTransfers(_ context.Context, before time.Time, _ int) ([]store.FileTemporaryTransfer, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var result []store.FileTemporaryTransfer
	for _, v := range r.transfers {
		if !v.ExpiresAt.After(before) {
			result = append(result, v)
		}
	}
	return result, nil
}
func (r *memoryRepository) DeleteFileTemporaryTransfer(_ context.Context, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.transfers[id]; !ok {
		return store.ErrFileTransferNotFound
	}
	delete(r.transfers, id)
	return nil
}
func (r *memoryRepository) SlackFileTransferCounts(context.Context) (store.SlackFileTransferCounts, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var c store.SlackFileTransferCounts
	for _, v := range r.transfers {
		c.TemporaryCount++
		c.TemporaryBytes += v.SizeBytes
	}
	return c, nil
}
func (r *memoryRepository) ListFileTemporaryTransfers(context.Context, int) ([]store.FileTemporaryTransfer, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var result []store.FileTemporaryTransfer
	for _, v := range r.transfers {
		result = append(result, v)
	}
	return result, nil
}
func (r *memoryRepository) RecordIncomingFileFailure(_ context.Context, _ string, code string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failure = code
	return nil
}

func digest(b []byte) string { sum := sha256.Sum256(b); return hex.EncodeToString(sum[:]) }

package fake

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sync"

	"github.com/stayradiated/mosscap/internal/slackapi"
)

// TransferOperation is one observable FileTransfer protocol phase.
type TransferOperation string

const (
	Download       TransferOperation = "download"
	ReserveUpload  TransferOperation = "reserve_upload"
	StageUpload    TransferOperation = "stage_upload"
	ValidateUpload TransferOperation = "validate_upload"
	CompleteUpload TransferOperation = "complete_upload"
)

// TransferStep scripts one protocol phase. Bytes are retained only by this
// in-memory fake for exact staging assertions; calls never expose them.
type TransferStep struct {
	Operation TransferOperation
	Request   any
	Result    any
	Bytes     []byte
	Err       error
	PreCall   CallScript
	PostCall  CallScript
}

// TransferCall is a safe observed phase request. It intentionally excludes
// staged bytes and private URLs.
type TransferCall struct {
	Operation TransferOperation
	Request   any
}

// FileTransfer is a strict, deterministic in-memory FileTransfer. Its phase
// barriers make pre-call no-exchange and post-call possible-action boundaries
// directly testable by effect services.
type FileTransfer struct {
	mu    sync.Mutex
	steps []TransferStep
	calls []TransferCall
}

var _ slackapi.FileTransfer = (*FileTransfer)(nil)

func NewFileTransfer(steps ...TransferStep) *FileTransfer {
	return &FileTransfer{steps: append([]TransferStep(nil), steps...)}
}

func (fake *FileTransfer) Download(ctx context.Context, request slackapi.DownloadRequest, writer io.Writer) (slackapi.File, error) {
	var result slackapi.File
	step, err := fake.invoke(ctx, Download, request)
	if err != nil {
		return result, err
	}
	if step.Result != nil {
		file, ok := step.Result.(slackapi.File)
		if !ok {
			return result, errors.New("fake Slack file transfer: download result has wrong type")
		}
		result = file
	}
	if writer == nil {
		return result, errors.New("fake Slack file transfer: nil download writer")
	}
	if _, err := io.Copy(writer, bytes.NewReader(step.Bytes)); err != nil {
		return result, err
	}
	if err := step.PostCall.apply(ctx); err != nil {
		return result, err
	}
	if step.Err != nil {
		return result, step.Err
	}
	return result, nil
}

func (fake *FileTransfer) Upload(ctx context.Context, request slackapi.UploadRequest) (slackapi.UploadResult, error) {
	if len(request.Files) == 0 {
		return slackapi.UploadResult{}, errors.New("fake Slack file transfer: empty upload")
	}
	reservations := make([]slackapi.UploadedFile, 0, len(request.Files))
	for _, source := range request.Files {
		step, err := fake.invoke(ctx, ReserveUpload, reserveRequest(source))
		if err != nil {
			return slackapi.UploadResult{}, err
		}
		if err := step.PostCall.apply(ctx); err != nil {
			return slackapi.UploadResult{}, err
		}
		if step.Err != nil {
			return slackapi.UploadResult{}, step.Err
		}
		file, ok := step.Result.(slackapi.UploadedFile)
		if !ok || slackapi.ValidateFileID(file.ID) != nil {
			return slackapi.UploadResult{}, errors.New("fake Slack file transfer: invalid reservation")
		}
		reservations = append(reservations, file)
	}
	for index, source := range request.Files {
		step, err := fake.invoke(ctx, StageUpload, stageRequest(reservations[index], source))
		if err != nil {
			return slackapi.UploadResult{}, err
		}
		content, err := readExact(source.Reader, source.SizeBytes)
		if err != nil {
			return slackapi.UploadResult{}, err
		}
		if step.Bytes != nil && !bytes.Equal(step.Bytes, content) {
			return slackapi.UploadResult{}, errors.New("fake Slack file transfer: staged bytes differ from script")
		}
		if err := step.PostCall.apply(ctx); err != nil {
			return slackapi.UploadResult{}, err
		}
		if step.Err != nil {
			return slackapi.UploadResult{}, step.Err
		}
	}
	for _, source := range request.Files {
		step, err := fake.invoke(ctx, ValidateUpload, validateRequest(source))
		if err != nil {
			return slackapi.UploadResult{}, err
		}
		if source.ValidateAfter != nil {
			if err := source.ValidateAfter(ctx); err != nil {
				return slackapi.UploadResult{}, err
			}
		}
		if err := step.PostCall.apply(ctx); err != nil {
			return slackapi.UploadResult{}, err
		}
		if step.Err != nil {
			return slackapi.UploadResult{}, step.Err
		}
	}
	step, err := fake.invoke(ctx, CompleteUpload, completeRequest(request, reservations))
	if err != nil {
		return slackapi.UploadResult{}, err
	}
	if err := step.PostCall.apply(ctx); err != nil {
		return slackapi.UploadResult{}, err
	}
	if step.Err != nil {
		return slackapi.UploadResult{}, step.Err
	}
	result, ok := step.Result.(slackapi.UploadResult)
	if !ok {
		return slackapi.UploadResult{}, errors.New("fake Slack file transfer: completion result has wrong type")
	}
	if len(result.Files) != len(reservations) {
		return slackapi.UploadResult{}, errors.New("fake Slack file transfer: malformed completion")
	}
	for index := range result.Files {
		if result.Files[index].ID != reservations[index].ID {
			return slackapi.UploadResult{}, errors.New("fake Slack file transfer: completion order differs")
		}
	}
	return result, nil
}

type uploadReservationRequest struct {
	Name      string
	SizeBytes int64
}
type uploadStageRequest struct {
	FileID    string
	Name      string
	SizeBytes int64
}
type uploadValidationRequest struct {
	Name      string
	SizeBytes int64
}
type uploadCompletionRequest struct {
	ChannelID      string
	ThreadTS       slackapi.Timestamp
	InitialComment string
	FileIDs        []string
}

func reserveRequest(source slackapi.UploadSource) uploadReservationRequest {
	return uploadReservationRequest{source.Name, source.SizeBytes}
}
func stageRequest(file slackapi.UploadedFile, source slackapi.UploadSource) uploadStageRequest {
	return uploadStageRequest{file.ID, source.Name, source.SizeBytes}
}
func validateRequest(source slackapi.UploadSource) uploadValidationRequest {
	return uploadValidationRequest{source.Name, source.SizeBytes}
}
func completeRequest(request slackapi.UploadRequest, files []slackapi.UploadedFile) uploadCompletionRequest {
	ids := make([]string, 0, len(files))
	for _, file := range files {
		ids = append(ids, file.ID)
	}
	return uploadCompletionRequest{request.ChannelID, request.ThreadTS, request.InitialComment, ids}
}

func readExact(reader io.Reader, size int64) ([]byte, error) {
	if reader == nil || size < 0 {
		return nil, errors.New("fake Slack file transfer: invalid source")
	}
	content, err := io.ReadAll(io.LimitReader(reader, size+1))
	if err != nil || int64(len(content)) != size {
		return nil, errors.New("fake Slack file transfer: source size differs")
	}
	var extra [1]byte
	count, err := reader.Read(extra[:])
	if count != 0 || err == nil {
		return nil, errors.New("fake Slack file transfer: source exceeds size")
	}
	if err != io.EOF {
		return nil, err
	}
	return content, nil
}

func (fake *FileTransfer) invoke(ctx context.Context, operation TransferOperation, request any) (TransferStep, error) {
	if err := ctx.Err(); err != nil {
		return TransferStep{}, err
	}
	fake.mu.Lock()
	if len(fake.steps) == 0 {
		fake.mu.Unlock()
		return TransferStep{}, fmt.Errorf("fake Slack file transfer: unexpected %s call", operation)
	}
	step := fake.steps[0]
	if step.Operation != operation {
		fake.mu.Unlock()
		return TransferStep{}, fmt.Errorf("fake Slack file transfer: got %s call, expected %s", operation, step.Operation)
	}
	if step.Request != nil && !reflect.DeepEqual(step.Request, request) {
		fake.mu.Unlock()
		return TransferStep{}, fmt.Errorf("fake Slack file transfer: %s request differs from script", operation)
	}
	if err := step.PreCall.apply(ctx); err != nil {
		fake.mu.Unlock()
		return TransferStep{}, err
	}
	fake.calls = append(fake.calls, TransferCall{Operation: operation, Request: request})
	fake.steps = fake.steps[1:]
	fake.mu.Unlock()
	return step, nil
}

func (fake *FileTransfer) Calls() []TransferCall {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return append([]TransferCall(nil), fake.calls...)
}
func (fake *FileTransfer) Remaining() int {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return len(fake.steps)
}
func (fake *FileTransfer) AssertDone() error {
	if remaining := fake.Remaining(); remaining != 0 {
		return fmt.Errorf("fake Slack file transfer: %d scripted calls remain", remaining)
	}
	return nil
}

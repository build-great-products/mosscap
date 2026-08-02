package fake

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stayradiated/mosscap/internal/slackapi"
)

func TestFileTransferRunsOrderedStagesAndDoesNotCompleteAfterValidationFailure(t *testing.T) {
	transfer := NewFileTransfer(
		TransferStep{Operation: ReserveUpload, Result: slackapi.UploadedFile{ID: "F001"}},
		TransferStep{Operation: StageUpload, Bytes: []byte("a")},
		TransferStep{Operation: ValidateUpload},
	)
	validatorFailure := errors.New("source changed")
	_, err := transfer.Upload(context.Background(), slackapi.UploadRequest{ChannelID: "C012ABC", Files: []slackapi.UploadSource{{Name: "one", SizeBytes: 1, Reader: strings.NewReader("a"), ValidateAfter: func(context.Context) error { return validatorFailure }}}})
	if !errors.Is(err, validatorFailure) {
		t.Fatalf("Upload = %v", err)
	}
	calls := transfer.Calls()
	if len(calls) != 3 || calls[0].Operation != ReserveUpload || calls[1].Operation != StageUpload || calls[2].Operation != ValidateUpload {
		t.Fatalf("calls = %#v", calls)
	}
	if transfer.Remaining() != 0 {
		t.Fatalf("completion unexpectedly attempted")
	}
}

func TestFileTransferPostCompletionFaultKeepsCompletionEvidence(t *testing.T) {
	transfer := NewFileTransfer(
		TransferStep{Operation: ReserveUpload, Result: slackapi.UploadedFile{ID: "F001"}},
		TransferStep{Operation: StageUpload, Bytes: []byte("a")},
		TransferStep{Operation: ValidateUpload},
		TransferStep{Operation: CompleteUpload, Result: slackapi.UploadResult{Files: []slackapi.UploadedFile{{ID: "F001"}}}, PostCall: CallScript{Fault: Disconnect}},
	)
	_, err := transfer.Upload(context.Background(), slackapi.UploadRequest{ChannelID: "C012ABC", Files: []slackapi.UploadSource{{Name: "one", SizeBytes: 1, Reader: strings.NewReader("a")}}})
	if slackapi.Classify(err, slackapi.FailureAfterSubmit).Class != slackapi.FailureAmbiguous {
		t.Fatalf("completion fault = %v", err)
	}
	calls := transfer.Calls()
	if len(calls) != 4 || calls[3].Operation != CompleteUpload {
		t.Fatalf("calls = %#v", calls)
	}
}

func TestFileTransferPreCallFailureProvesNoExchange(t *testing.T) {
	transfer := NewFileTransfer(TransferStep{Operation: Download, PreCall: CallScript{Fault: Timeout}})
	_, err := transfer.Download(context.Background(), slackapi.DownloadRequest{FileID: "F012ABC", MaximumBytes: 1}, &strings.Builder{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Download = %v", err)
	}
	if len(transfer.Calls()) != 0 || transfer.Remaining() != 1 {
		t.Fatalf("pre-call fault recorded exchange")
	}
}

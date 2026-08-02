package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestIncomingTransferCommitReplayExpiryAndDetachedIdentity(t *testing.T) {
	s := testStore(t)
	trustSourceTestUser(t, s)
	ctx := context.Background()
	event := sourceTestEvent("Ev-transfer-initial", SlackEventAppMention, 10)
	event.Files = []SlackIncomingFile{{SlackFileID: "F12345678", OriginalName: "original.txt"}}
	admitted, err := s.CommitSlackSourceEvent(ctx, sourceTestPolicy, event)
	if err != nil {
		t.Fatal(err)
	}
	files, err := s.ListSlackFiles(ctx, admitted.TaskID)
	if err != nil || len(files) != 1 {
		t.Fatalf("files=%+v err=%v", files, err)
	}
	transfer := FileTemporaryTransfer{
		TransferID: "xfr_abcdefghijklmnopqrstuv", FileID: files[0].FileID,
		GeneratedName: "mosscap_xfr_abcdefghijklmnopqrstuv", SizeBytes: 3,
		SHA256: strings.Repeat("a", 64), CreatedAt: time.UnixMicro(20), ExpiresAt: time.UnixMicro(30),
	}
	first, err := s.CommitIncomingFileTransfer(ctx, "file-get", transfer)
	if err != nil || first.FileID != files[0].FileID || first.TransferID != transfer.TransferID {
		t.Fatalf("commit=%+v err=%v", first, err)
	}
	replay, err := s.ReplayIncomingFileTransfer(ctx, "file-get", files[0].FileID)
	if err != nil || replay != first {
		t.Fatalf("live replay=%+v err=%v", replay, err)
	}
	replayedCommit, err := s.CommitIncomingFileTransfer(ctx, "file-get", FileTemporaryTransfer{TransferID: "xfr_bcdefghijklmnopqrstuvw", FileID: files[0].FileID, GeneratedName: "mosscap_xfr_bcdefghijklmnopqrstuvw", SizeBytes: 3, SHA256: strings.Repeat("a", 64), CreatedAt: time.UnixMicro(20), ExpiresAt: time.UnixMicro(30)})
	if err != nil || replayedCommit != first {
		t.Fatalf("reused request=%+v err=%v", replayedCommit, err)
	}
	if err := s.DeleteFileTemporaryTransfer(ctx, transfer.TransferID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReplayIncomingFileTransfer(ctx, "file-get", files[0].FileID); !errors.Is(err, ErrFileTransferExpired) {
		t.Fatalf("expired replay=%v", err)
	}

	edit := event
	edit.SourceEventID = "Ev-transfer-detach"
	edit.EventKind = SlackEventMessageChange
	edit.SlackEventAtUS = 11
	edit.Files = nil
	if _, err := s.CommitSlackSourceEvent(ctx, sourceTestPolicy, edit); err != nil {
		t.Fatal(err)
	}
	if attached, err := s.ListSlackFiles(ctx, admitted.TaskID); err != nil || len(attached) != 0 {
		t.Fatalf("attached=%+v err=%v", attached, err)
	}
	detached, err := s.GetSlackFile(ctx, files[0].FileID)
	if err != nil || detached.SourceAttached || detached.SourceDetachedAt == nil || detached.SHA256 != transfer.SHA256 || detached.ValidatedSizeBytes == nil || *detached.ValidatedSizeBytes != transfer.SizeBytes {
		t.Fatalf("detached=%+v err=%v", detached, err)
	}
	readd := edit
	readd.SourceEventID = "Ev-transfer-readd"
	readd.SlackEventAtUS = 12
	readd.Files = event.Files
	if _, err := s.CommitSlackSourceEvent(ctx, sourceTestPolicy, readd); err != nil {
		t.Fatal(err)
	}
	attached, err := s.ListSlackFiles(ctx, admitted.TaskID)
	if err != nil || len(attached) != 1 || attached[0].FileID != files[0].FileID || !attached[0].SourceAttached || attached[0].SHA256 != transfer.SHA256 {
		t.Fatalf("re-added=%+v err=%v", attached, err)
	}
}

func TestFileUploadPrepareFailRetryCloneAndConfirm(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	intent := SlackFileUploadIntent{Text: "initial comment", Files: []SlackUploadFile{{SourcePath: "/safe/input", OriginalName: "input.txt", SizeBytes: 0, SHA256: strings.Repeat("b", 64)}}}
	prepared, err := s.PrepareSlackFileUpload(ctx, "upload-prepare", effectTestTarget, intent)
	if err != nil || prepared.Kind != SlackEffectFileUpload || prepared.State != SlackEffectPrepared {
		t.Fatalf("prepare=%+v err=%v", prepared, err)
	}
	original, err := listOutgoingFilesForEffectTx(ctx, s.DB(), prepared.EffectID)
	if err != nil || len(original) != 1 || original[0].EffectOrdinal == nil || *original[0].EffectOrdinal != 0 || original[0].SourcePath != "/safe/input" {
		t.Fatalf("prepared files=%+v err=%v", original, err)
	}
	if _, err := s.FailPreparedSlackFileUpload(ctx, prepared.EffectID, SlackEffectDiagnosticFileChanged); err != nil {
		t.Fatal(err)
	}
	failed, err := s.GetSlackEffect(ctx, prepared.EffectID)
	if err != nil || failed.State != SlackEffectFailed || failed.AttemptCount != 0 || failed.SubmittedAt != nil {
		t.Fatalf("failed=%+v err=%v", failed, err)
	}
	retry, err := s.RetrySlackEffect(ctx, "upload-retry", prepared.EffectID, "operator retry", false)
	if err != nil {
		t.Fatal(err)
	}
	cloned, err := listOutgoingFilesForEffectTx(ctx, s.DB(), retry.EffectID)
	if err != nil || len(cloned) != 1 || cloned[0].FileID == original[0].FileID || cloned[0].SourcePath != original[0].SourcePath || cloned[0].SHA256 != original[0].SHA256 {
		t.Fatalf("cloned=%+v err=%v", cloned, err)
	}
	preserved, err := listOutgoingFilesForEffectTx(ctx, s.DB(), prepared.EffectID)
	if err != nil || len(preserved) != 1 || preserved[0].TransferState != string(SlackEffectFailed) {
		t.Fatalf("predecessor files=%+v err=%v", preserved, err)
	}
	if _, err := s.ClaimSlackEffect(ctx, retry.EffectID); err != nil {
		t.Fatal(err)
	}
	confirmed, err := s.CommitSlackFileUploadOutcome(ctx, retry.EffectID, SlackEffectOutcome{State: SlackEffectConfirmed}, []FileUploadOutcome{{FileID: cloned[0].FileID, SlackFileID: "F12345678"}})
	if err != nil || confirmed.State != SlackEffectConfirmed {
		t.Fatalf("confirm=%+v err=%v", confirmed, err)
	}
	confirmedFiles, err := listOutgoingFilesForEffectTx(ctx, s.DB(), retry.EffectID)
	if err != nil || len(confirmedFiles) != 1 || confirmedFiles[0].TransferState != string(SlackEffectConfirmed) || confirmedFiles[0].SlackFileID != "F12345678" {
		t.Fatalf("confirmed files=%+v err=%v", confirmedFiles, err)
	}
	counts, err := s.SlackFileTransferCounts(ctx)
	if err != nil || counts.Outgoing[string(SlackEffectConfirmed)] != 1 || counts.Outgoing[string(SlackEffectFailed)] != 1 || counts.TemporaryCount != 0 || counts.TemporaryBytes != 0 {
		t.Fatalf("counts=%+v err=%v", counts, err)
	}
}

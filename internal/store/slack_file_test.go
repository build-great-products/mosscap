package store

import (
	"context"
	"errors"
	"testing"
)

func TestSlackFileSelectorsAndReconciliationPreserveIdentity(t *testing.T) {
	s := testStore(t)
	trustSourceTestUser(t, s)
	ctx := context.Background()
	oneSize := int64(1)
	initial := sourceTestEvent("Ev-file-initial", SlackEventAppMention, 10)
	initial.Files = []SlackIncomingFile{
		{SlackFileID: "F22222222", OriginalName: "two.txt"},
		{SlackFileID: "F11111111", OriginalName: "one.txt", SizeBytes: &oneSize},
	}
	admitted, err := s.CommitSlackSourceEvent(ctx, sourceTestPolicy, initial)
	if err != nil {
		t.Fatal(err)
	}
	files, err := s.ListSlackFiles(ctx, admitted.TaskID)
	if err != nil || len(files) != 2 || files[0].SlackFileID != "F11111111" || files[1].SlackFileID != "F22222222" {
		t.Fatalf("initial files=%+v err=%v", files, err)
	}
	firstID := files[0].FileID
	selected, err := s.GetSlackFile(ctx, firstID[:12])
	if err != nil || selected.FileID != firstID || selected.RelatedTaskID != admitted.TaskID {
		t.Fatalf("selected=%+v err=%v", selected, err)
	}
	for _, selector := range []string{"tsk_wrong_type", "fil_%", "fil_bad!"} {
		if _, err := s.GetSlackFile(ctx, selector); !errors.Is(err, ErrNotFound) {
			t.Fatalf("unsafe selector %q: %v", selector, err)
		}
	}

	edit := initial
	edit.SourceEventID = "Ev-file-edit"
	edit.EventKind = SlackEventMessageChange
	edit.SlackEventAtUS = 11
	edit.Files = []SlackIncomingFile{
		{SlackFileID: "F11111111", OriginalName: "renamed.txt", DeclaredMIME: "text/plain"},
		{SlackFileID: "F33333333", OriginalName: "three.txt"},
	}
	result, err := s.CommitSlackSourceEvent(ctx, sourceTestPolicy, edit)
	if err != nil || !result.Changed || result.TaskVersion != 2 {
		t.Fatalf("edit=%+v err=%v", result, err)
	}
	files, err = s.ListSlackFiles(ctx, admitted.TaskID)
	if err != nil || len(files) != 2 || files[0].SlackFileID != "F11111111" || files[0].FileID != firstID || files[0].OriginalName != "renamed.txt" || files[1].SlackFileID != "F33333333" {
		t.Fatalf("reconciled files=%+v err=%v", files, err)
	}
	if _, err := s.GetSlackFile(ctx, selected.FileID); err != nil {
		t.Fatalf("preserved file ID: %v", err)
	}
	// Prefix-free IDs cannot use a type prefix as a guaranteed ambiguous
	// selector; callers use an opaque prefix or the full ID.
}

func TestSlackMalformedFileMetadataCreatesNoTaskOrFileAndLeaksNoCanary(t *testing.T) {
	s := testStore(t)
	trustSourceTestUser(t, s)
	ctx := context.Background()
	const canary = "SECRET-FILENAME-CANARY"
	event := sourceTestEvent("Ev-file-malformed", SlackEventMessage, 10)
	event.Files = []SlackIncomingFile{{SlackFileID: "F12345678", OriginalName: canary + "\x00"}}
	result, err := s.CommitSlackSourceEvent(ctx, sourceTestPolicy, event)
	if err != nil || result.AdmissionResult != SlackMalformed || result.DiagnosticCode != slackDiagnosticInvalidFile {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	var files, tasks, leaks int
	if err := s.DB().QueryRow("SELECT count(*) FROM files").Scan(&files); err != nil {
		t.Fatal(err)
	}
	if err := s.DB().QueryRow("SELECT count(*) FROM tasks").Scan(&tasks); err != nil {
		t.Fatal(err)
	}
	if err := s.DB().QueryRow("SELECT count(*) FROM slack_source_events WHERE diagnostic_code LIKE '%' || ? || '%'", canary).Scan(&leaks); err != nil {
		t.Fatal(err)
	}
	if files != 0 || tasks != 0 || leaks != 0 {
		t.Fatalf("durable malformed metadata files=%d tasks=%d leaks=%d", files, tasks, leaks)
	}
}

func TestSlackDuplicateFileIdentityIsMalformedBeforeAtomicAdmission(t *testing.T) {
	s := testStore(t)
	trustSourceTestUser(t, s)
	event := sourceTestEvent("Ev-file-duplicate", SlackEventAppMention, 10)
	event.Files = []SlackIncomingFile{
		{SlackFileID: "F12345678", OriginalName: "one"},
		{SlackFileID: "F12345678", OriginalName: "two"},
	}
	result, err := s.CommitSlackSourceEvent(context.Background(), sourceTestPolicy, event)
	if err != nil || result.AdmissionResult != SlackMalformed {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	counts, err := s.SlackSourceEventCounts(context.Background())
	if err != nil || counts.Total != 1 || counts.Sources != 0 || counts.Files != 0 {
		t.Fatalf("counts=%+v err=%v", counts, err)
	}
}

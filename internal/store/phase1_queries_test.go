package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestTaskReadModelPreservesLocalTasksAndJoinsSlackMetadata(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)

	local, err := s.Create(ctx, "phase1-query-local", "local task text")
	if err != nil {
		t.Fatal(err)
	}
	localRead, err := s.GetTaskReadModel(ctx, local.TaskID[:12])
	if err != nil {
		t.Fatal(err)
	}
	if localRead.Task.ID != local.TaskID || localRead.Task.Text != "local task text" || localRead.SlackSource != nil || len(localRead.Files) != 0 {
		t.Fatalf("local read = %#v", localRead)
	}

	if _, err := s.AddTrustedUser(ctx, "phase1-query-trust", "U12345678"); err != nil {
		t.Fatal(err)
	}
	size := int64(42)
	event := sourceTestEvent("Ev-phase1-query", SlackEventAppMention, 100)
	event.Files = []SlackIncomingFile{
		{SlackFileID: "F22222222", OriginalName: "second.txt"},
		{SlackFileID: "F11111111", OriginalName: "first.txt", DeclaredMIME: "text/plain", SizeBytes: &size},
	}
	admitted, err := s.CommitSlackSourceEvent(ctx, sourceTestPolicy, event)
	if err != nil {
		t.Fatal(err)
	}
	deleted := sourceTestEvent("Ev-phase1-query-delete", SlackEventMessageDelete, 101)
	deleted.MessageTS = event.MessageTS
	if _, err := s.CommitSlackSourceEvent(ctx, sourceTestPolicy, deleted); err != nil {
		t.Fatal(err)
	}

	read, err := s.GetTaskReadModel(ctx, admitted.TaskID[:12])
	if err != nil {
		t.Fatal(err)
	}
	if read.Task.ID != admitted.TaskID || read.Task.SourceKind != "slack" || read.Task.Text != "do this <@U99999999>\nnext" || read.SlackSource == nil || read.SlackSource.SourceDeletedAt == nil {
		t.Fatalf("Slack read = %#v", read)
	}
	if read.SlackSource.TaskID != read.Task.ID || read.SlackSource.MessageTS != event.MessageTS || len(read.Files) != 2 || read.Files[0].SlackFileID != "F11111111" || read.Files[0].SizeBytes == nil || *read.Files[0].SizeBytes != size || read.Files[1].SlackFileID != "F22222222" {
		t.Fatalf("Slack metadata = %#v", read)
	}
}

func TestTaskReadModelLatestSlackReplyEffectIsExactAndDeterministic(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	trustSourceTestUser(t, s)
	event := sourceTestEvent("Ev-phase1-reply", SlackEventAppMention, 100)
	admitted, err := s.CommitSlackSourceEvent(ctx, sourceTestPolicy, event)
	if err != nil {
		t.Fatal(err)
	}
	read := func() *SlackEffect {
		t.Helper()
		model, err := s.GetTaskReadModel(ctx, admitted.TaskID)
		if err != nil {
			t.Fatal(err)
		}
		return model.LatestReplyEffect
	}
	if effect := read(); effect != nil {
		t.Fatalf("no reply effect = %+v", effect)
	}
	source, err := s.GetSlackSource(ctx, admitted.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	// Effects must be sends related to this task and exact source thread.
	prepareEffect(t, s, "phase1-reply-unrelated", SlackEffectIntent{Kind: SlackEffectSend, RelatedTaskID: admitted.TaskID, ThreadTS: "1700000009.000001", Text: "unrelated"})
	prepareEffect(t, s, "phase1-reply-react", SlackEffectIntent{Kind: SlackEffectReact, RelatedTaskID: admitted.TaskID, TargetMessageTS: event.MessageTS, Emoji: "eyes"})
	if effect := read(); effect != nil {
		t.Fatalf("unrelated effect selected = %+v", effect)
	}

	first := prepareEffect(t, s, "phase1-reply-first", SlackEffectIntent{Kind: SlackEffectSend, RelatedTaskID: admitted.TaskID, ThreadTS: source.ThreadTS, Text: "secret first"})
	second := prepareEffect(t, s, "phase1-reply-second", SlackEffectIntent{Kind: SlackEffectSend, RelatedTaskID: admitted.TaskID, ThreadTS: source.ThreadTS, Text: "secret second"})
	// Equal update timestamps exercise the documented effect-ID tie-breaker.
	if _, err := s.DB().ExecContext(ctx, "UPDATE slack_effects SET updated_at_us=(SELECT max(created_at_us) FROM slack_effects) WHERE effect_id IN (?,?)", first.ID, second.ID); err != nil {
		t.Fatal(err)
	}
	want := first
	if second.ID > first.ID {
		want = second
	}
	if got := read(); got == nil || got.ID != want.ID || got.State != SlackEffectPrepared {
		t.Fatalf("prepared latest = %+v, want %s", got, want.ID)
	}
	if _, err := s.ClaimSlackEffect(ctx, want.ID); err != nil {
		t.Fatal(err)
	}
	if got := read(); got == nil || got.ID != want.ID || got.State != SlackEffectSubmitted {
		t.Fatalf("submitted latest = %+v", got)
	}
	if _, err := s.CommitSlackEffectOutcome(ctx, want.ID, SlackEffectOutcome{State: SlackEffectConfirmed, ResponseMessageTS: "1700000001.000001"}); err != nil {
		t.Fatal(err)
	}
	if got := read(); got == nil || got.ID != want.ID || got.State != SlackEffectConfirmed {
		t.Fatalf("confirmed latest = %+v", got)
	}
}

func TestTaskReadModelUsesSafeUnambiguousPrefixes(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)

	firstID := "tsk_aaaaabcdefghijklmnopqr"
	secondID := "tsk_aaaazbcdefghijklmnopqr"
	for _, id := range []string{firstID, secondID} {
		if _, err := s.db.ExecContext(ctx, "INSERT INTO tasks(task_id,version,state,text,source_kind,created_at,updated_at) VALUES(?,1,'open','task','local',1,1)", id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.GetTaskReadModel(ctx, "tsk_aaaa"); !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("shared prefix error = %v", err)
	}
	if _, err := s.GetTaskReadModel(ctx, "tsk_%"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unsafe selector error = %v", err)
	}
	read, err := s.GetTaskReadModel(ctx, secondID)
	if err != nil || read.Task.ID != secondID {
		t.Fatalf("exact read = %#v, %v", read, err)
	}
}

func TestSlackEffectCountsAreAccurateAndContainNoPayloadText(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	canary := "effect-payload-must-not-escape"

	prepared := prepareEffect(t, s, "phase1-count-prepared", SlackEffectIntent{Kind: SlackEffectSend, Text: canary})
	submitted := prepareEffect(t, s, "phase1-count-submitted", SlackEffectIntent{Kind: SlackEffectReact, TargetMessageTS: "1700000000.000001", Emoji: "eyes"})
	failed := prepareEffect(t, s, "phase1-count-failed", SlackEffectIntent{Kind: SlackEffectDelete, TargetMessageTS: "1700000000.000002", OperatorReason: canary})
	ambiguous := prepareEffect(t, s, "phase1-count-ambiguous", SlackEffectIntent{Kind: SlackEffectEdit, TargetMessageTS: "1700000000.000003", Text: canary})
	confirmed := prepareEffect(t, s, "phase1-count-confirmed", SlackEffectIntent{Kind: SlackEffectSend, Text: canary})
	if _, err := s.ClaimSlackEffect(ctx, submitted.ID); err != nil {
		t.Fatal(err)
	}
	for _, effect := range []SlackEffect{failed, ambiguous, confirmed} {
		if _, err := s.ClaimSlackEffect(ctx, effect.ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.CommitSlackEffectOutcome(ctx, failed.ID, SlackEffectOutcome{State: SlackEffectFailed, DiagnosticCode: SlackEffectDiagnosticAPIRejected}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CommitSlackEffectOutcome(ctx, ambiguous.ID, SlackEffectOutcome{State: SlackEffectAmbiguous, DiagnosticCode: SlackEffectDiagnosticTimeout}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CommitSlackEffectOutcome(ctx, confirmed.ID, SlackEffectOutcome{State: SlackEffectConfirmed, ResponseMessageTS: "1700000001.000001"}); err != nil {
		t.Fatal(err)
	}

	counts, err := s.SlackEffectCounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if counts.Total != 5 || counts.ActionRequired != 2 || counts.ByState[SlackEffectPrepared] != 1 || counts.ByState[SlackEffectSubmitted] != 1 || counts.ByState[SlackEffectConfirmed] != 1 || counts.ByState[SlackEffectFailed] != 1 || counts.ByState[SlackEffectAmbiguous] != 1 {
		t.Fatalf("counts = %#v", counts)
	}
	encoded, err := json.Marshal(counts)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), canary) {
		t.Fatalf("count output leaked payload text: %s", encoded)
	}
	_ = prepared
}

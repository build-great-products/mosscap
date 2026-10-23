package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stayradiated/mosscap/internal/clock"
)

var sourceTestPolicy = SlackAdmissionPolicy{TeamID: "T12345678", Channels: []string{"C12345678"}, BotUserID: "U99999999"}

func sourceTestEvent(id string, kind SlackSourceEventKind, order int64) SlackSourceEvent {
	return SlackSourceEvent{
		SourceEventID:  id,
		EnvelopeID:     "env-" + id,
		EventKind:      kind,
		TeamID:         sourceTestPolicy.TeamID,
		ChannelID:      sourceTestPolicy.Channels[0],
		UserID:         "U12345678",
		MessageTS:      "1700000000.000001",
		Text:           "do this <@U99999999>  \r\nnext\t",
		Permalink:      "https://example.invalid/archives/source",
		SlackEventAtUS: order,
		ReceivedAt:     time.UnixMicro(1_700_000_000_000_000).UTC(),
	}
}

func trustSourceTestUser(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.AddTrustedUser(context.Background(), "trust-source-test", "U12345678"); err != nil {
		t.Fatal(err)
	}
}

func TestCommitSlackInitialIsAtomicAndCanonical(t *testing.T) {
	s := testStore(t)
	trustSourceTestUser(t, s)
	ctx := context.Background()
	size := int64(17)
	event := sourceTestEvent("Ev-initial", SlackEventAppMention, 1_700_000_000_000_001)
	event.ThreadTS = "1699999999.000001"
	event.Files = []SlackIncomingFile{{SlackFileID: "F12345678", OriginalName: "canary-name.txt", DeclaredMIME: "text/plain", Permalink: "https://example.invalid/file", SizeBytes: &size}}

	result, err := s.CommitSlackSourceEvent(ctx, sourceTestPolicy, event)
	if err != nil {
		t.Fatal(err)
	}
	if result.AdmissionResult != SlackAdmitted || result.TaskID == "" || result.TaskVersion != 1 || !result.Changed || result.DuplicateDelivery {
		t.Fatalf("admission=%+v", result)
	}
	task, err := s.Get(ctx, result.TaskID)
	if err != nil || task.SourceKind != "slack" || task.Text != "do this <@U99999999>\nnext" {
		t.Fatalf("task=%+v err=%v", task, err)
	}
	source, err := s.GetSlackSource(ctx, result.TaskID[:12])
	if err != nil || source.InitialSourceEventID != event.SourceEventID || source.ThreadTS != event.ThreadTS || source.AuthorUserID != event.UserID {
		t.Fatalf("source=%+v err=%v", source, err)
	}
	found, err := s.FindSlackSource(ctx, event.TeamID, event.ChannelID, event.MessageTS)
	if err != nil || found.TaskID != result.TaskID {
		t.Fatalf("source identity=%+v err=%v", found, err)
	}
	files, err := s.ListSlackFiles(ctx, result.TaskID)
	if err != nil || len(files) != 1 || files[0].SlackFileID != "F12345678" || files[0].TransferState != "metadata_only" || files[0].SizeBytes == nil || *files[0].SizeBytes != size {
		t.Fatalf("files=%+v err=%v", files, err)
	}
	canonical, err := s.LookupSlackEventResult(ctx, event.SourceEventID)
	if err != nil || canonical.AdmissionResult != SlackAdmitted || canonical.TaskID != result.TaskID {
		t.Fatalf("canonical=%+v err=%v", canonical, err)
	}
	replay, err := s.CommitSlackSourceEvent(ctx, sourceTestPolicy, event)
	if err != nil || !replay.DuplicateDelivery || replay.AdmissionResult != SlackAdmitted || replay.TaskID != result.TaskID {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	counts, err := s.SlackSourceEventCounts(ctx)
	if err != nil || counts.Total != 1 || counts.Sources != 1 || counts.Files != 1 || counts.ByResult[SlackAdmitted] != 1 {
		t.Fatalf("counts=%+v err=%v", counts, err)
	}
}

func TestSlackAdmissionPolicyTrustAndMalformedResultsAreDurableAndSafe(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	tests := []struct {
		name   string
		mutate func(*SlackSourceEvent)
		want   SlackAdmissionResult
	}{
		{"workspace", func(e *SlackSourceEvent) { e.TeamID = "T87654321" }, SlackIgnoredWorkspace},
		{"channel", func(e *SlackSourceEvent) { e.ChannelID = "G87654321" }, SlackIgnoredChannel},
		{"untrusted", func(e *SlackSourceEvent) {}, SlackIgnoredUntrusted},
		{"untrusted chatter", func(e *SlackSourceEvent) { e.Text = "just talking" }, SlackIgnoredUnmentioned},
		{"bot", func(e *SlackSourceEvent) { e.UserID, e.BotID, e.Text = "", "B12345678", "" }, SlackIgnoredBot},
	}
	for i, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			event := sourceTestEvent(fmt.Sprintf("Ev-policy-%d", i), SlackEventMessage, 100+int64(i))
			event.MessageTS = fmt.Sprintf("170000000%d.000001", i)
			test.mutate(&event)
			result, err := s.CommitSlackSourceEvent(ctx, sourceTestPolicy, event)
			if err != nil || result.AdmissionResult != test.want || result.TaskID != "" {
				t.Fatalf("result=%+v err=%v", result, err)
			}
		})
	}
	trustSourceTestUser(t, s)
	unmentioned := sourceTestEvent("Ev-unmentioned", SlackEventMessage, 200)
	unmentioned.MessageTS = "1700000010.000001"
	unmentioned.Text = "ordinary message"
	if result, err := s.CommitSlackSourceEvent(ctx, sourceTestPolicy, unmentioned); err != nil || result.AdmissionResult != SlackIgnoredUnmentioned {
		t.Fatalf("unmentioned=%+v err=%v", result, err)
	}

	const canary = "SECRET-MESSAGE-CANARY"
	malformed := sourceTestEvent("Ev-malformed", SlackEventMessage, 300)
	malformed.MessageTS = "1700000020.000001"
	malformed.Text = canary + "\x00"
	result, err := s.CommitSlackSourceEvent(ctx, sourceTestPolicy, malformed)
	if err != nil || result.AdmissionResult != SlackMalformed || result.DiagnosticCode != slackDiagnosticInvalidText {
		t.Fatalf("malformed=%+v err=%v", result, err)
	}
	var leaks int
	if err := s.DB().QueryRow(`SELECT
		(SELECT count(*) FROM tasks WHERE text LIKE '%' || ? || '%') +
		(SELECT count(*) FROM slack_source_events WHERE diagnostic_code LIKE '%' || ? || '%')`, canary, canary).Scan(&leaks); err != nil || leaks != 0 {
		t.Fatalf("canary leaks=%d err=%v", leaks, err)
	}
	var tasks int
	if err := s.DB().QueryRow("SELECT count(*) FROM tasks").Scan(&tasks); err != nil || tasks != 0 {
		t.Fatalf("ignored tasks=%d err=%v", tasks, err)
	}
}

func TestConcurrentSlackEventDedupAndSourceIdentityProduceOneTask(t *testing.T) {
	s := testStore(t)
	trustSourceTestUser(t, s)
	ctx := context.Background()
	event := sourceTestEvent("Ev-concurrent", SlackEventAppMention, 1_000)
	const workers = 12
	results := make(chan SlackEventResult, workers)
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := s.CommitSlackSourceEvent(ctx, sourceTestPolicy, event)
			results <- result
			errs <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var taskID string
	var duplicates int
	for result := range results {
		if taskID == "" {
			taskID = result.TaskID
		}
		if result.TaskID != taskID || result.AdmissionResult != SlackAdmitted {
			t.Fatalf("inconsistent result=%+v first=%q", result, taskID)
		}
		if result.DuplicateDelivery {
			duplicates++
		}
	}
	if duplicates != workers-1 {
		t.Fatalf("duplicate deliveries=%d", duplicates)
	}
	alternate := event
	alternate.SourceEventID = "Ev-alternate-subscription"
	alternate.EnvelopeID = "env-alternate"
	result, err := s.CommitSlackSourceEvent(ctx, sourceTestPolicy, alternate)
	if err != nil || result.AdmissionResult != SlackDuplicate || result.TaskID != taskID || result.DuplicateDelivery {
		t.Fatalf("alternate source delivery=%+v err=%v", result, err)
	}
	var tasks, sources, events int
	if err := s.DB().QueryRow("SELECT (SELECT count(*) FROM tasks),(SELECT count(*) FROM slack_task_sources),(SELECT count(*) FROM slack_source_events)").Scan(&tasks, &sources, &events); err != nil || tasks != 1 || sources != 1 || events != 2 {
		t.Fatalf("inventory=%d/%d/%d err=%v", tasks, sources, events, err)
	}
}

func TestSlackEditStaleNoopMeaningfulAndDeletionOrdering(t *testing.T) {
	s := testStore(t)
	trustSourceTestUser(t, s)
	ctx := context.Background()
	initial := sourceTestEvent("Ev-edit-initial", SlackEventAppMention, 1_000)
	initial.Files = []SlackIncomingFile{{SlackFileID: "F12345678", OriginalName: "first.txt"}}
	admitted, err := s.CommitSlackSourceEvent(ctx, sourceTestPolicy, initial)
	if err != nil {
		t.Fatal(err)
	}
	stale := initial
	stale.SourceEventID = "Ev-edit-stale"
	stale.EventKind = SlackEventMessageChange
	stale.SlackEventAtUS = 999
	stale.UserID = "U87654321"
	stale.BotID = "B12345678"
	stale.Text = "invalid\x00mutable text"
	stale.Files = []SlackIncomingFile{{SlackFileID: "Fbad", OriginalName: "invalid"}}
	if result, err := s.CommitSlackSourceEvent(ctx, sourceTestPolicy, stale); err != nil || result.AdmissionResult != SlackIgnoredStale || result.TaskVersion != 1 || result.DiagnosticCode != "" {
		t.Fatalf("stale=%+v err=%v", result, err)
	}
	noop := initial
	noop.SourceEventID = "Ev-edit-noop"
	noop.EventKind = SlackEventMessageChange
	noop.SlackEventAtUS = 1_001
	if result, err := s.CommitSlackSourceEvent(ctx, sourceTestPolicy, noop); err != nil || result.AdmissionResult != SlackUpdated || result.Changed || result.TaskVersion != 1 {
		t.Fatalf("noop=%+v err=%v", result, err)
	}
	changed := noop
	changed.SourceEventID = "Ev-edit-changed"
	changed.SlackEventAtUS = 1_002
	changed.Text = "mention may now be absent"
	changed.Permalink = "https://example.invalid/new"
	changed.Files = []SlackIncomingFile{{SlackFileID: "F12345678", OriginalName: "renamed.txt"}, {SlackFileID: "F87654321", OriginalName: "new.txt"}}
	updated, err := s.CommitSlackSourceEvent(ctx, sourceTestPolicy, changed)
	if err != nil || !updated.Changed || updated.TaskVersion != 2 {
		t.Fatalf("updated=%+v err=%v", updated, err)
	}
	deletion := sourceTestEvent("Ev-delete", SlackEventMessageDelete, 1_003)
	deletion.Text = ""
	deleted, err := s.CommitSlackSourceEvent(ctx, sourceTestPolicy, deletion)
	if err != nil || deleted.AdmissionResult != SlackDeleted || deleted.TaskVersion != 3 || !deleted.Changed {
		t.Fatalf("deleted=%+v err=%v", deleted, err)
	}
	task, err := s.Get(ctx, admitted.TaskID)
	if err != nil || task.Text != changed.Text || task.State != "open" || task.Version != 3 {
		t.Fatalf("preserved task=%+v err=%v", task, err)
	}
	files, err := s.ListSlackFiles(ctx, admitted.TaskID)
	if err != nil || len(files) != 2 {
		t.Fatalf("preserved files=%+v err=%v", files, err)
	}
	repeated := deletion
	repeated.SourceEventID = "Ev-delete-repeated"
	repeated.SlackEventAtUS = 1_004
	if result, err := s.CommitSlackSourceEvent(ctx, sourceTestPolicy, repeated); err != nil || result.AdmissionResult != SlackIgnoredStale || result.TaskVersion != 3 {
		t.Fatalf("repeated=%+v err=%v", result, err)
	}
	source, err := s.GetSlackSource(ctx, admitted.TaskID)
	if err != nil || source.SourceDeletedAt == nil || source.LatestEventAtUS != 1_003 {
		t.Fatalf("deleted source=%+v err=%v", source, err)
	}
	canonical, err := s.CommitSlackSourceEvent(ctx, sourceTestPolicy, initial)
	if err != nil || !canonical.DuplicateDelivery || canonical.AdmissionResult != SlackAdmitted || canonical.TaskVersion != 1 || !canonical.Changed || canonical.RedeliveryCount != 1 {
		t.Fatalf("canonical initial replay after edits=%+v err=%v", canonical, err)
	}
}

func TestSlackRedeliveryAccountingAndCanonicalResultSurviveRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ledger.sqlite3")
	manual := clock.NewManual(time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC))
	s, err := Initialize(ctx, path, Options{Clock: manual})
	if err != nil {
		t.Fatal(err)
	}
	trustSourceTestUser(t, s)
	initial := sourceTestEvent("Ev-durable-redelivery", SlackEventAppMention, 100)
	first, err := s.CommitSlackSourceEvent(ctx, sourceTestPolicy, initial)
	if err != nil {
		t.Fatal(err)
	}
	edit := initial
	edit.SourceEventID = "Ev-after-original"
	edit.EventKind = SlackEventMessageChange
	edit.SlackEventAtUS = 101
	edit.Text = "later mutable state"
	if result, err := s.CommitSlackSourceEvent(ctx, sourceTestPolicy, edit); err != nil || result.TaskVersion != 2 || !result.Changed {
		t.Fatalf("later edit=%+v err=%v", result, err)
	}
	if err := manual.Advance(time.Second); err != nil {
		t.Fatal(err)
	}
	replay, err := s.CommitSlackSourceEvent(ctx, sourceTestPolicy, initial)
	if err != nil || !replay.DuplicateDelivery || replay.TaskVersion != first.TaskVersion || replay.Changed != first.Changed || replay.RedeliveryCount != 1 || !replay.LastSeenAt.After(first.LastSeenAt) {
		t.Fatalf("first replay=%+v first=%+v err=%v", replay, first, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := manual.Advance(time.Second); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, path, Options{Clock: manual})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	restarted, err := s.CommitSlackSourceEvent(ctx, sourceTestPolicy, initial)
	if err != nil || !restarted.DuplicateDelivery || restarted.AdmissionResult != first.AdmissionResult || restarted.TaskID != first.TaskID || restarted.TaskVersion != first.TaskVersion || restarted.Changed != first.Changed || restarted.RedeliveryCount != 2 || !restarted.LastSeenAt.After(replay.LastSeenAt) {
		t.Fatalf("restart replay=%+v first=%+v err=%v", restarted, first, err)
	}
	lookedUp, err := s.LookupSlackEventResult(ctx, initial.SourceEventID)
	if err != nil || lookedUp.DuplicateDelivery || lookedUp.RedeliveryCount != 2 || lookedUp.TaskVersion != 1 || !lookedUp.Changed || !lookedUp.LastSeenAt.Equal(restarted.LastSeenAt) {
		t.Fatalf("lookup=%+v err=%v", lookedUp, err)
	}
	counts, err := s.SlackSourceEventCounts(ctx)
	if err != nil || counts.Total != 2 || counts.Redeliveries != 2 || counts.ByResult[SlackAdmitted] != 1 || counts.ByResult[SlackUpdated] != 1 {
		t.Fatalf("counts=%+v err=%v", counts, err)
	}
}

func TestSlackAdmissionFailureRollsBackTaskEventSourceAndFiles(t *testing.T) {
	s := testStore(t)
	trustSourceTestUser(t, s)
	ctx := context.Background()
	if _, err := s.DB().Exec(`CREATE TRIGGER fail_slack_file BEFORE INSERT ON files BEGIN SELECT RAISE(ABORT, 'injected'); END`); err != nil {
		t.Fatal(err)
	}
	event := sourceTestEvent("Ev-rollback", SlackEventAppMention, 1_000)
	event.Files = []SlackIncomingFile{{SlackFileID: "F12345678", OriginalName: "rollback-canary"}}
	if _, err := s.CommitSlackSourceEvent(ctx, sourceTestPolicy, event); err == nil {
		t.Fatal("injected commit path succeeded")
	}
	var total int
	if err := s.DB().QueryRow("SELECT (SELECT count(*) FROM tasks)+(SELECT count(*) FROM slack_source_events)+(SELECT count(*) FROM slack_task_sources)+(SELECT count(*) FROM files)").Scan(&total); err != nil || total != 0 {
		t.Fatalf("partial rows=%d err=%v", total, err)
	}
	if _, err := s.DB().Exec("DROP TRIGGER fail_slack_file"); err != nil {
		t.Fatal(err)
	}
	if result, err := s.CommitSlackSourceEvent(ctx, sourceTestPolicy, event); err != nil || result.AdmissionResult != SlackAdmitted {
		t.Fatalf("redelivery=%+v err=%v", result, err)
	}
}

func TestSlackAdmissionBusyAndCancellationLeaveNoPartialLedger(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ledger.sqlite3")
	s, err := Initialize(ctx, path, Options{BusyTimeout: 25 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	trustSourceTestUser(t, s)
	event := sourceTestEvent("Ev-busy", SlackEventAppMention, 1_000)
	event.Files = []SlackIncomingFile{{SlackFileID: "F12345678", OriginalName: "busy.txt"}}

	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.CommitSlackSourceEvent(canceled, sourceTestPolicy, event); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled admission=%v", err)
	}

	locker, err := s.DB().Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer locker.Close()
	if _, err := locker.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CommitSlackSourceEvent(ctx, sourceTestPolicy, event); err == nil {
		t.Fatal("admission unexpectedly crossed an external SQLite write lock")
	}
	var total int
	if err := s.DB().QueryRow(`SELECT
		(SELECT count(*) FROM tasks) +
		(SELECT count(*) FROM slack_source_events) +
		(SELECT count(*) FROM slack_task_sources) +
		(SELECT count(*) FROM files)`).Scan(&total); err != nil || total != 0 {
		t.Fatalf("busy/canceled admission left rows=%d err=%v", total, err)
	}
	if _, err := locker.ExecContext(ctx, "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`CREATE TRIGGER cancel_slack_file BEFORE INSERT ON files BEGIN
		SELECT sum(x) FROM (WITH RECURSIVE counter(x) AS (
			VALUES(0) UNION ALL SELECT x+1 FROM counter WHERE x < 100000000
		) SELECT x FROM counter);
	END`); err != nil {
		t.Fatal(err)
	}
	deadlineCtx, deadlineCancel := context.WithTimeout(ctx, 5*time.Millisecond)
	_, deadlineErr := s.CommitSlackSourceEvent(deadlineCtx, sourceTestPolicy, event)
	deadlineState := deadlineCtx.Err()
	deadlineCancel()
	if deadlineErr == nil || !errors.Is(deadlineState, context.DeadlineExceeded) {
		t.Fatalf("canceled in-flight admission: err=%v ctx=%v", deadlineErr, deadlineState)
	}
	if _, err := s.DB().Exec("DROP TRIGGER cancel_slack_file"); err != nil {
		t.Fatal(err)
	}
	if err := s.DB().QueryRow(`SELECT
		(SELECT count(*) FROM tasks) +
		(SELECT count(*) FROM slack_source_events) +
		(SELECT count(*) FROM slack_task_sources) +
		(SELECT count(*) FROM files)`).Scan(&total); err != nil || total != 0 {
		t.Fatalf("in-flight cancellation left rows=%d err=%v", total, err)
	}
	if result, err := s.CommitSlackSourceEvent(ctx, sourceTestPolicy, event); err != nil || result.AdmissionResult != SlackAdmitted {
		t.Fatalf("retry after lock=%+v err=%v", result, err)
	}
}

func TestSlackSafeSelectorsAndMissingLookups(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.Create(ctx, "local-source-selector", "local"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetSlackSource(ctx, "tsk_%"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wildcard source selector=%v", err)
	}
	if _, err := s.LookupSlackEventResult(ctx, "Ev_%"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("exact event lookup=%v", err)
	}
	if _, err := s.FindSlackSource(ctx, "Tbad", "Cbad", "bad"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("malformed identity=%v", err)
	}
}

func TestSlackRepositoryRejectsSchemaValidLocalTaskSource(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	for _, statement := range []string{
		"INSERT INTO tasks(task_id,version,state,text,source_kind,created_at,updated_at) VALUES('abcdefghijklmnopqrstuv',1,'open','local','local',1,1)",
		"INSERT INTO slack_source_events(source_event_id,event_kind,team_id,channel_id,message_ts,user_id,slack_event_at_us,received_at_us,committed_at_us,admission_result,task_id,task_version,changed,last_seen_at_us) VALUES('Ev-local-invariant','message','T12345678','C12345678','1700000099.000001','U12345678',1,1,1,'admitted','abcdefghijklmnopqrstuv',1,1,1)",
		"INSERT INTO slack_task_sources(task_id,initial_source_event_id,team_id,channel_id,author_user_id,message_ts,thread_ts,latest_event_at_us,created_at_us,updated_at_us) VALUES('abcdefghijklmnopqrstuv','Ev-local-invariant','T12345678','C12345678','U12345678','1700000099.000001','1700000099.000001',1,1,1)",
	} {
		if _, err := s.DB().ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.GetSlackSource(ctx, "abcdefghijklmnopqrstuv"); !errors.Is(err, ErrSlackInvariant) {
		t.Fatalf("task source invariant=%v", err)
	}
	if _, err := s.FindSlackSource(ctx, "T12345678", "C12345678", "1700000099.000001"); !errors.Is(err, ErrSlackInvariant) {
		t.Fatalf("identity source invariant=%v", err)
	}
}

func TestSlackAdmissionWithoutAllowlistAdmitsEveryChannelSeparately(t *testing.T) {
	s := testStore(t)
	trustSourceTestUser(t, s)
	ctx := context.Background()
	open := SlackAdmissionPolicy{TeamID: sourceTestPolicy.TeamID, BotUserID: sourceTestPolicy.BotUserID}
	for i, channel := range []string{"C12345678", "C87654321", "G12345678"} {
		event := sourceTestEvent(fmt.Sprintf("Ev-open-%d", i), SlackEventAppMention, 100+int64(i))
		// The same message timestamp in different channels is a different source.
		event.ChannelID = channel
		result, err := s.CommitSlackSourceEvent(ctx, open, event)
		if err != nil || result.AdmissionResult != SlackAdmitted {
			t.Fatalf("%s result=%+v err=%v", channel, result, err)
		}
		source, err := s.GetSlackSource(ctx, result.TaskID)
		if err != nil || source.ChannelID != channel {
			t.Fatalf("%s source=%+v err=%v", channel, source, err)
		}
	}
	if _, err := s.CommitSlackSourceEvent(ctx, SlackAdmissionPolicy{TeamID: "T12345678", BotUserID: "U99999999", Channels: []string{"D12345678"}}, sourceTestEvent("Ev-bad-policy", SlackEventAppMention, 200)); err == nil {
		t.Fatal("accepted a DM channel in the allowlist")
	}
}

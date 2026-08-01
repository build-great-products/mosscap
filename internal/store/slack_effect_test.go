package store

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

var effectTestTarget = SlackEffectTarget{TeamID: "T12345678", ChannelID: "C12345678"}

func prepareEffect(t *testing.T, s *Store, requestID string, intent SlackEffectIntent) SlackEffect {
	t.Helper()
	result, err := s.PrepareSlackEffect(context.Background(), requestID, effectTestTarget, intent)
	if err != nil {
		t.Fatalf("prepare %s: %v", requestID, err)
	}
	effect, err := s.GetSlackEffect(context.Background(), result.EffectID)
	if err != nil {
		t.Fatalf("get %s: %v", result.EffectID, err)
	}
	return effect
}

func submitAndFailEffect(t *testing.T, s *Store, effectID string) SlackEffect {
	t.Helper()
	if _, err := s.ClaimSlackEffect(context.Background(), effectID); err != nil {
		t.Fatalf("claim %s: %v", effectID, err)
	}
	effect, err := s.CommitSlackEffectOutcome(context.Background(), effectID, SlackEffectOutcome{
		State: SlackEffectFailed, DiagnosticCode: SlackEffectDiagnosticAPIRejected,
	})
	if err != nil {
		t.Fatalf("fail %s: %v", effectID, err)
	}
	return effect
}

func TestSlackEffectPrepareReplayCanonicalHashAndCompactResult(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	intent := SlackEffectIntent{Kind: SlackEffectSend, Text: "hello  \r\nworld\t", ThreadTS: "1700000000.000001"}
	first, err := s.PrepareSlackEffect(ctx, "effect-send", effectTestTarget, intent)
	if err != nil {
		t.Fatal(err)
	}
	if first.State != SlackEffectPrepared || first.Kind != SlackEffectSend || first.TeamID != effectTestTarget.TeamID || first.ChannelID != effectTestTarget.ChannelID {
		t.Fatalf("prepare result=%+v", first)
	}
	// Equivalent normalized text is the same mutation request.
	replay, err := s.PrepareSlackEffect(ctx, "effect-send", effectTestTarget, SlackEffectIntent{
		Kind: SlackEffectSend, Text: "hello\nworld", ThreadTS: "1700000000.000001",
	})
	if err != nil || replay != first {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	if _, err := s.PrepareSlackEffect(ctx, "effect-send", effectTestTarget, SlackEffectIntent{Kind: SlackEffectSend, Text: "different"}); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("request conflict=%v", err)
	}
	effect, err := s.GetSlackEffect(ctx, first.EffectID)
	if err != nil {
		t.Fatal(err)
	}
	if effect.Text != "hello\nworld" || effect.ThreadTS != intent.ThreadTS || len(effect.PayloadHash) != 64 || len(effect.IdempotencyKey) != 64 {
		t.Fatalf("canonical effect=%+v", effect)
	}
	wantPayloadHash := hash("slack.effect.payload.v1", "send", effectTestTarget.TeamID, effectTestTarget.ChannelID, "", intent.ThreadTS, "hello\nworld", "")
	wantKey := hash("slack.effect.idempotency.v1", "effect-send", wantPayloadHash)
	if effect.PayloadHash != wantPayloadHash || effect.IdempotencyKey != wantKey {
		t.Fatalf("hash=%q key=%q", effect.PayloadHash, effect.IdempotencyKey)
	}
	var effectCount int
	var resultJSON string
	if err := s.DB().QueryRow("SELECT count(*) FROM slack_effects").Scan(&effectCount); err != nil {
		t.Fatal(err)
	}
	if err := s.DB().QueryRow("SELECT result_json FROM mutation_requests WHERE request_id='effect-send'").Scan(&resultJSON); err != nil {
		t.Fatal(err)
	}
	if effectCount != 1 || strings.Contains(resultJSON, "hello") || strings.Contains(resultJSON, "world") {
		t.Fatalf("effects=%d compact result=%q", effectCount, resultJSON)
	}
}

func TestSlackEffectMalformedPrepareHashPreservesInvalidUTF8(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	requestID := "invalid-utf8-conflict"
	first := SlackEffectIntent{Kind: SlackEffectSend, Text: string([]byte{0xff})}
	second := SlackEffectIntent{Kind: SlackEffectSend, Text: string([]byte{0xfe})}

	if _, err := s.PrepareSlackEffect(ctx, requestID, effectTestTarget, first); err == nil {
		t.Fatal("accepted first invalid UTF-8 payload")
	}
	if _, err := s.PrepareSlackEffect(ctx, requestID, effectTestTarget, second); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("distinct invalid UTF-8 payload did not conflict: %v", err)
	}
}

func TestSlackEffectRetryHashPreservesNULFieldBoundaries(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	requestID := "retry-nul-field-conflict"

	_, firstErr := s.RetrySlackEffect(ctx, requestID, "eff_missing\x00part", "because", false)
	if !errors.Is(firstErr, ErrSlackEffectNotFound) {
		t.Fatalf("first retry error=%v", firstErr)
	}
	// With NUL separators alone these fields encode exactly like the first call:
	// ("eff_missing\x00part", "because") versus ("eff_missing", "part\x00because").
	if _, err := s.RetrySlackEffect(ctx, requestID, "eff_missing", "part\x00because", false); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("NUL-shifted retry fields did not conflict: %v", err)
	}
}

func TestSlackEffectPrepareConcurrentReplayCreatesOneEffect(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const workers = 16
	results := make(chan SlackEffectMutationResult, workers)
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := s.PrepareSlackEffect(ctx, "concurrent-effect", effectTestTarget, SlackEffectIntent{Kind: SlackEffectReact, TargetMessageTS: "1700000000.000001", Emoji: ":thumbsup:"})
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
	var effectID string
	for result := range results {
		if effectID == "" {
			effectID = result.EffectID
		}
		if result.EffectID != effectID {
			t.Fatalf("effect IDs differ: %q and %q", effectID, result.EffectID)
		}
	}
	var effects, requests int
	if err := s.DB().QueryRow("SELECT count(*) FROM slack_effects").Scan(&effects); err != nil {
		t.Fatal(err)
	}
	if err := s.DB().QueryRow("SELECT count(*) FROM mutation_requests WHERE request_id='concurrent-effect'").Scan(&requests); err != nil {
		t.Fatal(err)
	}
	if effects != 1 || requests != 1 {
		t.Fatalf("effects=%d requests=%d", effects, requests)
	}
}

func TestSlackEffectPrepareAndMutationRequestRollBackTogether(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const trigger = "fail_slack_effect_mutation_request"
	if _, err := s.DB().Exec(`CREATE TRIGGER ` + trigger + `
		BEFORE INSERT ON mutation_requests
		WHEN NEW.operation='slack.effect.prepare'
		BEGIN SELECT RAISE(ABORT, 'injected mutation-request failure'); END`); err != nil {
		t.Fatal(err)
	}

	intent := SlackEffectIntent{Kind: SlackEffectSend, Text: "must roll back"}
	if _, err := s.PrepareSlackEffect(ctx, "atomic-prepare", effectTestTarget, intent); err == nil {
		t.Fatal("prepare succeeded through injected request-record failure")
	}
	var effects, requests int
	if err := s.DB().QueryRow("SELECT count(*) FROM slack_effects").Scan(&effects); err != nil {
		t.Fatal(err)
	}
	if err := s.DB().QueryRow("SELECT count(*) FROM mutation_requests WHERE request_id='atomic-prepare'").Scan(&requests); err != nil {
		t.Fatal(err)
	}
	if effects != 0 || requests != 0 {
		t.Fatalf("failed prepare was partially durable: effects=%d requests=%d", effects, requests)
	}

	if _, err := s.DB().Exec(`DROP TRIGGER ` + trigger); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PrepareSlackEffect(ctx, "atomic-prepare", effectTestTarget, intent); err != nil {
		t.Fatalf("retry after rolled-back fault: %v", err)
	}
}

func TestSlackEffectPrepareBusyAndCancellationLeaveNoPartialRows(t *testing.T) {
	s := testStore(t)
	blocker, err := s.DB().BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := blocker.Exec("INSERT INTO trusted_users(slack_user_id) VALUES('U12345678')"); err != nil {
		_ = blocker.Rollback()
		t.Fatal(err)
	}

	intent := SlackEffectIntent{Kind: SlackEffectSend, Text: "not partial"}
	if _, err := s.PrepareSlackEffect(context.Background(), "busy-prepare", effectTestTarget, intent); err == nil {
		_ = blocker.Rollback()
		t.Fatal("prepare succeeded while another writer held the database")
	}
	if err := blocker.Rollback(); err != nil {
		t.Fatal(err)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.PrepareSlackEffect(canceled, "canceled-prepare", effectTestTarget, intent); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled prepare error=%v", err)
	}

	var effects, requests int
	if err := s.DB().QueryRow("SELECT count(*) FROM slack_effects").Scan(&effects); err != nil {
		t.Fatal(err)
	}
	if err := s.DB().QueryRow("SELECT count(*) FROM mutation_requests WHERE request_id IN ('busy-prepare','canceled-prepare')").Scan(&requests); err != nil {
		t.Fatal(err)
	}
	if effects != 0 || requests != 0 {
		t.Fatalf("failed prepares were partially durable: effects=%d requests=%d", effects, requests)
	}
	for _, requestID := range []string{"busy-prepare", "canceled-prepare"} {
		if _, err := s.PrepareSlackEffect(context.Background(), requestID, effectTestTarget, intent); err != nil {
			t.Fatalf("retry %s: %v", requestID, err)
		}
	}
}

func TestSlackEffectOperationShapeSelectorsAndAuditTask(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	taskResult, err := s.Create(ctx, "related-create", "audit task")
	if err != nil {
		t.Fatal(err)
	}
	valid := []SlackEffectIntent{
		{Kind: SlackEffectSend, Text: "send", ThreadTS: "1700000000.000001", RelatedTaskID: taskResult.TaskID},
		{Kind: SlackEffectEdit, TargetMessageTS: "1700000000.000002", Text: "edit"},
		{Kind: SlackEffectDelete, TargetMessageTS: "1700000000.000003", OperatorReason: "operator requested"},
		{Kind: SlackEffectReact, TargetMessageTS: "1700000000.000004", Emoji: ":white_check_mark:"},
		{Kind: SlackEffectUnreact, TargetMessageTS: "1700000000.000005", Emoji: "white_check_mark"},
	}
	for i, intent := range valid {
		result, err := s.PrepareSlackEffect(ctx, "valid-shape-"+string(rune('a'+i)), effectTestTarget, intent)
		if err != nil {
			t.Fatalf("valid %s: %v", intent.Kind, err)
		}
		effect, err := s.GetSlackEffect(ctx, result.EffectID)
		if err != nil {
			t.Fatal(err)
		}
		if intent.RelatedTaskID != "" && effect.RelatedTaskID != taskResult.TaskID {
			t.Fatalf("related task=%q", effect.RelatedTaskID)
		}
		if (intent.Kind == SlackEffectReact || intent.Kind == SlackEffectUnreact) && effect.Emoji != "white_check_mark" {
			t.Fatalf("emoji=%q", effect.Emoji)
		}
	}

	invalid := []struct {
		name   string
		target SlackEffectTarget
		intent SlackEffectIntent
		is     error
	}{
		{"selector team", SlackEffectTarget{TeamID: "T123;DROP", ChannelID: "C12345678"}, SlackEffectIntent{Kind: SlackEffectSend, Text: "x"}, nil},
		{"selector channel", SlackEffectTarget{TeamID: "T12345678", ChannelID: "C123%"}, SlackEffectIntent{Kind: SlackEffectSend, Text: "x"}, nil},
		{"send target", effectTestTarget, SlackEffectIntent{Kind: SlackEffectSend, Text: "x", TargetMessageTS: "1700000000.000001"}, nil},
		{"edit no target", effectTestTarget, SlackEffectIntent{Kind: SlackEffectEdit, Text: "x"}, nil},
		{"delete no reason", effectTestTarget, SlackEffectIntent{Kind: SlackEffectDelete, TargetMessageTS: "1700000000.000001"}, nil},
		{"reaction text", effectTestTarget, SlackEffectIntent{Kind: SlackEffectReact, TargetMessageTS: "1700000000.000001", Emoji: "ok", Text: "forbidden"}, nil},
		{"bad timestamp", effectTestTarget, SlackEffectIntent{Kind: SlackEffectEdit, TargetMessageTS: "1700000000.1", Text: "x"}, nil},
		{"missing task", effectTestTarget, SlackEffectIntent{Kind: SlackEffectSend, Text: "x", RelatedTaskID: "tsk_missing"}, ErrNotFound},
		{"file upload", effectTestTarget, SlackEffectIntent{Kind: SlackEffectFileUpload}, ErrSlackEffectUnsupported},
	}
	for i, test := range invalid {
		t.Run(test.name, func(t *testing.T) {
			requestID := "invalid-shape-" + string(rune('a'+i))
			_, firstErr := s.PrepareSlackEffect(ctx, requestID, test.target, test.intent)
			_, replayErr := s.PrepareSlackEffect(ctx, requestID, test.target, test.intent)
			if firstErr == nil || replayErr == nil {
				t.Fatalf("errors first=%v replay=%v", firstErr, replayErr)
			}
			if test.is != nil && (!errors.Is(firstErr, test.is) || !errors.Is(replayErr, test.is)) {
				t.Fatalf("first=%v replay=%v want %v", firstErr, replayErr, test.is)
			}
		})
	}
}

func TestSlackEffectClaimAndTerminalOutcomeLegalTransitions(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	effect := prepareEffect(t, s, "transition-send", SlackEffectIntent{Kind: SlackEffectSend, Text: "exact intent"})
	if _, err := s.CommitSlackEffectOutcome(ctx, effect.ID, SlackEffectOutcome{State: SlackEffectFailed, DiagnosticCode: SlackEffectDiagnosticAPIRejected}); !errors.Is(err, ErrSlackEffectTransition) {
		t.Fatalf("prepared to terminal=%v", err)
	}

	const workers = 12
	errCh := make(chan error, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.ClaimSlackEffect(ctx, effect.ID)
			errCh <- err
		}()
	}
	wg.Wait()
	close(errCh)
	var claimed, rejected int
	for err := range errCh {
		switch {
		case err == nil:
			claimed++
		case errors.Is(err, ErrSlackEffectTransition):
			rejected++
		default:
			t.Fatalf("claim error=%v", err)
		}
	}
	if claimed != 1 || rejected != workers-1 {
		t.Fatalf("claimed=%d rejected=%d", claimed, rejected)
	}

	outcome := SlackEffectOutcome{State: SlackEffectConfirmed, ResponseMessageTS: "1700000001.000001"}
	confirmed, err := s.CommitSlackEffectOutcome(ctx, effect.ID, outcome)
	if err != nil || confirmed.State != SlackEffectConfirmed || confirmed.AttemptCount != 1 || confirmed.SubmittedAt == nil || confirmed.CompletedAt == nil {
		t.Fatalf("confirmed=%+v err=%v", confirmed, err)
	}
	replay, err := s.CommitSlackEffectOutcome(ctx, effect.ID, outcome)
	if err != nil || replay.ID != confirmed.ID || replay.State != confirmed.State {
		t.Fatalf("terminal replay=%+v err=%v", replay, err)
	}
	if _, err := s.CommitSlackEffectOutcome(ctx, effect.ID, SlackEffectOutcome{State: SlackEffectAmbiguous, DiagnosticCode: SlackEffectDiagnosticTimeout}); !errors.Is(err, ErrSlackEffectTransition) {
		t.Fatalf("terminal rewrite=%v", err)
	}
	if _, err := s.ClaimSlackEffect(ctx, effect.ID); !errors.Is(err, ErrSlackEffectTransition) {
		t.Fatalf("second claim=%v", err)
	}
	var attempts int
	if err := s.DB().QueryRow("SELECT attempt_count FROM slack_effects WHERE effect_id=?", effect.ID).Scan(&attempts); err != nil || attempts != 1 {
		t.Fatalf("attempts=%d err=%v", attempts, err)
	}
}

func TestSlackEffectOutcomeValidationAndStartupReconciliation(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	prepared := prepareEffect(t, s, "remain-prepared", SlackEffectIntent{Kind: SlackEffectDelete, TargetMessageTS: "1700000000.000001", OperatorReason: "cleanup"})
	submitted := prepareEffect(t, s, "reconcile-submitted", SlackEffectIntent{Kind: SlackEffectReact, TargetMessageTS: "1700000000.000002", Emoji: "eyes"})
	if _, err := s.ClaimSlackEffect(ctx, submitted.ID); err != nil {
		t.Fatal(err)
	}
	count, err := s.ReconcileSubmittedSlackEffects(ctx)
	if err != nil || count != 1 {
		t.Fatalf("reconciled=%d err=%v", count, err)
	}
	if count, err = s.ReconcileSubmittedSlackEffects(ctx); err != nil || count != 0 {
		t.Fatalf("second reconciliation=%d err=%v", count, err)
	}
	gotPrepared, _ := s.GetSlackEffect(ctx, prepared.ID)
	gotSubmitted, _ := s.GetSlackEffect(ctx, submitted.ID)
	if gotPrepared.State != SlackEffectPrepared || gotSubmitted.State != SlackEffectAmbiguous || gotSubmitted.DiagnosticCode != SlackEffectDiagnosticStartupReconciliation || gotSubmitted.CompletedAt == nil {
		t.Fatalf("prepared=%+v reconciled=%+v", gotPrepared, gotSubmitted)
	}

	badOutcomes := []SlackEffectOutcome{
		{State: SlackEffectSubmitted},
		{State: SlackEffectFailed, DiagnosticCode: "raw Slack error token=xoxb-secret"},
		{State: SlackEffectAmbiguous},
		{State: SlackEffectConfirmed, DiagnosticCode: SlackEffectDiagnosticAPIRejected},
		{State: SlackEffectFailed, DiagnosticCode: SlackEffectDiagnosticAPIRejected, ResponseMessageTS: "1700000000.000001"},
	}
	for _, outcome := range badOutcomes {
		if _, err := s.CommitSlackEffectOutcome(ctx, prepared.ID, outcome); err == nil {
			t.Fatalf("accepted outcome=%+v", outcome)
		}
	}
}

func TestSlackEffectRetryRulesReplayAndPredecessorPreservation(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	taskResult, err := s.Create(ctx, "retry-related-task", "related")
	if err != nil {
		t.Fatal(err)
	}
	predecessor := prepareEffect(t, s, "retry-original", SlackEffectIntent{
		Kind: SlackEffectEdit, TargetMessageTS: "1700000000.000001", Text: "new text", RelatedTaskID: taskResult.TaskID,
	})
	predecessor = submitAndFailEffect(t, s, predecessor.ID)

	result, err := s.RetrySlackEffect(ctx, "retry-request", predecessor.ID[:12], "explicit operator retry", false)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := s.RetrySlackEffect(ctx, "retry-request", predecessor.ID[:12], "explicit operator retry", false)
	if err != nil || replay != result {
		t.Fatalf("retry replay=%+v err=%v", replay, err)
	}
	if _, err := s.RetrySlackEffect(ctx, "retry-request", predecessor.ID[:12], "different reason", false); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("retry conflict=%v", err)
	}
	retry, err := s.GetSlackEffect(ctx, result.EffectID)
	if err != nil {
		t.Fatal(err)
	}
	if retry.PredecessorEffectID != predecessor.ID || retry.State != SlackEffectPrepared || retry.AttemptCount != 0 ||
		retry.Kind != predecessor.Kind || retry.TeamID != predecessor.TeamID || retry.ChannelID != predecessor.ChannelID ||
		retry.TargetMessageTS != predecessor.TargetMessageTS || retry.Text != predecessor.Text ||
		retry.RelatedTaskID != predecessor.RelatedTaskID || retry.PayloadHash != predecessor.PayloadHash ||
		retry.OperatorReason != "explicit operator retry" || retry.IdempotencyKey == predecessor.IdempotencyKey {
		t.Fatalf("retry=%+v predecessor=%+v", retry, predecessor)
	}
	unchanged, err := s.GetSlackEffect(ctx, predecessor.ID)
	if err != nil || unchanged.State != SlackEffectFailed || unchanged.OperatorReason != predecessor.OperatorReason {
		t.Fatalf("predecessor changed=%+v err=%v", unchanged, err)
	}
	var successors int
	if err := s.DB().QueryRow("SELECT count(*) FROM slack_effects WHERE predecessor_effect_id=?", predecessor.ID).Scan(&successors); err != nil || successors != 1 {
		t.Fatalf("successors=%d err=%v", successors, err)
	}

	ambiguous := prepareEffect(t, s, "ambiguous-original", SlackEffectIntent{Kind: SlackEffectReact, TargetMessageTS: "1700000000.000002", Emoji: "eyes"})
	if _, err := s.ClaimSlackEffect(ctx, ambiguous.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CommitSlackEffectOutcome(ctx, ambiguous.ID, SlackEffectOutcome{State: SlackEffectAmbiguous, DiagnosticCode: SlackEffectDiagnosticTimeout}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RetrySlackEffect(ctx, "ambiguous-no-confirm", ambiguous.ID, "risk understood later", false); !errors.Is(err, ErrSlackEffectDuplicateRisk) {
		t.Fatalf("ambiguous no confirmation=%v", err)
	}
	if _, err := s.RetrySlackEffect(ctx, "ambiguous-confirm", ambiguous.ID, "risk accepted", true); err != nil {
		t.Fatalf("ambiguous confirmation=%v", err)
	}

	for i, state := range []SlackEffectState{SlackEffectPrepared, SlackEffectSubmitted, SlackEffectConfirmed} {
		effect := prepareEffect(t, s, "retry-rejected-original-"+string(rune('a'+i)), SlackEffectIntent{Kind: SlackEffectSend, Text: "state test"})
		if state != SlackEffectPrepared {
			if _, err := s.ClaimSlackEffect(ctx, effect.ID); err != nil {
				t.Fatal(err)
			}
		}
		if state == SlackEffectConfirmed {
			if _, err := s.CommitSlackEffectOutcome(ctx, effect.ID, SlackEffectOutcome{State: SlackEffectConfirmed, ResponseMessageTS: "1700000002.000001"}); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := s.RetrySlackEffect(ctx, "retry-rejected-"+string(rune('a'+i)), effect.ID, "not legal", true); !errors.Is(err, ErrSlackEffectRetryNotAllowed) {
			t.Fatalf("retry state %s: %v", state, err)
		}
	}
}

func TestSlackEffectPrefixLookupListAndSelectorSecurity(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	hashValue := strings.Repeat("a", 64)
	insert := func(id, key, state string, attempt int, submitted, completed any) {
		t.Helper()
		_, err := s.DB().Exec(`INSERT INTO slack_effects(
			effect_id,idempotency_key,kind,team_id,channel_id,text,payload_hash,state,
			attempt_count,prepared_at_us,submitted_at_us,completed_at_us,created_at_us,updated_at_us
		) VALUES(?,?,'send','T12345678','C12345678','safe',?,?,?,?,?,?,1,1)`, id, key, hashValue, state, attempt, 1, submitted, completed)
		if err != nil {
			t.Fatal(err)
		}
	}
	firstID := "eff_aaaaaaaaaaaaaaaaaaaaaa"
	secondID := "eff_aaaaabbbbbbbbbbbbbbbbb"
	thirdID := "eff_bbbbbbbbbbbbbbbbbbbbbb"
	insert(firstID, "prefix-a", "prepared", 0, nil, nil)
	insert(secondID, "prefix-b", "prepared", 0, nil, nil)
	insert(thirdID, "prefix-c", "submitted", 1, 1, nil)

	if got, err := s.GetSlackEffect(ctx, firstID[:10]); err != nil || got.ID != firstID {
		t.Fatalf("unique prefix=%+v err=%v", got, err)
	}
	if _, err := s.GetSlackEffect(ctx, "eff_aaaa"); !errors.Is(err, ErrSlackEffectSelectorAmbiguous) {
		t.Fatalf("ambiguous prefix=%v", err)
	}
	for _, selector := range []string{"eff_%", "eff_' OR 1=1--", "tsk_aaaa", "eff_", firstID + "x"} {
		if _, err := s.GetSlackEffect(ctx, selector); !errors.Is(err, ErrSlackEffectNotFound) {
			t.Fatalf("selector %q: %v", selector, err)
		}
	}
	if _, err := s.ClaimSlackEffect(ctx, firstID[:10]); !errors.Is(err, ErrSlackEffectNotFound) {
		t.Fatalf("claim accepted prefix: %v", err)
	}

	prepared := SlackEffectPrepared
	list, err := s.ListSlackEffects(ctx, &prepared, 10)
	if err != nil || len(list) != 2 || list[0].ID != firstID || list[1].ID != secondID {
		t.Fatalf("prepared list=%+v err=%v", list, err)
	}
	all, err := s.ListSlackEffects(ctx, nil, 2)
	if err != nil || len(all) != 2 {
		t.Fatalf("bounded list=%+v err=%v", all, err)
	}
	badState := SlackEffectState("prepared' OR 1=1--")
	if _, err := s.ListSlackEffects(ctx, &badState, 10); err == nil {
		t.Fatal("accepted injected state")
	}
	var count int
	if err := s.DB().QueryRow("SELECT count(*) FROM slack_effects").Scan(&count); err != nil || count != 3 {
		t.Fatalf("effects after hostile selectors=%d err=%v", count, err)
	}
}

func TestSlackEffectReasonAndDiagnosticsDoNotPersistUnboundedSecrets(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	secret := "xoxb-super-secret-token"
	if _, err := s.PrepareSlackEffect(ctx, "oversize-reason", effectTestTarget, SlackEffectIntent{
		Kind: SlackEffectDelete, TargetMessageTS: "1700000000.000001", OperatorReason: strings.Repeat("r", maxSlackEffectReasonBytes+1) + secret,
	}); err == nil {
		t.Fatal("accepted unbounded reason")
	}
	if _, err := s.PrepareSlackEffect(ctx, "nul-text", effectTestTarget, SlackEffectIntent{Kind: SlackEffectSend, Text: "safe\x00" + secret}); err == nil {
		t.Fatal("accepted NUL text")
	}
	effect := prepareEffect(t, s, "security-outcome", SlackEffectIntent{Kind: SlackEffectSend, Text: "safe"})
	if _, err := s.ClaimSlackEffect(ctx, effect.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CommitSlackEffectOutcome(ctx, effect.ID, SlackEffectOutcome{State: SlackEffectFailed, DiagnosticCode: SlackEffectDiagnosticCode("timeout_" + secret)}); err == nil {
		t.Fatal("accepted non-allowlisted diagnostic")
	}
	var leaked int
	if err := s.DB().QueryRow(`SELECT count(*) FROM mutation_requests
		WHERE coalesce(result_json,'') LIKE ? OR coalesce(error_message,'') LIKE ?`, "%"+secret+"%", "%"+secret+"%").Scan(&leaked); err != nil {
		t.Fatal(err)
	}
	if leaked != 0 {
		t.Fatalf("secret persisted in mutation records: %d", leaked)
	}
}

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func TestConfirmMetadataPurgeDeletesOnlyFinalizedTaskGraphAndReplays(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const taskID = "tsk_purge_finalized"
	const deliveryID = "dlv_abcdefghijklmnopqrstuv"
	old := int64(1_700_000_000_000_000)
	cutoff := time.UnixMicro(old + 10).UTC()
	if _, err := s.DB().Exec(`INSERT INTO tasks(task_id,version,state,text,source_kind,created_at,updated_at,done_at) VALUES(?,1,'done','done','local',?,?,?)`, taskID, old, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`INSERT INTO task_state_events(event_id,task_id,prior_state,new_state,actor,request_id,committed_at) VALUES('evt_purge_finalized',?,'open','done','local','state-purge-finalized',?)`, taskID, old); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`INSERT INTO pi_deliveries(delivery_id,task_id,task_version,pi_session_id,delivery_key,state,completed_at_us,created_at_us,updated_at_us) VALUES(?,?,1,'session','sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa','persisted',?,?,?)`, deliveryID, taskID, old, old, old); err != nil {
		t.Fatal(err)
	}

	got, err := s.ConfirmMetadataPurge(ctx, "purge-finalized", cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if got.Removed.Tasks != 1 || got.Removed.TaskStateEvents != 1 || got.Removed.PiDeliveries != 1 {
		t.Fatalf("removed = %#v", got.Removed)
	}
	replay, err := s.ConfirmMetadataPurge(ctx, "purge-finalized", cutoff)
	if err != nil || replay != got {
		t.Fatalf("replay = %#v, %v; want %#v", replay, err, got)
	}
	if _, err := s.ConfirmMetadataPurge(ctx, "purge-finalized", cutoff.Add(time.Microsecond)); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("changed cutoff = %v", err)
	}
	assertPurgeIntegrity(t, s)
}

func TestConfirmMetadataPurgeRetainsNonFinalizedOrUnknownCompletionDelivery(t *testing.T) {
	for _, state := range []string{"pending", "inserting", "uncertain", "rejected"} {
		t.Run(state, func(t *testing.T) {
			s := testStore(t)
			const taskID = "tsk_purge_live"
			old := int64(1_700_000_000_000_000)
			if _, err := s.DB().Exec(`INSERT INTO tasks(task_id,version,state,text,source_kind,created_at,updated_at,done_at) VALUES(?,1,'done','done','local',?,?,?)`, taskID, old, old, old); err != nil {
				t.Fatal(err)
			}
			id := "dlv_abcdefghijklmnopqrst" + state[:2]
			// Non-persisted states may have a nullable completion timestamp; neither
			// form is purgeable. This freezes the no-updated_at fallback decision.
			if _, err := s.DB().Exec(`INSERT INTO pi_deliveries(delivery_id,task_id,task_version,pi_session_id,delivery_key,state,created_at_us,updated_at_us) VALUES(?,?,1,'session','sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb',?,?,?)`, id, taskID, state, old, old); err != nil {
				t.Fatal(err)
			}
			got, err := s.ConfirmMetadataPurge(context.Background(), "purge-live-"+state, time.UnixMicro(old+10).UTC())
			if err != nil {
				t.Fatal(err)
			}
			if got.Removed.Tasks != 0 || got.Skipped.Tasks != 1 {
				t.Fatalf("result = %#v", got)
			}
			var count int
			if err := s.DB().QueryRow("SELECT count(*) FROM tasks WHERE task_id=?", taskID).Scan(&count); err != nil || count != 1 {
				t.Fatalf("task count = %d, %v", count, err)
			}
			assertPurgeIntegrity(t, s)
		})
	}
}

func TestConfirmMetadataPurgeCapsAllRootsInOneInvocation(t *testing.T) {
	s := testStore(t)
	old := int64(1_700_000_000_000_000)
	for i := 0; i < PurgeBatchSize+1; i++ {
		id := fmt.Sprintf("source-bounded-%03d", i)
		if _, err := s.DB().Exec(`INSERT INTO slack_source_events(source_event_id,event_kind,received_at_us,committed_at_us,admission_result,task_version,changed,last_seen_at_us) VALUES(?,'message',?,?, 'duplicate',0,0,?)`, id, old, old, old); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.ConfirmMetadataPurge(context.Background(), "purge-root-budget", time.UnixMicro(old+1).UTC())
	if err != nil {
		t.Fatal(err)
	}
	if got.Removed.SlackSourceEvents != PurgeBatchSize {
		t.Fatalf("source removals = %d", got.Removed.SlackSourceEvents)
	}
	var remaining int
	if err := s.DB().QueryRow("SELECT count(*) FROM slack_source_events").Scan(&remaining); err != nil || remaining != 1 {
		t.Fatalf("remaining = %d, %v", remaining, err)
	}
	assertPurgeIntegrity(t, s)
}

func TestConfirmMetadataPurgeSharesBudgetAcrossRootTables(t *testing.T) {
	s := testStore(t)
	old := int64(1_700_000_000_000_000)
	if _, err := s.DB().Exec(`INSERT INTO slack_source_events(source_event_id,event_kind,received_at_us,committed_at_us,admission_result,task_version,changed,last_seen_at_us) VALUES('source-shared-budget','message',?,?,'duplicate',0,0,?)`, old, old, old); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < PurgeBatchSize; i++ {
		id := fmt.Sprintf("request-shared-budget-%03d", i)
		hash := fmt.Sprintf("%064x", i)
		if _, err := s.DB().Exec(`INSERT INTO mutation_requests(request_id,operation,input_hash,outcome,result_type,result_json,created_at,completed_at,expiry_eligible_at) VALUES(?, 'x', ?, 'success','x','{}',?,?,?)`, id, hash, old, old, old); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.ConfirmMetadataPurge(context.Background(), "purge-shared-budget", time.UnixMicro(old+1).UTC())
	if err != nil {
		t.Fatal(err)
	}
	if got.Removed.SlackSourceEvents != 1 || got.Removed.MutationRequests != PurgeBatchSize-1 {
		t.Fatalf("removed = %#v", got.Removed)
	}
	var requests int
	if err := s.DB().QueryRow("SELECT count(*) FROM mutation_requests").Scan(&requests); err != nil || requests != 2 {
		t.Fatalf("requests = %d, %v", requests, err)
	} // one old root plus this purge request
	assertPurgeIntegrity(t, s)
}

func TestConfirmMetadataPurgeSkipsOversizedDependentGraph(t *testing.T) {
	s := testStore(t)
	old := int64(1_700_000_000_000_000)
	if _, err := s.DB().Exec(`INSERT INTO tasks(task_id,version,state,text,source_kind,created_at,updated_at,done_at) VALUES('tsk_purge_oversized',1,'done','done','local',?,?,?)`, old, old, old); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < PurgeBatchSize; i++ {
		id, requestID := fmt.Sprintf("event-oversized-%03d", i), fmt.Sprintf("request-oversized-%03d", i)
		if _, err := s.DB().Exec(`INSERT INTO task_state_events(event_id,task_id,prior_state,new_state,actor,request_id,committed_at) VALUES(?, 'tsk_purge_oversized','open','done','local',?,?)`, id, requestID, old); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.ConfirmMetadataPurge(context.Background(), "purge-oversized", time.UnixMicro(old+1).UTC())
	if err != nil {
		t.Fatal(err)
	}
	if got.Removed.Tasks != 0 || got.Skipped.Tasks != 1 {
		t.Fatalf("result = %#v", got)
	}
	var tasks, events int
	if err := s.DB().QueryRow("SELECT count(*) FROM tasks WHERE task_id='tsk_purge_oversized'").Scan(&tasks); err != nil || tasks != 1 {
		t.Fatalf("tasks = %d, %v", tasks, err)
	}
	if err := s.DB().QueryRow("SELECT count(*) FROM task_state_events WHERE task_id='tsk_purge_oversized'").Scan(&events); err != nil || events != PurgeBatchSize {
		t.Fatalf("events = %d, %v", events, err)
	}
	assertPurgeIntegrity(t, s)
}

func TestConfirmMetadataPurgeStandaloneEffectPreservesRetainedPredecessorGraph(t *testing.T) {
	s := testStore(t)
	old := int64(1_700_000_000_000_000)
	if _, err := s.DB().Exec(`INSERT INTO slack_effects(effect_id,idempotency_key,kind,team_id,channel_id,payload_hash,state,attempt_count,prepared_at_us,submitted_at_us,completed_at_us,created_at_us,updated_at_us) VALUES('eff_abcdefghijklmnopqrstuv','purge-root','send','T1','C1','aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa','confirmed',1,?,?,?,?,?)`, old, old, old, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`INSERT INTO slack_effects(effect_id,idempotency_key,predecessor_effect_id,kind,team_id,channel_id,payload_hash,state,attempt_count,prepared_at_us,created_at_us,updated_at_us) VALUES('eff_bcdefghijklmnopqrstuvw','purge-child','eff_abcdefghijklmnopqrstuv','send','T1','C1','bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb','prepared',0,?,?,?)`, old, old, old); err != nil {
		t.Fatal(err)
	}
	got, err := s.ConfirmMetadataPurge(context.Background(), "purge-effect-predecessor", time.UnixMicro(old+1).UTC())
	if err != nil {
		t.Fatal(err)
	}
	if got.Removed.SlackEffects != 0 || got.Skipped.SlackEffects != 1 {
		t.Fatalf("result = %#v", got)
	}
	var count int
	if err := s.DB().QueryRow("SELECT count(*) FROM slack_effects").Scan(&count); err != nil || count != 2 {
		t.Fatalf("effects = %d, %v", count, err)
	}
	assertPurgeIntegrity(t, s)
}

func TestConfirmMetadataPurgeConcurrentSameRequestSerializesAndReplays(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ledger.sqlite3")
	s1, err := Initialize(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s1.Close()
	s2, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	old := int64(1_700_000_000_000_000)
	if _, err := s1.DB().Exec(`INSERT INTO tasks(task_id,version,state,text,source_kind,created_at,updated_at,done_at) VALUES('tsk_purge_concurrent',1,'done','done','local',?,?,?)`, old, old, old); err != nil {
		t.Fatal(err)
	}
	cutoff := time.UnixMicro(old + 1).UTC()
	start := make(chan struct{})
	results := make(chan PurgeResult, 2)
	errs := make(chan error, 2)
	for _, s := range []*Store{s1, s2} {
		go func(s *Store) {
			<-start
			r, err := s.ConfirmMetadataPurge(ctx, "purge-concurrent", cutoff)
			results <- r
			errs <- err
		}(s)
	}
	close(start)
	first, second := <-results, <-results
	if err := <-errs; err != nil {
		t.Fatal(err)
	}
	if err := <-errs; err != nil {
		t.Fatal(err)
	}
	if first != second || first.Removed.Tasks != 1 {
		t.Fatalf("concurrent results = %#v / %#v", first, second)
	}
	assertPurgeIntegrity(t, s1)
}

func TestConfirmMetadataPurgeSourceAndRequestStrictCutoff(t *testing.T) {
	s := testStore(t)
	old := int64(1_700_000_000_000_000)
	cutoff := time.UnixMicro(old + 10).UTC()
	for _, row := range []struct {
		id, result string
		committed  int64
	}{
		{"source-old", "duplicate", old},
		{"source-equal", "ignored_bot", old + 10},
		{"source-equal-malformed", "malformed", old + 10},
	} {
		if _, err := s.DB().Exec(`INSERT INTO slack_source_events(source_event_id,event_kind,received_at_us,committed_at_us,admission_result,task_version,changed,last_seen_at_us) VALUES(?,'message',?,?,?,0,0,?)`, row.id, old, row.committed, row.result, row.committed); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.DB().Exec(`INSERT INTO mutation_requests(request_id,operation,input_hash,outcome,result_type,result_json,created_at,completed_at,expiry_eligible_at) VALUES('old-request','x','aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa','success','x','{}',?,?,?)`, old, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`INSERT INTO mutation_requests(request_id,operation,input_hash,outcome,result_type,result_json,created_at,completed_at,expiry_eligible_at) VALUES('equal-request','x','bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb','success','x','{}',?,?,?)`, old, old, old+10); err != nil {
		t.Fatal(err)
	}
	got, err := s.ConfirmMetadataPurge(context.Background(), "purge-source-request", cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if got.Removed.SlackSourceEvents != 1 || got.Removed.MutationRequests != 1 {
		t.Fatalf("removed = %#v", got.Removed)
	}
	for _, id := range []string{"source-equal", "source-equal-malformed"} {
		var n int
		if err := s.DB().QueryRow("SELECT count(*) FROM slack_source_events WHERE source_event_id=?", id).Scan(&n); err != nil || n != 1 {
			t.Fatalf("source %s = %d, %v", id, n, err)
		}
	}
	var n int
	if err := s.DB().QueryRow("SELECT count(*) FROM mutation_requests WHERE request_id='purge-source-request'").Scan(&n); err != nil || n != 1 {
		t.Fatalf("live request = %d, %v", n, err)
	}
	assertPurgeIntegrity(t, s)
}

func assertPurgeIntegrity(t *testing.T, s *Store) {
	t.Helper()
	var foreign, integrity string
	if err := s.DB().QueryRow("PRAGMA foreign_key_check").Scan(&foreign); err != nil && !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("foreign_key_check: %v", err)
	}
	if foreign != "" {
		t.Fatalf("foreign_key_check = %q", foreign)
	}
	if err := s.DB().QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil || integrity != "ok" {
		t.Fatalf("integrity_check = %q, %v", integrity, err)
	}
}

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
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

func TestConfirmMetadataPurgeSkippedRootsConsumeCandidateBudget(t *testing.T) {
	s := testStore(t)
	old := int64(1_700_000_000_000_000)
	for i := 0; i < PurgeRootLimit; i++ {
		taskID := fmt.Sprintf("tsk_root_budget_skip_%03d", i)
		insertPurgeTask(t, s, taskID, old)
		if _, err := s.DB().Exec(`INSERT INTO pi_deliveries(delivery_id,task_id,task_version,pi_session_id,delivery_key,state,created_at_us,updated_at_us) VALUES(?,?,1,'session',?,'pending',?,?)`, fmt.Sprintf("dlv_%022d", i), taskID, fmt.Sprintf("sha256:%064x", i), old, old); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.DB().Exec(`INSERT INTO slack_source_events(source_event_id,event_kind,received_at_us,committed_at_us,admission_result,task_version,changed,last_seen_at_us) VALUES('source-after-skipped-roots','message',?,?, 'duplicate',0,0,?)`, old, old, old); err != nil {
		t.Fatal(err)
	}

	got, err := s.ConfirmMetadataPurge(context.Background(), "purge-skipped-root-budget", time.UnixMicro(old+1).UTC())
	if err != nil {
		t.Fatal(err)
	}
	if got.Skipped.Tasks != PurgeRootLimit || got.Removed.SlackSourceEvents != 0 {
		t.Fatalf("result = %#v", got)
	}
	var sources int
	if err := s.DB().QueryRow("SELECT count(*) FROM slack_source_events WHERE source_event_id='source-after-skipped-roots'").Scan(&sources); err != nil || sources != 1 {
		t.Fatalf("later source roots = %d, %v", sources, err)
	}
	assertPurgeIntegrity(t, s)
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

func TestConfirmMetadataPurgeDeletesTaskUploadFileAndChargesItsGraph(t *testing.T) {
	s := testStore(t)
	old := int64(1_700_000_000_000_000)
	insertPurgeTask(t, s, "tsk_purge_upload", old)
	insertPurgeUpload(t, s, "eff_0000000000000000000001", "fil_0000000000000000000001", "tsk_purge_upload", old)

	got, err := s.ConfirmMetadataPurge(context.Background(), "purge-upload", time.UnixMicro(old+1).UTC())
	if err != nil {
		t.Fatal(err)
	}
	if got.Removed.Tasks != 1 || got.Removed.SlackEffects != 1 || got.Removed.Files != 1 {
		t.Fatalf("removed = %#v", got.Removed)
	}
	assertPurgeIntegrity(t, s)
}

func TestConfirmMetadataPurgeRetainsUploadGraphWithTemporaryTransfer(t *testing.T) {
	s := testStore(t)
	old := int64(1_700_000_000_000_000)
	insertPurgeTask(t, s, "tsk_purge_upload_temp", old)
	insertPurgeUpload(t, s, "eff_0000000000000000000002", "fil_0000000000000000000002", "tsk_purge_upload_temp", old)
	if _, err := s.DB().Exec(`INSERT INTO file_temporary_transfers(transfer_id,file_id,generated_name,size_bytes,sha256,expires_at_us,created_at_us) VALUES('xfr_0000000000000000000002','fil_0000000000000000000002','mosscap-xfr-00000000000000000000000000000002',1,?,?,?)`, fmt.Sprintf("%064x", 2), old+100, old); err != nil {
		t.Fatal(err)
	}

	got, err := s.ConfirmMetadataPurge(context.Background(), "purge-upload-temp", time.UnixMicro(old+1).UTC())
	if err != nil {
		t.Fatal(err)
	}
	if got.Removed.Tasks != 0 || got.Skipped.Tasks != 1 || got.Removed.FileTemporaryTransfers != 0 {
		t.Fatalf("result = %#v", got)
	}
	var tasks, effects, files, transfers int
	if err := s.DB().QueryRow("SELECT count(*) FROM tasks").Scan(&tasks); err != nil || tasks != 1 {
		t.Fatalf("tasks = %d, %v", tasks, err)
	}
	if err := s.DB().QueryRow("SELECT count(*) FROM slack_effects").Scan(&effects); err != nil || effects != 1 {
		t.Fatalf("effects = %d, %v", effects, err)
	}
	if err := s.DB().QueryRow("SELECT count(*) FROM files").Scan(&files); err != nil || files != 1 {
		t.Fatalf("files = %d, %v", files, err)
	}
	if err := s.DB().QueryRow("SELECT count(*) FROM file_temporary_transfers").Scan(&transfers); err != nil || transfers != 1 {
		t.Fatalf("transfers = %d, %v", transfers, err)
	}
	assertPurgeIntegrity(t, s)
}

func TestConfirmMetadataPurgeSkipsOversizedTaskUploadGraph(t *testing.T) {
	s := testStore(t)
	old := int64(1_700_000_000_000_000)
	insertPurgeTask(t, s, "tsk_purge_upload_large", old)
	for i := 0; i < 50; i++ { // task + 50 effects + 50 files exceeds the 100-row budget.
		insertPurgeUpload(t, s, fmt.Sprintf("eff_%022d", i), fmt.Sprintf("fil_%022d", i), "tsk_purge_upload_large", old)
	}

	got, err := s.ConfirmMetadataPurge(context.Background(), "purge-upload-large", time.UnixMicro(old+1).UTC())
	if err != nil {
		t.Fatal(err)
	}
	if got.Removed.Tasks != 0 || got.Skipped.Tasks != 1 {
		t.Fatalf("result = %#v", got)
	}
	var tasks, effects, files int
	if err := s.DB().QueryRow("SELECT count(*) FROM tasks WHERE task_id='tsk_purge_upload_large'").Scan(&tasks); err != nil || tasks != 1 {
		t.Fatalf("tasks = %d, %v", tasks, err)
	}
	if err := s.DB().QueryRow("SELECT count(*) FROM slack_effects WHERE related_task_id='tsk_purge_upload_large'").Scan(&effects); err != nil || effects != 50 {
		t.Fatalf("effects = %d, %v", effects, err)
	}
	if err := s.DB().QueryRow("SELECT count(*) FROM files").Scan(&files); err != nil || files != 50 {
		t.Fatalf("files = %d, %v", files, err)
	}
	assertPurgeIntegrity(t, s)
}

func TestConfirmMetadataPurgeRetainsTaskWithNullOrOtherTaskEffectSuccessor(t *testing.T) {
	s := testStore(t)
	old := int64(1_700_000_000_000_000)
	insertPurgeTask(t, s, "tsk_purge_successor_parent", old)
	insertPurgeTask(t, s, "tsk_purge_successor_other", old)
	parentID := "eff_0000000000000000000010"
	insertPurgeEffect(t, s, parentID, "tsk_purge_successor_parent", "confirmed", "", old)
	// Prepared successors must keep their predecessors' task graphs intact.
	// This covers both SQL NULL and a different task owner.
	insertPurgeEffect(t, s, "eff_0000000000000000000011", "", "prepared", parentID, old)
	insertPurgeEffect(t, s, "eff_0000000000000000000012", "tsk_purge_successor_other", "prepared", parentID, old)

	got, err := s.ConfirmMetadataPurge(context.Background(), "purge-external-successor", time.UnixMicro(old+1).UTC())
	if err != nil {
		t.Fatal(err)
	}
	if got.Removed.Tasks != 0 || got.Skipped.Tasks != 2 {
		t.Fatalf("result = %#v", got)
	}
	var tasks, effects int
	if err := s.DB().QueryRow("SELECT count(*) FROM tasks").Scan(&tasks); err != nil || tasks != 2 {
		t.Fatalf("tasks = %d, %v", tasks, err)
	}
	if err := s.DB().QueryRow("SELECT count(*) FROM slack_effects").Scan(&effects); err != nil || effects != 3 {
		t.Fatalf("effects = %d, %v", effects, err)
	}
	assertPurgeIntegrity(t, s)
}

func insertPurgeTask(t *testing.T, s *Store, id string, at int64) {
	t.Helper()
	if _, err := s.DB().Exec(`INSERT INTO tasks(task_id,version,state,text,source_kind,created_at,updated_at,done_at) VALUES(?,1,'done','done','local',?,?,?)`, id, at, at, at); err != nil {
		t.Fatal(err)
	}
}

func insertPurgeEffect(t *testing.T, s *Store, id, taskID, state, predecessor string, at int64) {
	t.Helper()
	var relatedTask, predecessorID any
	if taskID != "" {
		relatedTask = taskID
	}
	if predecessor != "" {
		predecessorID = predecessor
	}
	if state == "confirmed" {
		if _, err := s.DB().Exec(`INSERT INTO slack_effects(effect_id,idempotency_key,predecessor_effect_id,related_task_id,kind,team_id,channel_id,payload_hash,state,attempt_count,prepared_at_us,submitted_at_us,completed_at_us,created_at_us,updated_at_us) VALUES(?,?,?,?,'file_upload','T1','C1',?,'confirmed',1,?,?,?,?,?)`, id, "purge-"+id, predecessorID, relatedTask, fmt.Sprintf("%064x", len(id)), at, at, at, at, at); err != nil {
			t.Fatal(err)
		}
		return
	}
	if _, err := s.DB().Exec(`INSERT INTO slack_effects(effect_id,idempotency_key,predecessor_effect_id,related_task_id,kind,team_id,channel_id,payload_hash,state,attempt_count,prepared_at_us,created_at_us,updated_at_us) VALUES(?,?,?,?,'send','T1','C1',?,'prepared',0,?,?,?)`, id, "purge-"+id, predecessorID, relatedTask, fmt.Sprintf("%064x", len(id)), at, at, at); err != nil {
		t.Fatal(err)
	}
}

func insertPurgeUpload(t *testing.T, s *Store, effectID, fileID, taskID string, at int64) {
	t.Helper()
	insertPurgeEffect(t, s, effectID, taskID, "confirmed", "", at)
	if _, err := s.DB().Exec(`INSERT INTO files(file_id,direction,original_name,declared_size_bytes,validated_size_bytes,sha256,related_effect_id,effect_ordinal,source_attached,source_path,transfer_state,created_at_us,updated_at_us) VALUES(?,'to_slack','upload.txt',1,1,?, ?,0,0,'/tmp/upload.txt','confirmed',?,?)`, fileID, fmt.Sprintf("%064x", len(fileID)), effectID, at, at); err != nil {
		t.Fatal(err)
	}
}

func TestConfirmMetadataPurgeRetriableFailureRollsBackAndReportsOnlyCommittedProgress(t *testing.T) {
	for _, cause := range []error{
		context.Canceled,
		errors.New("database is busy"),
		errors.New("database or disk is full"),
		errors.New("interrupted"),
	} {
		t.Run(cause.Error(), func(t *testing.T) {
			s := testStore(t)
			old := int64(1_700_000_000_000_000)
			insertPurgeTask(t, s, "tsk_purge_rollback", old)
			requestID := "purge-rollback-" + strings.ReplaceAll(cause.Error(), " ", "-")
			s.purgeDeleteHook = func() error { return cause }

			_, err := s.ConfirmMetadataPurge(context.Background(), requestID, time.UnixMicro(old+1).UTC())
			var purgeErr *PurgeError
			if !errors.As(err, &purgeErr) || !errors.Is(err, cause) {
				t.Fatalf("error = %v, want retriable %v", err, cause)
			}
			if purgeErr.Progress.Removed.total() != 0 {
				t.Fatalf("speculative progress leaked: %#v", purgeErr.Progress.Removed)
			}
			var tasks, requests int
			if err := s.DB().QueryRow("SELECT count(*) FROM tasks WHERE task_id='tsk_purge_rollback'").Scan(&tasks); err != nil || tasks != 1 {
				t.Fatalf("rollback tasks = %d, %v", tasks, err)
			}
			if err := s.DB().QueryRow("SELECT count(*) FROM mutation_requests WHERE request_id=?", requestID).Scan(&requests); err != nil || requests != 0 {
				t.Fatalf("rollback request = %d, %v", requests, err)
			}
			assertPurgeIntegrity(t, s)

			s.purgeDeleteHook = nil
			result, err := s.ConfirmMetadataPurge(context.Background(), requestID, time.UnixMicro(old+1).UTC())
			if err != nil || result.Removed.Tasks != 1 {
				t.Fatalf("safe replay = %#v, %v", result, err)
			}
			assertPurgeIntegrity(t, s)
		})
	}
}

func TestConfirmMetadataPurgeClassifiesCancellationAsRetriable(t *testing.T) {
	s := testStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := s.ConfirmMetadataPurge(ctx, "purge-cancelled", time.Now().UTC())
	var purgeErr *PurgeError
	if !errors.As(err, &purgeErr) || !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want retriable purge cancellation", err)
	}
}

func TestPreviewMetadataPurgeUsesConfirmGraphClassification(t *testing.T) {
	s := testStore(t)
	old := int64(1_700_000_000_000_000)
	insertPurgeTask(t, s, "tsk_preview_graph", old)
	if _, err := s.DB().Exec(`INSERT INTO pi_deliveries(delivery_id,task_id,task_version,pi_session_id,delivery_key,state,created_at_us,updated_at_us) VALUES('dlv_abcdefghijklmnopqrstuv', 'tsk_preview_graph',1,'session','sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa','pending',?,?)`, old, old); err != nil {
		t.Fatal(err)
	}
	cutoff := time.UnixMicro(old + 1).UTC()
	preview, err := s.PreviewMetadataPurge(context.Background(), cutoff)
	if err != nil {
		t.Fatal(err)
	}
	confirm, err := s.ConfirmMetadataPurge(context.Background(), "purge-preview-graph", cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if preview.Eligible != confirm.Eligible || preview.SkippedLive != confirm.SkippedLive || preview.SkippedReferenced != confirm.SkippedReferenced || preview.SkippedLive.Tasks != 1 {
		t.Fatalf("preview %#v differs from confirm %#v", preview, confirm)
	}
}

func TestPreviewMetadataPurgeIsReadOnly(t *testing.T) {
	s := testStore(t)
	old := int64(1_700_000_000_000_000)
	if _, err := s.DB().Exec(`INSERT INTO tasks(task_id,version,state,text,source_kind,created_at,updated_at,done_at) VALUES('tsk_preview',1,'done','done','local',?,?,?)`, old, old, old); err != nil {
		t.Fatal(err)
	}
	before, err := s.DB().Query("SELECT request_id FROM mutation_requests ORDER BY request_id")
	if err != nil {
		t.Fatal(err)
	}
	defer before.Close()
	result, err := s.PreviewMetadataPurge(context.Background(), time.UnixMicro(old+1).UTC())
	if err != nil {
		t.Fatal(err)
	}
	if result.Eligible.Tasks != 1 || result.Removed.Tasks != 0 {
		t.Fatalf("preview = %#v", result)
	}
	var tasks, requests int
	if err := s.DB().QueryRow("SELECT count(*) FROM tasks").Scan(&tasks); err != nil {
		t.Fatal(err)
	}
	if err := s.DB().QueryRow("SELECT count(*) FROM mutation_requests").Scan(&requests); err != nil {
		t.Fatal(err)
	}
	if tasks != 1 || requests != 0 {
		t.Fatalf("preview mutated database: tasks=%d requests=%d", tasks, requests)
	}
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

package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/stayradiated/mosscap/internal/slackapi"
)

func schemaV2Database(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(string(migration0001)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(string(migration0002)); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		"PRAGMA application_id=1296257072",
		"PRAGMA user_version=2",
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec("INSERT INTO schema_migrations(version,checksum,applied_at) VALUES(1,?,1),(2,?,2)", MigrationChecksum(1), MigrationChecksum(2)); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestOpenMigrationFailureRollsBackV2Upgrade(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ledger.sqlite3")
	db := schemaV2Database(t, path)
	if _, err := db.Exec("INSERT INTO tasks(task_id,version,state,text,source_kind,created_at,updated_at) VALUES('tsk_before_failed_upgrade',1,'open','preserved','local',1,1)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO trusted_users(slack_user_id) VALUES('U12345678')"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	breakMigration3(t)
	if opened, err := Open(ctx, path, Options{}); err == nil {
		_ = opened.Close()
		t.Fatal("upgrade with failing v3 migration succeeded")
	}
	assertFailedUpgradeRolledBack(t, path, 2, true)
}

func TestOpenUpgradesExactSchemaV2ToV5AndPreservesLedger(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ledger.sqlite3")
	db := schemaV2Database(t, path)
	if _, err := db.Exec("INSERT INTO tasks(task_id,version,state,text,source_kind,created_at,updated_at) VALUES('tsk_v2',2,'working','preserved v2 text','local',1,2)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO trusted_users(slack_user_id) VALUES('U12345678')"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO mutation_requests(request_id,operation,input_hash,outcome,result_type,result_json,created_at,completed_at,expiry_eligible_at) VALUES('v2-request','task.update',?,'success','task_mutation','{\"preserved\":true}',1,2,3)", strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatalf("Open v2: %v", err)
	}
	defer s.Close()

	var version, migrations int
	if err := s.DB().QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := s.DB().QueryRow("SELECT count(*) FROM schema_migrations").Scan(&migrations); err != nil {
		t.Fatal(err)
	}
	if version != 6 || migrations != 6 {
		t.Fatalf("migration state version=%d rows=%d", version, migrations)
	}
	got, err := s.Get(ctx, "tsk_v2")
	if err != nil || got.Text != "preserved v2 text" || got.Version != 2 || got.SourceKind != "local" {
		t.Fatalf("preserved task=%+v err=%v", got, err)
	}
	users, err := s.ListTrustedUsers(ctx)
	if err != nil || !reflect.DeepEqual(users, []string{"U12345678"}) {
		t.Fatalf("preserved trust=%v err=%v", users, err)
	}
	var resultJSON string
	if err := s.DB().QueryRow("SELECT result_json FROM mutation_requests WHERE request_id='v2-request'").Scan(&resultJSON); err != nil || resultJSON != `{"preserved":true}` {
		t.Fatalf("preserved mutation=%q err=%v", resultJSON, err)
	}
}

func TestOpenUpgradesV4FileMetadataWithoutChangingStableIdentity(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ledger.sqlite3")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, migration := range [][]byte{migration0001, migration0002, migration0003, migration0004} {
		if _, err := db.Exec(string(migration)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec("PRAGMA application_id=1296257072; PRAGMA user_version=4"); err != nil {
		t.Fatal(err)
	}
	for version := 1; version <= 4; version++ {
		if _, err := db.Exec("INSERT INTO schema_migrations(version,checksum,applied_at) VALUES(?,?,?)", version, MigrationChecksum(version), version); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec("INSERT INTO tasks(task_id,version,state,text,source_kind,created_at,updated_at) VALUES('tsk_v4_file',1,'open','preserved','slack',1,1)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO files(file_id,direction,slack_file_id,original_name,declared_mime,size_bytes,related_task_id,transfer_state,created_at_us,updated_at_us) VALUES('fil_abcdefghijklmnopqrstuv','from_slack','F12345678','preserved.txt','text/plain',7,'tsk_v4_file','metadata_only',1,1)"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	file, err := s.GetSlackFile(ctx, "fil_abcdefghijklmnopqrstuv")
	if err != nil || file.FileID != "fil_abcdefghijklmnopqrstuv" || file.SlackFileID != "F12345678" || file.DeclaredSizeBytes == nil || *file.DeclaredSizeBytes != 7 || !file.SourceAttached || file.TransferState != "metadata_only" {
		t.Fatalf("preserved file=%+v err=%v", file, err)
	}
	if _, err := s.DB().Exec("SELECT temporary_path FROM files"); err == nil {
		t.Fatal("legacy temporary path column survived v5 rebuild")
	}
}

func TestSlackMigrationChecksumsAndColumns(t *testing.T) {
	if MigrationChecksum(4) == "" || len(MigrationChecksum(4)) != 64 {
		t.Fatalf("v4 checksum=%q", MigrationChecksum(4))
	}
	if MigrationChecksum(4) == MigrationChecksum(1) || MigrationChecksum(4) == MigrationChecksum(2) || MigrationChecksum(4) == MigrationChecksum(3) {
		t.Fatal("v4 checksum is not migration-specific")
	}
	if MigrationChecksum(5) == "" || len(MigrationChecksum(5)) != 64 || MigrationChecksum(5) == MigrationChecksum(4) {
		t.Fatalf("v5 checksum=%q", MigrationChecksum(5))
	}
	if MigrationChecksum(6) == "" || len(MigrationChecksum(6)) != 64 || MigrationChecksum(6) == MigrationChecksum(5) {
		t.Fatalf("v6 checksum=%q", MigrationChecksum(6))
	}
	if MigrationChecksum(7) != "" {
		t.Fatalf("unknown checksum=%q", MigrationChecksum(7))
	}

	s := testStore(t)
	for table, want := range map[string][]string{
		"slack_task_sources":       {"task_id", "initial_source_event_id", "team_id", "channel_id", "author_user_id", "message_ts", "thread_ts", "permalink", "latest_event_at_us", "source_deleted_at_us", "created_at_us", "updated_at_us"},
		"slack_source_events":      {"source_event_id", "envelope_id", "event_kind", "team_id", "channel_id", "message_ts", "thread_ts", "user_id", "slack_event_at_us", "received_at_us", "committed_at_us", "admission_result", "task_id", "diagnostic_code", "task_version", "changed", "redelivery_count", "last_seen_at_us"},
		"files":                    {"file_id", "direction", "slack_file_id", "permalink", "original_name", "declared_mime", "declared_size_bytes", "validated_size_bytes", "sha256", "related_task_id", "related_effect_id", "effect_ordinal", "source_attached", "source_detached_at_us", "source_path", "source_device", "source_inode", "source_mtime_ns", "transfer_state", "diagnostic_code", "created_at_us", "updated_at_us"},
		"file_temporary_transfers": {"transfer_id", "file_id", "generated_name", "size_bytes", "sha256", "expires_at_us", "created_at_us"},
		"slack_effects":            {"effect_id", "idempotency_key", "predecessor_effect_id", "related_task_id", "kind", "team_id", "channel_id", "target_message_ts", "thread_ts", "text", "emoji", "operator_reason", "payload_hash", "state", "attempt_count", "prepared_at_us", "submitted_at_us", "completed_at_us", "response_message_ts", "response_file_id", "diagnostic_code", "created_at_us", "updated_at_us"},
	} {
		rows, err := s.DB().Query("PRAGMA table_info(" + table + ")")
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for rows.Next() {
			var cid, notNull, pk int
			var name, typ string
			var defaultValue any
			if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
				t.Fatal(err)
			}
			got = append(got, name)
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s columns=%v want=%v", table, got, want)
		}
	}
}

func TestSlackSchemaAcceptsCompleteRecordsAndEnforcesIdentity(t *testing.T) {
	s := testStore(t)
	db := s.DB()
	mustExec := func(statement string, args ...any) {
		t.Helper()
		if _, err := db.Exec(statement, args...); err != nil {
			t.Fatalf("exec %q: %v", statement, err)
		}
	}
	wantConstraint := func(name, statement string, args ...any) {
		t.Helper()
		if _, err := db.Exec(statement, args...); err == nil {
			t.Errorf("%s: constraint accepted invalid row", name)
		}
	}

	mustExec("INSERT INTO tasks(task_id,version,state,text,source_kind,created_at,updated_at) VALUES('tsk_slack',1,'open','current only','slack',1,1)")
	mustExec("INSERT INTO slack_source_events(source_event_id,envelope_id,event_kind,team_id,channel_id,message_ts,thread_ts,user_id,slack_event_at_us,received_at_us,committed_at_us,admission_result,task_id,task_version,changed,last_seen_at_us) VALUES('Ev-source','env-1','app_mention','T12345678','C12345678','1700000000.000001','1700000000.000001','U12345678',1,1,1,'admitted','tsk_slack',1,1,1)")
	mustExec("INSERT INTO slack_task_sources(task_id,initial_source_event_id,team_id,channel_id,author_user_id,message_ts,thread_ts,permalink,latest_event_at_us,created_at_us,updated_at_us) VALUES('tsk_slack','Ev-source','T12345678','C12345678','U12345678','1700000000.000001','1700000000.000001','https://example.invalid/archive',1,1,1)")
	mustExec("INSERT INTO files(file_id,direction,slack_file_id,permalink,original_name,declared_mime,declared_size_bytes,related_task_id,transfer_state,created_at_us,updated_at_us) VALUES('fil_abcdefghijklmnopqrstuv','from_slack','F12345678','https://example.invalid/file','untrusted.txt','text/plain',0,'tsk_slack','metadata_only',1,1)")
	mustExec("INSERT INTO slack_effects(effect_id,idempotency_key,related_task_id,kind,team_id,channel_id,text,payload_hash,state,attempt_count,prepared_at_us,created_at_us,updated_at_us) VALUES('eff_abcdefghijklmnopqrstuv','effect-key','tsk_slack','send','T12345678','C12345678','exact intent',?,'prepared',0,1,1,1)", strings.Repeat("a", 64))

	wantConstraint("source identity uniqueness", "INSERT INTO slack_task_sources(task_id,initial_source_event_id,team_id,channel_id,author_user_id,message_ts,thread_ts,latest_event_at_us,created_at_us,updated_at_us) VALUES('tsk_other','Ev-other','T12345678','C12345678','U12345678','1700000000.000001','1700000000.000001',1,1,1)")
	wantConstraint("file attachment uniqueness", "INSERT INTO files(file_id,direction,slack_file_id,original_name,related_task_id,transfer_state,created_at_us,updated_at_us) VALUES('fil_bcdefghijklmnopqrstuvw','from_slack','F12345678','duplicate','tsk_slack','metadata_only',1,1)")
	wantConstraint("effect ID prefix", "INSERT INTO slack_effects(effect_id,idempotency_key,kind,team_id,channel_id,text,payload_hash,state,attempt_count,prepared_at_us,created_at_us,updated_at_us) VALUES('bad_abcdefghijklmnopqrstuv','bad-id','send','T12345678','C12345678','text',?,'prepared',0,1,1,1)", strings.Repeat("a", 64))
	wantConstraint("file ID prefix", "INSERT INTO files(file_id,direction,slack_file_id,original_name,related_task_id,transfer_state,created_at_us,updated_at_us) VALUES('bad_abcdefghijklmnopqrstuv','from_slack','F99999999','name','tsk_slack','metadata_only',1,1)")
	wantConstraint("Slack timestamp", "UPDATE slack_task_sources SET message_ts='not-a-timestamp' WHERE task_id='tsk_slack'")
	wantConstraint("canonical admitted changed", "UPDATE slack_source_events SET changed=0 WHERE source_event_id='Ev-source'")
	wantConstraint("canonical task version", "UPDATE slack_source_events SET task_version=0 WHERE source_event_id='Ev-source'")
	wantConstraint("redelivery lower bound", "UPDATE slack_source_events SET redelivery_count=-1 WHERE source_event_id='Ev-source'")
	wantConstraint("last seen ordering", "UPDATE slack_source_events SET last_seen_at_us=0 WHERE source_event_id='Ev-source'")
}

func TestSlackInitialSourceEventCannotBelongToAnotherTask(t *testing.T) {
	s := testStore(t)
	db := s.DB()
	for _, statement := range []string{
		"INSERT INTO tasks(task_id,version,state,text,source_kind,created_at,updated_at) VALUES('tsk_source_a',1,'open','a','slack',1,1),('tsk_source_b',1,'open','b','slack',1,1)",
		"INSERT INTO slack_source_events(source_event_id,event_kind,team_id,channel_id,message_ts,user_id,received_at_us,committed_at_us,admission_result,task_id,task_version,changed,last_seen_at_us) VALUES('Ev-source-a','app_mention','T12345678','C12345678','1700000000.000001','U12345678',1,1,'admitted','tsk_source_a',1,1,1)",
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	statement := "INSERT INTO slack_task_sources(task_id,initial_source_event_id,team_id,channel_id,author_user_id,message_ts,thread_ts,latest_event_at_us,created_at_us,updated_at_us) VALUES('tsk_source_b','Ev-source-a','T12345678','C12345678','U12345678','1700000000.000001','1700000000.000001',1,1,1)"
	if _, err := db.Exec(statement); err == nil {
		t.Fatal("source accepted an initial event belonging to another task")
	}
}

func TestSlackSchemaByteBoundaries(t *testing.T) {
	s := testStore(t)
	db := s.DB()
	exact := func(prefix string, maximum int) string {
		t.Helper()
		value := prefix
		for len(value)+2 <= maximum {
			value += "é"
		}
		for len(value) < maximum {
			value += "x"
		}
		return value
	}
	mustExec := func(statement string, args ...any) {
		t.Helper()
		if _, err := db.Exec(statement, args...); err != nil {
			t.Fatalf("boundary statement failed: %v", err)
		}
	}
	wantConstraint := func(name, statement string, args ...any) {
		t.Helper()
		if _, err := db.Exec(statement, args...); err == nil {
			t.Fatalf("%s exceeded byte boundary", name)
		}
	}

	maxTeamID := "T" + strings.Repeat("A", slackapi.MaxIDBytes-1)
	maxChannelID := "C" + strings.Repeat("A", slackapi.MaxIDBytes-1)
	maxUserID := "U" + strings.Repeat("A", slackapi.MaxIDBytes-1)
	mustExec("INSERT INTO slack_source_events(source_event_id,envelope_id,event_kind,team_id,channel_id,user_id,received_at_us,committed_at_us,admission_result,task_version,changed,last_seen_at_us) VALUES(?,?,?,?,?,?,1,1,'malformed',0,0,1)", strings.Repeat("e", slackapi.MaxIDBytes), strings.Repeat("n", slackapi.MaxIDBytes), "message", maxTeamID, maxChannelID, maxUserID)
	wantConstraint("source event ID", "INSERT INTO slack_source_events(source_event_id,event_kind,received_at_us,committed_at_us,admission_result,task_version,changed,last_seen_at_us) VALUES(?,'message',1,1,'malformed',0,0,1)", strings.Repeat("e", slackapi.MaxIDBytes+1))
	wantConstraint("envelope ID", "INSERT INTO slack_source_events(source_event_id,envelope_id,event_kind,received_at_us,committed_at_us,admission_result,task_version,changed,last_seen_at_us) VALUES('Ev-long-envelope',?,'message',1,1,'malformed',0,0,1)", strings.Repeat("n", slackapi.MaxIDBytes+1))
	wantConstraint("team ID", "INSERT INTO slack_source_events(source_event_id,event_kind,team_id,received_at_us,committed_at_us,admission_result,task_version,changed,last_seen_at_us) VALUES('Ev-long-team','message',?,1,1,'malformed',0,0,1)", "T"+strings.Repeat("A", slackapi.MaxIDBytes))
	wantConstraint("channel ID", "INSERT INTO slack_source_events(source_event_id,event_kind,channel_id,received_at_us,committed_at_us,admission_result,task_version,changed,last_seen_at_us) VALUES('Ev-long-channel','message',?,1,1,'malformed',0,0,1)", "C"+strings.Repeat("A", slackapi.MaxIDBytes))
	wantConstraint("user ID", "INSERT INTO slack_source_events(source_event_id,event_kind,user_id,received_at_us,committed_at_us,admission_result,task_version,changed,last_seen_at_us) VALUES('Ev-long-user','message',?,1,1,'malformed',0,0,1)", "U"+strings.Repeat("A", slackapi.MaxIDBytes))

	mustExec("INSERT INTO tasks(task_id,version,state,text,source_kind,created_at,updated_at) VALUES('tsk_boundaries',1,'open','current','slack',1,1)")
	mustExec("INSERT INTO slack_source_events(source_event_id,event_kind,team_id,channel_id,message_ts,user_id,received_at_us,committed_at_us,admission_result,task_id,task_version,changed,last_seen_at_us) VALUES('Ev-boundaries','app_mention','T12345678','C12345678','1700000000.000001','U12345678',1,1,'admitted','tsk_boundaries',1,1,1)")
	permalink := exact("https://x/", slackapi.MaxPermalinkBytes)
	mustExec("INSERT INTO slack_task_sources(task_id,initial_source_event_id,team_id,channel_id,author_user_id,message_ts,thread_ts,permalink,latest_event_at_us,created_at_us,updated_at_us) VALUES('tsk_boundaries','Ev-boundaries','T12345678','C12345678','U12345678','1700000000.000001','1700000000.000001',?,1,1,1)", permalink)
	wantConstraint("source permalink", "UPDATE slack_task_sources SET permalink=? WHERE task_id='tsk_boundaries'", permalink+"x")

	name := exact("", slackapi.MaxFileNameBytes)
	mime := exact("", slackapi.MaxMIMETypeBytes)
	maxFileID := "F" + strings.Repeat("A", slackapi.MaxIDBytes-1)
	mustExec("INSERT INTO files(file_id,direction,slack_file_id,permalink,original_name,declared_mime,related_task_id,transfer_state,created_at_us,updated_at_us) VALUES('fil_abcdefghijklmnopqrstuv','from_slack',?,?,?,?,'tsk_boundaries','metadata_only',1,1)", maxFileID, permalink, name, mime)
	wantConstraint("file ID", "UPDATE files SET slack_file_id=? WHERE file_id='fil_abcdefghijklmnopqrstuv'", "F"+strings.Repeat("A", slackapi.MaxIDBytes))
	wantConstraint("file permalink", "UPDATE files SET permalink=? WHERE file_id='fil_abcdefghijklmnopqrstuv'", permalink+"x")
	wantConstraint("filename", "UPDATE files SET original_name=? WHERE file_id='fil_abcdefghijklmnopqrstuv'", name+"x")
	wantConstraint("MIME type", "UPDATE files SET declared_mime=? WHERE file_id='fil_abcdefghijklmnopqrstuv'", mime+"x")

	text := exact("", slackapi.MaxTextBytes)
	mustExec("INSERT INTO slack_effects(effect_id,idempotency_key,kind,team_id,channel_id,text,payload_hash,state,attempt_count,prepared_at_us,created_at_us,updated_at_us) VALUES('eff_abcdefghijklmnopqrstuv','boundary-effect','send','T12345678','C12345678',?,?,'prepared',0,1,1,1)", text, strings.Repeat("a", 64))
	wantConstraint("effect text", "UPDATE slack_effects SET text=? WHERE effect_id='eff_abcdefghijklmnopqrstuv'", text+"x")
}

func TestSlackSchemaEmojiAndLifecycleConstraints(t *testing.T) {
	s := testStore(t)
	db := s.DB()
	hash := strings.Repeat("a", 64)
	insert := func(id, key, emoji, state string, attempts, prepared int, submitted, completed any, created, updated int) error {
		_, err := db.Exec("INSERT INTO slack_effects(effect_id,idempotency_key,kind,team_id,channel_id,target_message_ts,emoji,payload_hash,state,attempt_count,prepared_at_us,submitted_at_us,completed_at_us,created_at_us,updated_at_us) VALUES(?,?, 'react','T12345678','C12345678','1700000000.000001',?,?,?,?,?,?,?,?,?)", id, key, emoji, hash, state, attempts, prepared, submitted, completed, created, updated)
		return err
	}
	if err := insert("eff_abcdefghijklmnopqrstuv", "tone", "thumbsup::skin-tone-3", "prepared", 0, 1, nil, nil, 1, 1); err != nil {
		t.Fatalf("canonical skin tone rejected: %v", err)
	}
	if err := insert("eff_bcdefghijklmnopqrstuvw", "max-emoji", strings.Repeat("a", slackapi.MaxEmojiBytes-13)+"::skin-tone-6", "prepared", 0, 1, nil, nil, 1, 1); err != nil {
		t.Fatalf("maximum-byte emoji rejected: %v", err)
	}
	for i, emoji := range []string{":thumbsup:", "thumbsup::skin-tone-1", "thumbsup::skin-tone-7", "thumbsup::skin-tone-3:extra", strings.Repeat("a", slackapi.MaxEmojiBytes+1)} {
		id := "eff_" + strings.Repeat(string(rune('d'+i)), 22)
		if err := insert(id, "bad-emoji-"+string(rune('0'+i)), emoji, "prepared", 0, 1, nil, nil, 1, 1); err == nil {
			t.Errorf("non-canonical emoji %q accepted", emoji)
		}
	}
	if err := insert("eff_"+strings.Repeat("i", 22), "submitted", "thumbsup", "submitted", 1, 2, 3, nil, 1, 3); err != nil {
		t.Fatalf("coherent submitted effect rejected: %v", err)
	}
	for _, test := range []struct {
		name                 string
		attempts, prepared   int
		submitted, completed any
		created, updated     int
		state                string
	}{
		{"second attempt", 2, 2, 3, nil, 1, 3, "submitted"},
		{"prepare before creation", 0, 1, nil, nil, 2, 2, "prepared"},
		{"prepare after update", 0, 3, nil, nil, 1, 2, "prepared"},
		{"submit after update", 1, 2, 4, nil, 1, 3, "submitted"},
		{"complete before submit", 1, 2, 3, 2, 1, 3, "failed"},
		{"complete after update", 1, 2, 3, 5, 1, 4, "ambiguous"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := insert("eff_"+strings.Repeat("z", 22), "bad-"+strings.ReplaceAll(test.name, " ", "-"), "thumbsup", test.state, test.attempts, test.prepared, test.submitted, test.completed, test.created, test.updated); err == nil {
				t.Fatal("incoherent lifecycle accepted")
			}
		})
	}
}

func TestSlackSchemaEnumHashAndLifecycleConstraints(t *testing.T) {
	s := testStore(t)
	db := s.DB()
	invalid := []struct {
		name string
		sql  string
	}{
		{"source result", "INSERT INTO slack_source_events(source_event_id,event_kind,received_at_us,committed_at_us,admission_result,task_version,changed,last_seen_at_us) VALUES('Ev-invalid-result','message',1,1,'other',0,0,1)"},
		{"source diagnostic", "INSERT INTO slack_source_events(source_event_id,event_kind,received_at_us,committed_at_us,admission_result,diagnostic_code,task_version,changed,last_seen_at_us) VALUES('Ev-invalid-diag','message',1,1,'malformed','raw Slack error',0,0,1)"},
		{"effect kind", "INSERT INTO slack_effects(effect_id,idempotency_key,kind,team_id,channel_id,payload_hash,state,attempt_count,prepared_at_us,created_at_us,updated_at_us) VALUES('eff_bcdefghijklmnopqrstuvw','kind','other','T12345678','C12345678','aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa','prepared',0,1,1,1)"},
		{"effect hash", "INSERT INTO slack_effects(effect_id,idempotency_key,kind,team_id,channel_id,text,payload_hash,state,attempt_count,prepared_at_us,created_at_us,updated_at_us) VALUES('eff_bcdefghijklmnopqrstuvw','hash','send','T12345678','C12345678','text','AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA','prepared',0,1,1,1)"},
		{"prepared attempt", "INSERT INTO slack_effects(effect_id,idempotency_key,kind,team_id,channel_id,text,payload_hash,state,attempt_count,prepared_at_us,created_at_us,updated_at_us) VALUES('eff_bcdefghijklmnopqrstuvw','attempt','send','T12345678','C12345678','text','aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa','prepared',1,1,1,1)"},
		{"submitted timestamp", "INSERT INTO slack_effects(effect_id,idempotency_key,kind,team_id,channel_id,text,payload_hash,state,attempt_count,prepared_at_us,created_at_us,updated_at_us) VALUES('eff_bcdefghijklmnopqrstuvw','submitted','send','T12345678','C12345678','text','aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa','submitted',1,1,1,1)"},
		{"file state", "INSERT INTO files(file_id,direction,slack_file_id,original_name,related_task_id,transfer_state,created_at_us,updated_at_us) VALUES('fil_bcdefghijklmnopqrstuvw','from_slack','F12345678','name','tsk_missing','live',1,1)"},
	}
	for _, test := range invalid {
		t.Run(test.name, func(t *testing.T) {
			if _, err := db.Exec(test.sql); err == nil {
				t.Fatal("accepted invalid record")
			}
		})
	}
}

func TestOpenRejectsAlteredV3HistoryAndObjects(t *testing.T) {
	ctx := context.Background()
	for _, test := range []struct {
		name   string
		alter  string
		assert func(error) bool
	}{
		{"checksum", "UPDATE schema_migrations SET checksum='0000000000000000000000000000000000000000000000000000000000000000' WHERE version=3", func(err error) bool { return errors.Is(err, ErrIncompatibleSchema) }},
		{"missing index", "DROP INDEX slack_effects_exceptional_idx", func(err error) bool { return errors.Is(err, ErrIncompatibleSchema) }},
		{"same-name altered table", "ALTER TABLE slack_source_events ADD COLUMN injected TEXT", func(err error) bool { return errors.Is(err, ErrIncompatibleSchema) }},
		{"same-name altered index", "DROP INDEX slack_source_events_committed_result_idx; CREATE INDEX slack_source_events_committed_result_idx ON slack_source_events(admission_result,committed_at_us)", func(err error) bool { return errors.Is(err, ErrIncompatibleSchema) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "ledger.sqlite3")
			s, err := Initialize(ctx, path, Options{})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.DB().Exec(test.alter); err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			opened, err := Open(ctx, path, Options{})
			if opened != nil {
				_ = opened.Close()
			}
			if !test.assert(err) {
				t.Fatalf("Open altered v3: %v", err)
			}
		})
	}
}

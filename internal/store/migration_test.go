package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// ledgerAtVersion builds an exact older ledger by applying the embedded
// migrations up to version, then runs the extra statements against it.
func ledgerAtVersion(t *testing.T, version int, statements ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ledger.sqlite3")
	db, err := sql.Open("sqlite", sqliteDSN(path, time.Second))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	migrations := [][]byte{migration0001, migration0002, migration0003, migration0004, migration0005, migration0006, migration0007, migration0008, migration0009, migration0010}
	for i, migration := range migrations[:version] {
		if _, err := db.Exec(string(migration)); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec("INSERT INTO schema_migrations(version,checksum,applied_at) VALUES(?,?,1)", i+1, migrationChecksum(i+1)); err != nil {
			t.Fatal(err)
		}
	}
	statements = append([]string{fmt.Sprintf("PRAGMA application_id=%d", ApplicationID), fmt.Sprintf("PRAGMA user_version=%d", version)}, statements...)
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func TestOpenUpgradeV7DropsPiDeliveriesAndKeepsTasks(t *testing.T) {
	path := ledgerAtVersion(t, 7,
		`INSERT INTO tasks(task_id,version,state,text,source_kind,created_at,updated_at,done_at) VALUES('tsk_v7_task',1,'open','keep me','local',1,1,NULL)`,
		`INSERT INTO pi_deliveries(delivery_id,task_id,task_version,pi_session_id,delivery_key,state,created_at_us,updated_at_us) VALUES('dlv_abcdefghijklmnopqrstuv','tsk_v7_task',1,'session','sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa','uncertain',1,1)`,
	)
	s, err := Open(context.Background(), path, Options{})
	if err != nil {
		t.Fatalf("Open v7: %v", err)
	}
	defer s.Close()
	var tables int
	if err := s.DB().QueryRow("SELECT count(*) FROM sqlite_master WHERE name LIKE 'pi_deliveries%'").Scan(&tables); err != nil || tables != 0 {
		t.Fatalf("pi_deliveries objects=%d err=%v", tables, err)
	}
	got, err := s.Get(context.Background(), "tsk_v7_task")
	if err != nil || got.Text != "keep me" {
		t.Fatalf("task after upgrade=%+v err=%v", got, err)
	}
}

func TestOpenUpgradeV9DropsTrustedUsersAndKeepsSlackWork(t *testing.T) {
	path := ledgerAtVersion(t, 9,
		`INSERT INTO tasks(task_id,version,state,text,source_kind,created_at,updated_at,done_at) VALUES('tsk_v9_task',1,'open','keep me','local',1,1,NULL)`,
		`INSERT INTO trusted_users(slack_user_id) VALUES('U12345678')`,
	)
	s, err := Open(context.Background(), path, Options{})
	if err != nil {
		t.Fatalf("Open v9: %v", err)
	}
	defer s.Close()
	var tables, version int
	if err := s.DB().QueryRow("SELECT count(*) FROM sqlite_master WHERE name='trusted_users'").Scan(&tables); err != nil || tables != 0 {
		t.Fatalf("trusted_users objects=%d err=%v", tables, err)
	}
	if err := s.DB().QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != SchemaVersion {
		t.Fatalf("version=%d err=%v", version, err)
	}
	got, err := s.Get(context.Background(), "tsk_v9_task")
	if err != nil || got.Text != "keep me" {
		t.Fatalf("task after upgrade=%+v err=%v", got, err)
	}
}

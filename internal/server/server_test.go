package server

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stayradiated/mosscap/internal/client"
	"github.com/stayradiated/mosscap/internal/config"
	"github.com/stayradiated/mosscap/internal/protocol"
	"github.com/stayradiated/mosscap/internal/store"
)

func TestStatusAndDoctorAreSecretSafeAndNonDestructive(t *testing.T) {
	c := config.Config{Version: 1, Paths: config.Paths{Database: filepath.Join(t.TempDir(), "ledger.sqlite"), Endpoint: "/private/control.sock"}}
	db, err := store.Initialize(context.Background(), c.Paths.Database, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Create(context.Background(), "status-create", "TOKEN_CANARY_must_not_escape"); err != nil {
		t.Fatal(err)
	}
	s := &Server{Config: c, Version: "test", store: db, endpoint: c.Paths.Endpoint, started: time.Now().Add(-time.Second)}
	before, err := db.Counts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"status.get", "doctor.run"} {
		r := s.dispatch(protocol.Request{RequestID: "status-" + command, Command: command})
		if r.Error != nil {
			t.Fatalf("%s: %v", command, r.Error)
		}
		if strings.Contains(string(r.Result), "TOKEN_CANARY") {
			t.Fatalf("%s leaked secret canary: %s", command, r.Result)
		}
	}
	after, err := db.Counts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatalf("doctor/status changed durable counts: before=%+v after=%+v", before, after)
	}
}

func TestRestartReplaysOneDurableCreate(t *testing.T) {
	root, err := os.MkdirTemp("/home/admin", "mosscap-server-")
	if err != nil {
		t.Skipf("cannot create private test tree: %v", err)
	}
	defer os.RemoveAll(root)
	state := filepath.Join(root, "state")
	tmp := filepath.Join(state, "tmp")
	for _, p := range []string{state, tmp} {
		if err := os.MkdirAll(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	c := config.Config{Version: 1, Paths: config.Paths{StateDir: state, Database: filepath.Join(state, "ledger.sqlite"), Endpoint: filepath.Join(state, "control.sock"), TmpDir: tmp}, Database: config.Database{BusyTimeout: "1s"}, Protocol: config.Protocol{MaxFrameBytes: 65536}}
	db, err := store.Initialize(context.Background(), c.Paths.Database, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	request := func() protocol.Request {
		b, _ := json.Marshal(protocol.TaskCreateParams{Text: "durable"})
		return protocol.Request{ProtocolVersion: protocol.Version, RequestID: "restart_create", ClientKind: "cli", ClientVersion: "test", Command: "task.create", Params: b}
	}
	call := func() protocol.TaskMutationDTO {
		cl, err := client.Dial(context.Background(), c.Paths.Endpoint, uint32(c.Protocol.MaxFrameBytes))
		if err != nil {
			t.Fatal(err)
		}
		defer cl.Close()
		if _, err = cl.Hello(context.Background(), "restart_hello", "cli", "test", nil); err != nil {
			t.Fatal(err)
		}
		r, err := cl.Call(context.Background(), request())
		if err != nil {
			t.Fatal(err)
		}
		if r.Error != nil {
			t.Fatal(r.Error)
		}
		var out protocol.TaskMutationDTO
		if err = json.Unmarshal(r.Result, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	start := func() (context.CancelFunc, chan error) {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- New(c, "test", nil).Serve(ctx) }()
		deadline := time.Now().Add(3 * time.Second)
		for {
			cl, e := client.Dial(context.Background(), c.Paths.Endpoint, 65536)
			if e == nil {
				_ = cl.Close()
				break
			}
			if time.Now().After(deadline) {
				t.Fatal(e)
			}
			time.Sleep(10 * time.Millisecond)
		}
		return cancel, done
	}
	cancel, done := start()
	first := call()
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	cancel, done = start()
	second := call()
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if first.TaskID != second.TaskID || !second.Changed {
		t.Fatalf("replay results: %#v %#v", first, second)
	}
	db, err = store.Open(context.Background(), c.Paths.Database, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tasks, err := db.List(context.Background(), nil, true, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 {
		t.Fatalf("tasks=%d, want 1", len(tasks))
	}
}

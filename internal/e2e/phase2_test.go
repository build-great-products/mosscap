// Package e2e verifies the Phase 2 Node extension wire against the real Go gateway.
package e2e

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stayradiated/mosscap/internal/config"
	"github.com/stayradiated/mosscap/internal/server"
	"github.com/stayradiated/mosscap/internal/store"
)

// TestPhase2NodeDeliveryWire is deliberately credential-free: it creates one
// local task, lets the native Node framed client persist it through the real
// Unix gateway and SQLite ledger, then reconnects with branch-wide evidence.
func TestPhase2NodeDeliveryWire(t *testing.T) {
	root, err := os.MkdirTemp(effectiveHome(t), ".mosscap-phase2-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	loc, err := config.DefaultLocation()
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Setup(loc)
	if err != nil {
		t.Fatal(err)
	}
	ledger, err := store.Open(context.Background(), cfg.Paths.Database, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	created, err := ledger.Create(context.Background(), "phase2-node-task", "local task for Node protocol E2E")
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.Close(); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := server.NewWithOptions(cfg, loc, "phase2-e2e", nil, server.Options{Disabled: true})
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx) }()
	waitPhase2Socket(t, cfg.Paths.Endpoint)
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("serve: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("gateway did not stop")
		}
	})

	cmd := exec.Command("node", "--experimental-strip-types", filepath.Join(sourceRoot(t), "pi-extension", "test", "e2e-client.ts"), cfg.Paths.Endpoint)
	cmd.Dir = sourceRoot(t)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node e2e: %v: %s", err, output)
	}
	var result struct{ DeliveryID, TaskID string }
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("node output %q: %v", output, err)
	}
	if result.DeliveryID == "" || result.TaskID != created.TaskID {
		t.Fatalf("node result=%+v created=%s", result, created.TaskID)
	}

	ledger, err = store.Open(context.Background(), cfg.Paths.Database, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close()
	rows, err := ledger.ListDeliveries(context.Background(), nil, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].State != store.DeliveryPersisted || rows[0].TaskID != created.TaskID {
		t.Fatalf("deliveries=%+v", rows)
	}
	shown, err := ledger.Get(context.Background(), created.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if shown.State != "open" {
		t.Fatalf("delivery changed task state: %s", shown.State)
	}
}

func waitPhase2Socket(t *testing.T, endpoint string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.Dial("unix", endpoint)
		if err == nil {
			_ = c.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("gateway did not create local socket")
}

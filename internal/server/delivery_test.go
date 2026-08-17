package server

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stayradiated/mosscap/internal/client"
	"github.com/stayradiated/mosscap/internal/config"
	"github.com/stayradiated/mosscap/internal/fileio"
	"github.com/stayradiated/mosscap/internal/protocol"
	"github.com/stayradiated/mosscap/internal/slackapi"
	"github.com/stayradiated/mosscap/internal/store"
)

func deliveryGateway(t *testing.T) (config.Config, context.CancelFunc, <-chan error) {
	t.Helper()
	root, err := os.MkdirTemp("/home/admin", ".mosscap-delivery-gateway-")
	if err != nil {
		t.Skipf("cannot create private gateway test tree: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	state, tmp := filepath.Join(root, "state"), filepath.Join(root, "state", "tmp")
	for _, dir := range []string{state, tmp} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	c := testConfig(config.Paths{ConfigDir: root, StateDir: state, Database: filepath.Join(state, "ledger.sqlite"), Endpoint: filepath.Join(state, "control.sock"), TmpDir: tmp})
	if err := os.WriteFile(filepath.Join(root, "config.toml"), []byte("test config"), 0600); err != nil {
		t.Fatal(err)
	}
	ledger, err := store.Initialize(context.Background(), c.Paths.Database, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- New(c, config.Location{Home: "/home/admin", Path: filepath.Join(root, "config.toml"), Paths: c.Paths}, "test", nil).Serve(ctx)
	}()
	for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(time.Millisecond * 10) {
		conn, err := net.Dial("unix", c.Paths.Endpoint)
		if err == nil {
			_ = conn.Close()
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal(err)
		}
	}
	return c, cancel, done
}

func deliveryClient(t *testing.T, c config.Config, kind string) *client.Client {
	t.Helper()
	cl, err := client.Dial(context.Background(), c.Paths.Endpoint, uint32(c.LocalProtocolMaximumFrame))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cl.Close() })
	hello, err := cl.Hello(context.Background(), "hello-"+kind+"-"+strings.ReplaceAll(t.Name(), "/", "-"), kind, "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(hello.Capabilities, protocol.CapabilityPiDeliveryMVP) {
		t.Fatalf("delivery capability absent: %#v", hello.Capabilities)
	}
	return cl
}
func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
func deliveryCall(t *testing.T, cl *client.Client, kind, requestID, command string, params any) protocol.Response {
	t.Helper()
	b, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	r, err := cl.Call(context.Background(), protocol.Request{ProtocolVersion: protocol.Version, RequestID: requestID, ClientKind: kind, ClientVersion: "test", Command: command, Params: b})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestFileTransferErrorCodesRemainSafeAndSpecific(t *testing.T) {
	cases := []struct {
		err  error
		code string
		kind string
	}{
		{fileio.ErrUnsafeFileType, "unsafe_file_type", "unsafe"},
		{slackapi.NewExternalError(slackapi.ReasonRejected, slackapi.DiagnosticUnsafeFileRedirect, "", nil), "unsafe_file_redirect", "unsafe"},
		{slackapi.NewExternalError(slackapi.ReasonRejected, slackapi.DiagnosticSlackFileUnavailable, "", nil), "slack_file_unavailable", "external_failed"},
	}
	for _, tc := range cases {
		response := errorResponse("req_test", "test", tc.err)
		if response.Error == nil || response.Error.Code != tc.code || response.Error.Category != tc.kind {
			t.Fatalf("errorResponse(%v) = %#v, want %s/%s", tc.err, response.Error, tc.code, tc.kind)
		}
	}
}

func TestDeliveryWorkAttachedFilesAreIncomingAndAttachedOnly(t *testing.T) {
	declared := int64(42)
	work := &store.DeliveryWork{Delivery: store.Delivery{ID: "dlv_AbCdEfGhIjKlMnOpQrStUv", DeliveryKey: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}, Task: store.Task{ID: "AbCdEfGhIjKlMnOpQrStUv", Version: 1, SourceKind: "slack"}}
	files := []store.SlackFile{
		{FileID: "fil_AbCdEfGhIjKlMnOpQrStUv", Direction: "from_slack", SourceAttached: true, OriginalName: "report\x1b.txt", DeclaredMIME: "text/plain", DeclaredSizeBytes: &declared, TransferState: "metadata_only"},
		{FileID: "fil_BbCdEfGhIjKlMnOpQrStUv", Direction: "from_slack", SourceAttached: false, OriginalName: "detached", TransferState: "validated", SHA256: strings.Repeat("a", 64)},
		{FileID: "fil_CbCdEfGhIjKlMnOpQrStUv", Direction: "to_slack", SourceAttached: true, OriginalName: "outgoing", TransferState: "confirmed", SourcePath: "/private/source"},
	}
	got := deliveryWorkDTO(work, files)
	if got == nil || len(got.AttachedFiles) != 1 {
		t.Fatalf("attached files = %#v", got)
	}
	file := got.AttachedFiles[0]
	if file.FileID != files[0].FileID || file.OriginalName != files[0].OriginalName || file.DeclaredMIME == nil || *file.DeclaredMIME != "text/plain" || file.DeclaredSizeBytes != &declared || file.TransferState != "metadata_only" {
		t.Fatalf("attachment = %#v", file)
	}
	encoded, err := json.Marshal(got)
	if err != nil || strings.Contains(string(encoded), "source_path") || strings.Contains(string(encoded), `"sha256":`) || strings.Contains(string(encoded), "outgoing") {
		t.Fatalf("delivery projection leaked forbidden data: %s (%v)", encoded, err)
	}
}

func TestPiDeliveryConnectionBindingAndRecovery(t *testing.T) {
	c, cancel, done := deliveryGateway(t)
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	ledger, err := store.Open(context.Background(), c.Paths.Database, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Create(context.Background(), "delivery-create", "TOKEN_CANARY task body"); err != nil {
		t.Fatal(err)
	}
	if err := ledger.Close(); err != nil {
		t.Fatal(err)
	}

	first := deliveryClient(t, c, "extension")
	if r := deliveryCall(t, first, "extension", "poll-before-open", "delivery.poll", protocol.EmptyParams{}); r.Error == nil || r.Error.Code != "delivery_open_required" {
		t.Fatalf("pre-open poll=%+v", r)
	}
	opened := deliveryCall(t, first, "extension", "open-first", "delivery.open", protocol.DeliveryOpenParams{PiSessionID: "persisted-session"})
	if opened.Error != nil {
		t.Fatal(opened.Error)
	}
	// Same connection/session can repeat open without losing its slot.
	if r := deliveryCall(t, first, "extension", "open-first-repeat", "delivery.open", protocol.DeliveryOpenParams{PiSessionID: "persisted-session"}); r.Error != nil {
		t.Fatal(r.Error)
	}
	poll := deliveryCall(t, first, "extension", "poll-first", "delivery.poll", protocol.EmptyParams{})
	if poll.Error != nil {
		t.Fatal(poll.Error)
	}
	var offered protocol.DeliveryPollDTO
	if err := json.Unmarshal(poll.Result, &offered); err != nil || offered.Work == nil || offered.Work.TaskText != "TOKEN_CANARY task body" {
		t.Fatalf("offered=%+v err=%v", offered, err)
	}

	second := deliveryClient(t, c, "extension")
	conflict := deliveryCall(t, second, "extension", "open-second", "delivery.open", protocol.DeliveryOpenParams{PiSessionID: "other-session"})
	if conflict.Error == nil || conflict.Error.Code != "pi_session_active" || conflict.Error.Details["active_pi_session_id"] != "persisted-session" || strings.Contains(conflict.Error.Message, "TOKEN_CANARY") {
		t.Fatalf("conflict=%+v", conflict.Error)
	}
	if r := deliveryCall(t, first, "extension", "inserting", "delivery.inserting", protocol.DeliveryInsertingParams{DeliveryID: offered.Work.DeliveryID, DeliveryKey: offered.Work.DeliveryKey}); r.Error != nil {
		t.Fatal(r.Error)
	}
	if r := deliveryCall(t, first, "extension", "close-first", "delivery.close", protocol.EmptyParams{}); r.Error != nil {
		t.Fatal(r.Error)
	}
	// Close reconciles before it releases, so successor admission cannot poll an
	// insertion whose durable outcome is uncertain.
	resumed := deliveryCall(t, second, "extension", "open-second-resume", "delivery.open", protocol.DeliveryOpenParams{PiSessionID: "persisted-session"})
	if resumed.Error != nil {
		t.Fatal(resumed.Error)
	}
	if r := deliveryCall(t, second, "extension", "poll-uncertain", "delivery.poll", protocol.EmptyParams{}); r.Error != nil {
		t.Fatal(r.Error)
	} else {
		var out protocol.DeliveryPollDTO
		if err := json.Unmarshal(r.Result, &out); err != nil || out.Work != nil || out.UncertainCount != 1 {
			t.Fatalf("uncertain poll=%+v err=%v", out, err)
		}
	}
	statusResponse := deliveryCall(t, second, "extension", "status", "status.get", protocol.EmptyParams{})
	if statusResponse.Error != nil || strings.Contains(string(statusResponse.Result), "TOKEN_CANARY") || !strings.Contains(string(statusResponse.Result), "persisted-session") {
		t.Fatalf("status=%s error=%v", statusResponse.Result, statusResponse.Error)
	}
}

func TestPiDeliveryObservedEvidenceAndClientKindBoundary(t *testing.T) {
	c, cancel, done := deliveryGateway(t)
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	ledger, err := store.Open(context.Background(), c.Paths.Database, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Create(context.Background(), "evidence-create", "task"); err != nil {
		t.Fatal(err)
	}
	if err := ledger.Close(); err != nil {
		t.Fatal(err)
	}
	cli := deliveryClient(t, c, "cli")
	if r := deliveryCall(t, cli, "cli", "cli-open", "delivery.open", protocol.DeliveryOpenParams{PiSessionID: "session"}); r.Error == nil || r.Error.Code != "extension_client_required" {
		t.Fatalf("CLI open=%+v", r)
	}

	first := deliveryClient(t, c, "extension")
	if r := deliveryCall(t, first, "extension", "evidence-open", "delivery.open", protocol.DeliveryOpenParams{PiSessionID: "session"}); r.Error != nil {
		t.Fatal(r.Error)
	}
	r := deliveryCall(t, first, "extension", "evidence-poll", "delivery.poll", protocol.EmptyParams{})
	var offered protocol.DeliveryPollDTO
	if r.Error != nil || json.Unmarshal(r.Result, &offered) != nil || offered.Work == nil {
		t.Fatalf("offer=%+v", r)
	}
	_ = first.Close() // pending work is replayable after socket loss.
	second := deliveryClient(t, c, "extension")
	if r := deliveryCall(t, second, "extension", "evidence-reopen", "delivery.open", protocol.DeliveryOpenParams{PiSessionID: "session", Observed: []protocol.DeliveryObservation{{DeliveryID: offered.Work.DeliveryID, DeliveryKey: offered.Work.DeliveryKey}}}); r.Error != nil {
		t.Fatal(r.Error)
	}
	if r := deliveryCall(t, second, "extension", "evidence-poll-after", "delivery.poll", protocol.EmptyParams{}); r.Error != nil {
		t.Fatal(r.Error)
	} else {
		var out protocol.DeliveryPollDTO
		if err := json.Unmarshal(r.Result, &out); err != nil || out.Work != nil {
			t.Fatalf("evidence was not reconciled: %+v %v", out, err)
		}
	}
	if r := deliveryCall(t, second, "extension", "extension-list", "delivery.list", protocol.DeliveryListParams{}); r.Error == nil || r.Error.Code != "operator_client_required" {
		t.Fatalf("extension operator=%+v", r)
	}
}

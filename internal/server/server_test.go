package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stayradiated/mosscap/internal/client"
	"github.com/stayradiated/mosscap/internal/config"
	"github.com/stayradiated/mosscap/internal/protocol"
	"github.com/stayradiated/mosscap/internal/slackapi"
	"github.com/stayradiated/mosscap/internal/slackapi/fake"
	"github.com/stayradiated/mosscap/internal/store"
)

func testConfig(paths config.Paths) config.Config {
	return config.Config{Version: 1, DeliveryLease: "60s", LeaderHeartbeat: "10s", LeaderExpiry: "30s", SlackReconnectInitialBackoff: "1s", SlackReconnectMaxBackoff: "60s", SlackReconnectJitter: true, SQLiteBusyTimeout: "1s", MaximumFileSize: 100 << 20, MaximumFilesPerSlackOperation: 20, TemporaryDownloadLifetime: "24h", LocalProtocolMaximumFrame: 65536, Paths: paths}
}

func newTestServer(t *testing.T, c config.Config) *Server {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp(home, ".mosscap-server-config-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "config.toml")
	if err := os.WriteFile(path, []byte("test config"), 0600); err != nil {
		t.Fatal(err)
	}
	paths := c.Paths
	paths.ConfigDir = root
	c.Paths = paths
	return New(c, config.Location{Home: home, Path: path, Paths: paths}, "test", nil)
}

func TestReloadIsFailureAtomicAndUsesImmutableSnapshots(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp(home, ".mosscap-reload-")
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
	initial, err := config.Setup(loc)
	if err != nil {
		t.Fatal(err)
	}
	s := New(initial, loc, "test", nil)
	busy, err := time.ParseDuration(initial.SQLiteBusyTimeout)
	if err != nil {
		t.Fatal(err)
	}
	s.store, err = store.Open(context.Background(), initial.Paths.Database, store.Options{BusyTimeout: busy})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.store.Close() })
	if _, err := config.Set(loc, "maximum_file_size", "200MiB"); err != nil {
		t.Fatal(err)
	}
	s.Verifier = ConfigurationVerifierFunc(func(context.Context, config.Config) error {
		return errors.New("TOKEN_CANARY configured channel is inaccessible")
	})
	if _, err := s.reload(context.Background(), "reload-verify-failure"); err == nil || !strings.Contains(err.Error(), "verification failed") || strings.Contains(err.Error(), "TOKEN_CANARY") {
		t.Fatalf("failed reload error = %v", err)
	}
	if got := s.snapshot(); got.MaximumFileSize != initial.MaximumFileSize || s.generation.Load() != 1 {
		t.Fatalf("failed reload changed active snapshot: size=%d generation=%d", got.MaximumFileSize, s.generation.Load())
	}
	s.Verifier = ConfigurationVerifierFunc(func(_ context.Context, candidate config.Config) error {
		if candidate.MaximumFileSize != 200<<20 {
			return errors.New("candidate was not complete")
		}
		return nil
	})
	result, err := s.reload(context.Background(), "reload-success")
	if err != nil {
		t.Fatal(err)
	}
	if !result.Activated || !result.SlackNetworkVerificationDeferred || result.ActiveConfigGeneration != 2 {
		t.Fatalf("reload result = %+v", result)
	}
	if got := s.snapshot(); got.MaximumFileSize != 200<<20 {
		t.Fatalf("activated maximum file size = %d", got.MaximumFileSize)
	}
	if err := os.WriteFile(loc.Path, []byte("version = 1\n[paths]\nstate_dir = \"/tmp/legacy\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.reload(context.Background(), "reload-invalid"); err == nil || !strings.Contains(err.Error(), "candidate is invalid") {
		t.Fatalf("legacy reload error = %v", err)
	}
	if got := s.snapshot(); got.MaximumFileSize != 200<<20 || s.generation.Load() != 2 {
		t.Fatalf("invalid reload changed active snapshot: size=%d generation=%d", got.MaximumFileSize, s.generation.Load())
	}
}

type fakeRuntimeFactory struct {
	webs    []slackapi.Web
	sockets []slackapi.Socket
}

func (f *fakeRuntimeFactory) Build(config.Config) (slackapi.Web, slackapi.Socket, error) {
	if len(f.webs) == 0 || len(f.sockets) == 0 {
		return nil, nil, errors.New("fake runtime factory exhausted")
	}
	web, socket := f.webs[0], f.sockets[0]
	f.webs, f.sockets = f.webs[1:], f.sockets[1:]
	return web, socket, nil
}

func phase1VerifiedWeb() slackapi.Web {
	return fake.NewWeb(
		fake.Step{Operation: fake.Auth, Result: slackapi.AuthIdentity{TeamID: "T012ABC", BotUserID: "U012BOT"}},
		fake.Step{Operation: fake.Team, Result: slackapi.Team{ID: "T012ABC"}},
		fake.Step{Operation: fake.Channel, Result: slackapi.Channel{ID: "C012ABC", TeamID: "T012ABC", Member: true, Accessible: true}},
		fake.Step{Operation: fake.Scopes, Result: slackapi.RequiredBotScopes(false)},
	)
}

func phase1Envelope(id string) (slackapi.Envelope, *fake.AckRecorder) {
	ack, recorder := fake.NewAckRecorder(nil)
	return slackapi.Envelope{EnvelopeID: "env-" + id, EventID: "Ev-" + id, Kind: slackapi.EventAppMention, TeamID: "T012ABC", ChannelID: "C012ABC", UserID: "U12345678", MessageTS: "1700000000.000001", EventTS: "1700000001.000001", Text: "do this <@U012BOT>", Ack: ack}, recorder
}

func waitForAck(t *testing.T, recorder *fake.AckRecorder) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for recorder.Calls() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := recorder.Calls(); got != 1 {
		t.Fatalf("ack calls = %d", got)
	}
}

func TestPhase1ReloadRetainsFailedCandidateThenSwapsAndDrains(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp(home, ".mosscap-phase1-reload-")
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
	initial, err := config.Setup(loc)
	if err != nil {
		t.Fatal(err)
	}
	for key, value := range map[string]string{
		"slack_app_token": "xapp-test", "slack_bot_token": "xoxb-test", "slack_team_id": "T012ABC", "slack_bot_user_id": "U012BOT", "slack_channel_id": "C012ABC",
	} {
		if _, err := config.Set(loc, key, value); err != nil {
			t.Fatal(err)
		}
	}
	initial, _, err = config.LoadWithDigest(loc)
	if err != nil {
		t.Fatal(err)
	}
	busy, _ := time.ParseDuration(initial.SQLiteBusyTimeout)
	ledger, err := store.Open(context.Background(), initial.Paths.Database, store.Options{BusyTimeout: busy})
	if err != nil {
		t.Fatal(err)
	}
	oldSocket := fake.NewSocket(fake.SocketSession{})
	failedEnvelope, failedAck := phase1Envelope("failed")
	failedSocket := fake.NewSocket(fake.SocketSession{Receives: []fake.ReceiveStep{{Envelope: failedEnvelope}}})
	successEnvelope, successAck := phase1Envelope("success")
	newSocket := fake.NewSocket(fake.SocketSession{Receives: []fake.ReceiveStep{{Envelope: successEnvelope}}})
	replayEnvelope, replayAck := phase1Envelope("replay")
	replaySocket := fake.NewSocket(fake.SocketSession{Receives: []fake.ReceiveStep{{Envelope: replayEnvelope}}})
	factory := &fakeRuntimeFactory{
		webs:    []slackapi.Web{phase1VerifiedWeb(), fake.NewWeb(fake.Step{Operation: fake.Auth, Err: errors.New("xoxb-TOKEN_CANARY")}), phase1VerifiedWeb(), phase1VerifiedWeb()},
		sockets: []slackapi.Socket{oldSocket, failedSocket, newSocket, replaySocket},
	}
	s := NewWithOptions(initial, loc, "test", nil, Options{Factory: factory})
	s.store = ledger
	if err := s.startSlack(context.Background(), initial); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.closeSlack(); _ = ledger.Close() })
	oldRuntime := s.runtime
	if _, err := config.Set(loc, "maximum_file_size", "200MiB"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.reload(context.Background(), "phase1-failed"); err == nil || strings.Contains(err.Error(), "TOKEN_CANARY") {
		t.Fatalf("unsafe candidate failure: %v", err)
	}
	if s.runtime != oldRuntime || s.snapshot().MaximumFileSize != initial.MaximumFileSize {
		t.Fatal("failed candidate changed published runtime or config")
	}
	if failedAck.Calls() != 0 || len(failedSocket.OpenedStreams()) != 0 {
		t.Fatal("failed candidate handled an envelope")
	}
	result, err := s.reload(context.Background(), "phase1-success")
	if err != nil || !result.Activated || result.SlackNetworkVerificationDeferred {
		t.Fatalf("reload result=%+v err=%v", result, err)
	}
	if s.runtime == oldRuntime || s.snapshot().MaximumFileSize != 200<<20 {
		t.Fatal("successful candidate was not atomically published")
	}
	waitForAck(t, successAck)
	if _, err := s.reload(context.Background(), "phase1-success"); err != nil {
		t.Fatalf("replayed reload: %v", err)
	}
	if replayAck.Calls() != 0 {
		t.Fatalf("replayed candidate acknowledged before activation: %d", replayAck.Calls())
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := oldSocket.OpenedStreams()[0].WaitForClose(ctx); err != nil {
		t.Fatalf("old runtime did not drain: %v", err)
	}
	if len(newSocket.OpenedStreams()) != 1 {
		t.Fatal("new runtime was not started before publication")
	}
	if len(replaySocket.OpenedStreams()) != 1 || !replaySocket.OpenedStreams()[0].Closed() {
		t.Fatal("replayed candidate stream was not closed without activation")
	}
}

func TestReloadAppliesSQLiteBusyTimeoutWithoutStoringTokens(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp(home, ".mosscap-busy-reload-")
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
	initial, err := config.Setup(loc)
	if err != nil {
		t.Fatal(err)
	}
	busy, _ := time.ParseDuration(initial.SQLiteBusyTimeout)
	live, err := store.Open(context.Background(), initial.Paths.Database, store.Options{BusyTimeout: busy})
	if err != nil {
		t.Fatal(err)
	}
	s := New(initial, loc, "test", nil)
	s.store = live
	t.Cleanup(func() { _ = s.store.Close() })
	if _, err := config.Set(loc, "sqlite_busy_timeout", "25ms"); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Set(loc, "slack_app_token", "xapp-TOKEN_CANARY"); err != nil {
		t.Fatal(err)
	}
	first, err := s.reload(context.Background(), "reload-busy-timeout")
	if err != nil {
		t.Fatal(err)
	}
	if first.ActiveConfigDigest == "" || first.ActiveConfigGeneration != 2 {
		t.Fatalf("reload result = %+v", first)
	}
	if replay, err := s.reload(context.Background(), "reload-busy-timeout"); err != nil || replay != first || s.generation.Load() != 2 {
		t.Fatalf("reload replay = %+v, %v; generation=%d", replay, err, s.generation.Load())
	}
	if _, err := config.Set(loc, "maximum_file_size", "200MiB"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.reload(context.Background(), "reload-busy-timeout"); !errors.Is(err, store.ErrRequestConflict) || s.generation.Load() != 2 {
		t.Fatalf("changed staged candidate reused request ID: err=%v generation=%d", err, s.generation.Load())
	}
	var rows int
	if err := s.store.DB().QueryRow("SELECT count(*) FROM mutation_requests WHERE result_json LIKE '%TOKEN_CANARY%' OR input_hash LIKE '%TOKEN_CANARY%' OR error_message LIKE '%TOKEN_CANARY%'").Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("token entered SQLite rows=%d err=%v", rows, err)
	}
	locker, err := sql.Open("sqlite", initial.Paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer locker.Close()
	if _, err := locker.Exec("BEGIN EXCLUSIVE"); err != nil {
		t.Fatal(err)
	}
	defer locker.Exec("ROLLBACK")
	started := time.Now()
	_, err = s.store.Create(context.Background(), "busy-timeout-create", "must wait briefly")
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("write unexpectedly succeeded while exclusive SQLite lock held")
	}
	if elapsed > time.Second {
		t.Fatalf("live busy timeout was not applied: waited %s", elapsed)
	}
}

func TestStatusAndDoctorAreSecretSafeAndNonDestructive(t *testing.T) {
	c := testConfig(config.Paths{Database: filepath.Join(t.TempDir(), "ledger.sqlite"), Endpoint: "/private/control.sock"})
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
		r := s.dispatch(context.Background(), protocol.Request{RequestID: "status-" + command, Command: command})
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

func TestGatewayStartupRejectsUnsafeConfigAncestors(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		unsafe func(root, configDir string) error
	}{
		{"group_writable", func(root, _ string) error { return os.Chmod(root, 0770) }},
		{"symlink", func(_, configDir string) error {
			real := configDir + ".real"
			if err := os.Rename(configDir, real); err != nil {
				return err
			}
			return os.Symlink(real, configDir)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, err := os.MkdirTemp(home, ".mosscap-config-ancestor-")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(root)
			if err := os.Chmod(root, 0700); err != nil {
				t.Fatal(err)
			}
			state, configDir := filepath.Join(root, "state"), filepath.Join(root, "config")
			if err := os.MkdirAll(filepath.Join(state, "tmp"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(configDir, 0700); err != nil {
				t.Fatal(err)
			}
			c := testConfig(config.Paths{ConfigDir: configDir, StateDir: state, Database: filepath.Join(state, "ledger.sqlite"), Endpoint: filepath.Join(state, "control.sock"), TmpDir: filepath.Join(state, "tmp")})
			db, err := store.Initialize(context.Background(), c.Paths.Database, store.Options{})
			if err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			configPath := filepath.Join(configDir, "config.toml")
			if err := os.WriteFile(configPath, []byte("test config"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := tc.unsafe(root, configDir); err != nil {
				t.Fatal(err)
			}
			if err := New(c, config.Location{Home: home, Path: configPath, Paths: c.Paths}, "test", nil).Serve(context.Background()); err == nil {
				t.Fatal("gateway accepted unsafe config ancestor")
			}
		})
	}
}

func TestGatewayStartupRejectsReplacedIntermediateComponent(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, "state")
	nested := filepath.Join(state, "nested")
	tmp := filepath.Join(state, "tmp")
	for _, p := range []string{nested, tmp} {
		if err := os.MkdirAll(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	c := testConfig(config.Paths{StateDir: state, Database: filepath.Join(nested, "ledger.sqlite"), Endpoint: filepath.Join(nested, "control.sock"), TmpDir: tmp})
	db, err := store.Initialize(context.Background(), c.Paths.Database, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	backup := nested + ".original"
	if err := os.Rename(nested, backup); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(backup, nested); err != nil {
		t.Fatal(err)
	}
	if err := newTestServer(t, c).Serve(context.Background()); err == nil {
		t.Fatal("gateway accepted a replaced intermediate symlink")
	}
}

func TestShutdownLeavesReplacementSocketUntouched(t *testing.T) {
	root, err := os.MkdirTemp("/home/admin", "mosscap-server-")
	if err != nil {
		t.Skipf("cannot create private test tree: %v", err)
	}
	defer os.RemoveAll(root)
	state := filepath.Join(root, "state")
	for _, p := range []string{state, filepath.Join(state, "tmp")} {
		if err := os.MkdirAll(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	c := testConfig(config.Paths{StateDir: state, Database: filepath.Join(state, "ledger.sqlite"), Endpoint: filepath.Join(state, "control.sock"), TmpDir: filepath.Join(state, "tmp")})
	db, err := store.Initialize(context.Background(), c.Paths.Database, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := newTestServer(t, c)
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx) }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if info, err := os.Lstat(c.Paths.Endpoint); err == nil && info.Mode()&os.ModeSocket != 0 {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("gateway did not bind endpoint")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := os.Remove(c.Paths.Endpoint); err != nil {
		t.Fatal(err)
	}
	replacement, err := net.ListenUnix("unix", &net.UnixAddr{Name: c.Paths.Endpoint, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Close()
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(c.Paths.Endpoint); err != nil || info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("replacement socket removed or changed: info=%v err=%v", info, err)
	}
}

func TestUnsupportedProtocolGetsOneTypedResponseThenClose(t *testing.T) {
	root, err := os.MkdirTemp("/home/admin", "mosscap-server-")
	if err != nil {
		t.Skipf("cannot create private test tree: %v", err)
	}
	defer os.RemoveAll(root)
	state := filepath.Join(root, "state")
	tmp := filepath.Join(state, "tmp")
	if err := os.MkdirAll(tmp, 0700); err != nil {
		t.Fatal(err)
	}
	c := testConfig(config.Paths{StateDir: state, Database: filepath.Join(state, "ledger.sqlite"), Endpoint: filepath.Join(state, "control.sock"), TmpDir: tmp})
	db, err := store.Initialize(context.Background(), c.Paths.Database, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- newTestServer(t, c).Serve(ctx) }()
	var conn net.Conn
	for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		conn, err = net.Dial("unix", c.Paths.Endpoint)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
	}
	defer conn.Close()
	codec := protocol.NewCodec(conn, conn, 65536)
	if err := codec.WriteFrame([]byte(`{"protocol_version":2,"request_id":"req_old","client_kind":"cli","client_version":"test","command":"hello","params":{"supported_versions":[2],"capabilities":[]}}`)); err != nil {
		t.Fatal(err)
	}
	payload, err := codec.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	response, err := protocol.DecodeResponse(payload)
	if err != nil || response.Error == nil || response.Error.Code != "unsupported_protocol_version" || response.Error.Category != "unsupported" {
		t.Fatalf("response = %#v, %v", response, err)
	}
	if _, err := codec.ReadFrame(); err == nil {
		t.Fatal("server kept incompatible connection open")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
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
	c := testConfig(config.Paths{StateDir: state, Database: filepath.Join(state, "ledger.sqlite"), Endpoint: filepath.Join(state, "control.sock"), TmpDir: tmp})
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
		cl, err := client.Dial(context.Background(), c.Paths.Endpoint, uint32(c.LocalProtocolMaximumFrame))
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
		go func() { done <- newTestServer(t, c).Serve(ctx) }()
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
	tasks, _, err := db.List(context.Background(), nil, true, 10, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 {
		t.Fatalf("tasks=%d, want 1", len(tasks))
	}
}

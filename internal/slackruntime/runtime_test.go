package slackruntime

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stayradiated/mosscap/internal/config"
	"github.com/stayradiated/mosscap/internal/slackapi"
	"github.com/stayradiated/mosscap/internal/slackapi/fake"
)

type factory struct {
	mu     sync.Mutex
	web    []slackapi.Web
	socket []slackapi.Socket
}

func (f *factory) Build(config.Config) (slackapi.Web, slackapi.Socket, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.web) == 0 || len(f.socket) == 0 {
		return nil, nil, errors.New("factory exhausted")
	}
	w, s := f.web[0], f.socket[0]
	f.web = f.web[1:]
	f.socket = f.socket[1:]
	return w, s, nil
}
func validConfig(t *testing.T) config.Config {
	t.Helper()
	l := config.Location{Home: "/home/test", Path: "/home/test/.config/mosscap/config.toml", Paths: config.Paths{StateDir: "/home/test/.local/state/mosscap"}}
	c, err := config.Default(l)
	if err != nil {
		t.Fatal(err)
	}
	c.SlackAppToken = "xapp-test"
	c.SlackBotToken = "xoxb-test"
	c.SlackTeamID = "T012ABC"
	c.SlackBotUserID = "U012BOT"
	c.SlackChannelID = "C012ABC"
	c.SlackReconnectJitter = false
	c.SlackReconnectInitialBackoff = "1ms"
	c.SlackReconnectMaxBackoff = "2ms"
	return c
}
func verifiedWeb() slackapi.Web {
	return fake.NewWeb(fake.Step{Operation: fake.Auth, Result: slackapi.AuthIdentity{TeamID: "T012ABC", BotUserID: "U012BOT"}}, fake.Step{Operation: fake.Team, Result: slackapi.Team{ID: "T012ABC"}}, fake.Step{Operation: fake.Channel, Result: slackapi.Channel{ID: "C012ABC", TeamID: "T012ABC", Member: true, Accessible: true}}, fake.Step{Operation: fake.Scopes, Result: slackapi.RequiredBotScopes(false)})
}
func TestStartFailureLeavesNoSnapshotAndSecretsSafe(t *testing.T) {
	c := validConfig(t)
	f := &factory{web: []slackapi.Web{fake.NewWeb(fake.Step{Operation: fake.Auth, Err: errors.New(c.SlackBotToken)})}, socket: []slackapi.Socket{fake.NewSocket()}}
	r, err := New(Options{Factory: f, Handler: func(context.Context, slackapi.Envelope) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	err = r.Start(context.Background(), c)
	if err == nil || contains(err.Error(), c.SlackBotToken) {
		t.Fatalf("unsafe start error: %v", err)
	}
	if _, err := r.Acquire(); err == nil {
		t.Fatal("failed candidate published")
	}
	_ = r.Close()
}
func TestSafeErrorRendersMissingScopesRemediation(t *testing.T) {
	err := safeError(slackapi.NewMissingScopesError([]slackapi.Scope{
		slackapi.ScopeUsersRead, slackapi.ScopeChannelsHistory, slackapi.ScopeAppMentionsRead,
	}))
	const want = "Slack app is missing required bot OAuth scopes: app_mentions:read, channels:history, users:read. Add them in Slack app OAuth & Permissions, reinstall/re-authorize the app, then restart or gateway reload."
	if err == nil || err.Error() != want {
		t.Fatalf("safeError = %q, want %q", err, want)
	}
}

func TestStartGatesEnvelopeHandlingUntilActivation(t *testing.T) {
	c := validConfig(t)
	ack, ackRecorder := fake.NewAckRecorder(nil)
	socket := fake.NewSocket(fake.SocketSession{Receives: []fake.ReceiveStep{{Envelope: slackapi.Envelope{EnvelopeID: "env-gated", Ack: ack}}}})
	f := &factory{web: []slackapi.Web{verifiedWeb()}, socket: []slackapi.Socket{socket}}
	var handled atomic.Int32
	r, err := New(Options{Factory: f, Handler: func(ctx context.Context, envelope slackapi.Envelope) error {
		handled.Add(1)
		return envelope.Ack.Acknowledge(ctx)
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Start(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if got := handled.Load(); got != 0 || ackRecorder.Calls() != 0 {
		t.Fatalf("unactivated candidate handled=%d acked=%d", got, ackRecorder.Calls())
	}
	if err := r.Activate(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for handled.Load() != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := handled.Load(); got != 1 || ackRecorder.Calls() != 1 {
		t.Fatalf("activated candidate handled=%d acked=%d", got, ackRecorder.Calls())
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if !socket.OpenedStreams()[0].Closed() {
		t.Fatal("activated candidate stream was not drained")
	}
}

func TestFailedReloadRetainsOldSnapshot(t *testing.T) {
	c := validConfig(t)
	oldSocket := fake.NewSocket(fake.SocketSession{})
	f := &factory{web: []slackapi.Web{verifiedWeb(), fake.NewWeb(fake.Step{Operation: fake.Auth, Result: slackapi.AuthIdentity{TeamID: "T999BAD", BotUserID: "U012BOT"}})}, socket: []slackapi.Socket{oldSocket, fake.NewSocket()}}
	r, err := New(Options{Factory: f, Handler: func(context.Context, slackapi.Envelope) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Start(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	before, err := r.Acquire()
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Reload(context.Background(), c); err == nil {
		t.Fatal("expected candidate rejection")
	}
	after, err := r.Acquire()
	if err != nil || after != before {
		t.Fatal("failed candidate replaced the active snapshot")
	}
	after.Release()
	before.Release()
	_ = r.Close()
}

func TestReloadRetainsOldThenSwapsAndDrains(t *testing.T) {
	c := validConfig(t)
	oldSocket := fake.NewSocket(fake.SocketSession{})
	newSocket := fake.NewSocket(fake.SocketSession{})
	f := &factory{web: []slackapi.Web{verifiedWeb(), verifiedWeb()}, socket: []slackapi.Socket{oldSocket, newSocket}}
	r, err := New(Options{Factory: f, Handler: func(context.Context, slackapi.Envelope) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Start(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	held, err := r.Acquire()
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Reload(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	current, err := r.Acquire()
	if err != nil || current == held {
		t.Fatal("candidate was not swapped")
	}
	current.Release()
	held.Release()
	deadline := context.Background()
	ctx, cancel := context.WithTimeout(deadline, time.Second)
	defer cancel()
	if err := oldSocket.OpenedStreams()[0].WaitForClose(ctx); err != nil {
		t.Fatal(err)
	}
	_ = r.Close()
	if !newSocket.OpenedStreams()[0].Closed() {
		t.Fatal("close did not close active stream")
	}
}
func TestReconnectUsesBoundedBackoffAndResets(t *testing.T) {
	c := validConfig(t)
	first := fake.NewSocket(fake.SocketSession{Receives: []fake.ReceiveStep{{Err: errors.New("disconnect")}}})
	second := fake.NewSocket(fake.SocketSession{})
	var delays []time.Duration
	var mu sync.Mutex
	f := &factory{web: []slackapi.Web{verifiedWeb(), fake.NewWeb()}, socket: []slackapi.Socket{first, second}}
	r, err := New(Options{Factory: f, Handler: func(context.Context, slackapi.Envelope) error { return nil }, Sleep: func(_ context.Context, d time.Duration) error {
		mu.Lock()
		delays = append(delays, d)
		mu.Unlock()
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Start(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if err = r.Activate(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := second.WaitForOpenedStreams(ctx, 1); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	if len(delays) == 0 || delays[0] != time.Millisecond {
		t.Fatalf("delays=%v", delays)
	}
	mu.Unlock()
	_ = r.Close()
}
func contains(s, sub string) bool {
	return len(sub) > 0 && len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}

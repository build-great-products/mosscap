package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stayradiated/mosscap/internal/client"
	"github.com/stayradiated/mosscap/internal/config"
	"github.com/stayradiated/mosscap/internal/fileio"
	"github.com/stayradiated/mosscap/internal/filetransfer"
	"github.com/stayradiated/mosscap/internal/protocol"
	"github.com/stayradiated/mosscap/internal/slackapi"
	"github.com/stayradiated/mosscap/internal/slackapi/fake"
	"github.com/stayradiated/mosscap/internal/slackruntime"
	"github.com/stayradiated/mosscap/internal/store"
	"github.com/stayradiated/mosscap/internal/task"
)

func testConfig(paths config.Paths) config.Config {
	return config.Config{Version: 1, SlackReconnectInitialBackoff: "1s", SlackReconnectMaxBackoff: "60s", SlackReconnectJitter: true, SQLiteBusyTimeout: "1s", MaximumFileSize: 100 << 20, MaximumFilesPerSlackOperation: 20, TemporaryDownloadLifetime: "24h", LocalProtocolMaximumFrame: 65536, Paths: paths}
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

func TestMetadataPurgeGenericAfterDeleteFailureMapsToRetriableZeroProgress(t *testing.T) {
	r := errorResponse("req_server_purge_error", "test", &store.PurgeError{
		Cause:    errors.New("after-delete generic failure"),
		Progress: store.PurgeResult{},
	})
	if r.Error == nil || r.Error.Code != "purge_interrupted" || !r.Error.Retriable || r.Error.Progress == nil || r.Error.Progress.Removed != (protocol.PurgeCountsDTO{}) {
		t.Fatalf("response = %#v", r)
	}
}

func TestMetadataPurgeRouteDryRunDoesNotWriteRequest(t *testing.T) {
	db, err := store.Initialize(context.Background(), filepath.Join(t.TempDir(), "mosscap.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := &Server{Version: "test", store: db}
	old := int64(1_700_000_000_000_000)
	if _, err := s.store.DB().Exec(`INSERT INTO tasks(task_id,version,state,text,source_kind,created_at,updated_at,done_at) VALUES('tsk_server_preview',1,'done','done','local',?,?,?)`, old, old, old); err != nil {
		t.Fatal(err)
	}
	params := []byte(`{"cutoff":"2023-11-14T22:13:20.000001Z","mode":"dry_run"}`)
	r := s.dispatch(context.Background(), protocol.Request{RequestID: "req_server_purge_preview", Command: "metadata.purge", Params: params})
	if r.Error != nil || r.ResultType != "metadata_purge" {
		t.Fatalf("response = %#v", r)
	}
	var got protocol.MetadataPurgeDTO
	if err := json.Unmarshal(r.Result, &got); err != nil {
		t.Fatal(err)
	}
	if got.Mode != "dry_run" || got.Eligible.Tasks != 1 || got.Removed.Tasks != 0 {
		t.Fatalf("result = %#v", got)
	}
	var requests int
	if err := s.store.DB().QueryRow("SELECT count(*) FROM mutation_requests").Scan(&requests); err != nil || requests != 0 {
		t.Fatalf("dry-run request records = %d, %v", requests, err)
	}
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

func verifiedSlackWeb() slackapi.Web {
	return fake.NewWeb(
		fake.Step{Operation: fake.Auth, Result: slackapi.AuthIdentity{TeamID: "T012ABC", BotUserID: "U012BOT"}},
		fake.Step{Operation: fake.Team, Result: slackapi.Team{ID: "T012ABC"}},
		fake.Step{Operation: fake.Channel, Result: slackapi.Channel{ID: "C012ABC", TeamID: "T012ABC", Member: true, Accessible: true}},
		fake.Step{Operation: fake.Scopes, Result: slackapi.RequiredBotScopes(false)},
	)
}

func slackEnvelope(id string) (slackapi.Envelope, *fake.AckRecorder) {
	ack, recorder := fake.NewAckRecorder(nil)
	return slackapi.Envelope{EnvelopeID: "env-" + id, EventID: "Ev-" + id, Kind: slackapi.EventAppMention, TeamID: "T012ABC", ChannelID: "C012ABC", UserID: "U12345678", MessageTS: "1700000000.000001", EventTS: "1700000001.000001", Text: "do this <@U012BOT>", Ack: ack}, recorder
}

func TestUntrustedSlackAdmissionLogsActionableSafeMetadata(t *testing.T) {
	var output bytes.Buffer
	s := &Server{Log: slog.New(slog.NewTextHandler(&output, nil))}
	envelope, _ := slackEnvelope("TOKEN_AND_MESSAGE_CANARY")

	s.logSlackAdmission(envelope, store.SlackEventResult{
		AdmissionResult: store.SlackIgnoredUntrusted,
		SourceEventID:   envelope.EventID,
	})

	log := output.String()
	for _, want := range []string{"WARN", "sender is not trusted", "slack_user_id=U12345678", "channel_id=C012ABC", "source_event_id=Ev-TOKEN_AND_MESSAGE_CANARY"} {
		if !strings.Contains(log, want) {
			t.Fatalf("log %q does not contain %q", log, want)
		}
	}
	if strings.Contains(log, envelope.Text) {
		t.Fatalf("log exposed Slack message text: %q", log)
	}

	output.Reset()
	s.logSlackAdmission(envelope, store.SlackEventResult{AdmissionResult: store.SlackAdmitted, SourceEventID: envelope.EventID})
	if output.Len() != 0 {
		t.Fatalf("admitted event produced warning: %q", output.String())
	}
}

func TestAutomaticUntrustedReactionIsDurableAndMessageIdempotent(t *testing.T) {
	ctx := context.Background()
	ledger, err := store.Initialize(ctx, filepath.Join(t.TempDir(), "mosscap.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close()

	envelope, _ := slackEnvelope("untrusted-reaction")
	permalinkRequest := slackapi.PermalinkRequest{ChannelID: envelope.ChannelID, MessageTS: envelope.MessageTS}
	reactionRequest := slackapi.ReactionRequest{ChannelID: envelope.ChannelID, MessageTS: envelope.MessageTS, Emoji: untrustedSenderReaction}
	web := fake.NewWeb(
		fake.Step{Operation: fake.Auth, Result: slackapi.AuthIdentity{TeamID: envelope.TeamID, BotUserID: "U012BOT"}},
		fake.Step{Operation: fake.Team, Result: slackapi.Team{ID: envelope.TeamID}},
		fake.Step{Operation: fake.Channel, Result: slackapi.Channel{ID: envelope.ChannelID, TeamID: envelope.TeamID, Member: true, Accessible: true}},
		fake.Step{Operation: fake.Scopes, Result: slackapi.RequiredBotScopes(false)},
		fake.Step{Operation: fake.Permalink, Request: permalinkRequest, Result: "https://example.invalid/message"},
		fake.Step{Operation: fake.AddReaction, Request: reactionRequest},
		fake.Step{Operation: fake.Permalink, Request: permalinkRequest, Result: "https://example.invalid/message"},
	)
	factory := &fakeRuntimeFactory{webs: []slackapi.Web{web}, sockets: []slackapi.Socket{fake.NewSocket(fake.SocketSession{})}}
	rt, err := slackruntime.New(slackruntime.Options{Factory: factory, Handler: func(context.Context, slackapi.Envelope) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	c := testConfig(config.Paths{})
	c.SlackAppToken, c.SlackBotToken, c.SlackChannelID = "xapp-test", "xoxb-test", envelope.ChannelID
	if err := rt.Start(ctx, c); err != nil {
		t.Fatal(err)
	}
	defer rt.Close()

	requestID := automaticUntrustedReactionRequestID(envelope.TeamID, envelope.ChannelID, string(envelope.MessageTS))
	intent := store.SlackEffectIntent{Kind: store.SlackEffectReact, TargetMessageTS: string(envelope.MessageTS), Emoji: untrustedSenderReaction}
	prepared, err := ledger.PrepareSlackEffect(ctx, requestID, store.SlackEffectTarget{TeamID: envelope.TeamID, ChannelID: envelope.ChannelID}, intent)
	if err != nil || prepared.State != store.SlackEffectPrepared || len(web.Calls()) != 4 {
		t.Fatalf("prepared effect=%+v calls=%+v err=%v", prepared, web.Calls(), err)
	}

	first, err := executeAutomaticUntrustedReaction(ctx, rt, ledger, envelope)
	if err != nil || first.ID != prepared.EffectID || first.State != store.SlackEffectConfirmed || first.Kind != store.SlackEffectReact || first.Emoji != untrustedSenderReaction {
		t.Fatalf("first effect=%+v err=%v", first, err)
	}
	second, err := executeAutomaticUntrustedReaction(ctx, rt, ledger, envelope)
	if err != nil || second.ID != first.ID || second.State != store.SlackEffectConfirmed {
		t.Fatalf("replayed effect=%+v err=%v, want confirmed %s", second, err, first.ID)
	}
	if calls := web.Calls(); len(calls) != 7 || calls[5].Operation != fake.AddReaction {
		t.Fatalf("Web calls=%+v", calls)
	}
	effects, err := ledger.ListSlackEffects(ctx, nil, 10)
	if err != nil || len(effects) != 1 || effects[0].ID != first.ID {
		t.Fatalf("effects=%+v err=%v", effects, err)
	}
}

func newTaskReplySource(t *testing.T, ledger *store.Store, id string) store.SlackEventResult {
	t.Helper()
	ctx := context.Background()
	id = strings.ReplaceAll(id, " ", "_")
	if _, err := ledger.AddTrustedUser(ctx, "req_trust_"+id, "U12345678"); err != nil {
		t.Fatal(err)
	}
	source, err := ledger.CommitSlackSourceEvent(ctx, store.SlackAdmissionPolicy{TeamID: "T012ABC", ChannelID: "C012ABC", BotUserID: "U012BOT"}, store.SlackSourceEvent{SourceEventID: "Ev-" + id, EventKind: store.SlackEventAppMention, TeamID: "T012ABC", ChannelID: "C012ABC", UserID: "U12345678", MessageTS: "1700000000.000001", ThreadTS: "1699999999.000001", Text: "do this <@U012BOT>", SlackEventAtUS: 1700000000000001})
	if err != nil {
		t.Fatal(err)
	}
	return source
}

func newTaskReplyServer(t *testing.T, ledger *store.Store, steps ...fake.Step) (*Server, *fake.Web) {
	t.Helper()
	verification := []fake.Step{
		{Operation: fake.Auth, Result: slackapi.AuthIdentity{TeamID: "T012ABC", BotUserID: "U012BOT"}},
		{Operation: fake.Team, Result: slackapi.Team{ID: "T012ABC"}},
		{Operation: fake.Channel, Result: slackapi.Channel{ID: "C012ABC", TeamID: "T012ABC", Member: true, Accessible: true}},
		{Operation: fake.Scopes, Result: slackapi.RequiredBotScopes(false)},
	}
	web := fake.NewWeb(append(verification, steps...)...)
	runtime, err := slackruntime.New(slackruntime.Options{Factory: &fakeRuntimeFactory{webs: []slackapi.Web{web}, sockets: []slackapi.Socket{fake.NewSocket(fake.SocketSession{})}}, Handler: func(context.Context, slackapi.Envelope) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	c := testConfig(config.Paths{})
	c.SlackAppToken, c.SlackBotToken, c.SlackChannelID = "xapp-test", "xoxb-test", "C012ABC"
	if err := runtime.Start(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	return &Server{store: ledger, runtime: runtime}, web
}

func reactionStep(operation fake.Operation, emoji string) []fake.Step {
	return []fake.Step{
		{Operation: fake.Permalink, Result: "https://example.invalid/message"},
		{Operation: operation, Request: slackapi.ReactionRequest{ChannelID: "C012ABC", MessageTS: "1700000000.000001", Emoji: emoji}},
	}
}

func reactionSteps(steps ...[]fake.Step) []fake.Step {
	var out []fake.Step
	for _, step := range steps {
		out = append(out, step...)
	}
	return out
}

// transitionAndProject commits a state change and then runs its lifecycle
// projection, as the task.transition dispatch does.
func transitionAndProject(t *testing.T, s *Server, ledger *store.Store, requestID, taskID string, state task.State) store.MutationResult {
	t.Helper()
	mutation, err := ledger.Transition(context.Background(), requestID, taskID, state, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.projectLatestTransition(context.Background(), taskID); err != nil {
		t.Fatalf("project %s: %v", state, err)
	}
	return mutation
}

func TestSlackTaskReactionLifecycleUsesDurableOrderedEffects(t *testing.T) {
	ctx := context.Background()
	ledger, err := store.Initialize(ctx, filepath.Join(t.TempDir(), "mosscap.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close()
	source := newTaskReplySource(t, ledger, "reaction-lifecycle")
	s, web := newTaskReplyServer(t, ledger, reactionSteps(
		reactionStep(fake.AddReaction, queuedTaskReaction),
		reactionStep(fake.AddReaction, workingTaskReaction),
		reactionStep(fake.RemoveReaction, queuedTaskReaction),
		reactionStep(fake.RemoveReaction, workingTaskReaction),
	)...)
	if err := s.queueTaskReaction(ctx, source.TaskID); err != nil {
		t.Fatal(err)
	}
	transitionAndProject(t, s, ledger, "reaction-lifecycle-working", source.TaskID, task.Working)
	transitionAndProject(t, s, ledger, "reaction-lifecycle-done", source.TaskID, task.Done)
	effects, err := ledger.ListSlackEffects(ctx, nil, 10)
	if err != nil || len(effects) != 4 {
		t.Fatalf("effects=%+v err=%v", effects, err)
	}
	for _, effect := range effects {
		if effect.State != store.SlackEffectConfirmed {
			t.Fatalf("effect not confirmed: %+v", effect)
		}
	}
	if err := s.queueTaskReaction(ctx, source.TaskID); err != nil {
		t.Fatal(err)
	}
	if err := s.projectLatestTransition(ctx, source.TaskID); err != nil {
		t.Fatal(err)
	}
	if len(web.Calls()) != 12 { // verification plus exactly four preflight/write pairs
		t.Fatalf("duplicate lifecycle action made a Slack call: %+v", web.Calls())
	}
}

func TestTaskReactionRecoveryDoesNotRemoveQueueWhenWorkingAddFails(t *testing.T) {
	ctx := context.Background()
	ledger, err := store.Initialize(ctx, filepath.Join(t.TempDir(), "mosscap.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close()
	source := newTaskReplySource(t, ledger, "reaction-recovery-order")
	failedAdd := reactionStep(fake.AddReaction, workingTaskReaction)
	failedAdd[1].Err = slackapi.NewExternalError(slackapi.ReasonRejected, slackapi.DiagnosticRejected, "", nil)
	s, web := newTaskReplyServer(t, ledger, reactionSteps(reactionStep(fake.AddReaction, queuedTaskReaction), failedAdd)...)
	if err = s.queueTaskReaction(ctx, source.TaskID); err != nil {
		t.Fatal(err)
	}
	// The transition commits, then the gateway stops before projecting it.
	if _, err = ledger.Transition(ctx, "recovery-working", source.TaskID, task.Working, nil); err != nil {
		t.Fatal(err)
	}
	if err = s.reconcileTaskReactions(ctx); err != nil {
		t.Fatal(err)
	}
	op, release, err := s.ops()
	if err != nil {
		t.Fatal(err)
	}
	_, err = op.Recover(ctx)
	release()
	if err != nil {
		t.Fatal(err)
	}
	if err = s.reconcileTaskReactions(ctx); err != nil {
		t.Fatal(err)
	}
	effects, err := ledger.ListSlackEffects(ctx, nil, 10)
	if err != nil || len(effects) != 2 || effects[1].State != store.SlackEffectFailed {
		t.Fatalf("effects=%+v err=%v", effects, err)
	}
	for _, effect := range effects {
		if effect.Kind == store.SlackEffectUnreact {
			t.Fatalf("queue removal prepared despite failed working add: %+v", effect)
		}
	}
	if len(web.Calls()) != 8 { // verification plus two preflight/write pairs
		t.Fatalf("unexpected recovery calls: %+v", web.Calls())
	}
}

func TestTaskReactionProjectionSkipsLocalAndUntransitionedTasks(t *testing.T) {
	ctx := context.Background()
	ledger, err := store.Initialize(ctx, filepath.Join(t.TempDir(), "mosscap.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close()
	source := newTaskReplySource(t, ledger, "reaction-skips")
	s := &Server{store: ledger}
	// A Slack edit changes the version but not the state, so it never moves
	// the lifecycle marker.
	if _, err := ledger.Update(ctx, "reaction-skips-update", source.TaskID, "new source text"); err != nil {
		t.Fatal(err)
	}
	if err := s.projectLatestTransition(ctx, source.TaskID); err != nil {
		t.Fatal(err)
	}
	local, err := ledger.Create(ctx, "reaction-skips-local", "local task")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Transition(ctx, "reaction-skips-local-working", local.TaskID, task.Working, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.projectLatestTransition(ctx, local.TaskID); err != nil {
		t.Fatalf("local lifecycle projection should be a silent no-op: %v", err)
	}
	effects, err := ledger.ListSlackEffects(ctx, nil, 10)
	if err != nil || len(effects) != 0 {
		t.Fatalf("edit and local projections must not prepare effects: %+v err=%v", effects, err)
	}
}

func TestReopenedSlackTaskRestoresQueuedReactionEachCycle(t *testing.T) {
	ctx := context.Background()
	ledger, err := store.Initialize(ctx, filepath.Join(t.TempDir(), "mosscap.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close()
	source := newTaskReplySource(t, ledger, "reaction-reopen")
	s, web := newTaskReplyServer(t, ledger, reactionSteps(
		reactionStep(fake.AddReaction, queuedTaskReaction),
		reactionStep(fake.AddReaction, workingTaskReaction),
		reactionStep(fake.RemoveReaction, queuedTaskReaction),
		reactionStep(fake.RemoveReaction, workingTaskReaction),
		reactionStep(fake.AddReaction, queuedTaskReaction),
		reactionStep(fake.RemoveReaction, queuedTaskReaction),
		reactionStep(fake.AddReaction, queuedTaskReaction),
	)...)
	if err := s.queueTaskReaction(ctx, source.TaskID); err != nil {
		t.Fatal(err)
	}
	transitionAndProject(t, s, ledger, "reopen-working", source.TaskID, task.Working)
	transitionAndProject(t, s, ledger, "reopen-done", source.TaskID, task.Done)
	transitionAndProject(t, s, ledger, "reopen-open", source.TaskID, task.Open)
	// A delayed duplicate projection must not change the reopened marker.
	if err := s.projectLatestTransition(ctx, source.TaskID); err != nil {
		t.Fatal(err)
	}
	if len(web.Calls()) != 14 {
		t.Fatalf("duplicate projection changed reopened marker: %+v", web.Calls())
	}
	transitionAndProject(t, s, ledger, "reopen-done-again", source.TaskID, task.Done)
	transitionAndProject(t, s, ledger, "reopen-open-again", source.TaskID, task.Open)
	effects, err := ledger.ListSlackEffects(ctx, nil, 10)
	if err != nil || len(effects) != 7 || effects[6].Emoji != queuedTaskReaction || effects[6].Kind != store.SlackEffectReact || effects[6].State != store.SlackEffectConfirmed {
		t.Fatalf("reopened task did not restore queued marker per cycle: %+v err=%v", effects, err)
	}
	if len(web.Calls()) != 18 { // verification plus seven preflight/write pairs
		t.Fatalf("each reopen did not issue a fresh queued reaction: %+v", web.Calls())
	}
}

func TestTaskReplyConfirmedSuccessReplaysAndConsumesReplyRequestID(t *testing.T) {
	ctx := context.Background()
	ledger, err := store.Initialize(ctx, filepath.Join(t.TempDir(), "mosscap.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close()
	source := newTaskReplySource(t, ledger, "reply-success")
	s, web := newTaskReplyServer(t, ledger,
		fake.Step{Operation: fake.Permalink, Result: "https://example.invalid/thread"},
		fake.Step{Operation: fake.PostMessage, Result: slackapi.MessageResult{ChannelID: "C012ABC", MessageTS: "1700000001.000001"}},
	)
	requestID := "req_reply_success"
	params := protocol.TaskReplyParams{Task: source.TaskID, Text: "completed"}
	out, err := s.taskReply(ctx, requestID, params)
	if err != nil || out.Effect.State != string(store.SlackEffectConfirmed) || out.Task != nil {
		t.Fatalf("reply=%+v err=%v", out, err)
	}
	params.Done = true
	out, err = s.taskReply(ctx, requestID, params)
	if err != nil || out.Task == nil || out.Task.State != "done" {
		t.Fatalf("done replay=%+v err=%v", out, err)
	}
	if _, err := s.taskReply(ctx, requestID, params); err != nil {
		t.Fatalf("done replay: %v", err)
	}
	posts := 0
	for _, call := range web.Calls() {
		if call.Operation == fake.PostMessage {
			posts++
		}
	}
	if posts != 1 {
		t.Fatalf("Slack post calls=%d, want one", posts)
	}
	if _, err := s.taskReply(ctx, requestID, protocol.TaskReplyParams{Task: source.TaskID, Text: "changed", Done: true}); !errors.Is(err, store.ErrRequestConflict) {
		t.Fatalf("changed reply error=%v, want request conflict", err)
	}
	for _, id := range []string{requestID, taskReplyDoneRequestID(requestID)} {
		var count int
		if err := ledger.DB().QueryRow("SELECT count(*) FROM mutation_requests WHERE request_id=?", id).Scan(&count); err != nil || count != 1 {
			t.Fatalf("mutation request %q count=%d err=%v", id, count, err)
		}
	}
}

func TestSlackTaskActionsDTOReportsDurableRecoveryStateWithoutReplyText(t *testing.T) {
	source := store.SlackSource{TeamID: "T012ABC", ChannelID: "C012ABC", ThreadTS: "1699999999.000001"}
	for _, tc := range []struct {
		name       string
		effect     *store.SlackEffect
		state      task.State
		reply      string
		completion string
	}{
		{"no reply open", nil, task.Open, "none", "not_done"},
		{"prepared", &store.SlackEffect{ID: "eff_prepared", State: store.SlackEffectPrepared}, task.Open, "prepared", "not_done"},
		{"submitted", &store.SlackEffect{ID: "eff_submitted", State: store.SlackEffectSubmitted}, task.Open, "submitted", "not_done"},
		{"confirmed recovery", &store.SlackEffect{ID: "eff_confirmed", State: store.SlackEffectConfirmed}, task.Working, "confirmed", "not_done"},
		{"failed open", &store.SlackEffect{ID: "eff_failed", State: store.SlackEffectFailed}, task.Open, "failed", "not_done"},
		{"ambiguous open", &store.SlackEffect{ID: "eff_ambiguous", State: store.SlackEffectAmbiguous}, task.Open, "ambiguous", "not_done"},
		{"done", &store.SlackEffect{ID: "eff_done", State: store.SlackEffectConfirmed}, task.Done, "confirmed", "done"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.effect != nil {
				tc.effect.Text = "secret reply payload"
			}
			got := slackTaskActionsDTO(source, tc.effect, tc.state)
			if got.ReplyStatus != tc.reply || got.CompletionStatus != tc.completion || got.ReplyTarget.TeamID != source.TeamID || got.ReplyTarget.ChannelID != source.ChannelID || got.ReplyTarget.ThreadTS != source.ThreadTS {
				t.Fatalf("actions = %+v", got)
			}
			if tc.effect != nil && got.ReplyEffectID != tc.effect.ID {
				t.Fatalf("effect ID = %q, want %q", got.ReplyEffectID, tc.effect.ID)
			}
			encoded, err := json.Marshal(got)
			if err != nil || bytes.Contains(encoded, []byte("Text")) || bytes.Contains(encoded, []byte("secret")) {
				t.Fatalf("actions JSON = %s, err=%v", encoded, err)
			}
		})
	}
}

func TestTaskReplyTerminalSlackOutcomesDoNotCloseTask(t *testing.T) {
	for _, tc := range []struct {
		name  string
		step  fake.Step
		state store.SlackEffectState
	}{
		{"conclusive failure", fake.Step{Operation: fake.PostMessage, Err: slackapi.NewExternalError(slackapi.ReasonRejected, slackapi.DiagnosticRejected, "", nil)}, store.SlackEffectFailed},
		{"ambiguous timeout", fake.Step{Operation: fake.PostMessage, PreCall: fake.CallScript{Fault: fake.Timeout}}, store.SlackEffectAmbiguous},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			ledger, err := store.Initialize(ctx, filepath.Join(t.TempDir(), "mosscap.db"), store.Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer ledger.Close()
			source := newTaskReplySource(t, ledger, "reply-"+tc.name)
			s, _ := newTaskReplyServer(t, ledger, fake.Step{Operation: fake.Permalink, Result: "https://example.invalid/thread"}, tc.step)
			if _, err := s.taskReply(ctx, "req_reply_"+strings.ReplaceAll(tc.name, " ", "_"), protocol.TaskReplyParams{Task: source.TaskID, Text: "completed", Done: true}); err == nil {
				t.Fatal("task reply unexpectedly succeeded")
			}
			effects, err := ledger.ListSlackEffects(ctx, nil, 10)
			if err != nil || len(effects) != 1 || effects[0].State != tc.state {
				t.Fatalf("effects=%+v err=%v", effects, err)
			}
			model, err := ledger.GetTaskReadModel(ctx, source.TaskID)
			if err != nil || model.Task.State != task.Open {
				t.Fatalf("task=%+v err=%v", model.Task, err)
			}
		})
	}
}

func TestTaskReplyRecoversConfirmedEffectWithoutGateway(t *testing.T) {
	ctx := context.Background()
	ledger, err := store.Initialize(ctx, filepath.Join(t.TempDir(), "mosscap.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close()
	source := newTaskReplySource(t, ledger, "reply-recovery")
	requestID := "req_reply_recovery"
	prepared, err := ledger.PrepareSlackEffect(ctx, requestID, store.SlackEffectTarget{TeamID: "T012ABC", ChannelID: "C012ABC"}, store.SlackEffectIntent{Kind: store.SlackEffectSend, ThreadTS: "1699999999.000001", Text: "done", RelatedTaskID: source.TaskID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.ClaimSlackEffect(ctx, prepared.EffectID); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.CommitSlackEffectOutcome(ctx, prepared.EffectID, store.SlackEffectOutcome{State: store.SlackEffectConfirmed, ResponseMessageTS: "1700000001.000001"}); err != nil {
		t.Fatal(err)
	}

	out, err := (&Server{store: ledger}).taskReply(ctx, requestID, protocol.TaskReplyParams{Task: source.TaskID, Text: "done", Done: true})
	if err != nil || out.Effect.State != string(store.SlackEffectConfirmed) || out.Task == nil || out.Task.State != "done" {
		t.Fatalf("reply=%+v err=%v", out, err)
	}
	model, err := ledger.GetTaskReadModel(ctx, source.TaskID)
	if err != nil || string(model.Task.State) != "done" {
		t.Fatalf("task=%+v err=%v", model.Task, err)
	}
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

// TestSlackReloadRetainsFailedCandidateThenSwapsAndDrains verifies that reload
// failure is atomic, successful replacement starts before publication, replay
// is deduplicated, and the previous Socket runtime drains afterward.
func TestSlackReloadRetainsFailedCandidateThenSwapsAndDrains(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp(home, ".mosscap-slack-reload-")
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
		"slack_app_token": "xapp-test", "slack_bot_token": "xoxb-test", "slack_channel_id": "C012ABC",
	} {
		set := config.Set
		if key == "slack_app_token" || key == "slack_bot_token" {
			set = config.SetSecret
		}
		if _, err := set(loc, key, value); err != nil {
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
	failedEnvelope, failedAck := slackEnvelope("failed")
	failedSocket := fake.NewSocket(fake.SocketSession{Receives: []fake.ReceiveStep{{Envelope: failedEnvelope}}})
	successEnvelope, successAck := slackEnvelope("success")
	newSocket := fake.NewSocket(fake.SocketSession{Receives: []fake.ReceiveStep{{Envelope: successEnvelope}}})
	replayEnvelope, replayAck := slackEnvelope("replay")
	replaySocket := fake.NewSocket(fake.SocketSession{Receives: []fake.ReceiveStep{{Envelope: replayEnvelope}}})
	factory := &fakeRuntimeFactory{
		webs:    []slackapi.Web{verifiedSlackWeb(), fake.NewWeb(fake.Step{Operation: fake.Auth, Err: slackapi.NewVerificationError(slackapi.DiagnosticChannelNotFound)}), verifiedSlackWeb(), verifiedSlackWeb()},
		sockets: []slackapi.Socket{oldSocket, failedSocket, newSocket, replaySocket},
	}
	s := NewWithOptions(initial, loc, "test", nil, Options{Factory: factory})
	s.store = ledger
	if err := s.startSlack(context.Background(), initial); err != nil {
		t.Fatal(err)
	}
	status, err := s.slackStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.TeamID != "T012ABC" || status.BotUserID != "U012BOT" || status.ChannelID != "C012ABC" || status.IdentitySource != "auth_test" {
		t.Fatalf("runtime identity status = %#v", status)
	}
	t.Cleanup(func() { s.closeSlack(); _ = ledger.Close() })
	oldRuntime := s.runtime
	if _, err := config.Set(loc, "maximum_file_size", "200MiB"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.reload(context.Background(), "slack-reload-failed"); err == nil || !strings.Contains(err.Error(), "channel_not_found") || !strings.Contains(err.Error(), "invite the bot") {
		t.Fatalf("unhelpful candidate failure: %v", err)
	}
	if s.runtime != oldRuntime || s.snapshot().MaximumFileSize != initial.MaximumFileSize {
		t.Fatal("failed candidate changed published runtime or config")
	}
	if failedAck.Calls() != 0 || len(failedSocket.OpenedStreams()) != 0 {
		t.Fatal("failed candidate handled an envelope")
	}
	failedStatus, err := s.slackStatus(context.Background())
	if err != nil || failedStatus.LastVerificationDiagnosticCode != string(slackapi.DiagnosticChannelNotFound) || !failedStatus.PriorRuntimeRetained {
		t.Fatalf("failed reload status=%#v err=%v", failedStatus, err)
	}
	checks := s.slackDoctor(context.Background())
	if len(checks) != 1 || checks[0].Status != "warn" || !strings.Contains(checks[0].Message, "prior runtime remains active") || !strings.Contains(checks[0].Action, "invite the bot") {
		t.Fatalf("failed reload doctor=%#v", checks)
	}
	result, err := s.reload(context.Background(), "slack-reload-success")
	if err != nil || !result.Activated || result.SlackNetworkVerificationDeferred {
		t.Fatalf("reload result=%+v err=%v", result, err)
	}
	if s.runtime == oldRuntime || s.snapshot().MaximumFileSize != 200<<20 {
		t.Fatal("successful candidate was not atomically published")
	}
	successStatus, err := s.slackStatus(context.Background())
	if err != nil || successStatus.LastVerificationDiagnosticCode != "" || successStatus.PriorRuntimeRetained {
		t.Fatalf("successful reload status=%#v err=%v", successStatus, err)
	}
	waitForAck(t, successAck)
	if _, err := s.reload(context.Background(), "slack-reload-success"); err != nil {
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
	if _, err := config.SetSecret(loc, "slack_app_token", "xapp-TOKEN_CANARY"); err != nil {
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

func TestFileTransferStatusUsesDescriptorChecksForTemporarySafety(t *testing.T) {
	tests := []struct {
		name       string
		makeUnsafe func(t *testing.T, tmp *fileio.TempFile)
		wantUnsafe int64
	}{
		{name: "private regular file", wantUnsafe: 0},
		{name: "hard link", wantUnsafe: 1, makeUnsafe: func(t *testing.T, tmp *fileio.TempFile) {
			t.Helper()
			link := filepath.Join("/home/admin", "mosscap-temporary-status-link-"+filepath.Base(tmp.Path))
			t.Cleanup(func() { _ = os.Remove(link) })
			if err := os.Link(tmp.Path, link); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "symlink", wantUnsafe: 1, makeUnsafe: func(t *testing.T, tmp *fileio.TempFile) {
			t.Helper()
			target := filepath.Join(filepath.Dir(filepath.Dir(tmp.Path)), "replacement")
			if err := os.WriteFile(target, []byte("replacement"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(tmp.Path); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, tmp.Path); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			root, err := os.MkdirTemp("/home/admin", "mosscap-temporary-status-")
			if err != nil {
				t.Skipf("cannot create private test tree: %v", err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(root) })
			if err := os.Chmod(root, 0700); err != nil {
				t.Fatal(err)
			}
			temporaryRoot := filepath.Join(root, "tmp")
			if err := os.Mkdir(temporaryRoot, 0700); err != nil {
				t.Fatal(err)
			}
			temporary, err := fileio.OpenTempDir(temporaryRoot)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = temporary.Close() })
			tmp, err := temporary.Create()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := tmp.File.Write([]byte("private")); err != nil {
				t.Fatal(err)
			}
			if err := tmp.File.Close(); err != nil {
				t.Fatal(err)
			}
			if tt.makeUnsafe != nil {
				tt.makeUnsafe(t, tmp)
			}

			db, err := store.Initialize(ctx, filepath.Join(root, "ledger.sqlite"), store.Options{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			task, err := db.Create(ctx, "temporary-status-task", "temporary status fixture")
			if err != nil {
				t.Fatal(err)
			}
			const fileID = "fil_abcdefghijklmnopqrstuv"
			now := time.Now().UnixMicro()
			if _, err := db.DB().ExecContext(ctx, `INSERT INTO files(file_id,direction,slack_file_id,original_name,related_task_id,source_attached,transfer_state,created_at_us,updated_at_us) VALUES(?,'from_slack','F12345678','fixture',?,1,'metadata_only',?,?)`, fileID, task.TaskID, now, now); err != nil {
				t.Fatal(err)
			}
			if err := db.InsertFileTemporaryTransfer(ctx, store.FileTemporaryTransfer{TransferID: "xfr_abcdefghijklmnopqrstuv", FileID: fileID, GeneratedName: tmp.Name, SizeBytes: 7, SHA256: strings.Repeat("a", 64), CreatedAt: time.UnixMicro(now), ExpiresAt: time.UnixMicro(now + 1_000_000)}); err != nil {
				t.Fatal(err)
			}

			s := &Server{store: db, temporaryDir: temporary, fileTransfer: &filetransfer.Service{}}
			inventory, err := s.fileTransferStatus(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if inventory.UnsafeTemporaryEntries != tt.wantUnsafe {
				t.Fatalf("unsafe temporary entries = %d, want %d", inventory.UnsafeTemporaryEntries, tt.wantUnsafe)
			}
		})
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

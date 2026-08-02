package slackops

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stayradiated/mosscap/internal/fileio"
	"github.com/stayradiated/mosscap/internal/slackapi"
	"github.com/stayradiated/mosscap/internal/slackapi/fake"
	"github.com/stayradiated/mosscap/internal/store"
)

var testTarget = store.SlackEffectTarget{TeamID: "T12345678", ChannelID: "C12345678"}

type cancelAfterPrepareStore struct {
	EffectStore
	cancel func()
}

func (s cancelAfterPrepareStore) GetSlackEffect(ctx context.Context, selector string) (store.SlackEffect, error) {
	effect, err := s.EffectStore.GetSlackEffect(ctx, selector)
	s.cancel()
	return effect, err
}

func testService(t *testing.T, web slackapi.Web) (*Service, *store.Store) {
	t.Helper()
	s, err := store.Initialize(context.Background(), filepath.Join(t.TempDir(), "ledger.sqlite"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return New(s, web, testTarget), s
}

func TestReadsUseConfiguredChannelAndBounds(t *testing.T) {
	web := fake.NewWeb(
		fake.Step{Operation: fake.History, Request: slackapi.HistoryRequest{ChannelID: testTarget.ChannelID, Limit: 50}},
		fake.Step{Operation: fake.Replies, Request: slackapi.RepliesRequest{ChannelID: testTarget.ChannelID, ThreadTS: "1700000000.000001", Limit: 100}},
	)
	svc, _ := testService(t, web)
	if _, err := svc.History(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Thread(context.Background(), "1700000000.000001"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.History(context.Background(), 101); err == nil {
		t.Fatal("accepted oversized limit")
	}
	if err := web.AssertDone(); err != nil {
		t.Fatal(err)
	}
}

func TestExecuteMappingsAndSubmittedBeforeWrite(t *testing.T) {
	barrier := fake.NewBarrier()
	web := fake.NewWeb(
		fake.Step{Operation: fake.PostMessage, PostCall: fake.CallScript{Barrier: barrier}, Result: slackapi.MessageResult{ChannelID: testTarget.ChannelID, MessageTS: "1700000001.000001"}},
		fake.Step{Operation: fake.Permalink}, fake.Step{Operation: fake.UpdateMessage, Result: slackapi.MessageResult{ChannelID: testTarget.ChannelID, MessageTS: "1700000002.000001"}},
		fake.Step{Operation: fake.Permalink}, fake.Step{Operation: fake.DeleteMessage},
		fake.Step{Operation: fake.Permalink}, fake.Step{Operation: fake.AddReaction},
		fake.Step{Operation: fake.Permalink}, fake.Step{Operation: fake.RemoveReaction},
	)
	svc, ledger := testService(t, web)
	result := make(chan error, 1)
	go func() {
		_, err := svc.Execute(context.Background(), "send-request", store.SlackEffectIntent{Kind: store.SlackEffectSend, Text: "hello"})
		result <- err
	}()
	select {
	case <-barrier.Reached():
	case <-time.After(time.Second):
		t.Fatal("write barrier not reached")
	}
	effects, err := ledger.ListSlackEffects(context.Background(), nil, 10)
	if err != nil || len(effects) != 1 || effects[0].State != store.SlackEffectSubmitted {
		t.Fatalf("effect before write = %#v, %v", effects, err)
	}
	barrier.Release()
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		id string
		in store.SlackEffectIntent
	}{
		{"edit-request", store.SlackEffectIntent{Kind: store.SlackEffectEdit, TargetMessageTS: "1700000002.000001", Text: "new"}},
		{"delete-request", store.SlackEffectIntent{Kind: store.SlackEffectDelete, TargetMessageTS: "1700000002.000001", OperatorReason: "cleanup"}},
		{"react-request", store.SlackEffectIntent{Kind: store.SlackEffectReact, TargetMessageTS: "1700000002.000001", Emoji: ":eyes:"}},
		{"unreact-request", store.SlackEffectIntent{Kind: store.SlackEffectUnreact, TargetMessageTS: "1700000002.000001", Emoji: "eyes"}},
	} {
		if _, err := svc.Execute(context.Background(), tc.id, tc.in); err != nil {
			t.Fatalf("%s: %v", tc.id, err)
		}
	}
	calls := web.Calls()
	if len(calls) != 9 {
		t.Fatalf("calls=%#v", calls)
	}
	if _, ok := calls[0].Request.(slackapi.PostMessageRequest); !ok {
		t.Fatalf("send request=%T", calls[0].Request)
	}
	if got := calls[2].Request.(slackapi.UpdateMessageRequest); got.ChannelID != testTarget.ChannelID || got.Text != "new" {
		t.Fatalf("edit=%#v", got)
	}
	if got := calls[4].Request.(slackapi.DeleteMessageRequest); got.ChannelID != testTarget.ChannelID {
		t.Fatalf("delete=%#v", got)
	}
	if got := calls[6].Request.(slackapi.ReactionRequest); got.Emoji != "eyes" {
		t.Fatalf("react=%#v", got)
	}
	if got := calls[8].Request.(slackapi.ReactionRequest); got.Emoji != "eyes" {
		t.Fatalf("unreact=%#v", got)
	}
	if err := web.AssertDone(); err != nil {
		t.Fatal(err)
	}
}

func TestFileUploadPreparesOrderedIntentBeforeSlackAndConfirms(t *testing.T) {
	ledger, err := store.Initialize(context.Background(), filepath.Join(t.TempDir(), "ledger.sqlite"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ledger.Close() })
	dir := t.TempDir()
	one, two := filepath.Join(dir, "one.txt"), filepath.Join(dir, "two.bin")
	if err := os.WriteFile(one, []byte("one"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(two, []byte{0, 1, 2}, 0644); err != nil {
		t.Fatal(err)
	}
	barrier := fake.NewBarrier()
	transfer := fake.NewFileTransfer(
		fake.TransferStep{Operation: fake.ReserveUpload, Result: slackapi.UploadedFile{ID: "F12345678"}, PostCall: fake.CallScript{Barrier: barrier}},
		fake.TransferStep{Operation: fake.ReserveUpload, Result: slackapi.UploadedFile{ID: "F23456789"}},
		fake.TransferStep{Operation: fake.StageUpload, Bytes: []byte("one")},
		fake.TransferStep{Operation: fake.StageUpload, Bytes: []byte{0, 1, 2}},
		fake.TransferStep{Operation: fake.ValidateUpload}, fake.TransferStep{Operation: fake.ValidateUpload},
		fake.TransferStep{Operation: fake.CompleteUpload, Result: slackapi.UploadResult{Files: []slackapi.UploadedFile{{ID: "F12345678"}, {ID: "F23456789"}}}},
	)
	svc := NewWithFileTransfer(ledger, nil, transfer, testTarget, 1024, 2)
	result := make(chan struct {
		effect store.SlackEffect
		err    error
	}, 1)
	go func() {
		effect, err := svc.ExecuteFileUpload(context.Background(), "file-upload", store.SlackFileUploadIntent{Text: "comment", Files: []store.SlackUploadFile{{SourcePath: one}, {SourcePath: two}}})
		result <- struct {
			effect store.SlackEffect
			err    error
		}{effect, err}
	}()
	select {
	case <-barrier.Reached():
	case <-time.After(time.Second):
		t.Fatal("upload reservation was not reached")
	}
	effects, err := ledger.ListSlackEffects(context.Background(), nil, 10)
	if err != nil || len(effects) != 1 || effects[0].Kind != store.SlackEffectFileUpload || effects[0].State != store.SlackEffectSubmitted {
		t.Fatalf("effect before first exchange = %#v, %v", effects, err)
	}
	var count int
	if err := ledger.DB().QueryRow(`SELECT count(*) FROM files WHERE related_effect_id=? AND direction='to_slack'`, effects[0].ID).Scan(&count); err != nil || count != 2 {
		t.Fatalf("ordered file intent = %d, %v", count, err)
	}
	barrier.Release()
	got := <-result
	if got.err != nil || got.effect.State != store.SlackEffectConfirmed {
		t.Fatalf("upload = %#v, %v", got.effect, got.err)
	}
	if err := transfer.AssertDone(); err != nil {
		t.Fatal(err)
	}
}

func TestFileUploadChangedAfterStagingFailsWithoutCompletion(t *testing.T) {
	ledger, err := store.Initialize(context.Background(), filepath.Join(t.TempDir(), "ledger.sqlite"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ledger.Close() })
	path := filepath.Join(t.TempDir(), "source.txt")
	if err := os.WriteFile(path, []byte("before"), 0644); err != nil {
		t.Fatal(err)
	}
	barrier := fake.NewBarrier()
	transfer := fake.NewFileTransfer(
		fake.TransferStep{Operation: fake.ReserveUpload, Result: slackapi.UploadedFile{ID: "F12345678"}},
		fake.TransferStep{Operation: fake.StageUpload, Bytes: []byte("before"), PostCall: fake.CallScript{Barrier: barrier}},
		fake.TransferStep{Operation: fake.ValidateUpload},
		fake.TransferStep{Operation: fake.CompleteUpload, Result: slackapi.UploadResult{Files: []slackapi.UploadedFile{{ID: "F12345678"}}}},
	)
	svc := NewWithFileTransfer(ledger, nil, transfer, testTarget, 1024, 1)
	result := make(chan struct {
		effect store.SlackEffect
		err    error
	}, 1)
	go func() {
		effect, err := svc.ExecuteFileUpload(context.Background(), "changed-upload", store.SlackFileUploadIntent{Files: []store.SlackUploadFile{{SourcePath: path}}})
		result <- struct {
			effect store.SlackEffect
			err    error
		}{effect, err}
	}()
	select {
	case <-barrier.Reached():
	case <-time.After(time.Second):
		t.Fatal("upload staging was not reached")
	}
	if err := os.WriteFile(path, []byte("changed"), 0644); err != nil {
		t.Fatal(err)
	}
	barrier.Release()
	got := <-result
	if got.err == nil || got.effect.State != store.SlackEffectFailed || got.effect.DiagnosticCode != store.SlackEffectDiagnosticFileChanged {
		t.Fatalf("changed upload = %#v, %v", got.effect, got.err)
	}
	if calls := transfer.Calls(); len(calls) != 3 || calls[len(calls)-1].Operation != fake.ValidateUpload {
		t.Fatalf("changed upload completed Slack call sequence: %#v", calls)
	}
	if err := transfer.AssertDone(); err == nil || !strings.Contains(err.Error(), "scripted calls remain") {
		t.Fatalf("completion should remain uncalled: %v", err)
	}
}

func TestRecoverPreparedUploadMismatchFailsWithoutSlackCall(t *testing.T) {
	ledger, err := store.Initialize(context.Background(), filepath.Join(t.TempDir(), "ledger.sqlite"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ledger.Close() })
	path := filepath.Join(t.TempDir(), "source.txt")
	if err := os.WriteFile(path, []byte("prepared"), 0644); err != nil {
		t.Fatal(err)
	}
	source, err := fileio.OpenSource(path, 1024)
	if err != nil {
		t.Fatal(err)
	}
	device, inode, mtime := int64(source.Device()), int64(source.Inode()), source.MtimeNS()
	intent := store.SlackFileUploadIntent{Files: []store.SlackUploadFile{{SourcePath: path, OriginalName: "source.txt", SizeBytes: source.Size, SHA256: source.SHA256, SourceDevice: &device, SourceInode: &inode, SourceMTimeNS: &mtime}}}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	prepared, err := ledger.PrepareSlackFileUpload(context.Background(), "recovery-mismatch", testTarget, intent)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("changed!"), 0644); err != nil {
		t.Fatal(err)
	}
	transfer := fake.NewFileTransfer()
	svc := NewWithFileTransfer(ledger, fake.NewWeb(), transfer, testTarget, 1024, 1)
	if _, err := svc.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	effect, err := ledger.GetSlackEffect(context.Background(), prepared.EffectID)
	if err != nil || effect.State != store.SlackEffectFailed || effect.AttemptCount != 0 || effect.DiagnosticCode != store.SlackEffectDiagnosticFileChanged {
		t.Fatalf("recovered mismatch = %#v, %v", effect, err)
	}
	if calls := transfer.Calls(); len(calls) != 0 {
		t.Fatalf("recovered mismatch called Slack: %#v", calls)
	}
}

func TestReplayAndOutcomesDoNotDuplicateWrites(t *testing.T) {
	web := fake.NewWeb(
		fake.Step{Operation: fake.PostMessage, Result: slackapi.MessageResult{ChannelID: testTarget.ChannelID, MessageTS: "1700000001.000001"}},
		fake.Step{Operation: fake.PostMessage, PreCall: fake.CallScript{Fault: fake.Timeout}},
	)
	svc, ledger := testService(t, web)
	in := store.SlackEffectIntent{Kind: store.SlackEffectSend, Text: "hello"}
	if _, err := svc.Execute(context.Background(), "replay", in); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Execute(context.Background(), "replay", in); err != nil {
		t.Fatal(err)
	}
	if got := len(web.Calls()); got != 1 {
		t.Fatalf("replay writes=%d", got)
	}
	// Cancellation after prepare but before Claim leaves durable work prepared.
	ctx, cancel := context.WithCancel(context.Background())
	preSvc := New(cancelAfterPrepareStore{EffectStore: ledger, cancel: cancel}, web, testTarget)
	_, err := preSvc.Execute(ctx, "pre-timeout", store.SlackEffectIntent{Kind: store.SlackEffectSend, Text: "later"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-submit error=%v", err)
	}
	state := store.SlackEffectPrepared
	prepared, err := ledger.ListSlackEffects(context.Background(), &state, 10)
	if err != nil || len(prepared) != 1 {
		t.Fatalf("prepared effects=%#v %v", prepared, err)
	}
	// The fake pre-call timeout occurs after the durable claim, hence uncertainty.
	post, err := svc.Execute(context.Background(), "post-timeout", store.SlackEffectIntent{Kind: store.SlackEffectSend, Text: "maybe"})
	if err == nil || post.State != store.SlackEffectAmbiguous {
		t.Fatalf("pre-call result=%#v %v", post, err)
	}
	if got := len(web.Calls()); got != 1 {
		t.Fatalf("pre-call was observed as write: %d", got)
	}
	if _, err := svc.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := len(web.Calls()); got != 1 {
		t.Fatalf("recovery retried ambiguous write: %d", got)
	}

	postWeb := fake.NewWeb(fake.Step{Operation: fake.PostMessage, PostCall: fake.CallScript{Fault: fake.Timeout}})
	postSvc, _ := testService(t, postWeb)
	post, err = postSvc.Execute(context.Background(), "after-timeout", store.SlackEffectIntent{Kind: store.SlackEffectSend, Text: "maybe later"})
	if err == nil || post.State != store.SlackEffectAmbiguous {
		t.Fatalf("post-call result=%#v %v", post, err)
	}
	if got := len(postWeb.Calls()); got != 1 {
		t.Fatalf("post-call writes=%d", got)
	}
	if _, err := postSvc.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := len(postWeb.Calls()); got != 1 {
		t.Fatalf("post recovery retried ambiguous write: %d", got)
	}
}

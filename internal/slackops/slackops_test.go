package slackops

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

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

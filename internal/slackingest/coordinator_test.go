package slackingest

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stayradiated/mosscap/internal/slackapi"
	"github.com/stayradiated/mosscap/internal/slackapi/fake"
	"github.com/stayradiated/mosscap/internal/store"
)

var policy = store.SlackAdmissionPolicy{TeamID: "T12345678", ChannelID: "C12345678", BotUserID: "U99999999"}

type fixedClock struct{ at time.Time }

func (c fixedClock) Now() time.Time { return c.at }

type commitCall struct {
	policy store.SlackAdmissionPolicy
	event  store.SlackSourceEvent
}
type fakeStore struct {
	mu     sync.Mutex
	calls  []commitCall
	result store.SlackEventResult
	err    error
}

func (s *fakeStore) CommitSlackSourceEvent(_ context.Context, p store.SlackAdmissionPolicy, e store.SlackSourceEvent) (store.SlackEventResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, commitCall{p, e})
	if s.err != nil {
		return store.SlackEventResult{}, s.err
	}
	r := s.result
	r.SourceEventID = e.SourceEventID
	return r, nil
}
func (s *fakeStore) call(t *testing.T) commitCall {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.calls) != 1 {
		t.Fatalf("calls=%d", len(s.calls))
	}
	return s.calls[0]
}

func envelope(id string) (slackapi.Envelope, *fake.AckRecorder) {
	ack, recorder := fake.NewAckRecorder(nil)
	return slackapi.Envelope{EnvelopeID: "env-" + id, EventID: "Ev-" + id, Kind: slackapi.EventAppMention, TeamID: policy.TeamID, ChannelID: policy.ChannelID, UserID: "U12345678", MessageTS: "1700000000.000001", EventTS: "1700000001.000001", Text: "do this <@U99999999>", Ack: ack}, recorder
}

func TestHandleCommitsBeforeOneShotAcknowledgementAndNormalizes(t *testing.T) {
	e, recorder := envelope("eligible")
	s := &fakeStore{result: store.SlackEventResult{AdmissionResult: store.SlackAdmitted, CommittedAt: time.Unix(1700000002, 0).UTC()}}
	c := New(s, policy, fixedClock{time.Unix(1700000000, 0).UTC()})
	result, err := c.Handle(context.Background(), e)
	if err != nil || result.AdmissionResult != store.SlackAdmitted {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if recorder.Calls() != 1 {
		t.Fatalf("ack count=%d", recorder.Calls())
	}
	call := s.call(t)
	if call.policy != policy || call.event.SourceEventID != e.EventID || call.event.ThreadTS != string(e.MessageTS) || call.event.ReceivedAt != time.Unix(1700000000, 0).UTC() || call.event.SlackEventAtUS != 1700000001000001 {
		t.Fatalf("call=%+v", call)
	}
	obs := c.Observations()
	if obs.LastReceivedAt != call.event.ReceivedAt || obs.LastCommittedAt != result.CommittedAt || obs.Results[store.SlackAdmitted] != 1 {
		t.Fatalf("observations=%+v", obs)
	}
}

func TestHandleAcknowledgesEveryCommittedAdmissionResult(t *testing.T) {
	results := []store.SlackAdmissionResult{store.SlackAdmitted, store.SlackUpdated, store.SlackDeleted, store.SlackDuplicate, store.SlackIgnoredWorkspace, store.SlackIgnoredChannel, store.SlackIgnoredUntrusted, store.SlackIgnoredUnmentioned, store.SlackIgnoredBot, store.SlackIgnoredStale, store.SlackIgnoredUntracked, store.SlackMalformed}
	for _, want := range results {
		t.Run(string(want), func(t *testing.T) {
			e, recorder := envelope(string(want))
			e.Kind = slackapi.EventMessage
			s := &fakeStore{result: store.SlackEventResult{AdmissionResult: want}}
			got, err := New(s, policy, fixedClock{}).Handle(context.Background(), e)
			if err != nil || got.AdmissionResult != want || recorder.Calls() != 1 {
				t.Fatalf("result=%+v err=%v acks=%d", got, err, recorder.Calls())
			}
		})
	}
}

func TestHandleUsesReturnedEmptyAndLiveTrustResults(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result store.SlackAdmissionResult
	}{{"empty trust", store.SlackIgnoredUntrusted}, {"live trust", store.SlackAdmitted}} {
		t.Run(tc.name, func(t *testing.T) {
			e, ack := envelope("trust")
			s := &fakeStore{result: store.SlackEventResult{AdmissionResult: tc.result}}
			got, err := New(s, policy, fixedClock{}).Handle(context.Background(), e)
			if err != nil || got.AdmissionResult != tc.result || ack.Calls() != 1 {
				t.Fatalf("got=%+v err=%v acks=%d", got, err, ack.Calls())
			}
		})
	}
}

func TestHandleRequiresBeforeAcknowledgeHookToSucceed(t *testing.T) {
	e, recorder := envelope("before-ack")
	s := &fakeStore{result: store.SlackEventResult{AdmissionResult: store.SlackIgnoredUntrusted}}
	boom := errors.New("effect intent unavailable")
	var hookedEvent store.SlackSourceEvent
	c := NewWithOptions(s, policy, fixedClock{}, Options{BeforeAcknowledge: func(_ context.Context, event store.SlackSourceEvent, result store.SlackEventResult) error {
		hookedEvent = event
		if result.AdmissionResult != store.SlackIgnoredUntrusted {
			t.Fatalf("hook result=%+v", result)
		}
		return boom
	}})
	result, err := c.Handle(context.Background(), e)
	if !errors.Is(err, boom) || result.AdmissionResult != store.SlackIgnoredUntrusted || recorder.Calls() != 0 {
		t.Fatalf("result=%+v err=%v acks=%d", result, err, recorder.Calls())
	}
	if hookedEvent.SourceEventID != e.EventID || hookedEvent.Text != e.Text {
		t.Fatalf("hook event=%+v", hookedEvent)
	}
	if got := c.Observations().LastDiagnosticCode; got != "before_ack_failed" {
		t.Fatalf("diagnostic=%q", got)
	}
}

func TestHandleDoesNotAcknowledgeStoreOrCanonicalFailure(t *testing.T) {
	e, recorder := envelope("store-error")
	boom := errors.New("database unavailable")
	s := &fakeStore{err: boom}
	c := New(s, policy, fixedClock{})
	if _, err := c.Handle(context.Background(), e); !errors.Is(err, boom) || recorder.Calls() != 0 || c.Observations().LastDiagnosticCode != "commit_failed" {
		t.Fatalf("err=%v acks=%d observations=%+v", err, recorder.Calls(), c.Observations())
	}
	e, recorder = envelope("bad-result")
	s = &fakeStore{result: store.SlackEventResult{AdmissionResult: "unknown"}}
	if _, err := New(s, policy, fixedClock{}).Handle(context.Background(), e); err == nil || recorder.Calls() != 0 {
		t.Fatalf("acks=%d", recorder.Calls())
	}
}

func TestHandleCommitsIdentifiableMalformedButLeavesUnidentifiableInput(t *testing.T) {
	ack, recorder := fake.NewAckRecorder(nil)
	malformed := slackapi.Envelope{EnvelopeID: "env-malformed", Ack: ack}
	s := &fakeStore{result: store.SlackEventResult{AdmissionResult: store.SlackMalformed}}
	if got, err := New(s, policy, fixedClock{}).Handle(context.Background(), malformed); err != nil || got.AdmissionResult != store.SlackMalformed || recorder.Calls() != 1 {
		t.Fatalf("got=%+v err=%v acks=%d", got, err, recorder.Calls())
	}
	if call := s.call(t); call.event.SourceEventID != "malformed:ee92913b39c194158c2d980180ad1a2736d3c4a375400781f1ee4dc35c4c2b66" {
		t.Fatalf("event=%+v", call.event)
	}
	noAck := slackapi.Envelope{EnvelopeID: "\x00", Ack: ack}
	if got, err := New(s, policy, fixedClock{}).Handle(context.Background(), noAck); err != nil || got.SourceEventID != "" || recorder.Calls() != 1 {
		t.Fatalf("got=%+v err=%v acks=%d", got, err, recorder.Calls())
	}
}

func TestNormalizePreservesBoundedMetadataAndThreadIdentity(t *testing.T) {
	e, _ := envelope("thread")
	e.Kind, e.ThreadTS = slackapi.EventMessageChange, "1699999999.000001"
	size := int64(7)
	e.Files = []slackapi.File{{ID: "F12345678", Name: "a.txt", MIMEType: "text/plain", Permalink: "https://example.invalid/file", SizeBytes: &size}}
	s := &fakeStore{result: store.SlackEventResult{AdmissionResult: store.SlackUpdated}}
	if _, err := New(s, policy, fixedClock{}).Handle(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	call := s.call(t)
	if call.event.ThreadTS != string(e.ThreadTS) || len(call.event.Files) != 1 || call.event.Files[0].SizeBytes == e.Files[0].SizeBytes || *call.event.Files[0].SizeBytes != size {
		t.Fatalf("event=%+v", call.event)
	}
}

func TestHandleRedeliveryAndSourceLifecycleResultsAreOneCommitOneAck(t *testing.T) {
	for _, tc := range []struct {
		name   string
		kind   slackapi.EventKind
		result store.SlackAdmissionResult
	}{
		{"redelivery", slackapi.EventAppMention, store.SlackDuplicate}, {"stale", slackapi.EventMessageChange, store.SlackIgnoredStale}, {"noop edit", slackapi.EventMessageChange, store.SlackUpdated}, {"meaningful edit", slackapi.EventMessageChange, store.SlackUpdated}, {"edit first mention", slackapi.EventMessageChange, store.SlackAdmitted}, {"deletion", slackapi.EventMessageDelete, store.SlackDeleted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, ack := envelope("lifecycle")
			e.EventID = "Ev-" + string(tc.result)
			e.EnvelopeID = "env-" + string(tc.result)
			e.Kind = tc.kind
			s := &fakeStore{result: store.SlackEventResult{AdmissionResult: tc.result}}
			if got, err := New(s, policy, fixedClock{}).Handle(context.Background(), e); err != nil || got.AdmissionResult != tc.result || ack.Calls() != 1 {
				t.Fatalf("got=%+v err=%v ack=%d", got, err, ack.Calls())
			}
			s.call(t)
		})
	}
}

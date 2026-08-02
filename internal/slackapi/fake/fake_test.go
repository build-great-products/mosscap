package fake

import (
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stayradiated/mosscap/internal/slackapi"
)

func TestWebRunsStrictScriptAndRecordsCalls(t *testing.T) {
	historyRequest := slackapi.HistoryRequest{ChannelID: "C012ABC", Limit: 20}
	page := slackapi.MessagePage{Messages: []slackapi.Message{{MessageTS: "1712345678.000001", ThreadTS: "1712345678.000001", AuthorID: "U012ABC", Text: "bounded read text"}}}
	postRequest := slackapi.PostMessageRequest{ChannelID: "C012ABC", Text: "exact intent", ClientMessageID: "eff_123"}
	postResult := slackapi.MessageResult{ChannelID: "C012ABC", MessageTS: "1712345679.000001"}
	web := NewWeb(
		Step{Operation: History, Request: historyRequest, Result: page},
		Step{Operation: PostMessage, Request: postRequest, Result: postResult},
	)

	gotPage, err := web.History(context.Background(), historyRequest)
	if err != nil || !reflect.DeepEqual(gotPage, page) {
		t.Fatalf("History = %#v, %v", gotPage, err)
	}
	gotPost, err := web.PostMessage(context.Background(), postRequest)
	if err != nil || gotPost != postResult {
		t.Fatalf("PostMessage = %#v, %v", gotPost, err)
	}
	if err := web.AssertDone(); err != nil {
		t.Fatal(err)
	}
	calls := web.Calls()
	if len(calls) != 2 || calls[0].Operation != History || calls[1].Operation != PostMessage {
		t.Fatalf("Calls = %#v", calls)
	}
}

func TestWebFailsOnScriptMismatchWithoutPrintingPayload(t *testing.T) {
	web := NewWeb(Step{Operation: DeleteMessage, Request: slackapi.DeleteMessageRequest{ChannelID: "C012ABC", MessageTS: "1.000001"}})
	err := web.AddReaction(context.Background(), slackapi.ReactionRequest{ChannelID: "C012ABC", MessageTS: "1.000001", Emoji: "secret-payload"})
	if err == nil || err.Error() != "fake Slack Web: got add_reaction call, expected delete_message" {
		t.Fatalf("mismatch error = %v", err)
	}
	if got := err.Error(); got == "" || strings.Contains(got, "secret-payload") {
		t.Fatalf("mismatch error leaked request payload: %q", got)
	}
}

func TestWebCallScriptsGateFIFOAndModelSubmissionBoundary(t *testing.T) {
	preBarrier := NewBarrier()
	postBarrier := NewBarrier()
	postRequest := slackapi.PostMessageRequest{ChannelID: "C012ABC", Text: "exact intent", ClientMessageID: "eff_123"}
	historyRequest := slackapi.HistoryRequest{ChannelID: "C012ABC", Limit: 1}
	web := NewWeb(
		Step{Operation: PostMessage, Request: postRequest, PreCall: CallScript{Barrier: preBarrier}},
		Step{Operation: History, Request: historyRequest, PostCall: CallScript{Barrier: postBarrier, Fault: Disconnect}},
	)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	postDone := make(chan error, 1)
	go func() { _, err := web.PostMessage(ctx, postRequest); postDone <- err }()
	select {
	case <-preBarrier.Reached():
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	historyDone := make(chan error, 1)
	go func() { _, err := web.History(ctx, historyRequest); historyDone <- err }()
	preBarrier.Release()
	if err := <-postDone; err != nil {
		t.Fatalf("PostMessage: %v", err)
	}
	select {
	case <-postBarrier.Reached():
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	calls := web.Calls()
	if len(calls) != 2 || calls[0].Operation != PostMessage || calls[1].Operation != History {
		t.Fatalf("FIFO calls = %#v", calls)
	}
	if web.Remaining() != 0 {
		t.Fatalf("remaining after post-call gate = %d", web.Remaining())
	}
	postBarrier.Release()
	if err := <-historyDone; slackapi.Classify(err, slackapi.FailureAfterSubmit).Diagnostic != slackapi.DiagnosticDisconnected {
		t.Fatalf("post-call disconnect = %v", err)
	}
}

func TestWebPreCallTimeoutDoesNotCaptureOrConsume(t *testing.T) {
	barrier := NewBarrier()
	request := slackapi.DeleteMessageRequest{ChannelID: "C012ABC", MessageTS: "1.000001"}
	web := NewWeb(Step{Operation: DeleteMessage, Request: request, PreCall: CallScript{Barrier: barrier, Fault: Timeout}})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- web.DeleteMessage(ctx, request) }()
	<-barrier.Reached()
	barrier.Release()
	if err := <-done; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("pre-call timeout = %v", err)
	}
	if calls := web.Calls(); len(calls) != 0 || web.Remaining() != 1 {
		t.Fatalf("pre-call timeout calls=%#v remaining=%d", calls, web.Remaining())
	}
}

func TestSocketScriptsReconnectAndReceive(t *testing.T) {
	openFailure := errors.New("disconnected")
	envelope := slackapi.Envelope{EnvelopeID: "env-1"}
	socket := NewSocket(
		SocketSession{OpenError: openFailure},
		SocketSession{Receives: []ReceiveStep{{Envelope: envelope}, {Err: io.ErrUnexpectedEOF}}},
	)
	if _, err := socket.Open(context.Background()); !errors.Is(err, openFailure) {
		t.Fatalf("first Open error = %v", err)
	}
	stream, err := socket.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got, err := stream.Receive(context.Background())
	if err != nil || got.EnvelopeID != envelope.EnvelopeID {
		t.Fatalf("Receive = %#v, %v", got, err)
	}
	if _, err := stream.Receive(context.Background()); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("second Receive error = %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if socket.Opens() != 2 || socket.Remaining() != 0 {
		t.Fatalf("opens=%d remaining=%d", socket.Opens(), socket.Remaining())
	}
}

func TestSocketBarriersCancellationAndLifecycle(t *testing.T) {
	openBarrier := NewBarrier()
	receiveBarrier := NewBarrier()
	socket := NewSocket(SocketSession{
		OpenBarrier: openBarrier,
		Receives: []ReceiveStep{{
			Envelope: slackapi.Envelope{EnvelopeID: "env-blocked"},
			Barrier:  receiveBarrier,
		}},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	opened := make(chan slackapi.EnvelopeStream, 1)
	openErrors := make(chan error, 1)
	go func() {
		stream, err := socket.Open(ctx)
		opened <- stream
		openErrors <- err
	}()
	select {
	case <-openBarrier.Reached():
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := socket.WaitForOpens(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if got := len(socket.OpenedStreams()); got != 0 {
		t.Fatalf("opened streams before barrier release = %d", got)
	}
	openBarrier.Release()
	streamContract := <-opened
	if err := <-openErrors; err != nil {
		t.Fatal(err)
	}
	stream, ok := streamContract.(*Stream)
	if !ok {
		t.Fatalf("stream type = %T", streamContract)
	}
	if err := socket.WaitForOpenedStreams(ctx, 1); err != nil {
		t.Fatal(err)
	}

	received := make(chan slackapi.Envelope, 1)
	receiveErrors := make(chan error, 1)
	go func() {
		envelope, err := stream.Receive(ctx)
		received <- envelope
		receiveErrors <- err
	}()
	select {
	case <-receiveBarrier.Reached():
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := stream.WaitForReceiveCalls(ctx, 1); err != nil {
		t.Fatal(err)
	}
	select {
	case envelope := <-received:
		t.Fatalf("Receive crossed barrier early: %#v", envelope)
	default:
	}
	receiveBarrier.Release()
	if envelope := <-received; envelope.EnvelopeID != "env-blocked" {
		t.Fatalf("Receive envelope = %#v", envelope)
	}
	if err := <-receiveErrors; err != nil {
		t.Fatal(err)
	}

	// Exhausted live streams block rather than inventing EOF, and cancellation
	// is observed without sleeps or leaked goroutines.
	receiveCtx, cancelReceive := context.WithCancel(ctx)
	blocked := make(chan error, 1)
	go func() {
		_, err := stream.Receive(receiveCtx)
		blocked <- err
	}()
	if err := stream.WaitForReceiveCalls(ctx, 2); err != nil {
		t.Fatal(err)
	}
	cancelReceive()
	if err := <-blocked; !errors.Is(err, context.Canceled) {
		t.Fatalf("blocked Receive error = %v", err)
	}

	go func() {
		_, err := stream.Receive(ctx)
		blocked <- err
	}()
	if err := stream.WaitForReceiveCalls(ctx, 3); err != nil {
		t.Fatal(err)
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	if err := stream.WaitForClose(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-blocked; !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("Receive after Close error = %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	if !stream.Closed() || stream.CloseCalls() != 2 {
		t.Fatalf("closed=%t close calls=%d", stream.Closed(), stream.CloseCalls())
	}
}

func TestSocketRejectsConcurrentReceive(t *testing.T) {
	barrier := NewBarrier()
	stream := newStream([]ReceiveStep{{Barrier: barrier}})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	first := make(chan error, 1)
	go func() { _, err := stream.Receive(ctx); first <- err }()
	<-barrier.Reached()
	if _, err := stream.Receive(ctx); !errors.Is(err, slackapi.ErrConcurrentReceive) {
		t.Fatalf("concurrent Receive error = %v", err)
	}
	barrier.Release()
	if err := <-first; err != nil {
		t.Fatalf("first Receive error = %v", err)
	}
}

func TestSocketOpenBarrierHonorsCancellation(t *testing.T) {
	barrier := NewBarrier()
	socket := NewSocket(SocketSession{OpenBarrier: barrier})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := socket.Open(ctx)
		done <- err
	}()
	<-barrier.Reached()
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Open error = %v", err)
	}
	if socket.Opens() != 1 || len(socket.OpenedStreams()) != 0 {
		t.Fatalf("opens=%d opened streams=%d", socket.Opens(), len(socket.OpenedStreams()))
	}
}

func TestAckRecorderAndJitter(t *testing.T) {
	ack, recorder := NewAckRecorder(nil)
	if err := ack.Acknowledge(context.Background()); err != nil {
		t.Fatal(err)
	}
	if recorder.Calls() != 1 {
		t.Fatalf("ack calls = %d", recorder.Calls())
	}
	if err := ack.Acknowledge(context.Background()); !errors.Is(err, slackapi.ErrAlreadyAcknowledged) {
		t.Fatalf("second acknowledgement error = %v", err)
	}
	if recorder.Calls() != 1 {
		t.Fatalf("ack callback retried: %d calls", recorder.Calls())
	}

	jitter := NewJitter(250*time.Millisecond, 0)
	if got := jitter.Jitter(time.Second); got != 250*time.Millisecond {
		t.Fatalf("first jitter = %v", got)
	}
	if got := jitter.Jitter(500 * time.Millisecond); got != 0 {
		t.Fatalf("second jitter = %v", got)
	}
	if got := jitter.Maximums(); !reflect.DeepEqual(got, []time.Duration{time.Second, 500 * time.Millisecond}) {
		t.Fatalf("maximums = %v", got)
	}
}

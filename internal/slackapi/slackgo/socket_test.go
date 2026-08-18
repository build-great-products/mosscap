package slackgo

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stayradiated/mosscap/internal/slackapi"
)

func TestSocketOwnsDialNormalizeAcknowledgeAndClose(t *testing.T) {
	t.Parallel()
	acknowledged := make(chan string, 1)
	payload := eventsAPIRequest("env-1", "Ev-1", map[string]any{
		"type":      "app_mention",
		"user":      "U012USR",
		"bot_id":    "B012BOT",
		"text":      "<@U012BOT> task",
		"ts":        "1700000000.000001",
		"thread_ts": "1700000000.000000",
		"channel":   "C012ABC",
		"event_ts":  "1700000000.000002",
		"files": []any{map[string]any{
			"id": "F012ABC", "name": "note.txt", "mimetype": "text/plain",
			"permalink": "https://example.slack.com/files/F012ABC", "size": 12,
		}},
	})
	websocketServer := newWebSocketServer(t, func(connection *websocket.Conn) {
		if err := connection.WriteJSON(map[string]any{"type": "hello", "num_connections": 1}); err != nil {
			return
		}
		if err := connection.WriteJSON(payload); err != nil {
			return
		}
		var ack struct {
			EnvelopeID string `json:"envelope_id"`
		}
		if err := connection.ReadJSON(&ack); err == nil {
			acknowledged <- ack.EnvelopeID
		}
	})
	defer websocketServer.Close()
	apiServer := newSocketOpenServer(t, websocketURL(websocketServer.URL))
	defer apiServer.Close()

	socket := NewSocket("xapp-test", SocketAPIURL(apiServer.URL), SocketAllowInsecureWebSocket())
	stream, err := socket.Open(context.Background())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	envelope, err := stream.Receive(context.Background())
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if envelope.Kind != slackapi.EventAppMention || envelope.EnvelopeID != "env-1" || envelope.EventID != "Ev-1" || envelope.TeamID != "T012ABC" || envelope.ChannelID != "C012ABC" {
		t.Fatalf("identity = %#v", envelope)
	}
	if envelope.UserID != "U012USR" || envelope.BotID != "B012BOT" || envelope.MessageTS != "1700000000.000001" || envelope.ThreadTS != "1700000000.000000" || envelope.EventTS != "1700000000.000002" || envelope.Text != "<@U012BOT> task" {
		t.Fatalf("message = %#v", envelope)
	}
	if len(envelope.Files) != 1 || envelope.Files[0].ID != "F012ABC" || envelope.Files[0].SizeBytes == nil || *envelope.Files[0].SizeBytes != 12 {
		t.Fatalf("files = %#v", envelope.Files)
	}
	if err := envelope.Ack.Acknowledge(context.Background()); err != nil {
		t.Fatalf("Acknowledge: %v", err)
	}
	if err := envelope.Ack.Acknowledge(context.Background()); !errors.Is(err, slackapi.ErrAlreadyAcknowledged) {
		t.Fatalf("second Acknowledge = %v", err)
	}
	select {
	case id := <-acknowledged:
		if id != "env-1" {
			t.Fatalf("ack envelope = %q", id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ack was not received")
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestSocketMapsMessageVariantsAndDerivesStableEventID(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		inner       map[string]any
		wantKind    slackapi.EventKind
		wantMessage slackapi.Timestamp
		wantThread  slackapi.Timestamp
		wantText    string
		wantUser    string
		wantBot     string
		wantSubtype string
	}{
		{
			name:     "ordinary reply",
			inner:    map[string]any{"type": "message", "user": "U012USR", "text": "reply", "ts": "1700000000.000003", "thread_ts": "1700000000.000001", "channel": "C012ABC", "event_ts": "1700000000.000004"},
			wantKind: slackapi.EventMessage, wantMessage: "1700000000.000003", wantThread: "1700000000.000001", wantText: "reply", wantUser: "U012USR",
		},
		{
			name:     "bot system message",
			inner:    map[string]any{"type": "message", "subtype": "bot_message", "bot_id": "B012BOT", "text": "bot", "ts": "1700000000.000003", "channel": "C012ABC", "event_ts": "1700000000.000004"},
			wantKind: slackapi.EventMessage, wantMessage: "1700000000.000003", wantText: "bot", wantBot: "B012BOT", wantSubtype: "bot_message",
		},
		{
			name:     "changed",
			inner:    map[string]any{"type": "message", "subtype": "message_changed", "channel": "C012ABC", "event_ts": "1700000000.000005", "message": map[string]any{"type": "message", "user": "U012USR", "text": "current", "ts": "1700000000.000003", "thread_ts": "1700000000.000001"}},
			wantKind: slackapi.EventMessageChange, wantMessage: "1700000000.000003", wantThread: "1700000000.000001", wantText: "current", wantUser: "U012USR",
		},
		{
			name:     "deleted",
			inner:    map[string]any{"type": "message", "subtype": "message_deleted", "channel": "C012ABC", "event_ts": "1700000000.000006", "deleted_ts": "1700000000.000003", "previous_message": map[string]any{"type": "message", "user": "U012USR", "ts": "1700000000.000003", "thread_ts": "1700000000.000001"}},
			wantKind: slackapi.EventMessageDelete, wantMessage: "1700000000.000003", wantThread: "1700000000.000001", wantUser: "U012USR",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			request := eventsAPIRequest("env-variant", "", test.inner)
			websocketServer := newWebSocketServer(t, func(connection *websocket.Conn) {
				_ = connection.WriteJSON(map[string]any{"type": "hello", "num_connections": 1})
				_ = connection.WriteJSON(request)
			})
			defer websocketServer.Close()
			apiServer := newSocketOpenServer(t, websocketURL(websocketServer.URL))
			defer apiServer.Close()
			stream, err := NewSocket("xapp-test", SocketAPIURL(apiServer.URL), SocketAllowInsecureWebSocket()).Open(context.Background())
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer stream.Close()
			envelope, err := stream.Receive(context.Background())
			if err != nil {
				t.Fatalf("Receive: %v", err)
			}
			if envelope.Kind != test.wantKind || envelope.MessageTS != test.wantMessage || envelope.ThreadTS != test.wantThread || envelope.Text != test.wantText || envelope.UserID != test.wantUser || envelope.BotID != test.wantBot || envelope.Subtype != test.wantSubtype {
				t.Fatalf("envelope = %#v", envelope)
			}
			if !strings.HasPrefix(envelope.EventID, "sha256-") || len(envelope.EventID) != len("sha256-")+64 {
				t.Fatalf("derived event ID = %q", envelope.EventID)
			}
		})
	}
}

func TestSocketReceiveCancellationClosesOwnedTransport(t *testing.T) {
	t.Parallel()
	connected := make(chan struct{})
	websocketServer := newWebSocketServer(t, func(connection *websocket.Conn) {
		_ = connection.WriteJSON(map[string]any{"type": "hello", "num_connections": 1})
		close(connected)
		var ignored any
		_ = connection.ReadJSON(&ignored)
	})
	defer websocketServer.Close()
	apiServer := newSocketOpenServer(t, websocketURL(websocketServer.URL))
	defer apiServer.Close()
	stream, err := NewSocket("xapp-test", SocketAPIURL(apiServer.URL), SocketAllowInsecureWebSocket()).Open(context.Background())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	<-connected
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = stream.Receive(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Receive = %v, want context canceled", err)
	}
	_ = stream.Close()
}

func TestSocketRejectsMalformedEnvelopeSecretSafely(t *testing.T) {
	t.Parallel()
	const canary = "xapp-secret-canary"
	websocketServer := newWebSocketServer(t, func(connection *websocket.Conn) {
		_ = connection.WriteJSON(map[string]any{"type": "hello", "num_connections": 1})
		_ = connection.WriteMessage(websocket.TextMessage, []byte(`{"type":"events_api","envelope_id":"`+canary+`","payload":`))
	})
	defer websocketServer.Close()
	apiServer := newSocketOpenServer(t, websocketURL(websocketServer.URL))
	defer apiServer.Close()
	stream, err := NewSocket(canary, SocketAPIURL(apiServer.URL), SocketAllowInsecureWebSocket()).Open(context.Background())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer stream.Close()
	_, err = stream.Receive(context.Background())
	if err == nil || strings.Contains(err.Error(), canary) {
		t.Fatalf("unsafe malformed error: %v", err)
	}
	failure := slackapi.Classify(err, slackapi.FailureSocket)
	if failure.Class != slackapi.FailureUnavailable || failure.Diagnostic != slackapi.DiagnosticMalformedResponse {
		t.Fatalf("classification = %#v", failure)
	}
}

func TestSocketReturnsIdentifiableMalformedEnvelopeWithAckPath(t *testing.T) {
	t.Parallel()
	acknowledged := make(chan string, 1)
	websocketServer := newWebSocketServer(t, func(connection *websocket.Conn) {
		_ = connection.WriteJSON(map[string]any{"type": "hello", "num_connections": 1})
		_ = connection.WriteJSON(map[string]any{"type": "events_api", "envelope_id": "env-malformed", "payload": map[string]any{"type": "wrong"}})
		var ack struct {
			EnvelopeID string `json:"envelope_id"`
		}
		if connection.ReadJSON(&ack) == nil {
			acknowledged <- ack.EnvelopeID
		}
	})
	defer websocketServer.Close()
	apiServer := newSocketOpenServer(t, websocketURL(websocketServer.URL))
	defer apiServer.Close()
	stream, err := NewSocket("xapp-test", SocketAPIURL(apiServer.URL), SocketAllowInsecureWebSocket()).Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	envelope, err := stream.Receive(context.Background())
	if err == nil || envelope.EnvelopeID != "env-malformed" || envelope.Ack == nil {
		t.Fatalf("malformed envelope = %#v, %v", envelope, err)
	}
	if err := envelope.Ack.Acknowledge(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-acknowledged:
		if got != "env-malformed" {
			t.Fatalf("ack = %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("malformed envelope was not ackable")
	}
}

func TestSocketLeavesUnidentifiableMalformedFrameUnacked(t *testing.T) {
	t.Parallel()
	websocketServer := newWebSocketServer(t, func(connection *websocket.Conn) {
		_ = connection.WriteJSON(map[string]any{"type": "hello", "num_connections": 1})
		_ = connection.WriteJSON(map[string]any{"type": "events_api", "payload": map[string]any{"type": "wrong"}})
	})
	defer websocketServer.Close()
	apiServer := newSocketOpenServer(t, websocketURL(websocketServer.URL))
	defer apiServer.Close()
	stream, err := NewSocket("xapp-test", SocketAPIURL(apiServer.URL), SocketAllowInsecureWebSocket()).Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	envelope, err := stream.Receive(context.Background())
	if err == nil || envelope.EnvelopeID != "" || envelope.Ack != nil {
		t.Fatalf("unidentifiable envelope = %#v, %v", envelope, err)
	}
}

func TestSocketOpenRequiresHelloReadinessAndSecureProductionURL(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		first    map[string]any
		insecure bool
	}{
		{"missing hello", map[string]any{"type": "events_api", "envelope_id": "env-1"}, true},
		{"invalid hello", map[string]any{"type": "hello", "num_connections": 0}, true},
		{"ws refused by default", map[string]any{"type": "hello", "num_connections": 1}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			websocketServer := newWebSocketServer(t, func(connection *websocket.Conn) { _ = connection.WriteJSON(test.first) })
			defer websocketServer.Close()
			apiServer := newSocketOpenServer(t, websocketURL(websocketServer.URL))
			defer apiServer.Close()
			options := []SocketOption{SocketAPIURL(apiServer.URL), SocketHelloTimeout(time.Second)}
			if test.insecure {
				options = append(options, SocketAllowInsecureWebSocket())
			}
			stream, err := NewSocket("xapp-test", options...).Open(context.Background())
			if err == nil || stream != nil {
				t.Fatalf("Open = %T, %v", stream, err)
			}
		})
	}
}

func TestSocketOpenReportsSocketConnectionLimit(t *testing.T) {
	t.Parallel()
	websocketServer := newWebSocketServer(t, func(connection *websocket.Conn) {
		_ = connection.WriteJSON(map[string]any{"type": "disconnect", "reason": "too_many_websockets"})
	})
	defer websocketServer.Close()
	apiServer := newSocketOpenServer(t, websocketURL(websocketServer.URL))
	defer apiServer.Close()

	stream, err := NewSocket("xapp-test", SocketAPIURL(apiServer.URL), SocketAllowInsecureWebSocket()).Open(context.Background())
	if err == nil || stream != nil {
		t.Fatalf("Open = %T, %v", stream, err)
	}
	failure := slackapi.Classify(err, slackapi.FailureSocket)
	if failure.Class != slackapi.FailureUnavailable || failure.Diagnostic != slackapi.DiagnosticSocketConnectionLimit {
		t.Fatalf("classification = %#v (%v)", failure, err)
	}
}

func TestSocketLivenessPingsAndBoundsSilentRead(t *testing.T) {
	t.Parallel()
	pingSeen := make(chan struct{}, 1)
	websocketServer := newWebSocketServer(t, func(connection *websocket.Conn) {
		_ = connection.WriteJSON(map[string]any{"type": "hello", "num_connections": 1})
		connection.SetPingHandler(func(string) error {
			select {
			case pingSeen <- struct{}{}:
			default:
			}
			return nil // deliberately withhold pong so the read deadline expires
		})
		var ignored any
		_ = connection.ReadJSON(&ignored)
	})
	defer websocketServer.Close()
	apiServer := newSocketOpenServer(t, websocketURL(websocketServer.URL))
	defer apiServer.Close()
	stream, err := NewSocket("xapp-test", SocketAPIURL(apiServer.URL), SocketAllowInsecureWebSocket(), SocketLiveness(10*time.Millisecond, 40*time.Millisecond)).Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	_, err = stream.Receive(context.Background())
	failure := slackapi.Classify(err, slackapi.FailureSocket)
	if failure.Diagnostic != slackapi.DiagnosticTimeout {
		t.Fatalf("liveness failure = %#v (%v)", failure, err)
	}
	select {
	case <-pingSeen:
	default:
		t.Fatal("no liveness ping observed")
	}
}

func TestSocketRejectsConcurrentReceiveWithoutStealingReader(t *testing.T) {
	t.Parallel()
	websocketServer := newWebSocketServer(t, func(connection *websocket.Conn) {
		_ = connection.WriteJSON(map[string]any{"type": "hello", "num_connections": 1})
		var ignored any
		_ = connection.ReadJSON(&ignored)
	})
	defer websocketServer.Close()
	apiServer := newSocketOpenServer(t, websocketURL(websocketServer.URL))
	defer apiServer.Close()
	stream, err := NewSocket("xapp-test", SocketAPIURL(apiServer.URL), SocketAllowInsecureWebSocket()).Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() { _, err := stream.Receive(ctx); first <- err }()
	time.Sleep(10 * time.Millisecond)
	if _, err := stream.Receive(context.Background()); !errors.Is(err, slackapi.ErrConcurrentReceive) {
		t.Fatalf("concurrent Receive = %v", err)
	}
	cancel()
	if err := <-first; !errors.Is(err, context.Canceled) {
		t.Fatalf("first Receive = %v", err)
	}
}

func TestSocketMaxFilesAndFallbackIdentityAreActive(t *testing.T) {
	t.Parallel()
	first := slackapi.Envelope{TeamID: "T012ABC", Kind: slackapi.EventMessageChange, ChannelID: "C012ABC", MessageTS: "1700000000.000001", EventTS: "1700000000.000000", UserID: "U012USR", Text: "first"}
	second := first
	second.Text = "second"
	if deriveEventID(first) == deriveEventID(second) {
		t.Fatal("imprecise edits collided")
	}

	request := eventsAPIRequest("env-files", "", map[string]any{
		"type": "message", "user": "U012USR", "text": "files", "ts": "1700000000.000001", "channel": "C012ABC", "event_ts": "1700000000.000002",
		"files": []any{map[string]any{"id": "F001", "name": "one", "size": 1}, map[string]any{"id": "F002", "name": "two", "size": 2}},
	})
	websocketServer := newWebSocketServer(t, func(connection *websocket.Conn) {
		_ = connection.WriteJSON(map[string]any{"type": "hello", "num_connections": 1})
		_ = connection.WriteJSON(request)
	})
	defer websocketServer.Close()
	apiServer := newSocketOpenServer(t, websocketURL(websocketServer.URL))
	defer apiServer.Close()
	stream, err := NewSocket("xapp-test", SocketAPIURL(apiServer.URL), SocketAllowInsecureWebSocket(), SocketMaxFiles(1)).Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	envelope, err := stream.Receive(context.Background())
	if err == nil || envelope.EnvelopeID != "env-files" || envelope.Ack == nil {
		t.Fatalf("bounded malformed envelope = %#v, %v", envelope, err)
	}
}

func TestVersionReportsPinnedSDK(t *testing.T) {
	t.Parallel()
	if got := Version(); got != "slack-go/v0.27.0" {
		t.Fatalf("Version = %q", got)
	}
}

func eventsAPIRequest(envelopeID, eventID string, inner map[string]any) map[string]any {
	return map[string]any{
		"type":        "events_api",
		"envelope_id": envelopeID,
		"payload": map[string]any{
			"team_id": "T012ABC", "type": "event_callback", "event_id": eventID,
			"event_time": 1700000000, "event": inner,
		},
	}
}

func newSocketOpenServer(t *testing.T, socketURL string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/apps.connections.open" {
			http.NotFound(writer, request)
			return
		}
		if authorization := request.Header.Get("Authorization"); authorization != "Bearer xapp-test" && authorization != "Bearer xapp-secret-canary" {
			t.Errorf("authorization = %q", authorization)
		}
		writer.Header().Set("Content-Type", "application/json")
		writeJSON(t, writer, map[string]any{"ok": true, "url": socketURL})
	}))
}

func newWebSocketServer(t *testing.T, handler func(*websocket.Conn)) *httptest.Server {
	t.Helper()
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	return httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connection, err := upgrader.Upgrade(writer, request, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer connection.Close()
		handler(connection)
	}))
}

func websocketURL(httpURL string) string { return "ws" + strings.TrimPrefix(httpURL, "http") }

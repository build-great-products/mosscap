package slackgo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/gorilla/websocket"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
	"github.com/stayradiated/mosscap/internal/slackapi"
)

const (
	maxSocketFrameBytes = int64(16 << 20)
	defaultWriteTimeout = 10 * time.Second
	defaultHelloTimeout = 10 * time.Second
	defaultPingInterval = 30 * time.Second
	defaultReadTimeout  = 90 * time.Second
)

type socketOptions struct {
	apiURL        string
	httpClient    HTTPDoer
	dialer        *websocket.Dialer
	allowInsecure bool
	helloTimeout  time.Duration
	pingInterval  time.Duration
	readTimeout   time.Duration
	maxFiles      int
	optionErr     error
}

// SocketOption configures the owned Socket Mode adapter.
type SocketOption func(*socketOptions)

// SocketAPIURL overrides the apps.connections.open endpoint for local tests.
func SocketAPIURL(value string) SocketOption {
	return func(options *socketOptions) { options.apiURL = value }
}

// SocketHTTPClient supplies the HTTP transport used only by OpenContext.
func SocketHTTPClient(client HTTPDoer) SocketOption {
	return func(options *socketOptions) { options.httpClient = client }
}

// SocketDialer supplies the Gorilla WebSocket dialer used after OpenContext.
func SocketDialer(dialer *websocket.Dialer) SocketOption {
	return func(options *socketOptions) { options.dialer = dialer }
}

// SocketAllowInsecureWebSocket permits ws:// only for local deterministic
// tests. Production defaults to requiring wss:// from apps.connections.open.
func SocketAllowInsecureWebSocket() SocketOption {
	return func(options *socketOptions) { options.allowInsecure = true }
}

// SocketLiveness configures active ping and bounded read liveness. The read
// timeout must be greater than the ping interval.
func SocketLiveness(pingInterval, readTimeout time.Duration) SocketOption {
	return func(options *socketOptions) {
		if pingInterval <= 0 || readTimeout <= pingInterval {
			options.optionErr = errors.New("invalid Slack Socket liveness")
			return
		}
		options.pingInterval = pingInterval
		options.readTimeout = readTimeout
	}
}

// SocketHelloTimeout bounds candidate readiness validation.
func SocketHelloTimeout(timeout time.Duration) SocketOption {
	return func(options *socketOptions) {
		if timeout <= 0 {
			options.optionErr = errors.New("invalid Slack Socket hello timeout")
			return
		}
		options.helloTimeout = timeout
	}
}

// SocketMaxFiles sets the active maximum normalized files per envelope.
func SocketMaxFiles(maximum int) SocketOption {
	return func(options *socketOptions) {
		if maximum < 1 || maximum > slackapi.MaxFiles {
			options.optionErr = errors.New("invalid Slack maximum file count")
			return
		}
		options.maxFiles = maximum
	}
}

// Socket uses slack-go only for apps.connections.open and owns the Gorilla
// dial/read/ack/close lifecycle. Reconnection policy remains outside it.
type Socket struct {
	client        *socketmode.Client
	dialer        *websocket.Dialer
	allowInsecure bool
	helloTimeout  time.Duration
	pingInterval  time.Duration
	readTimeout   time.Duration
	maxFiles      int
	optionErr     error
}

// NewSocket creates a Socket Mode adapter for an app-level token.
func NewSocket(appToken string, options ...SocketOption) *Socket {
	configuration := socketOptions{
		httpClient: http.DefaultClient, dialer: websocket.DefaultDialer,
		helloTimeout: defaultHelloTimeout, pingInterval: defaultPingInterval,
		readTimeout: defaultReadTimeout, maxFiles: slackapi.MaxFiles,
	}
	for _, option := range options {
		if option != nil {
			option(&configuration)
		}
	}
	if configuration.httpClient == nil {
		configuration.httpClient = http.DefaultClient
	}
	if configuration.dialer == nil {
		configuration.dialer = websocket.DefaultDialer
	}
	sdkOptions := []slack.Option{
		slack.OptionAppLevelToken(appToken),
		slack.OptionHTTPClient(limitHTTPResponses(refuseRedirects(configuration.httpClient), maxWebResponseBytes)),
		slack.OptionRetry(0),
	}
	if configuration.apiURL != "" {
		sdkOptions = append(sdkOptions, slack.OptionAPIURL(withTrailingSlash(configuration.apiURL)))
	}
	api := slack.New("", sdkOptions...)
	return &Socket{
		client: socketmode.New(api), dialer: cloneDialer(configuration.dialer),
		allowInsecure: configuration.allowInsecure, helloTimeout: configuration.helloTimeout,
		pingInterval: configuration.pingInterval, readTimeout: configuration.readTimeout,
		maxFiles: configuration.maxFiles, optionErr: configuration.optionErr,
	}
}

func (socket *Socket) Open(ctx context.Context) (slackapi.EnvelopeStream, error) {
	if socket.optionErr != nil {
		return nil, invalidRequest()
	}
	_, websocketURL, err := socket.client.OpenContext(ctx)
	if err != nil {
		return nil, sanitizeError(err)
	}
	if err := validateWebSocketURL(websocketURL, socket.allowInsecure); err != nil {
		return nil, invalidResponse()
	}
	connection, response, err := socket.dialer.DialContext(ctx, websocketURL, nil)
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	if err != nil {
		return nil, sanitizeError(err)
	}
	connection.SetReadLimit(maxSocketFrameBytes)
	if err := validateHello(connection, ctx, socket.helloTimeout); err != nil {
		_ = connection.Close()
		return nil, err
	}
	if err := connection.SetReadDeadline(time.Now().Add(socket.readTimeout)); err != nil {
		_ = connection.Close()
		return nil, sanitizeSocketError(err)
	}
	stream := &envelopeStream{
		connection: connection, done: make(chan struct{}),
		pingInterval: socket.pingInterval, readTimeout: socket.readTimeout,
		maxFiles: socket.maxFiles,
	}
	connection.SetPongHandler(func(string) error { return connection.SetReadDeadline(time.Now().Add(stream.readTimeout)) })
	go stream.pingLoop()
	return stream, nil
}

func validateHello(connection *websocket.Conn, ctx context.Context, timeout time.Duration) error {
	stopCancellation := context.AfterFunc(ctx, func() { _ = connection.Close() })
	defer stopCancellation()
	deadline := time.Now().Add(timeout)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	if err := connection.SetReadDeadline(deadline); err != nil {
		return sanitizeSocketError(err)
	}
	messageType, payload, err := connection.ReadMessage()
	if err != nil {
		if contextError := ctx.Err(); contextError != nil {
			return contextError
		}
		return sanitizeSocketError(err)
	}
	if messageType != websocket.TextMessage {
		return invalidResponse()
	}
	var hello socketmode.Request
	if json.Unmarshal(payload, &hello) != nil || hello.Type != socketmode.RequestTypeHello || hello.NumConnections < 1 {
		return invalidResponse()
	}
	return nil
}

type envelopeStream struct {
	connection   *websocket.Conn
	done         chan struct{}
	pingInterval time.Duration
	readTimeout  time.Duration
	maxFiles     int

	receiving atomic.Bool
	writeMu   sync.Mutex
	closeOnce sync.Once
	stateMu   sync.RWMutex
	closed    bool
}

func (stream *envelopeStream) Receive(ctx context.Context) (slackapi.Envelope, error) {
	if err := ctx.Err(); err != nil {
		return slackapi.Envelope{}, err
	}
	if !stream.receiving.CompareAndSwap(false, true) {
		return slackapi.Envelope{}, slackapi.ErrConcurrentReceive
	}
	defer stream.receiving.Store(false)
	if stream.isClosed() {
		return slackapi.Envelope{}, disconnected()
	}

	// Cancellation intentionally closes the owned transport. A WebSocket permits
	// one reader and cannot otherwise interrupt ReadMessage safely; callers must
	// reconnect rather than issue concurrent or resumed receives on this stream.
	stopCancellation := context.AfterFunc(ctx, func() { stream.closeTransport(false) })
	defer stopCancellation()
	for {
		if err := stream.connection.SetReadDeadline(time.Now().Add(stream.readTimeout)); err != nil {
			return slackapi.Envelope{}, sanitizeSocketError(err)
		}
		messageType, payload, err := stream.connection.ReadMessage()
		if err != nil {
			if contextError := ctx.Err(); contextError != nil {
				return slackapi.Envelope{}, contextError
			}
			return slackapi.Envelope{}, sanitizeSocketError(err)
		}
		if messageType != websocket.TextMessage {
			return slackapi.Envelope{}, invalidResponse()
		}
		var request socketmode.Request
		if err := json.Unmarshal(payload, &request); err != nil {
			return slackapi.Envelope{}, invalidResponse()
		}
		switch request.Type {
		case socketmode.RequestTypeDisconnect:
			stream.closeTransport(false)
			return slackapi.Envelope{}, disconnected()
		case socketmode.RequestTypeEventsAPI:
			envelope, normalizeErr := stream.normalize(request)
			return envelope, normalizeErr
		default:
			if envelope, ok := stream.malformedEnvelope(request); ok {
				return envelope, invalidResponse()
			}
			return slackapi.Envelope{}, invalidResponse()
		}
	}
}

func (stream *envelopeStream) Close() error {
	stream.closeTransport(true)
	return nil
}

func (stream *envelopeStream) normalize(request socketmode.Request) (envelope slackapi.Envelope, err error) {
	malformed, identifiable := stream.malformedEnvelope(request)
	if !identifiable || len(request.Payload) == 0 {
		return malformed, invalidResponse()
	}
	defer func() {
		if err != nil {
			envelope = malformed
		}
	}()
	event, err := slackevents.ParseEvent(request.Payload, slackevents.OptionNoVerifyToken())
	if err != nil || event.Type != slackevents.CallbackEvent {
		return slackapi.Envelope{}, invalidResponse()
	}
	callback, ok := event.Data.(*slackevents.EventsAPICallbackEvent)
	if !ok || callback == nil {
		return slackapi.Envelope{}, invalidResponse()
	}
	envelope = slackapi.Envelope{
		EnvelopeID: request.EnvelopeID,
		EventID:    callback.EventID,
		TeamID:     event.TeamID,
		Ack: slackapi.NewAcknowledgement(func(ctx context.Context) error {
			return stream.acknowledge(ctx, request.EnvelopeID)
		}),
	}
	if envelope.TeamID == "" {
		envelope.TeamID = callback.TeamID
	}
	fallbackEventTS := ""
	if callback.EventTime > 0 {
		fallbackEventTS = strconv.Itoa(callback.EventTime) + ".000000"
	}

	switch inner := event.InnerEvent.Data.(type) {
	case *slackevents.AppMentionEvent:
		if inner == nil {
			return slackapi.Envelope{}, invalidResponse()
		}
		envelope.Kind = slackapi.EventAppMention
		envelope.ChannelID = inner.Channel
		envelope.UserID = inner.User
		envelope.BotID = inner.BotID
		envelope.MessageTS = slackapi.Timestamp(inner.TimeStamp)
		envelope.ThreadTS = slackapi.Timestamp(inner.ThreadTimeStamp)
		envelope.EventTS = slackapi.Timestamp(firstNonEmpty(inner.EventTimeStamp, fallbackEventTS))
		envelope.Text = inner.Text
		envelope.Files, err = normalizeFiles(inner.Files, stream.maxFiles)
	case *slackevents.MessageEvent:
		if inner == nil {
			return slackapi.Envelope{}, invalidResponse()
		}
		err = normalizeMessageEvent(&envelope, inner, fallbackEventTS, stream.maxFiles)
		if err == nil && (envelope.Kind == slackapi.EventMessageChange || envelope.Kind == slackapi.EventMessageDelete) && inner.EventTimeStamp == "" {
			// A seconds-only callback fallback cannot safely order two edits or
			// deletions. Preserve the identifiable malformed/ack path instead of
			// inventing an ordering value that can collide.
			err = errors.New("imprecise changed or deleted event")
		}
	default:
		return slackapi.Envelope{}, invalidResponse()
	}
	if err != nil {
		return slackapi.Envelope{}, invalidResponse()
	}
	if envelope.EventID == "" {
		envelope.EventID = deriveEventID(envelope)
	}
	if err := slackapi.ValidateEnvelope(envelope); err != nil {
		return slackapi.Envelope{}, invalidResponse()
	}
	return envelope, nil
}

func normalizeMessageEvent(envelope *slackapi.Envelope, event *slackevents.MessageEvent, fallbackEventTS string, maxFiles int) error {
	envelope.ChannelID = event.Channel
	envelope.EventTS = slackapi.Timestamp(firstNonEmpty(event.EventTimeStamp, fallbackEventTS))

	switch event.SubType {
	case slack.MsgSubTypeMessageChanged:
		current := event.Message
		if current == nil {
			return errors.New("missing changed message")
		}
		envelope.Kind = slackapi.EventMessageChange
		envelope.UserID = current.User
		envelope.BotID = current.BotID
		envelope.Subtype = current.SubType
		envelope.MessageTS = slackapi.Timestamp(current.Timestamp)
		envelope.ThreadTS = slackapi.Timestamp(current.ThreadTimestamp)
		envelope.Text = current.Text
		envelope.Permalink = current.Permalink
		files, err := normalizeFiles(current.Files, maxFiles)
		if err != nil {
			return err
		}
		envelope.Files = files
	case slack.MsgSubTypeMessageDeleted:
		previous := event.PreviousMessage
		envelope.Kind = slackapi.EventMessageDelete
		envelope.MessageTS = slackapi.Timestamp(event.DeletedTimeStamp)
		if previous != nil {
			envelope.UserID = previous.User
			envelope.BotID = previous.BotID
			envelope.Subtype = previous.SubType
			envelope.ThreadTS = slackapi.Timestamp(previous.ThreadTimestamp)
			if envelope.MessageTS == "" {
				envelope.MessageTS = slackapi.Timestamp(previous.Timestamp)
			}
		}
	default:
		current := event.Message
		if current == nil {
			return errors.New("missing message")
		}
		envelope.Kind = slackapi.EventMessage
		envelope.UserID = current.User
		envelope.BotID = current.BotID
		envelope.Subtype = event.SubType
		envelope.MessageTS = slackapi.Timestamp(current.Timestamp)
		envelope.ThreadTS = slackapi.Timestamp(current.ThreadTimestamp)
		envelope.Text = current.Text
		envelope.Permalink = firstNonEmpty(event.Permalink, current.Permalink)
		files, err := normalizeFiles(current.Files, maxFiles)
		if err != nil {
			return err
		}
		envelope.Files = files
	}
	return nil
}

func (stream *envelopeStream) malformedEnvelope(request socketmode.Request) (slackapi.Envelope, bool) {
	if validateEnvelopeID(request.EnvelopeID) != nil {
		return slackapi.Envelope{}, false
	}
	return slackapi.Envelope{
		EnvelopeID: request.EnvelopeID,
		Ack: slackapi.NewAcknowledgement(func(ctx context.Context) error {
			return stream.acknowledge(ctx, request.EnvelopeID)
		}),
	}, true
}

func (stream *envelopeStream) pingLoop() {
	ticker := time.NewTicker(stream.pingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-stream.done:
			return
		case <-ticker.C:
			stream.writeMu.Lock()
			err := stream.connection.WriteControl(websocket.PingMessage, nil, time.Now().Add(defaultWriteTimeout))
			stream.writeMu.Unlock()
			if err != nil {
				stream.closeTransport(false)
				return
			}
		}
	}
}

func (stream *envelopeStream) acknowledge(ctx context.Context, envelopeID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if stream.isClosed() {
		return disconnected()
	}
	stream.writeMu.Lock()
	defer stream.writeMu.Unlock()
	if stream.isClosed() {
		return disconnected()
	}
	deadline := time.Now().Add(defaultWriteTimeout)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	if err := stream.connection.SetWriteDeadline(deadline); err != nil {
		return sanitizeSocketError(err)
	}
	if err := stream.connection.WriteJSON(socketmode.Response{EnvelopeID: envelopeID}); err != nil {
		return sanitizeSocketError(err)
	}
	return nil
}

func (stream *envelopeStream) closeTransport(graceful bool) {
	stream.closeOnce.Do(func() {
		stream.stateMu.Lock()
		stream.closed = true
		stream.stateMu.Unlock()
		close(stream.done)
		if graceful {
			stream.writeMu.Lock()
			_ = stream.connection.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
			stream.writeMu.Unlock()
		}
		_ = stream.connection.Close()
	})
}

func (stream *envelopeStream) isClosed() bool {
	stream.stateMu.RLock()
	defer stream.stateMu.RUnlock()
	return stream.closed
}

func validateEnvelopeID(value string) error {
	if value == "" || len(value) > slackapi.MaxIDBytes || !utf8.ValidString(value) || strings.IndexFunc(value, func(r rune) bool { return r <= 0x20 || r == 0x7f }) >= 0 {
		return errors.New("invalid envelope ID")
	}
	return nil
}

func deriveEventID(envelope slackapi.Envelope) string {
	hash := sha256.New()
	// Deliberately exclude EnvelopeID: Slack may assign a new transport
	// envelope when redelivering the same Events API event. Current content and
	// bounded file metadata are included with immutable source/order fields so
	// multiple imprecisely-timed edits cannot collapse to one fallback identity.
	for _, value := range []string{
		envelope.TeamID,
		string(envelope.Kind),
		envelope.ChannelID,
		string(envelope.MessageTS),
		string(envelope.EventTS),
		envelope.UserID,
		envelope.BotID,
		envelope.Subtype,
		string(envelope.ThreadTS),
		envelope.Text,
		envelope.Permalink,
	} {
		_, _ = hash.Write([]byte(value))
		_, _ = hash.Write([]byte{0})
	}
	for _, file := range envelope.Files {
		for _, value := range []string{file.ID, file.Name, file.MIMEType, file.Permalink} {
			_, _ = hash.Write([]byte(value))
			_, _ = hash.Write([]byte{0})
		}
		if file.SizeBytes != nil {
			_, _ = hash.Write([]byte(strconv.FormatInt(*file.SizeBytes, 10)))
		}
		_, _ = hash.Write([]byte{0})
	}
	return "sha256-" + hex.EncodeToString(hash.Sum(nil))
}

func validateWebSocketURL(value string, allowInsecure bool) error {
	if value == "" || len(value) > slackapi.MaxPermalinkBytes || !utf8.ValidString(value) {
		return errors.New("invalid WebSocket URL")
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" || parsed.Scheme != "wss" && !(allowInsecure && parsed.Scheme == "ws") {
		return errors.New("invalid WebSocket URL")
	}
	return nil
}

func cloneDialer(dialer *websocket.Dialer) *websocket.Dialer {
	clone := *dialer
	return &clone
}

func sanitizeSocketError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return sanitizeError(err)
	}
	var closeError *websocket.CloseError
	if errors.As(err, &closeError) {
		return disconnected()
	}
	var networkError interface{ Timeout() bool }
	if errors.As(err, &networkError) && networkError.Timeout() {
		return slackapi.NewExternalError(slackapi.ReasonTimeout, slackapi.DiagnosticTimeout, "", nil)
	}
	return slackapi.NewExternalError(slackapi.ReasonDisconnected, slackapi.DiagnosticDisconnected, "", nil)
}

func disconnected() error {
	return slackapi.NewExternalError(slackapi.ReasonDisconnected, slackapi.DiagnosticDisconnected, "", nil)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

package slackgo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/slack-go/slack"
	"github.com/stayradiated/mosscap/internal/slackapi"
)

const maxWebResponseBytes int64 = 16 << 20

// HTTPDoer is the part of an HTTP client used by slack-go. It is exported so
// callers and tests can supply a transport without exposing SDK types. Custom
// implementations must perform exactly one exchange; redirect traversal is
// owned and always refused by this adapter (ordinary *http.Client values are
// cloned with an overriding no-redirect policy).
type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

type webOptions struct {
	apiURL     string
	httpClient HTTPDoer
	maxFiles   int
	optionErr  error
}

// WebOption configures a Web adapter.
type WebOption func(*webOptions)

// WebAPIURL overrides Slack's Web API endpoint. It is intended for local tests.
func WebAPIURL(value string) WebOption {
	return func(options *webOptions) { options.apiURL = value }
}

// WebHTTPClient supplies the HTTP transport used by slack-go.
func WebHTTPClient(client HTTPDoer) WebOption {
	return func(options *webOptions) { options.httpClient = client }
}

// WebMaxFiles sets the active per-message file normalization limit. It must be
// in [1, slackapi.MaxFiles]; invalid configuration fails locally before I/O.
func WebMaxFiles(maximum int) WebOption {
	return func(options *webOptions) {
		if maximum < 1 || maximum > slackapi.MaxFiles {
			options.optionErr = errors.New("invalid Slack maximum file count")
			return
		}
		options.maxFiles = maximum
	}
}

// Web implements slackapi.Web with the pinned slack-go client.
type Web struct {
	client    *slack.Client
	token     string
	apiURL    string
	apiOrigin *url.URL
	apiHTTP   HTTPDoer
	fileHTTP  HTTPDoer
	maxFiles  int
	optionErr error

	identityMu sync.RWMutex
	identity   slackapi.AuthIdentity
}

// NewWeb creates a Web adapter. slack-go retries are explicitly left disabled;
// in particular, mutating requests are attempted exactly once.
func NewWeb(botToken string, options ...WebOption) *Web {
	configuration := webOptions{httpClient: http.DefaultClient, maxFiles: slackapi.MaxFiles}
	for _, option := range options {
		if option != nil {
			option(&configuration)
		}
	}
	if configuration.httpClient == nil {
		configuration.httpClient = http.DefaultClient
	}
	apiURL := configuration.apiURL
	if apiURL == "" {
		apiURL = defaultWebAPIURL
	}
	apiOrigin, parseErr := url.Parse(apiURL)
	if parseErr != nil || apiOrigin.Scheme == "" || apiOrigin.Host == "" {
		configuration.optionErr = errors.New("invalid Slack Web API URL")
		apiOrigin, _ = url.Parse(defaultWebAPIURL)
	}
	apiHTTP := refuseRedirects(configuration.httpClient)
	httpClient := validateProofResponses(apiHTTP, maxWebResponseBytes)
	sdkOptions := []slack.Option{
		slack.OptionHTTPClient(limitHTTPResponses(httpClient, maxWebResponseBytes)),
		slack.OptionRetry(0),
	}
	if configuration.apiURL != "" {
		sdkOptions = append(sdkOptions, slack.OptionAPIURL(withTrailingSlash(configuration.apiURL)))
	}
	return &Web{
		client: slack.New(botToken, sdkOptions...), token: botToken, apiURL: apiURL,
		apiOrigin: apiOrigin, apiHTTP: apiHTTP, fileHTTP: noAutoRedirect(configuration.httpClient),
		maxFiles: configuration.maxFiles, optionErr: configuration.optionErr,
	}
}

func (web *Web) Auth(ctx context.Context) (slackapi.AuthIdentity, error) {
	if web.optionErr != nil {
		return slackapi.AuthIdentity{}, invalidRequest()
	}
	response, err := web.client.AuthTestContext(ctx)
	if err != nil {
		return slackapi.AuthIdentity{}, sanitizeError(err)
	}
	if response == nil {
		return slackapi.AuthIdentity{}, invalidResponse()
	}
	identity := slackapi.AuthIdentity{
		TeamID:    response.TeamID,
		BotUserID: response.UserID,
		BotID:     response.BotID,
	}
	if err := slackapi.ValidateTeamID(identity.TeamID); err != nil {
		return slackapi.AuthIdentity{}, invalidResponse()
	}
	if err := slackapi.ValidateUserID(identity.BotUserID); err != nil {
		return slackapi.AuthIdentity{}, invalidResponse()
	}
	if identity.BotID != "" {
		if err := slackapi.ValidateBotID(identity.BotID); err != nil {
			return slackapi.AuthIdentity{}, invalidResponse()
		}
	}
	identity.Scopes, err = scopesFromHeader(response.Header)
	if err != nil {
		return slackapi.AuthIdentity{}, invalidResponse()
	}
	web.identityMu.Lock()
	web.identity = cloneIdentity(identity)
	web.identityMu.Unlock()
	return cloneIdentity(identity), nil
}

func (web *Web) Team(ctx context.Context, teamID string) (slackapi.Team, error) {
	if web.optionErr != nil {
		return slackapi.Team{}, invalidRequest()
	}
	if err := slackapi.ValidateTeamID(teamID); err != nil {
		return slackapi.Team{}, invalidRequest()
	}
	response, err := web.client.GetOtherTeamInfoContext(ctx, teamID)
	if err != nil {
		return slackapi.Team{}, sanitizeError(err)
	}
	if response == nil || response.ID != teamID {
		return slackapi.Team{}, slackapi.NewExternalError(slackapi.ReasonRejected, slackapi.DiagnosticTeamMismatch, "", nil)
	}
	if err := slackapi.ValidateTeamID(response.ID); err != nil {
		return slackapi.Team{}, invalidResponse()
	}
	return slackapi.Team{ID: response.ID}, nil
}

func (web *Web) User(ctx context.Context, userID string) (slackapi.User, error) {
	if web.optionErr != nil {
		return slackapi.User{}, invalidRequest()
	}
	if err := slackapi.ValidateUserID(userID); err != nil {
		return slackapi.User{}, invalidRequest()
	}
	response, err := web.client.GetUserInfoContext(ctx, userID)
	if err != nil {
		return slackapi.User{}, sanitizeError(err)
	}
	if response == nil || response.ID != userID {
		return slackapi.User{}, slackapi.NewExternalError(slackapi.ReasonRejected, slackapi.DiagnosticUserNotFound, "", nil)
	}
	if err := slackapi.ValidateUserID(response.ID); err != nil {
		return slackapi.User{}, invalidResponse()
	}
	if err := slackapi.ValidateTeamID(response.TeamID); err != nil {
		return slackapi.User{}, invalidResponse()
	}
	if response.Name != "" {
		if err := slackapi.ValidateUserName(response.Name); err != nil {
			return slackapi.User{}, invalidResponse()
		}
	}
	return slackapi.User{
		ID:          response.ID,
		TeamID:      response.TeamID,
		Name:        response.Name,
		Bot:         response.IsBot || response.IsAppUser || response.IsConnectorBot || response.IsWorkflowBot,
		Deactivated: response.Deleted,
	}, nil
}

func (web *Web) Channel(ctx context.Context, channelID string) (slackapi.Channel, error) {
	if web.optionErr != nil {
		return slackapi.Channel{}, invalidRequest()
	}
	if err := slackapi.ValidateChannelID(channelID); err != nil {
		return slackapi.Channel{}, invalidRequest()
	}
	response, err := web.client.GetConversationInfoContext(ctx, &slack.GetConversationInfoInput{ChannelID: channelID})
	if err != nil {
		return slackapi.Channel{}, sanitizeError(err)
	}
	if response == nil || response.ID != channelID {
		return slackapi.Channel{}, slackapi.NewExternalError(slackapi.ReasonRejected, slackapi.DiagnosticChannelNotFound, "", nil)
	}
	teamID := response.ContextTeamID
	if teamID == "" {
		web.identityMu.RLock()
		teamID = web.identity.TeamID
		web.identityMu.RUnlock()
	}
	if teamID == "" {
		identity, err := web.Auth(ctx)
		if err != nil {
			return slackapi.Channel{}, err
		}
		teamID = identity.TeamID
	}
	if err := slackapi.ValidateTeamID(teamID); err != nil {
		return slackapi.Channel{}, invalidResponse()
	}
	channel := slackapi.Channel{
		ID:         response.ID,
		TeamID:     teamID,
		Private:    response.IsPrivate,
		Member:     response.IsMember,
		Accessible: true,
		Archived:   response.IsArchived,
		ReadOnly:   response.IsReadOnly,
		IM:         response.IsIM,
		MPIM:       response.IsMpIM,
	}
	if err := slackapi.VerifyChannel(channel); err != nil {
		return slackapi.Channel{}, err
	}
	return channel, nil
}

func (web *Web) Scopes(ctx context.Context) ([]slackapi.Scope, error) {
	if web.optionErr != nil {
		return nil, invalidRequest()
	}
	web.identityMu.RLock()
	identity := cloneIdentity(web.identity)
	web.identityMu.RUnlock()
	if identity.TeamID == "" {
		var err error
		identity, err = web.Auth(ctx)
		if err != nil {
			return nil, err
		}
	}
	return append([]slackapi.Scope(nil), identity.Scopes...), nil
}

func (web *Web) Permalink(ctx context.Context, request slackapi.PermalinkRequest) (string, error) {
	if web.optionErr != nil {
		return "", invalidRequest()
	}
	if err := slackapi.ValidatePermalinkRequest(request); err != nil {
		return "", invalidRequest()
	}
	result, err := web.client.GetPermalinkContext(ctx, &slack.PermalinkParameters{
		Channel: request.ChannelID,
		Ts:      string(request.MessageTS),
	})
	if err != nil {
		return "", sanitizeError(err)
	}
	if err := validatePermalink(result); err != nil {
		return "", invalidResponse()
	}
	return result, nil
}

func (web *Web) History(ctx context.Context, request slackapi.HistoryRequest) (slackapi.MessagePage, error) {
	if web.optionErr != nil {
		return slackapi.MessagePage{}, invalidRequest()
	}
	if err := slackapi.ValidateHistoryRequest(request); err != nil {
		return slackapi.MessagePage{}, invalidRequest()
	}
	response, err := web.client.GetConversationHistoryContext(ctx, &slack.GetConversationHistoryParameters{
		ChannelID: request.ChannelID,
		Limit:     request.Limit,
		Cursor:    request.Cursor,
	})
	if err != nil {
		return slackapi.MessagePage{}, sanitizeError(err)
	}
	if response == nil || len(response.Messages) > request.Limit || len(response.Messages) > slackapi.MaxReadLimit {
		return slackapi.MessagePage{}, invalidResponse()
	}
	return web.messagePage(response.Messages, response.ResponseMetaData.NextCursor)
}

func (web *Web) Replies(ctx context.Context, request slackapi.RepliesRequest) (slackapi.MessagePage, error) {
	if web.optionErr != nil {
		return slackapi.MessagePage{}, invalidRequest()
	}
	if err := slackapi.ValidateRepliesRequest(request); err != nil {
		return slackapi.MessagePage{}, invalidRequest()
	}
	messages, _, cursor, err := web.client.GetConversationRepliesContext(ctx, &slack.GetConversationRepliesParameters{
		ChannelID: request.ChannelID,
		Timestamp: string(request.ThreadTS),
		Limit:     request.Limit,
		Cursor:    request.Cursor,
	})
	if err != nil {
		return slackapi.MessagePage{}, sanitizeError(err)
	}
	if len(messages) > request.Limit || len(messages) > slackapi.MaxReadLimit {
		return slackapi.MessagePage{}, invalidResponse()
	}
	return web.messagePage(messages, cursor)
}

func (web *Web) PostMessage(ctx context.Context, request slackapi.PostMessageRequest) (slackapi.MessageResult, error) {
	if web.optionErr != nil {
		return slackapi.MessageResult{}, invalidRequest()
	}
	if err := slackapi.ValidatePostMessageRequest(request); err != nil {
		return slackapi.MessageResult{}, invalidRequest()
	}
	options := []slack.MsgOption{slack.MsgOptionText(request.Text, false)}
	if request.ThreadTS != "" {
		options = append(options, slack.MsgOptionTS(string(request.ThreadTS)))
	}
	// slack-go v0.27.0's Web API surface has no client_msg_id option. The
	// durable request ID is therefore validated here but cannot be attached.
	channelID, timestamp, err := web.client.PostMessageContext(ctx, request.ChannelID, options...)
	if err != nil {
		return slackapi.MessageResult{}, sanitizeError(err)
	}
	return validateMessageResult(channelID, timestamp, request.ChannelID, "")
}

func (web *Web) UpdateMessage(ctx context.Context, request slackapi.UpdateMessageRequest) (slackapi.MessageResult, error) {
	if web.optionErr != nil {
		return slackapi.MessageResult{}, invalidRequest()
	}
	if err := slackapi.ValidateUpdateMessageRequest(request); err != nil {
		return slackapi.MessageResult{}, invalidRequest()
	}
	channelID, timestamp, _, err := web.client.UpdateMessageContext(ctx, request.ChannelID, string(request.MessageTS), slack.MsgOptionText(request.Text, false))
	if err != nil {
		return slackapi.MessageResult{}, sanitizeError(err)
	}
	return validateMessageResult(channelID, timestamp, request.ChannelID, request.MessageTS)
}

func (web *Web) DeleteMessage(ctx context.Context, request slackapi.DeleteMessageRequest) error {
	if web.optionErr != nil {
		return invalidRequest()
	}
	if err := slackapi.ValidateDeleteMessageRequest(request); err != nil {
		return invalidRequest()
	}
	channelID, timestamp, err := web.client.DeleteMessageContext(ctx, request.ChannelID, string(request.MessageTS))
	if err != nil {
		return sanitizeError(err)
	}
	_, err = validateMessageResult(channelID, timestamp, request.ChannelID, request.MessageTS)
	return err
}

func (web *Web) AddReaction(ctx context.Context, request slackapi.ReactionRequest) error {
	if web.optionErr != nil {
		return invalidRequest()
	}
	if err := slackapi.ValidateReactionRequest(request); err != nil {
		return invalidRequest()
	}
	err := web.client.AddReactionContext(ctx, request.Emoji, slack.ItemRef{Channel: request.ChannelID, Timestamp: string(request.MessageTS)})
	if slackErrorCode(err) == "already_reacted" {
		// The requested end state already holds; this is a conclusive success.
		return nil
	}
	return sanitizeError(err)
}

func (web *Web) RemoveReaction(ctx context.Context, request slackapi.ReactionRequest) error {
	if web.optionErr != nil {
		return invalidRequest()
	}
	if err := slackapi.ValidateReactionRequest(request); err != nil {
		return invalidRequest()
	}
	err := web.client.RemoveReactionContext(ctx, request.Emoji, slack.ItemRef{Channel: request.ChannelID, Timestamp: string(request.MessageTS)})
	if slackErrorCode(err) == "no_reaction" {
		// The reaction is already absent, which is the requested end state.
		return nil
	}
	return sanitizeError(err)
}

func (web *Web) messagePage(messages []slack.Message, cursor string) (slackapi.MessagePage, error) {
	if err := validateCursor(cursor); err != nil {
		return slackapi.MessagePage{}, invalidResponse()
	}
	page := slackapi.MessagePage{Messages: make([]slackapi.Message, 0, len(messages)), NextCursor: cursor}
	web.identityMu.RLock()
	botUserID := web.identity.BotUserID
	web.identityMu.RUnlock()
	for index := range messages {
		message, err := normalizeMessage(&messages[index], botUserID, web.maxFiles)
		if err != nil {
			return slackapi.MessagePage{}, invalidResponse()
		}
		page.Messages = append(page.Messages, message)
	}
	return page, nil
}

func normalizeMessage(message *slack.Message, botUserID string, maxFiles int) (slackapi.Message, error) {
	if message == nil {
		return slackapi.Message{}, errors.New("nil message")
	}
	timestamp, err := slackapi.ParseTimestamp(message.Timestamp)
	if err != nil {
		return slackapi.Message{}, err
	}
	thread := timestamp
	if message.ThreadTimestamp != "" {
		thread, err = slackapi.ParseTimestamp(message.ThreadTimestamp)
		if err != nil {
			return slackapi.Message{}, err
		}
	}
	authorID := message.User
	if authorID == "" {
		authorID = message.BotID
	}
	if err := validateBoundedID(authorID); err != nil {
		return slackapi.Message{}, err
	}
	if err := validateText(message.Text); err != nil {
		return slackapi.Message{}, err
	}
	if len(message.Reactions) > slackapi.MaxFiles {
		return slackapi.Message{}, errors.New("too many reactions")
	}
	reactions := make([]slackapi.Reaction, 0, len(message.Reactions))
	for _, reaction := range message.Reactions {
		emoji, err := slackapi.NormalizeEmoji(reaction.Name)
		if err != nil || emoji != reaction.Name || reaction.Count < 0 || len(reaction.Users) > slackapi.MaxFiles {
			return slackapi.Message{}, errors.New("invalid reaction")
		}
		mine := false
		for _, userID := range reaction.Users {
			if err := validateBoundedID(userID); err != nil {
				return slackapi.Message{}, err
			}
			mine = mine || botUserID != "" && userID == botUserID
		}
		reactions = append(reactions, slackapi.Reaction{Name: emoji, Count: reaction.Count, Mine: mine})
	}
	files, err := normalizeFiles(message.Files, maxFiles)
	if err != nil {
		return slackapi.Message{}, err
	}
	return slackapi.Message{
		MessageTS: timestamp,
		ThreadTS:  thread,
		AuthorID:  authorID,
		Text:      message.Text,
		Reactions: reactions,
		Files:     files,
		Forwarded: normalizeForwardedAttachments(message.Attachments),
	}, nil
}

func validateMessageResult(channelID, timestamp, expectedChannel string, expectedTimestamp slackapi.Timestamp) (slackapi.MessageResult, error) {
	parsed, err := slackapi.ParseTimestamp(timestamp)
	if err != nil || channelID != expectedChannel || expectedTimestamp != "" && parsed != expectedTimestamp {
		return slackapi.MessageResult{}, invalidResponse()
	}
	if err := slackapi.ValidateChannelID(channelID); err != nil {
		return slackapi.MessageResult{}, invalidResponse()
	}
	return slackapi.MessageResult{ChannelID: channelID, MessageTS: parsed}, nil
}

func scopesFromHeader(header http.Header) ([]slackapi.Scope, error) {
	value := header.Get("X-OAuth-Scopes")
	if strings.TrimSpace(value) == "" {
		return []slackapi.Scope{}, nil
	}
	parts := strings.Split(value, ",")
	if len(parts) > slackapi.MaxFiles {
		return nil, errors.New("too many scopes")
	}
	set := make(map[slackapi.Scope]struct{}, len(parts))
	for _, part := range parts {
		scope := slackapi.Scope(strings.TrimSpace(part))
		if !scope.Valid() {
			return nil, errors.New("invalid scope")
		}
		set[scope] = struct{}{}
	}
	result := make([]slackapi.Scope, 0, len(set))
	for scope := range set {
		result = append(result, scope)
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result, nil
}

func cloneIdentity(identity slackapi.AuthIdentity) slackapi.AuthIdentity {
	identity.Scopes = append([]slackapi.Scope(nil), identity.Scopes...)
	return identity
}

func validateCursor(value string) error {
	if len(value) > slackapi.MaxCursorBytes || !utf8.ValidString(value) || strings.IndexFunc(value, func(r rune) bool { return unicode.IsControl(r) || unicode.IsSpace(r) }) >= 0 {
		return errors.New("invalid cursor")
	}
	return nil
}

func validateBoundedID(value string) error {
	if value == "" || len(value) > slackapi.MaxIDBytes || !utf8.ValidString(value) || strings.IndexFunc(value, func(r rune) bool { return r <= 0x20 || r == 0x7f }) >= 0 {
		return errors.New("invalid ID")
	}
	return nil
}

func validateText(value string) error {
	if len(value) > slackapi.MaxTextBytes || !utf8.ValidString(value) || strings.IndexByte(value, 0) >= 0 {
		return errors.New("invalid text")
	}
	return nil
}

func withTrailingSlash(value string) string {
	if strings.HasSuffix(value, "/") {
		return value
	}
	return value + "/"
}

type boundedHTTPClient struct {
	client HTTPDoer
	limit  int64
}

func limitHTTPResponses(client HTTPDoer, limit int64) HTTPDoer {
	return boundedHTTPClient{client: client, limit: limit}
}

// refuseRedirects prevents authorization headers, form tokens, and mutation
// bodies from being replayed to any redirect target. A supplied *http.Client is
// cloned so the caller's redirect policy cannot weaken this invariant.
func refuseRedirects(client HTTPDoer) HTTPDoer {
	if httpClient, ok := client.(*http.Client); ok {
		clone := *httpClient
		clone.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		client = &clone
	}
	return redirectRefusingDoer{client: client}
}

type redirectRefusingDoer struct{ client HTTPDoer }

func (client redirectRefusingDoer) Do(request *http.Request) (*http.Response, error) {
	response, err := client.client.Do(request)
	if err != nil || response == nil {
		return response, err
	}
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		if response.Body != nil {
			_ = response.Body.Close()
		}
		return nil, errRedirectRefused
	}
	return response, nil
}

// validateProofResponses closes a slack-go gap where reactions.add/remove can
// treat an ok:false response with no error string (or a missing ok field) as
// success. The body is restored byte-for-byte for ordinary SDK decoding.
func validateProofResponses(client HTTPDoer, limit int64) HTTPDoer {
	return proofValidatingDoer{client: client, limit: limit}
}

type proofValidatingDoer struct {
	client HTTPDoer
	limit  int64
}

func (client proofValidatingDoer) Do(request *http.Request) (*http.Response, error) {
	response, err := client.client.Do(request)
	if err != nil || response == nil || response.Body == nil || !isReactionPath(request.URL.Path) {
		return response, err
	}
	body, readErr := io.ReadAll(io.LimitReader(response.Body, client.limit+1))
	_ = response.Body.Close()
	if readErr != nil || int64(len(body)) > client.limit {
		return nil, errResponseTooLarge
	}
	var proof struct {
		OK    *bool  `json:"ok"`
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &proof) != nil || proof.OK == nil || !*proof.OK && strings.TrimSpace(proof.Error) == "" {
		return nil, errUnprovenResponse
	}
	response.Body = io.NopCloser(bytes.NewReader(body))
	response.ContentLength = int64(len(body))
	return response, nil
}

func isReactionPath(path string) bool {
	return strings.HasSuffix(path, "/reactions.add") || strings.HasSuffix(path, "/reactions.remove")
}

func (client boundedHTTPClient) Do(request *http.Request) (*http.Response, error) {
	response, err := client.client.Do(request)
	if err != nil || response == nil || response.Body == nil {
		return response, err
	}
	response.Body = &boundedReadCloser{reader: io.LimitReader(response.Body, client.limit+1), closer: response.Body, remaining: client.limit + 1}
	return response, nil
}

type boundedReadCloser struct {
	reader    io.Reader
	closer    io.Closer
	remaining int64
}

func (body *boundedReadCloser) Read(buffer []byte) (int, error) {
	if body.remaining <= 0 {
		return 0, errResponseTooLarge
	}
	if int64(len(buffer)) > body.remaining {
		buffer = buffer[:body.remaining]
	}
	count, err := body.reader.Read(buffer)
	body.remaining -= int64(count)
	if body.remaining == 0 && err == nil {
		err = errResponseTooLarge
	}
	return count, err
}

func (body *boundedReadCloser) Close() error { return body.closer.Close() }

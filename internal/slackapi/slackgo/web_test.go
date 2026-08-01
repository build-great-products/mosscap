package slackgo

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stayradiated/mosscap/internal/slackapi"
)

func TestWebAdapterTranslationAndContextOperations(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	calls := make(map[string]int)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		mu.Lock()
		calls[request.URL.Path]++
		mu.Unlock()
		if err := request.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
		}
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/auth.test":
			writer.Header().Set("X-OAuth-Scopes", "users:read, chat:write,users:read")
			writeJSON(t, writer, map[string]any{"ok": true, "team_id": "T012ABC", "user_id": "U012BOT", "bot_id": "B012BOT"})
		case "/team.info":
			requireForm(t, request.Form, "team", "T012ABC")
			writeJSON(t, writer, map[string]any{"ok": true, "team": map[string]any{"id": "T012ABC", "name": strings.Repeat("ignored", 100)}})
		case "/users.info":
			requireForm(t, request.Form, "user", "U012USR")
			writeJSON(t, writer, map[string]any{"ok": true, "user": map[string]any{"id": "U012USR", "team_id": "T012ABC", "is_bot": false, "deleted": false}})
		case "/conversations.info":
			requireForm(t, request.Form, "channel", "C012ABC")
			writeJSON(t, writer, map[string]any{"ok": true, "channel": map[string]any{"id": "C012ABC", "is_private": true, "is_member": true}})
		case "/chat.getPermalink":
			requireForm(t, request.Form, "channel", "C012ABC")
			writeJSON(t, writer, map[string]any{"ok": true, "permalink": "https://example.slack.com/archives/C012ABC/p1700000000000001"})
		case "/conversations.history", "/conversations.replies":
			requireForm(t, request.Form, "channel", "C012ABC")
			requireForm(t, request.Form, "limit", "2")
			file := map[string]any{"id": "F012ABC", "name": "note.txt", "mimetype": "text/plain", "permalink": "https://example.slack.com/files/F012ABC", "size": 12}
			message := map[string]any{"ts": "1700000000.000001", "thread_ts": "1700000000.000001", "user": "U012USR", "text": "bounded text", "files": []any{file}, "reactions": []any{map[string]any{"name": "eyes", "count": 1, "users": []string{"U012BOT"}}}}
			writeJSON(t, writer, map[string]any{"ok": true, "messages": []any{message}, "response_metadata": map[string]any{"next_cursor": "cursor-2"}})
		case "/chat.postMessage":
			requireForm(t, request.Form, "channel", "C012ABC")
			requireForm(t, request.Form, "text", "exact text")
			requireForm(t, request.Form, "thread_ts", "1700000000.000001")
			writeJSON(t, writer, map[string]any{"ok": true, "channel": "C012ABC", "ts": "1700000000.000002"})
		case "/chat.update":
			requireForm(t, request.Form, "channel", "C012ABC")
			requireForm(t, request.Form, "ts", "1700000000.000002")
			writeJSON(t, writer, map[string]any{"ok": true, "channel": "C012ABC", "ts": "1700000000.000002", "text": "updated"})
		case "/chat.delete":
			requireForm(t, request.Form, "channel", "C012ABC")
			writeJSON(t, writer, map[string]any{"ok": true, "channel": "C012ABC", "ts": "1700000000.000002"})
		case "/reactions.add", "/reactions.remove":
			requireForm(t, request.Form, "channel", "C012ABC")
			requireForm(t, request.Form, "timestamp", "1700000000.000002")
			requireForm(t, request.Form, "name", "eyes")
			writeJSON(t, writer, map[string]any{"ok": true})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	web := NewWeb("xoxb-test-canary", WebAPIURL(server.URL))
	ctx := context.Background()
	identity, err := web.Auth(ctx)
	if err != nil {
		t.Fatalf("Auth: %v", err)
	}
	if identity.TeamID != "T012ABC" || identity.BotUserID != "U012BOT" || len(identity.Scopes) != 2 || identity.Scopes[0] != slackapi.ScopeChatWrite {
		t.Fatalf("unexpected identity: %#v", identity)
	}
	team, err := web.Team(ctx, "T012ABC")
	if err != nil || team.ID != "T012ABC" {
		t.Fatalf("Team = %#v, %v", team, err)
	}
	user, err := web.User(ctx, "U012USR")
	if err != nil || user.ID != "U012USR" || user.TeamID != "T012ABC" || user.Bot || user.Deactivated {
		t.Fatalf("User = %#v, %v", user, err)
	}
	channel, err := web.Channel(ctx, "C012ABC")
	if err != nil || channel.TeamID != "T012ABC" || !channel.Private || !channel.Member || !channel.Accessible {
		t.Fatalf("Channel = %#v, %v", channel, err)
	}
	permalink, err := web.Permalink(ctx, slackapi.PermalinkRequest{ChannelID: "C012ABC", MessageTS: "1700000000.000001"})
	if err != nil || !strings.HasPrefix(permalink, "https://") {
		t.Fatalf("Permalink = %q, %v", permalink, err)
	}
	history, err := web.History(ctx, slackapi.HistoryRequest{ChannelID: "C012ABC", Limit: 2})
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	assertPage(t, history)
	replies, err := web.Replies(ctx, slackapi.RepliesRequest{ChannelID: "C012ABC", ThreadTS: "1700000000.000001", Limit: 2})
	if err != nil {
		t.Fatalf("Replies: %v", err)
	}
	assertPage(t, replies)

	posted, err := web.PostMessage(ctx, slackapi.PostMessageRequest{ChannelID: "C012ABC", ThreadTS: "1700000000.000001", Text: "exact text", ClientMessageID: "eff_123"})
	if err != nil || posted.MessageTS != "1700000000.000002" {
		t.Fatalf("PostMessage = %#v, %v", posted, err)
	}
	updated, err := web.UpdateMessage(ctx, slackapi.UpdateMessageRequest{ChannelID: "C012ABC", MessageTS: "1700000000.000002", Text: "updated"})
	if err != nil || updated.MessageTS != "1700000000.000002" {
		t.Fatalf("UpdateMessage = %#v, %v", updated, err)
	}
	if err := web.DeleteMessage(ctx, slackapi.DeleteMessageRequest{ChannelID: "C012ABC", MessageTS: "1700000000.000002"}); err != nil {
		t.Fatalf("DeleteMessage: %v", err)
	}
	reaction := slackapi.ReactionRequest{ChannelID: "C012ABC", MessageTS: "1700000000.000002", Emoji: "eyes"}
	if err := web.AddReaction(ctx, reaction); err != nil {
		t.Fatalf("AddReaction: %v", err)
	}
	if err := web.RemoveReaction(ctx, reaction); err != nil {
		t.Fatalf("RemoveReaction: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls["/chat.postMessage"] != 1 || calls["/chat.update"] != 1 || calls["/chat.delete"] != 1 {
		t.Fatalf("writes were not exactly once: %#v", calls)
	}
}

func TestWebAdapterClassifiesWithoutLeakingExternalText(t *testing.T) {
	t.Parallel()
	const canary = "xoxb-super-secret-message-canary"
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls++
		writer.Header().Set("Content-Type", "application/json")
		writeJSON(t, writer, map[string]any{"ok": false, "error": "channel_not_found-" + canary})
	}))
	defer server.Close()
	web := NewWeb(canary, WebAPIURL(server.URL))
	_, err := web.History(context.Background(), slackapi.HistoryRequest{ChannelID: "C012ABC", Limit: 1})
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), canary) || strings.Contains(err.Error(), "channel_not_found-") {
		t.Fatalf("error leaked external text: %v", err)
	}
	failure := slackapi.Classify(err, slackapi.FailureRead)
	if failure.Class != slackapi.FailureConclusive || failure.Diagnostic != slackapi.DiagnosticRejected {
		t.Fatalf("classification = %#v", failure)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
}

func TestWebAdapterDisablesRateLimitRetries(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		writer.Header().Set("Retry-After", "1")
		writer.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()
	web := NewWeb("xoxb-test", WebAPIURL(server.URL))
	_, err := web.PostMessage(context.Background(), slackapi.PostMessageRequest{ChannelID: "C012ABC", Text: "text", ClientMessageID: "eff_1"})
	if err == nil {
		t.Fatal("expected rate limit error")
	}
	failure := slackapi.Classify(err, slackapi.FailureAfterSubmit)
	if failure.Class != slackapi.FailureAmbiguous || failure.Diagnostic != slackapi.DiagnosticRateLimited {
		t.Fatalf("classification = %#v", failure)
	}
	if calls.Load() != 1 {
		t.Fatalf("calls = %d, want exactly one", calls.Load())
	}
}

func TestWebAdapterPostSubmitTimeoutIsAmbiguousAndNotRetried(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		select {
		case <-request.Context().Done():
		case <-release:
		}
	}))
	defer server.Close()
	defer close(release)
	web := NewWeb("xoxb-test", WebAPIURL(server.URL))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := web.PostMessage(ctx, slackapi.PostMessageRequest{ChannelID: "C012ABC", Text: "text", ClientMessageID: "eff_1"})
	failure := slackapi.Classify(err, slackapi.FailureAfterSubmit)
	if failure.Class != slackapi.FailureAmbiguous || failure.Diagnostic != slackapi.DiagnosticTimeout {
		t.Fatalf("classification = %#v (%v)", failure, err)
	}
	if calls.Load() != 1 {
		t.Fatalf("calls = %d, want exactly one", calls.Load())
	}
}

func TestWebAdapterMalformedResponseIsUnavailableForRead(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"ok":true,"messages":`))
	}))
	defer server.Close()
	web := NewWeb("xoxb-test", WebAPIURL(server.URL))
	_, err := web.History(context.Background(), slackapi.HistoryRequest{ChannelID: "C012ABC", Limit: 1})
	failure := slackapi.Classify(err, slackapi.FailureRead)
	if failure.Class != slackapi.FailureUnavailable || failure.Diagnostic != slackapi.DiagnosticMalformedResponse {
		t.Fatalf("classification = %#v (%v)", failure, err)
	}
}

func TestWebAdapterRejectsLocallyBeforeIO(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer server.Close()
	web := NewWeb("xoxb-test", WebAPIURL(server.URL))
	_, err := web.History(context.Background(), slackapi.HistoryRequest{ChannelID: "not-a-channel", Limit: 1})
	failure := slackapi.Classify(err, slackapi.FailureRead)
	if failure.Class != slackapi.FailureConclusive || failure.Diagnostic != slackapi.DiagnosticInvalidRequest || calls.Load() != 0 {
		t.Fatalf("failure=%#v calls=%d", failure, calls.Load())
	}
}

func TestWebAdapterRefusesRedirectsWithoutTokenOrWriteLeakage(t *testing.T) {
	t.Parallel()
	const token = "xoxb-redirect-canary"
	var redirectedCalls atomic.Int32
	var leaked atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		redirectedCalls.Add(1)
		_ = request.ParseForm()
		if strings.Contains(request.Header.Get("Authorization"), token) || strings.Contains(request.Form.Encode(), token) || request.Form.Get("text") != "" {
			leaked.Store(true)
		}
	}))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, target.URL+request.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer source.Close()

	web := NewWeb(token, WebAPIURL(source.URL))
	_, readErr := web.History(context.Background(), slackapi.HistoryRequest{ChannelID: "C012ABC", Limit: 1})
	_, writeErr := web.PostMessage(context.Background(), slackapi.PostMessageRequest{ChannelID: "C012ABC", Text: "write-canary", ClientMessageID: "eff_1"})
	if readErr == nil || writeErr == nil {
		t.Fatalf("redirected read/write errors = %v / %v", readErr, writeErr)
	}
	if redirectedCalls.Load() != 0 || leaked.Load() {
		t.Fatalf("redirect target calls=%d leaked=%t", redirectedCalls.Load(), leaked.Load())
	}
}

func TestReactionRequiresProvenOKResponse(t *testing.T) {
	t.Parallel()
	for _, response := range []string{`{}`, `{"ok":false}`, `{"ok":"yes"}`} {
		response := response
		t.Run(response, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("Content-Type", "application/json")
				_, _ = writer.Write([]byte(response))
			}))
			defer server.Close()
			web := NewWeb("xoxb-test", WebAPIURL(server.URL))
			err := web.AddReaction(context.Background(), slackapi.ReactionRequest{ChannelID: "C012ABC", MessageTS: "1700000000.000001", Emoji: "eyes"})
			failure := slackapi.Classify(err, slackapi.FailureAfterSubmit)
			if failure.Class != slackapi.FailureAmbiguous || failure.Diagnostic != slackapi.DiagnosticMalformedResponse {
				t.Fatalf("failure = %#v (%v)", failure, err)
			}
		})
	}
}

func TestHTTPStatusClassificationDependsOnSubmissionPoint(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name       string
		status     int
		diagnostic slackapi.DiagnosticCode
	}{
		{"408", http.StatusRequestTimeout, slackapi.DiagnosticTimeout},
		{"429", http.StatusTooManyRequests, slackapi.DiagnosticRateLimited},
		{"500", http.StatusInternalServerError, slackapi.DiagnosticUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				if test.status == http.StatusTooManyRequests {
					writer.Header().Set("Retry-After", "1")
				}
				writer.WriteHeader(test.status)
			}))
			defer server.Close()
			web := NewWeb("xoxb-test", WebAPIURL(server.URL))
			_, readErr := web.History(context.Background(), slackapi.HistoryRequest{ChannelID: "C012ABC", Limit: 1})
			read := slackapi.Classify(readErr, slackapi.FailureRead)
			if read.Class != slackapi.FailureUnavailable || read.Diagnostic != test.diagnostic {
				t.Fatalf("read failure = %#v (%v)", read, readErr)
			}
			_, mutationErr := web.PostMessage(context.Background(), slackapi.PostMessageRequest{ChannelID: "C012ABC", Text: "text", ClientMessageID: "eff_1"})
			mutation := slackapi.Classify(mutationErr, slackapi.FailureAfterSubmit)
			if mutation.Class != slackapi.FailureAmbiguous || mutation.Diagnostic != test.diagnostic {
				t.Fatalf("mutation failure = %#v (%v)", mutation, mutationErr)
			}
		})
	}
}

func TestWebMaxFilesIsActivelyEnforced(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		file := func(id string) map[string]any { return map[string]any{"id": id, "name": "f", "size": 1} }
		writeJSON(t, writer, map[string]any{"ok": true, "messages": []any{map[string]any{
			"ts": "1700000000.000001", "user": "U012USR", "text": "text", "files": []any{file("F001"), file("F002")},
		}}})
	}))
	defer server.Close()
	web := NewWeb("xoxb-test", WebAPIURL(server.URL), WebMaxFiles(1))
	_, err := web.History(context.Background(), slackapi.HistoryRequest{ChannelID: "C012ABC", Limit: 1})
	failure := slackapi.Classify(err, slackapi.FailureRead)
	if failure.Diagnostic != slackapi.DiagnosticMalformedResponse {
		t.Fatalf("failure = %#v (%v)", failure, err)
	}
}

func assertPage(t *testing.T, page slackapi.MessagePage) {
	t.Helper()
	if page.NextCursor != "cursor-2" || len(page.Messages) != 1 {
		t.Fatalf("page = %#v", page)
	}
	message := page.Messages[0]
	if message.MessageTS != "1700000000.000001" || message.ThreadTS != message.MessageTS || message.AuthorID != "U012USR" || message.Text != "bounded text" {
		t.Fatalf("message = %#v", message)
	}
	if len(message.Files) != 1 || message.Files[0].ID != "F012ABC" || len(message.Reactions) != 1 || !message.Reactions[0].Mine {
		t.Fatalf("message metadata = %#v", message)
	}
}

func requireForm(t *testing.T, form url.Values, key, expected string) {
	t.Helper()
	if actual := form.Get(key); actual != expected {
		t.Errorf("form %s = %q, want %q", key, actual, expected)
	}
}

func writeJSON(t *testing.T, writer http.ResponseWriter, value any) {
	t.Helper()
	if err := json.NewEncoder(writer).Encode(value); err != nil {
		t.Errorf("encode response: %v", err)
	}
}

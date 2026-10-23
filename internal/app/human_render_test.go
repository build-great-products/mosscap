package app

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stayradiated/mosscap/internal/protocol"
)

func TestHumanRenderGoldenCardsAndRows(t *testing.T) {
	var out bytes.Buffer
	input := map[string]any{"tasks": []any{map[string]any{
		"task_id": "AbCdEfGh", "state": "open", "source_kind": "slack", "version": 3,
		"updated_at": "2026-01-01T00:00:00Z", "text": "Please investigate\n  [31mFAILED[0m\n☃",
	}}}
	if err := renderHuman(&out, input); err != nil {
		t.Fatal(err)
	}
	const want = "AbCdEfGh  open  slack  v3  updated 2026-01-01T00:00:00Z\n│ Please investigate\n│   \\u001b[31mFAILED\\u001b[0m\n│ ☃\n"
	if got := out.String(); got != want {
		t.Fatalf("human task card\nwant:\n%s\ngot:\n%s", want, got)
	}
}

func TestHumanRenderDoesNotSerializeJSONOrControls(t *testing.T) {
	var out bytes.Buffer
	if err := renderHuman(&out, map[string]any{"messages": []any{map[string]any{"message_ts": "1.000001", "author_user_id": "U1", "text": "hello\rworld"}}}); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if strings.ContainsAny(got, "{}\r") || !strings.Contains(got, `hello\u000dworld`) {
		t.Fatalf("unsafe or JSON-like output: %q", got)
	}
}

func TestHumanRenderKeepsMutationRequestAndWarningVisible(t *testing.T) {
	var out bytes.Buffer
	input := map[string]any{
		"request_id": "req_123", "warning": "duplicate submission is possible",
		"result": map[string]any{"effect_id": "eff_123", "kind": "send", "state": "ambiguous"},
	}
	if err := renderHuman(&out, input); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Request: req_123", "Warning: duplicate submission is possible", "eff_123", "ambiguous"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("human mutation output missing %q: %q", want, out.String())
		}
	}
}

func TestHumanRenderStatusLabelsGatewayAndSlackAccurately(t *testing.T) {
	var out bytes.Buffer
	if err := renderHuman(&out, map[string]any{"slack": map[string]any{"state": "disconnected"}}); err != nil {
		t.Fatal(err)
	}
	if got, want := out.String(), "Gateway  reachable\nSlack    disconnected\n"; got != want {
		t.Fatalf("status = %q, want %q", got, want)
	}
}

func TestTrustedUserNamesEscapeTerminalControls(t *testing.T) {
	var out bytes.Buffer
	a := App{Out: &out}
	users := protocol.TrustedUserListDTO{SlackUserIDs: []string{"U1"}, SlackUserNames: map[string]string{"U1": "name\x1b[2J"}}
	if err := a.renderTrustedUsers(users, false); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "\x1b") || !strings.Contains(out.String(), `@name\u001b[2J`) {
		t.Fatalf("unsafe trusted-user output: %q", out.String())
	}
}

func TestEscapeTerminalRetainsPrintableUnicode(t *testing.T) {
	if got, want := escapeTerminal("é ☃\t\x1b"), `é ☃\u0009\u001b`; got != want {
		t.Fatalf("escapeTerminal = %q, want %q", got, want)
	}
}

func TestPaginationContinuationIsHumanOnlyAndShellSafe(t *testing.T) {
	var out bytes.Buffer
	page := map[string]any{"next_cursor": "cursor with ' quote"}
	if err := renderPaginationContinuation(&out, page, "mosscap task list --state open --limit 20"); err != nil {
		t.Fatal(err)
	}
	want := "More results available.\nNext: mosscap task list --state open --limit 20 --cursor 'cursor with '\"'\"' quote'\n"
	if got := out.String(); got != want {
		t.Fatalf("continuation = %q, want %q", got, want)
	}
	out.Reset()
	if err := renderPaginationContinuation(&out, map[string]any{}, "mosscap task list"); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Fatalf("unexpected final-page continuation: %q", out.String())
	}
}

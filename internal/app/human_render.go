package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode"
)

// renderHuman is deliberately a presentation boundary: it receives a decoded
// copy, so escaping cannot affect DTOs, protocol frames, or durable text.
func renderHuman(w io.Writer, value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	var v any
	if err := json.Unmarshal(encoded, &v); err != nil {
		return err
	}
	var out bytes.Buffer
	renderHumanValue(&out, v)
	_, err = io.Copy(w, &out)
	return err
}

func renderHumanValue(out *bytes.Buffer, v any) {
	m, ok := v.(map[string]any)
	if !ok {
		fmt.Fprintln(out, present(v))
		return
	}
	// Mutation envelopes consistently expose both the durable request identity
	// and concise resulting resource state. Warnings belong to the envelope,
	// so print them before descending into its result.
	if requestID, ok := m["request_id"].(string); ok {
		fmt.Fprintf(out, "Request: %s\n", present(requestID))
		renderWarnings(out, m)
		for _, key := range []string{"result", "effect"} {
			if nested, exists := m[key]; exists {
				renderHumanValue(out, nested)
				return
			}
		}
	}
	switch {
	case m["tasks"] != nil:
		tasks, _ := m["tasks"].([]any)
		if len(tasks) == 0 {
			fmt.Fprintln(out, "No tasks.")
			return
		}
		for i, task := range tasks {
			if i > 0 {
				fmt.Fprintln(out)
			}
			renderTaskCard(out, task.(map[string]any))
		}
	case m["messages"] != nil:
		messages, _ := m["messages"].([]any)
		if len(messages) == 0 {
			fmt.Fprintln(out, "No Slack messages.")
			return
		}
		for i, message := range messages {
			if i > 0 {
				fmt.Fprintln(out)
			}
			renderMessageCard(out, message.(map[string]any))
		}
	case m["effects"] != nil:
		renderRows(out, "Effects", m["effects"].([]any), []string{"effect_id", "kind", "state", "related_task_id", "diagnostic_code"})
	case m["effect_id"] != nil:
		renderRows(out, "Effect", []any{m}, []string{"effect_id", "kind", "state", "related_task_id", "target_message_ts", "thread_ts", "diagnostic_code"})
	case m["task"] != nil && m["thread"] != nil && m["effects"] != nil:
		renderTaskContext(out, m)
	case m["text"] != nil && m["task_id"] != nil:
		renderTaskCard(out, m)
	case m["text"] != nil && m["message_ts"] != nil:
		renderMessageCard(out, m)
	case m["slack"] != nil:
		renderGatewayStatus(out, m)
	case m["effect"] != nil:
		renderHumanValue(out, m["effect"])
		if task, ok := m["task"]; ok {
			fmt.Fprintln(out, "Task:")
			renderHumanValue(out, task)
		}
	default:
		renderFields(out, m)
	}
}

func renderWarnings(out *bytes.Buffer, m map[string]any) {
	if warning := present(m["warning"]); warning != "" {
		fmt.Fprintf(out, "Warning: %s\n", warning)
	}
	warnings, _ := m["warnings"].([]any)
	for _, warning := range warnings {
		if text := present(warning); text != "" {
			fmt.Fprintf(out, "Warning: %s\n", text)
		}
	}
}

func renderTaskContext(out *bytes.Buffer, context map[string]any) {
	fmt.Fprintln(out, "Task:")
	if task, ok := context["task"].(map[string]any); ok {
		renderTaskCard(out, task)
	}
	renderWarnings(out, context)
	if thread, ok := context["thread"].(map[string]any); ok {
		fmt.Fprintf(out, "Thread: %s\n", present(thread["status"]))
		if messages, _ := thread["messages"].([]any); len(messages) > 0 {
			for _, message := range messages {
				if m, ok := message.(map[string]any); ok {
					renderMessageCard(out, m)
				}
			}
		}
	}
	if effects, ok := context["effects"].(map[string]any); ok {
		if count := present(effects["non_confirmed_count"]); count != "" && count != "0" {
			fmt.Fprintf(out, "Related effects requiring attention: %s\n", count)
		}
	}
}

func renderTaskCard(out *bytes.Buffer, task map[string]any) {
	fmt.Fprintf(out, "%s  %s  %s  v%v", present(task["task_id"]), present(task["state"]), present(task["source_kind"]), task["version"])
	if updated := present(task["updated_at"]); updated != "" {
		fmt.Fprintf(out, "  updated %s", updated)
	}
	fmt.Fprintln(out)
	renderCardText(out, fmt.Sprint(task["text"]))
}
func renderMessageCard(out *bytes.Buffer, message map[string]any) {
	fmt.Fprintf(out, "%s  Slack", present(message["message_ts"]))
	if author := present(message["author_user_id"]); author != "" {
		fmt.Fprintf(out, "  %s", author)
	}
	fmt.Fprintln(out)
	renderCardText(out, fmt.Sprint(message["text"]))
}
func renderCardText(out *bytes.Buffer, text string) {
	for _, line := range strings.Split(text, "\n") {
		fmt.Fprintf(out, "│ %s\n", escapeTerminal(line))
	}
}
func renderRows(out *bytes.Buffer, heading string, rows []any, fields []string) {
	if len(rows) == 0 {
		fmt.Fprintf(out, "No %s.\n", strings.ToLower(heading))
		return
	}
	fmt.Fprintln(out, heading+":")
	for _, raw := range rows {
		m, _ := raw.(map[string]any)
		values := make([]string, 0, len(fields))
		for _, field := range fields {
			if s := present(m[field]); s != "" {
				values = append(values, s)
			}
		}
		fmt.Fprintln(out, "  "+strings.Join(values, "  "))
	}
}
func renderGatewayStatus(out *bytes.Buffer, m map[string]any) {
	slack, _ := m["slack"].(map[string]any)
	if slack == nil {
		fmt.Fprintln(out, "Gateway  unavailable")
		return
	}
	// Reaching this status response proves the local gateway is reachable;
	// Slack state is reported separately and must not be relabelled as gateway
	// health.
	fmt.Fprintf(out, "Gateway  reachable\nSlack    %s\n", present(slack["state"]))
	if effects, ok := slack["effects"].(map[string]any); ok {
		fmt.Fprintf(out, "Action   %s action-required effects\n", present(effects["action_required"]))
	}
}
func renderFields(out *bytes.Buffer, m map[string]any) {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		value := m[key]
		if nested, ok := value.(map[string]any); ok {
			fmt.Fprintf(out, "%s:\n", label(key))
			renderFieldsIndented(out, nested, "  ")
			continue
		}
		if list, ok := value.([]any); ok {
			fmt.Fprintf(out, "%s: %d\n", label(key), len(list))
			continue
		}
		fmt.Fprintf(out, "%s: %s\n", label(key), present(value))
	}
}
func renderFieldsIndented(out *bytes.Buffer, m map[string]any, indent string) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(out, "%s%s: %s\n", indent, label(k), present(m[k]))
	}
}
func label(key string) string { return strings.ReplaceAll(key, "_", " ") }
func present(v any) string {
	if v == nil {
		return ""
	}
	return escapeTerminal(fmt.Sprint(v))
}

// escapeTerminal retains printable Unicode and only renders controls as visible
// Go-style Unicode escapes. Newlines are handled by cards before this function.
func escapeTerminal(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsControl(r) || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			fmt.Fprintf(&b, "\\u%04x", r)
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// renderPaginationContinuation is deliberately outside renderHuman: JSON
// responses expose only next_cursor, while human responses get an executable
// command without adding a shell-command field to the protocol DTO.
func renderPaginationContinuation(w io.Writer, value any, continuation string) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	var page struct {
		NextCursor string `json:"next_cursor"`
	}
	if err := json.Unmarshal(encoded, &page); err != nil {
		return err
	}
	if page.NextCursor == "" {
		return nil
	}
	fmt.Fprintln(w, "More results available.")
	fmt.Fprintf(w, "Next: %s --cursor %s\n", continuation, shellQuote(page.NextCursor))
	return nil
}

func shellQuote(value string) string {
	if value != "" && strings.IndexFunc(value, func(r rune) bool {
		return !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || strings.ContainsRune("_@%+=:,./-", r))
	}) == -1 {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

package app

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/stayradiated/mosscap/internal/config"
	"github.com/stayradiated/mosscap/internal/protocol"
)

func TestCobraTreeContainsEveryDocumentedLeafWithCutoverMetadata(t *testing.T) {
	tree := (App{Version: "test"}).NewCommandTree()
	leaves := []struct {
		path       string
		readOnly   bool
		capability string
		flags      []string
	}{
		{"setup", true, "", []string{"json"}},
		{"config show", true, "", []string{"json"}}, {"config set", false, "", []string{"json"}}, {"config secret set", false, "", []string{"stdin"}},
		{"gateway run", false, "", nil}, {"gateway reload", false, "config_reload", []string{"request-id", "json"}}, {"gateway status", true, "status", []string{"json"}}, {"doctor", true, "doctor", []string{"json"}},
		{"trust list", true, "trusted_users", []string{"json"}}, {"trust add", false, "trusted_users", []string{"request-id", "json"}}, {"trust remove", false, "trusted_users", []string{"request-id", "json"}},
		{"task create", false, "local_tasks", []string{"text", "text-file", "request-id", "json"}}, {"task list", true, "local_tasks", []string{"state", "all", "limit", "json"}}, {"task show", true, "local_tasks", []string{"json"}}, {"task update", false, "local_tasks", []string{"text", "text-file", "request-id", "json"}}, {"task open", false, "local_tasks", []string{"note", "request-id", "json"}}, {"task working", false, "local_tasks", []string{"note", "request-id", "json"}}, {"task done", false, "local_tasks", []string{"note", "request-id", "json"}}, {"task reply", false, "task_reply_v1", []string{"text", "text-file", "done", "request-id", "json"}},
		{"slack history", true, protocol.CapabilitySlackPagination, []string{"limit", "cursor", "json"}}, {"slack thread", true, protocol.CapabilitySlackPagination, []string{"limit", "cursor", "json"}}, {"slack send", false, "slack_effects", []string{"text", "text-file", "thread", "task", "file", "request-id", "json"}}, {"slack file", false, "file_transfer_v1", []string{"thread", "file", "comment", "task", "request-id", "json"}}, {"slack edit", false, "slack_effects", []string{"text", "text-file", "task", "request-id", "json"}}, {"slack delete", false, "slack_effects", []string{"reason", "task", "request-id", "json"}}, {"slack react", false, "slack_effects", []string{"task", "request-id", "json"}}, {"slack unreact", false, "slack_effects", []string{"task", "request-id", "json"}},
		{"effect list", true, "slack_effects", []string{"state", "json"}}, {"effect inspect", true, "slack_effects", []string{"json"}}, {"effect retry", false, "slack_effects", []string{"reason", "confirm-duplicate-risk", "request-id", "json"}},
		{"file show", true, "slack_source_metadata", []string{"json"}}, {"file get", false, "file_transfer_v1", []string{"output", "force", "request-id", "json"}},
		{"delivery list", true, "pi_delivery_mvp", []string{"state", "json"}}, {"delivery inspect", true, "pi_delivery_mvp", []string{"json"}}, {"delivery requeue", false, "pi_delivery_mvp", []string{"reason", "request-id", "json"}},
		{"purge", false, "metadata_purge_v1", []string{"before", "dry-run", "confirm", "request-id", "json"}},
	}
	for _, tc := range leaves {
		t.Run(tc.path, func(t *testing.T) {
			cmd := commandAt(tree, strings.Fields(tc.path))
			if cmd == nil || cmd.Name() != strings.Fields(tc.path)[len(strings.Fields(tc.path))-1] {
				t.Fatalf("missing command %q", tc.path)
			}
			if cmd.RunE == nil || cmd.Use == "" || cmd.Short == "" || cmd.Long == "" || cmd.Example == "" {
				t.Fatalf("incomplete command metadata: %#v", cmd)
			}
			if got := cmd.Annotations[annotationReadOnly]; got != "true" && got != "false" {
				t.Fatalf("read-only annotation=%q", got)
			} else if (got == "true") != tc.readOnly {
				t.Fatalf("read-only annotation=%q, want %v", got, tc.readOnly)
			}
			if got := cmd.Annotations[annotationCapability]; got != tc.capability {
				t.Fatalf("capability=%q, want %q", got, tc.capability)
			}
			if cmd.Annotations[annotationHandler] == "" {
				t.Fatal("missing registered handler")
			}
			for _, flag := range tc.flags {
				if cmd.Flags().Lookup(flag) == nil {
					t.Errorf("missing --%s", flag)
				}
			}
			if flag := cmd.Flags().Lookup("limit"); flag != nil && flag.Value.Type() != "int" {
				t.Errorf("--limit type=%q, want int", flag.Value.Type())
			}
		})
	}
	if tree.CompletionOptions.DisableDefaultCmd != true {
		t.Fatal("Cobra completion command is enabled")
	}
}

func commandAt(root *cobra.Command, path []string) *cobra.Command {
	command := root
	for _, name := range path {
		found := false
		for _, child := range command.Commands() {
			if child.Name() == name {
				command, found = child, true
				break
			}
		}
		if !found {
			return nil
		}
	}
	return command
}

func TestCobraHelpAndHighConfidenceSuggestions(t *testing.T) {
	for _, args := range [][]string{nil, {"task"}, {"task", "create", "--help"}} {
		var out, err bytes.Buffer
		if exit := (App{Out: &out, Err: &err, Version: "test"}).Run(context.Background(), args); exit != ExitSuccess || err.Len() != 0 || !strings.Contains(out.String(), "Usage:") {
			t.Fatalf("help %v: exit=%d stdout=%q stderr=%q", args, exit, out.String(), err.String())
		}
	}

	for _, tc := range []struct {
		args        []string
		wantUsage   string
		wantSuggest string
	}{
		{[]string{"task", "udpate"}, "mosscap task", `Did you mean "update"?`},
		{[]string{"task", "list", "--sttae", "open"}, "mosscap task list", "Did you mean --state?"},
		// Operands are never candidates for correction.
		{[]string{"task", "show", "task-typo", "extra"}, "mosscap task show", ""},
	} {
		var out, err bytes.Buffer
		if exit := (App{Out: &out, Err: &err, Version: "test"}).Run(context.Background(), tc.args); exit != ExitUsage {
			t.Fatalf("%v exit=%d stdout=%q stderr=%q", tc.args, exit, out.String(), err.String())
		}
		if !strings.Contains(err.String(), tc.wantUsage) || (tc.wantSuggest != "" && !strings.Contains(err.String(), tc.wantSuggest)) || (tc.wantSuggest == "" && strings.Contains(err.String(), "Did you mean")) {
			t.Errorf("%v stderr=%q, usage=%q suggestion=%q", tc.args, err.String(), tc.wantUsage, tc.wantSuggest)
		}
	}
}

func TestRuntimeErrorsDoNotRenderUsageAndGatewayRunExplainsSetup(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), "state"))

	var out, err bytes.Buffer
	if exit := (App{Out: &out, Err: &err, Version: "test"}).Run(context.Background(), []string{"gateway", "status"}); exit != ExitUnavailable || strings.Contains(err.String(), "Usage:") {
		t.Fatalf("gateway status exit=%d stdout=%q stderr=%q", exit, out.String(), err.String())
	}
	out.Reset()
	err.Reset()
	if exit := (App{Out: &out, Err: &err, Version: "test"}).Run(context.Background(), []string{"gateway", "run"}); exit != ExitUnsafe || !strings.Contains(err.String(), "run mosscap setup") || strings.Contains(err.String(), "Usage:") {
		t.Fatalf("gateway run exit=%d stdout=%q stderr=%q", exit, out.String(), err.String())
	}
}

func TestTaskReplyFramePreflightCoversLiteralAndFileText(t *testing.T) {
	params := protocol.TaskReplyParams{Task: "abcdefghijklmnopqrstuv", Text: strings.Repeat("x", 4096)}
	for _, source := range []bool{false, true} {
		err := checkTaskFrame(config.Config{LocalProtocolMaximumFrame: 100}, "reply", "req_reply_frame", params, source, "test")
		if err == nil || !strings.Contains(err.Error(), "request exceeds the protocol frame limit") {
			t.Fatalf("textFile=%t frame error=%v", source, err)
		}
	}
}

func TestTypedLeavesBypassLegacyParserBridge(t *testing.T) {
	tree := (App{}).NewCommandTree()
	for _, path := range []string{
		"setup", "config show", "config set", "config secret set",
		"gateway run", "gateway reload", "gateway status", "doctor",
		"trust list", "trust add", "trust remove", "purge",
		"task create", "task list", "task show", "task update", "task open", "task working", "task done", "task reply",
		"effect list", "effect inspect", "effect retry",
		"file show", "file get", "slack history", "slack thread", "slack send", "slack file", "slack edit", "slack delete", "slack react", "slack unreact",
		"delivery list", "delivery inspect", "delivery requeue",
	} {
		cmd := commandAt(tree, strings.Fields(path))
		if cmd == nil {
			t.Fatalf("missing %q", path)
		}
		if !isTypedCobraHandler(cmd.Annotations[annotationHandler]) {
			t.Errorf("%s is still bound to the legacy parser bridge", path)
		}
	}
}

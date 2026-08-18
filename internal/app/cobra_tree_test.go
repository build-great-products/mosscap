package app

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
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
		{"slack history", true, "slack_reads", []string{"limit", "json"}}, {"slack thread", true, "slack_reads", []string{"json"}}, {"slack send", false, "slack_effects", []string{"text", "text-file", "thread", "task", "file", "request-id", "json"}}, {"slack file", false, "file_transfer_v1", []string{"thread", "file", "comment", "task", "request-id", "json"}}, {"slack edit", false, "slack_effects", []string{"text", "text-file", "task", "request-id", "json"}}, {"slack delete", false, "slack_effects", []string{"reason", "task", "request-id", "json"}}, {"slack react", false, "slack_effects", []string{"task", "request-id", "json"}}, {"slack unreact", false, "slack_effects", []string{"task", "request-id", "json"}},
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

func TestTypedLeavesBypassLegacyParserBridge(t *testing.T) {
	tree := (App{}).NewCommandTree()
	for _, path := range []string{
		"setup", "config show", "config set", "config secret set",
		"gateway run", "gateway reload", "gateway status", "doctor",
		"trust list", "trust add", "trust remove", "purge",
		"effect list", "effect inspect", "effect retry",
		"file show", "file get", "slack file",
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
	for _, handler := range []string{"task.list", "slack.send", "task.reply"} {
		if isTypedCobraHandler(handler) {
			t.Errorf("%s unexpectedly left the compatibility bridge", handler)
		}
	}
}

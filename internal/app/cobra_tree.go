package app

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/stayradiated/mosscap/internal/protocol"
)

// CommandMetadata describes properties the Cobra cutover must retain. It is
// deliberately separate from the legacy parser while both implementations are
// present, so no command family is released with mixed parsing behavior.
type CommandMetadata struct {
	ReadOnly          bool
	GatewayCapability string
	Handler           string
}

const (
	annotationReadOnly   = "mosscap.read_only"
	annotationCapability = "mosscap.gateway_capability"
	annotationHandler    = "mosscap.handler"
)

// NewCommandTree returns the complete, not-yet-active Cobra command tree.
// App.Execute intentionally continues to use the legacy parser until every
// command family can switch in one atomic release.
func (a App) NewCommandTree() *cobra.Command {
	root := &cobra.Command{
		Use:           "mosscap",
		Short:         "Mosscap is a durable local work coordinator.",
		Long:          "Mosscap coordinates durable local tasks and explicit Slack operations.",
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       a.Version,
		Args:          cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	root.CompletionOptions.DisableDefaultCmd = true
	root.SetVersionTemplate("{{.Version}}\n")
	root.AddCommand(&cobra.Command{Use: "version", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		_, err := fmt.Fprintln(cmd.OutOrStdout(), a.Version)
		return err
	}})
	root.AddCommand(commandBranch("config", "Inspect or stage configuration."), commandBranch("gateway", "Run or inspect the local gateway."), commandBranch("trust", "Manage trusted Slack users."), commandBranch("task", "Manage durable tasks."), commandBranch("slack", "Read and operate in the configured Slack channel."), commandBranch("effect", "Inspect durable Slack effects."), commandBranch("file", "Inspect or retrieve admitted Slack files."), commandBranch("delivery", "Inspect and recover Pi delivery attempts."))
	findCommand(root, []string{"config"}).AddCommand(commandBranch("secret", "Manage private Slack credentials."))
	root.AddCommand(a.cobraLeaf([]string{"setup"}, "Create private local configuration and state directories.", "mosscap setup [--json]", true, "", "setup", []flagSpec{boolFlag("json", "Render the result as JSON.")}, nil))
	root.AddCommand(a.cobraLeaf([]string{"doctor"}, "Run non-destructive local and gateway diagnostics.", "mosscap doctor [--json]", true, protocol.CapabilityDoctor, "doctor", []flagSpec{boolFlag("json", "Render the report as JSON.")}, nil))
	root.AddCommand(a.cobraLeaf([]string{"purge"}, "Preview or confirm irreversible metadata removal.", "mosscap purge --before RFC3339 (--dry-run|--confirm) [--request-id ID] [--json]", false, protocol.CapabilityMetadataPurge, "purge", []flagSpec{trackedStringFlag("before", "RFC3339 cutoff for eligible metadata."), trackedBoolFlag("dry-run", "Preview removal without changing state."), trackedBoolFlag("confirm", "Confirm irreversible removal."), stringFlag("request-id", "Durable mutation request ID."), boolFlag("json", "Render the result as JSON.")}, []string{"before"}))

	for _, spec := range cobraLeafSpecs {
		parent := findCommand(root, spec.path[:len(spec.path)-1])
		parent.AddCommand(a.cobraLeaf(spec.path, spec.short, spec.use, spec.readOnly, spec.capability, spec.handler, spec.flags, spec.required))
	}
	return root
}

type cobraLeafSpec struct {
	path       []string
	short, use string
	readOnly   bool
	capability string
	handler    string
	flags      []flagSpec
	required   []string
}

type flagSpec struct {
	name, usage string
	bool        bool
	integer     bool
	repeat      bool
	tracked     bool
}

func stringFlag(name, usage string) flagSpec { return flagSpec{name: name, usage: usage} }
func repeatStringFlag(name, usage string) flagSpec {
	return flagSpec{name: name, usage: usage, repeat: true}
}
func intFlag(name, usage string) flagSpec  { return flagSpec{name: name, usage: usage, integer: true} }
func boolFlag(name, usage string) flagSpec { return flagSpec{name: name, usage: usage, bool: true} }
func trackedStringFlag(name, usage string) flagSpec {
	return flagSpec{name: name, usage: usage, tracked: true}
}
func trackedBoolFlag(name, usage string) flagSpec {
	return flagSpec{name: name, usage: usage, bool: true, tracked: true}
}

func commandBranch(use, short string) *cobra.Command {
	return &cobra.Command{Use: use, Short: short, Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() }}
}
func findCommand(root *cobra.Command, path []string) *cobra.Command {
	c := root
	for _, name := range path {
		for _, child := range c.Commands() {
			if child.Name() == name {
				c = child
				break
			}
		}
	}
	return c
}

func (a App) cobraLeaf(path []string, short, use string, readOnly bool, capability, handler string, flags []flagSpec, required []string) *cobra.Command {
	commandUse := strings.TrimPrefix(use, "mosscap ")
	if len(path) > 1 {
		commandUse = strings.TrimPrefix(commandUse, strings.Join(path[:len(path)-1], " ")+" ")
	}
	cmd := &cobra.Command{Use: commandUse, Short: short, Long: short, Example: "  " + use, Args: cobra.ExactArgs(expectedArgs(handler)), Annotations: map[string]string{annotationReadOnly: fmt.Sprint(readOnly), annotationCapability: capability, annotationHandler: handler}}
	if isTypedCobraHandler(handler) {
		// Keep migrated leaves structurally separate from the compatibility
		// bridge: their execution can never reach a legacy flag.FlagSet parser.
		cmd.RunE = func(cmd *cobra.Command, args []string) error {
			return a.runTypedCobraHandler(handler, cmd, args)
		}
	} else {
		cmd.RunE = func(cmd *cobra.Command, args []string) error {
			return a.runCobraHandler(handler, canonicalArgs(cmd, args))
		}
	}
	for _, f := range flags {
		switch {
		case f.tracked:
			cmd.Flags().Var(newTrackedFlag(f.bool), f.name, f.usage)
			if f.bool {
				cmd.Flags().Lookup(f.name).NoOptDefVal = "true"
			}
		case f.bool:
			cmd.Flags().Bool(f.name, false, f.usage)
		case f.integer:
			cmd.Flags().Int(f.name, 0, f.usage)
		case f.repeat:
			cmd.Flags().StringArray(f.name, nil, f.usage)
		default:
			cmd.Flags().String(f.name, "", f.usage)
		}
	}
	// Required combinations are validated by the request handlers so their
	// established, command-specific diagnostics remain intact.
	for _, pair := range [][2]string{{"text", "text-file"}, {"state", "all"}, {"dry-run", "confirm"}} {
		if cmd.Flags().Lookup(pair[0]) != nil && cmd.Flags().Lookup(pair[1]) != nil {
			cmd.MarkFlagsMutuallyExclusive(pair[0], pair[1])
		}
	}
	return cmd
}

// canonicalArgs gives the business handlers flags-first input after Cobra has
// performed all command selection and option parsing. Keeping positional values
// separate is what permits options on either side of selectors.
func canonicalArgs(cmd *cobra.Command, args []string) []string {
	out := make([]string, 0, len(args)+cmd.Flags().NFlag()*2)
	cmd.Flags().VisitAll(func(f *pflag.Flag) {
		if !f.Changed {
			return
		}
		if f.Value.Type() == "bool" {
			out = append(out, "--"+f.Name+"="+f.Value.String())
			return
		}
		if f.Value.Type() == "stringArray" {
			values, _ := cmd.Flags().GetStringArray(f.Name)
			for _, value := range values {
				out = append(out, "--"+f.Name, value)
			}
			return
		}
		out = append(out, "--"+f.Name, f.Value.String())
	})
	if len(args) != 0 {
		// The second-stage compatibility handlers use flag.FlagSet, whose parser
		// must not reinterpret a positional value beginning with '-'. Cobra has
		// already identified these as operands, including after a user-supplied
		// end-of-options marker.
		out = append(out, "--")
	}
	return append(out, args...)
}

// runTypedCobraHandler binds the first migrated family directly to typed
// operations. These leaves must not pass through the legacy flag.FlagSet
// adapters in runCobraHandler.
func isTypedCobraHandler(handler string) bool {
	switch handler {
	case "setup", "config.show", "config.set", "config.secret.set", "gateway.run", "gateway.reload", "gateway.status", "doctor", "purge", "trust.list", "trust.add", "trust.remove", "effect.list", "effect.inspect", "effect.retry", "file.show", "file.get", "slack.file", "delivery.list", "delivery.inspect", "delivery.requeue":
		return true
	default:
		return false
	}
}

func (a App) runTypedCobraHandler(handler string, cmd *cobra.Command, args []string) error {
	jsonOut, _ := cmd.Flags().GetBool("json")
	switch handler {
	case "setup":
		return a.setupTyped(jsonOut)
	case "config.show":
		return a.configShowTyped(jsonOut)
	case "config.set":
		return a.configSetTyped(args[0], args[1], jsonOut)
	case "config.secret.set":
		stdin, _ := cmd.Flags().GetBool("stdin")
		return a.configSecretSetTyped(args[0], stdin)
	case "gateway.run":
		return a.gatewayRunTyped()
	case "gateway.reload":
		requestID, _ := cmd.Flags().GetString("request-id")
		return a.gatewayReloadTyped(requestID, jsonOut)
	case "gateway.status":
		return a.gatewayStatusTyped(jsonOut)
	case "doctor":
		return a.doctorTyped(jsonOut)
	case "purge":
		before := trackedString(cmd, "before")
		dryRun := trackedBool(cmd, "dry-run")
		confirm := trackedBool(cmd, "confirm")
		requestID, _ := cmd.Flags().GetString("request-id")
		return a.purgeTyped(before, dryRun, confirm, trackedOccurrences(cmd, "before"), trackedOccurrences(cmd, "dry-run"), trackedOccurrences(cmd, "confirm"), requestID, jsonOut)
	case "trust.list":
		return a.trustTyped("list", "", "", jsonOut)
	case "trust.add", "trust.remove":
		requestID, _ := cmd.Flags().GetString("request-id")
		return a.trustTyped(strings.TrimPrefix(handler, "trust."), args[0], requestID, jsonOut)
	case "effect.list":
		state, _ := cmd.Flags().GetString("state")
		return a.effectTyped("list", "", state, "", false, "", jsonOut)
	case "effect.inspect":
		return a.effectTyped("inspect", args[0], "", "", false, "", jsonOut)
	case "effect.retry":
		reason, _ := cmd.Flags().GetString("reason")
		confirm, _ := cmd.Flags().GetBool("confirm-duplicate-risk")
		requestID, _ := cmd.Flags().GetString("request-id")
		return a.effectTyped("retry", args[0], "", reason, confirm, requestID, jsonOut)
	case "file.show":
		return a.fileShowTyped(args[0], jsonOut)
	case "file.get":
		output, _ := cmd.Flags().GetString("output")
		force, _ := cmd.Flags().GetBool("force")
		requestID, _ := cmd.Flags().GetString("request-id")
		return a.fileGetTyped(args[0], output, force, requestID, jsonOut)
	case "slack.file":
		thread, _ := cmd.Flags().GetString("thread")
		files, _ := cmd.Flags().GetStringArray("file")
		comment, _ := cmd.Flags().GetString("comment")
		task, _ := cmd.Flags().GetString("task")
		requestID, _ := cmd.Flags().GetString("request-id")
		return a.slackFileTyped(thread, files, comment, task, requestID, jsonOut)
	case "delivery.list":
		state, _ := cmd.Flags().GetString("state")
		return a.deliveryTyped("list", "", state, "", "", jsonOut)
	case "delivery.inspect":
		return a.deliveryTyped("inspect", args[0], "", "", "", jsonOut)
	case "delivery.requeue":
		reason, _ := cmd.Flags().GetString("reason")
		requestID, _ := cmd.Flags().GetString("request-id")
		return a.deliveryTyped("requeue", args[0], "", reason, requestID, jsonOut)
	default:
		return fmt.Errorf("unbound typed Cobra handler %q", handler)
	}
}

// runCobraHandler is the temporary compatibility bridge for command families
// that have not yet been migrated to typed Cobra bindings.
func (a App) runCobraHandler(handler string, args []string) error {
	switch handler {
	case "setup":
		return a.setup("", args)
	case "doctor":
		return a.doctorCommand("", args)
	case "purge":
		return a.purge(args)
	case "config.show":
		return a.configShow("", args)
	case "config.set":
		return a.configSet("", args)
	case "config.secret.set":
		return a.configSecret("", append([]string{"set"}, args...))
	case "gateway.run":
		return a.gateway("", append([]string{"run"}, args...))
	case "gateway.reload":
		return a.gatewayReload(args)
	case "gateway.status":
		return a.gatewayStatus("", args)
	case "trust.list", "trust.add", "trust.remove":
		return a.trust(append([]string{strings.TrimPrefix(handler, "trust.")}, args...))
	case "task.reply":
		return a.taskReply(args)
	case "task.create", "task.list", "task.show", "task.update", "task.open", "task.working", "task.done":
		return a.task("", append([]string{strings.TrimPrefix(handler, "task.")}, args...))
	case "slack.history", "slack.thread", "slack.send", "slack.file", "slack.edit", "slack.delete", "slack.react", "slack.unreact":
		return a.slackCommand(strings.TrimPrefix(handler, "slack."), args)
	case "effect.list", "effect.inspect", "effect.retry":
		return a.effect(append([]string{strings.TrimPrefix(handler, "effect.")}, args...))
	case "file.show":
		return a.file(append([]string{"show"}, args...))
	case "file.get":
		return a.fileGet(args)
	case "delivery.list", "delivery.inspect", "delivery.requeue":
		return a.delivery(append([]string{strings.TrimPrefix(handler, "delivery.")}, args...))
	default:
		return fmt.Errorf("unbound Cobra handler %q", handler)
	}
}

func expectedArgs(handler string) int {
	switch handler {
	case "config.set", "config.secret.set", "slack.react", "slack.unreact":
		if handler == "slack.react" || handler == "slack.unreact" {
			return 2
		}
		if handler == "config.set" {
			return 2
		}
		return 1
	case "trust.add", "trust.remove", "task.show", "task.update", "task.open", "task.working", "task.done", "task.reply", "slack.thread", "slack.edit", "slack.delete", "effect.inspect", "effect.retry", "file.show", "file.get", "delivery.inspect", "delivery.requeue":
		return 1
	default:
		return 0
	}
}

var cobraLeafSpecs = []cobraLeafSpec{
	{[]string{"config", "show"}, "Show the staged configuration with credentials redacted.", "mosscap config show [--json]", true, "", "config.show", []flagSpec{boolFlag("json", "Render the configuration as JSON.")}, nil},
	{[]string{"config", "set"}, "Stage one validated non-secret configuration value.", "mosscap config set KEY VALUE [--json]", false, "", "config.set", []flagSpec{boolFlag("json", "Render the configuration as JSON.")}, nil},
	{[]string{"config", "secret", "set"}, "Store a Slack credential from stdin or a controlling TTY.", "mosscap config secret set (slack_app_token|slack_bot_token) [--stdin]", false, "", "config.secret.set", []flagSpec{boolFlag("stdin", "Read the credential from non-terminal standard input.")}, nil},
	{[]string{"gateway", "run"}, "Run the foreground local gateway.", "mosscap gateway run", false, "", "gateway.run", nil, nil},
	{[]string{"gateway", "reload"}, "Validate and atomically activate staged configuration.", "mosscap gateway reload [--request-id ID] [--json]", false, "config_reload", "gateway.reload", []flagSpec{stringFlag("request-id", "Durable mutation request ID."), boolFlag("json", "Render the result as JSON.")}, nil},
	{[]string{"gateway", "status"}, "Show local gateway status.", "mosscap gateway status [--json]", true, protocol.CapabilityStatus, "gateway.status", []flagSpec{boolFlag("json", "Render the status as JSON.")}, nil},
	{[]string{"trust", "list"}, "List trusted Slack users.", "mosscap trust list [--json]", true, "trusted_users", "trust.list", []flagSpec{boolFlag("json", "Render the list as JSON.")}, nil},
	{[]string{"trust", "add"}, "Add a trusted Slack user.", "mosscap trust add SLACK_USER_ID [--request-id ID] [--json]", false, "trusted_users", "trust.add", []flagSpec{stringFlag("request-id", "Durable mutation request ID."), boolFlag("json", "Render the result as JSON.")}, nil},
	{[]string{"trust", "remove"}, "Remove a trusted Slack user.", "mosscap trust remove SLACK_USER_ID [--request-id ID] [--json]", false, "trusted_users", "trust.remove", []flagSpec{stringFlag("request-id", "Durable mutation request ID."), boolFlag("json", "Render the result as JSON.")}, nil},
	{[]string{"task", "create"}, "Create a local task.", "mosscap task create (--text TEXT|--text-file PATH) [--request-id ID] [--json]", false, "local_tasks", "task.create", textFlags(), nil},
	{[]string{"task", "list"}, "List durable tasks.", "mosscap task list [--state open|working|done|--all] [--limit N] [--json]", true, "local_tasks", "task.list", []flagSpec{stringFlag("state", "Filter by open, working, or done state."), boolFlag("all", "Include completed tasks."), intFlag("limit", "Maximum number of tasks."), boolFlag("json", "Render the list as JSON.")}, nil},
	{[]string{"task", "show"}, "Show current durable task state.", "mosscap task show TASK [--json]", true, "local_tasks", "task.show", []flagSpec{boolFlag("json", "Render the task as JSON.")}, nil},
	{[]string{"task", "update"}, "Replace a task's text.", "mosscap task update TASK (--text TEXT|--text-file PATH) [--request-id ID] [--json]", false, "local_tasks", "task.update", textFlags(), nil},
	{[]string{"task", "open"}, "Move a task to open.", "mosscap task open TASK [--note TEXT] [--request-id ID] [--json]", false, "local_tasks", "task.open", transitionFlags(), nil},
	{[]string{"task", "working"}, "Move a task to working.", "mosscap task working TASK [--note TEXT] [--request-id ID] [--json]", false, "local_tasks", "task.working", transitionFlags(), nil},
	{[]string{"task", "done"}, "Move a task to done.", "mosscap task done TASK [--note TEXT] [--request-id ID] [--json]", false, "local_tasks", "task.done", transitionFlags(), nil},
	{[]string{"task", "reply"}, "Reply to a Slack task, optionally completing it after confirmation.", "mosscap task reply TASK (--text TEXT|--text-file PATH) [--done] [--request-id ID] [--json]", false, protocol.CapabilityTaskReply, "task.reply", append(textFlags(), boolFlag("done", "Complete only after Slack confirms the reply.")), nil},
	{[]string{"slack", "history"}, "Read recent messages from the configured Slack channel.", "mosscap slack history [--limit N] [--json]", true, "slack_reads", "slack.history", []flagSpec{intFlag("limit", "Maximum messages to return."), boolFlag("json", "Render messages as JSON.")}, nil},
	{[]string{"slack", "thread"}, "Read a Slack message thread.", "mosscap slack thread THREAD_TS [--json]", true, "slack_reads", "slack.thread", []flagSpec{boolFlag("json", "Render messages as JSON.")}, nil},
	{[]string{"slack", "send"}, "Send a Slack message or threaded reply.", "mosscap slack send [--thread THREAD_TS] (--text TEXT|--text-file PATH) [--file PATH ...] [--task TASK] [--request-id ID] [--json]", false, "slack_effects", "slack.send", append(textFlags(), stringFlag("thread", "Reply thread timestamp."), stringFlag("task", "Related durable task."), repeatStringFlag("file", "Regular file to upload; repeatable.")), nil},
	{[]string{"slack", "file"}, "Upload a file to the configured Slack channel.", "mosscap slack file [--thread THREAD_TS] --file PATH [--comment TEXT] [--task TASK] [--request-id ID] [--json]", false, protocol.CapabilityFileTransfer, "slack.file", []flagSpec{stringFlag("thread", "Reply thread timestamp."), repeatStringFlag("file", "Regular file to upload."), stringFlag("comment", "Optional file comment."), stringFlag("task", "Related durable task."), stringFlag("request-id", "Durable mutation request ID."), boolFlag("json", "Render the result as JSON.")}, []string{"file"}},
	{[]string{"slack", "edit"}, "Replace a Slack message's text.", "mosscap slack edit MESSAGE_TS (--text TEXT|--text-file PATH) [--task TASK] [--request-id ID] [--json]", false, "slack_effects", "slack.edit", append(textFlags(), stringFlag("task", "Related durable task.")), nil},
	{[]string{"slack", "delete"}, "Delete a Slack message with an audit reason.", "mosscap slack delete MESSAGE_TS --reason TEXT [--task TASK] [--request-id ID] [--json]", false, "slack_effects", "slack.delete", []flagSpec{stringFlag("reason", "Required audit reason."), stringFlag("task", "Related durable task."), stringFlag("request-id", "Durable mutation request ID."), boolFlag("json", "Render the result as JSON.")}, []string{"reason"}},
	{[]string{"slack", "react"}, "Add a reaction to a Slack message.", "mosscap slack react MESSAGE_TS EMOJI [--task TASK] [--request-id ID] [--json]", false, "slack_effects", "slack.react", taskMutationFlags(), nil},
	{[]string{"slack", "unreact"}, "Remove a reaction from a Slack message.", "mosscap slack unreact MESSAGE_TS EMOJI [--task TASK] [--request-id ID] [--json]", false, "slack_effects", "slack.unreact", taskMutationFlags(), nil},
	{[]string{"effect", "list"}, "List durable Slack effects.", "mosscap effect list [--state prepared|submitted|confirmed|ambiguous|failed] [--json]", true, "slack_effects", "effect.list", []flagSpec{stringFlag("state", "Filter by effect state."), boolFlag("json", "Render effects as JSON.")}, nil},
	{[]string{"effect", "inspect"}, "Inspect durable effect evidence.", "mosscap effect inspect EFFECT [--json]", true, "slack_effects", "effect.inspect", []flagSpec{boolFlag("json", "Render effect evidence as JSON.")}, nil},
	{[]string{"effect", "retry"}, "Create a successor for a failed or explicitly confirmed risky effect.", "mosscap effect retry EFFECT --reason TEXT [--confirm-duplicate-risk] [--request-id ID] [--json]", false, "slack_effects", "effect.retry", []flagSpec{stringFlag("reason", "Required retry reason."), boolFlag("confirm-duplicate-risk", "Acknowledge duplicate risk for ambiguous effects."), stringFlag("request-id", "Durable mutation request ID."), boolFlag("json", "Render the result as JSON.")}, []string{"reason"}},
	{[]string{"file", "show"}, "Show admitted Slack file metadata; use file get for bytes.", "mosscap file show FILE [--json]", true, "slack_source_metadata", "file.show", []flagSpec{boolFlag("json", "Render metadata as JSON.")}, nil},
	{[]string{"file", "get"}, "Retrieve an admitted Slack file to a safe destination.", "mosscap file get FILE [--output PATH] [--force] [--request-id ID] [--json]", false, protocol.CapabilityFileTransfer, "file.get", []flagSpec{stringFlag("output", "Absolute destination path."), boolFlag("force", "Replace an existing regular destination file."), stringFlag("request-id", "Durable mutation request ID."), boolFlag("json", "Render the result as JSON.")}, nil},
	{[]string{"delivery", "list"}, "List Pi delivery attempts.", "mosscap delivery list [--state pending|inserting|persisted|uncertain|rejected] [--json]", true, "pi_delivery_mvp", "delivery.list", []flagSpec{stringFlag("state", "Filter by delivery state."), boolFlag("json", "Render deliveries as JSON.")}, nil},
	{[]string{"delivery", "inspect"}, "Inspect Pi delivery evidence.", "mosscap delivery inspect DELIVERY [--json]", true, "pi_delivery_mvp", "delivery.inspect", []flagSpec{boolFlag("json", "Render delivery evidence as JSON.")}, nil},
	{[]string{"delivery", "requeue"}, "Create a new Pi delivery attempt; duplicate insertion is possible.", "mosscap delivery requeue DELIVERY --reason TEXT [--request-id ID] [--json]", false, "pi_delivery_mvp", "delivery.requeue", []flagSpec{stringFlag("reason", "Required requeue reason."), stringFlag("request-id", "Durable mutation request ID."), boolFlag("json", "Render the result as JSON.")}, []string{"reason"}},
}

func textFlags() []flagSpec {
	return []flagSpec{stringFlag("text", "Literal message text."), stringFlag("text-file", "Regular file containing message text."), stringFlag("request-id", "Durable mutation request ID."), boolFlag("json", "Render the result as JSON.")}
}
func transitionFlags() []flagSpec {
	return []flagSpec{stringFlag("note", "Transition audit note."), stringFlag("request-id", "Durable mutation request ID."), boolFlag("json", "Render the result as JSON.")}
}
func taskMutationFlags() []flagSpec {
	return []flagSpec{stringFlag("task", "Related durable task."), stringFlag("request-id", "Durable mutation request ID."), boolFlag("json", "Render the result as JSON.")}
}

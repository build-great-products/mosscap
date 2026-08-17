package app

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/stayradiated/mosscap/internal/config"
	"github.com/stayradiated/mosscap/internal/identity"
	"github.com/stayradiated/mosscap/internal/protocol"
	"github.com/stayradiated/mosscap/internal/securepath"
)

func (a App) slack(args []string) error {
	if len(args) == 0 || isHelp(args[0]) {
		a.printBranchHelp("slack", []string{"history", "thread", "send", "file", "edit", "delete", "react", "unreact"})
		return nil
	}
	cmd := args[0]
	if !contains([]string{"history", "thread", "send", "file", "edit", "delete", "react", "unreact"}, cmd) {
		return usage(fmt.Sprintf("unknown slack command %q", cmd))
	}
	return a.slackCommand(cmd, args[1:])
}

func (a App) slackCommand(cmd string, args []string) error {
	fs := newCommandFlagSet("slack " + cmd)
	jsonOut := fs.Bool("json", false, "")
	text := fs.String("text", "", "")
	textFile := fs.String("text-file", "", "")
	comment := fs.String("comment", "", "")
	thread := fs.String("thread", "", "")
	task := fs.String("task", "", "")
	reason := fs.String("reason", "", "")
	requestID := fs.String("request-id", "", "")
	var files stringList
	fs.Var(&files, "file", "")
	limit := fs.Int("limit", 50, "")
	help := fs.Bool("help", false, "")
	fs.BoolVar(help, "h", false, "")
	if err := fs.Parse(args); err != nil {
		return usage(err.Error())
	}
	if *help {
		a.printSlackHelp(cmd)
		return nil
	}
	allowed := map[string][]string{
		"history": {"json", "limit"}, "thread": {"json"},
		"send":   {"json", "text", "text-file", "thread", "task", "request-id", "file"},
		"file":   {"json", "file", "thread", "comment", "task", "request-id"},
		"edit":   {"json", "text", "text-file", "task", "request-id"},
		"delete": {"json", "reason", "task", "request-id"},
		"react":  {"json", "task", "request-id"}, "unreact": {"json", "task", "request-id"},
	}
	if err := rejectUnsupportedFlags(fs, allowed[cmd]); err != nil {
		return err
	}

	mutation := cmd != "history" && cmd != "thread"
	var params any
	switch cmd {
	case "history":
		if fs.NArg() != 0 || *requestID != "" {
			return usage("slack history accepts no arguments and no --request-id")
		}
		params = protocol.SlackHistoryParams{Limit: *limit}
	case "thread":
		if fs.NArg() != 1 || *requestID != "" {
			return usage("slack thread requires THREAD_TS and no --request-id")
		}
		params = protocol.SlackThreadParams{ThreadTS: fs.Arg(0)}
	case "send":
		t, err := a.readCommandText(*text, *textFile)
		if err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return usage("slack send accepts no positional arguments")
		}
		paths, err := absolutePaths(files)
		if err != nil {
			return err
		}
		params = protocol.SlackSendParams{ThreadTS: *thread, Text: t, Task: *task, FilePaths: paths}
	case "file":
		if fs.NArg() != 0 || *text != "" || *textFile != "" || *reason != "" {
			return usage("slack file accepts exactly one --file and no positional arguments")
		}
		if len(files) != 1 {
			return usage("slack file requires exactly one --file")
		}
		paths, err := absolutePaths(files)
		if err != nil {
			return err
		}
		params = protocol.SlackFileParams{ThreadTS: *thread, FilePath: paths[0], Comment: *comment, Task: *task}
	case "edit":
		if fs.NArg() != 1 {
			return usage("slack edit requires MESSAGE_TS")
		}
		t, err := a.readCommandText(*text, *textFile)
		if err != nil {
			return err
		}
		params = protocol.SlackEditParams{MessageTS: fs.Arg(0), Text: t, Task: *task}
	case "delete":
		if fs.NArg() != 1 || *reason == "" {
			return usage("slack delete requires MESSAGE_TS and --reason TEXT")
		}
		params = protocol.SlackDeleteParams{MessageTS: fs.Arg(0), Reason: *reason, Task: *task}
	case "react", "unreact":
		if fs.NArg() != 2 {
			return usage("slack " + cmd + " requires MESSAGE_TS and EMOJI")
		}
		params = protocol.SlackReactionParams{MessageTS: fs.Arg(0), Emoji: fs.Arg(1), Task: *task}
	}
	if mutation && *requestID == "" {
		id, err := identity.NewGenerator(nil).NewRequestID()
		if err != nil {
			return err
		}
		*requestID = id
	}
	if !mutation {
		*requestID = "req_slack_" + cmd
	}
	c, err := loadCommandConfig()
	if err != nil {
		return err
	}
	if err := checkSlackTextFrame(c, *textFile, params, cmd, *requestID, a.Version); err != nil {
		return err
	}
	capability := protocol.CapabilitySlackReads
	if mutation {
		capability = protocol.CapabilitySlackEffects
	}
	if cmd == "file" || cmd == "send" && len(files) > 0 {
		capability = protocol.CapabilityFileTransfer
	}
	out, err := a.gatewayCall(c, "slack."+cmd, *requestID, params, capability)
	if err != nil {
		return mutationJSONError(err, mutation, *jsonOut, *requestID)
	}
	if mutation {
		out = map[string]any{"request_id": *requestID, "effect": out}
	}
	return a.renderSlackCommand(out, *jsonOut, cmd == "history" || cmd == "thread")
}

func (a App) effect(args []string) error {
	if len(args) == 0 || isHelp(args[0]) {
		a.printBranchHelp("effect", []string{"list", "inspect", "retry"})
		return nil
	}
	cmd := args[0]
	if !contains([]string{"list", "inspect", "retry"}, cmd) {
		return usage(fmt.Sprintf("unknown effect command %q", cmd))
	}
	fs := newCommandFlagSet("effect " + cmd)
	jsonOut := fs.Bool("json", false, "")
	state := fs.String("state", "", "")
	reason := fs.String("reason", "", "")
	confirm := fs.Bool("confirm-duplicate-risk", false, "")
	requestID := fs.String("request-id", "", "")
	help := fs.Bool("help", false, "")
	fs.BoolVar(help, "h", false, "")
	if err := fs.Parse(args[1:]); err != nil {
		return usage(err.Error())
	}
	if *help {
		a.printEffectHelp(cmd)
		return nil
	}
	allowed := map[string][]string{"list": {"json", "state"}, "inspect": {"json"}, "retry": {"json", "reason", "confirm-duplicate-risk", "request-id"}}
	if err := rejectUnsupportedFlags(fs, allowed[cmd]); err != nil {
		return err
	}
	var params any
	mutation := cmd == "retry"
	switch cmd {
	case "list":
		if fs.NArg() != 0 || *requestID != "" {
			return usage("effect list accepts no arguments and no --request-id")
		}
		if *state != "" && !contains([]string{"prepared", "submitted", "confirmed", "ambiguous", "failed"}, *state) {
			return usage("invalid --state")
		}
		var p *string
		if *state != "" {
			p = state
		}
		params = protocol.EffectListParams{State: p}
	case "inspect":
		if fs.NArg() != 1 || *requestID != "" {
			return usage("effect inspect requires EFFECT and no --request-id")
		}
		params = protocol.EffectSelector{Effect: fs.Arg(0)}
	case "retry":
		if fs.NArg() != 1 || *reason == "" {
			return usage("effect retry requires EFFECT and --reason TEXT")
		}
		params = protocol.EffectRetryParams{Effect: fs.Arg(0), Reason: *reason, ConfirmDuplicateRisk: *confirm}
	}
	if mutation && *requestID == "" {
		id, err := identity.NewGenerator(nil).NewRequestID()
		if err != nil {
			return err
		}
		*requestID = id
	}
	if !mutation {
		*requestID = "req_effect_" + cmd
	}
	c, err := loadCommandConfig()
	if err != nil {
		return err
	}
	out, err := a.gatewayCall(c, "effect."+cmd, *requestID, params, protocol.CapabilitySlackEffects)
	if err != nil {
		return mutationJSONError(err, mutation, *jsonOut, *requestID)
	}
	if mutation {
		out = map[string]any{"request_id": *requestID, "effect": out}
	}
	return a.renderSlackCommand(out, *jsonOut, cmd == "inspect")
}

func (a App) file(args []string) error {
	if len(args) == 0 || isHelp(args[0]) {
		a.printBranchHelp("file", []string{"show", "get"})
		return nil
	}
	if args[0] == "get" {
		return a.fileGet(args[1:])
	}
	if args[0] != "show" {
		return usage(fmt.Sprintf("unknown file command %q", args[0]))
	}
	fs := newCommandFlagSet("file show")
	jsonOut := fs.Bool("json", false, "")
	help := fs.Bool("help", false, "")
	fs.BoolVar(help, "h", false, "")
	if err := fs.Parse(args[1:]); err != nil {
		return usage(err.Error())
	}
	if *help {
		fmt.Fprintln(a.Out, "Usage: mosscap file show FILE [--json]\n\nShow incoming Slack file metadata; file bytes are not supported.")
		return nil
	}
	if err := rejectUnsupportedFlags(fs, []string{"json"}); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return usage("file show requires FILE")
	}
	c, err := loadCommandConfig()
	if err != nil {
		return err
	}
	out, err := a.gatewayCall(c, "file.show", "req_file_show", protocol.FileSelector{File: fs.Arg(0)}, protocol.CapabilitySlackSourceMetadata)
	if err != nil {
		return err
	}
	return a.renderSlackCommand(out, *jsonOut, false)
}

func (a App) readCommandText(text, path string) (string, error) {
	if (text == "") == (path == "") {
		return "", usage("exactly one of --text or --text-file is required")
	}
	if path == "" {
		return text, nil
	}
	c, err := loadCommandConfig()
	if err != nil {
		return "", err
	}
	b, err := securepath.ReadRegularFile(path, int64(c.LocalProtocolMaximumFrame))
	if err != nil {
		return "", fmt.Errorf("--text-file: %w", err)
	}
	return string(b), nil
}
func loadCommandConfig() (config.Config, error) {
	loc, err := config.DefaultLocation()
	if err != nil {
		return config.Config{}, err
	}
	return config.Load(loc)
}
func checkSlackTextFrame(c config.Config, path string, params any, cmd, requestID, version string) error {
	_ = path
	// Local text and file paths are resolved before the request is framed; check
	// the complete request rather than each individual input.
	if b, err := protocol.EncodeRequest(protocol.Request{ProtocolVersion: protocol.Version, RequestID: requestID, ClientKind: "cli", ClientVersion: version, Command: "slack." + cmd, Params: mustJSON(params)}); err != nil || len(b) > c.LocalProtocolMaximumFrame {
		return usage("request exceeds the protocol frame limit")
	}
	return nil
}
func (a App) renderSlackCommand(v any, jsonOut, explicitRead bool) error {
	if jsonOut {
		return a.render(v, true)
	}
	if explicitRead {
		return a.render(v, false)
	}
	// Mutation output intentionally exposes durable identity/state but no effect body.
	if m, ok := v.(map[string]any); ok {
		if e, ok := m["effect"].(map[string]any); ok {
			fmt.Fprintf(a.Out, "request_id: %v\neffect_id: %v\nkind: %v\nstate: %v\ntarget: %v\n", m["request_id"], e["effect_id"], e["kind"], e["state"], first(e["target_message_ts"], e["thread_ts"]))
			return nil
		}
	}
	return a.render(v, false)
}
func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }
func first(a, b any) any {
	if a != nil && a != "" {
		return a
	}
	return b
}
func rejectUnsupportedFlags(fs *flag.FlagSet, allowed []string) error {
	ok := map[string]bool{"help": true, "h": true}
	for _, name := range allowed {
		ok[name] = true
	}
	var bad string
	fs.Visit(func(f *flag.Flag) {
		if !ok[f.Name] {
			bad = "--" + f.Name
		}
	})
	if bad != "" {
		return usage(bad + " is not valid for this command")
	}
	return nil
}
func contains(xs []string, x string) bool {
	for _, y := range xs {
		if x == y {
			return true
		}
	}
	return false
}

type stringList []string

func (s *stringList) String() string { return "" }
func (s *stringList) Set(value string) error {
	*s = append(*s, value)
	return nil
}

func absolutePaths(paths []string) ([]string, error) {
	out := make([]string, len(paths))
	for i, value := range paths {
		path, err := filepath.Abs(value)
		if err != nil {
			return nil, usage("invalid file path")
		}
		if len(path) == 0 || len(path) > 4096 || strings.IndexByte(path, 0) >= 0 {
			return nil, usage("invalid file path")
		}
		out[i] = path
	}
	return out, nil
}

func newCommandFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}
func (a App) printSlackHelp(cmd string) {
	usages := map[string]string{"history": "[--limit N] [--json]", "thread": "THREAD_TS [--json]", "send": "[--thread THREAD_TS] (--text TEXT|--text-file PATH) [--file PATH ...] [--task TASK] [--request-id ID] [--json]", "file": "[--thread THREAD_TS] --file PATH [--comment TEXT] [--task TASK] [--request-id ID] [--json]", "edit": "MESSAGE_TS (--text TEXT|--text-file PATH) [--task TASK] [--request-id ID] [--json]", "delete": "MESSAGE_TS --reason TEXT [--task TASK] [--request-id ID] [--json]", "react": "MESSAGE_TS EMOJI [--task TASK] [--request-id ID] [--json]", "unreact": "MESSAGE_TS EMOJI [--task TASK] [--request-id ID] [--json]"}
	fmt.Fprintf(a.Out, "Usage: mosscap slack %s %s\n", cmd, usages[cmd])
}
func (a App) printEffectHelp(cmd string) {
	usages := map[string]string{"list": "[--state prepared|submitted|confirmed|ambiguous|failed] [--json]", "inspect": "EFFECT [--json]", "retry": "EFFECT --reason TEXT [--confirm-duplicate-risk] [--request-id ID] [--json]"}
	fmt.Fprintf(a.Out, "Usage: mosscap effect %s %s\n", cmd, usages[cmd])
}

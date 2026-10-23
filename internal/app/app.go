// Package app contains Mosscap's command tree and process-facing rendering.
package app

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"

	"github.com/stayradiated/mosscap/internal/client"
	"github.com/stayradiated/mosscap/internal/config"
	"github.com/stayradiated/mosscap/internal/doctor"
	"github.com/stayradiated/mosscap/internal/protocol"
	"github.com/stayradiated/mosscap/internal/securepath"
	"github.com/stayradiated/mosscap/internal/store"
)

const (
	ExitSuccess        = 0
	ExitUsage          = 2
	ExitNotFound       = 3
	ExitConflict       = 4
	ExitUnsafe         = 5
	ExitUnavailable    = 6
	ExitExternalFailed = 7
	ExitAmbiguous      = 8
	ExitUnsupported    = 9
)

type gatewayBudget struct {
	once     sync.Once
	deadline time.Time
}

type App struct {
	Out     io.Writer
	Err     io.Writer
	In      io.Reader
	Version string

	// ctx and timeout are set for the copied App captured by each Cobra leaf.
	// They keep the caller's cancellation/deadline attached to every gateway RPC.
	ctx     context.Context
	timeout time.Duration
	budget  *gatewayBudget
}

func (a App) Run(ctx context.Context, args []string) int {
	if err := a.Execute(ctx, args); err != nil {
		var re *jsonRuntimeError
		if errors.As(err, &re) {
			a.renderRuntimeError(re)
			return exitCode(re.err)
		}
		var je *jsonMutationError
		if errors.As(err, &je) {
			a.renderMutationError(je)
			return exitCode(je.err)
		}
		var ce *commandError
		if errors.As(err, &ce) {
			fmt.Fprintln(a.Err, ce.Error())
			return ce.code
		}
		fmt.Fprintln(a.Err, err)
		if errors.Is(err, store.ErrIncompatibleSchema) {
			return ExitUnsupported
		}
		return ExitUnsafe
	}
	return ExitSuccess
}
func (a App) Execute(ctx context.Context, args []string) error {
	if a.Out == nil {
		a.Out = io.Discard
	}
	if a.Err == nil {
		a.Err = io.Discard
	}
	if a.In == nil {
		a.In = os.Stdin
	}
	if a.Version == "" {
		a.Version = "devel"
	}
	if ctx == nil {
		ctx = context.Background()
	}
	a.ctx = ctx
	root := a.NewCommandTree()
	root.SetArgs(args)
	root.SetContext(ctx)
	root.SetOut(a.Out)
	root.SetErr(a.Err)
	if err := root.Execute(); err != nil {
		var re *jsonRuntimeError
		if errors.As(err, &re) {
			return re
		}
		var ce *commandError
		if errors.As(err, &ce) && ce.code != ExitUsage {
			return ce
		}
		root.SetOut(a.Err)
		// Cobra stops at the command that rejected an option or argument. Find
		// that command again without interpreting operands, so usage errors guide
		// the relevant command family rather than always falling back to root.
		_ = usageCommand(root, args).Usage()
		if ce != nil {
			return ce
		}
		return usage(err.Error())
	}
	return nil
}

func usageCommand(root *cobra.Command, args []string) *cobra.Command {
	cmd := root
	for _, arg := range args {
		if arg == "--" || strings.HasPrefix(arg, "-") {
			break
		}
		var next *cobra.Command
		for _, child := range cmd.Commands() {
			if child.Name() == arg {
				next = child
				break
			}
		}
		if next == nil {
			break
		}
		cmd = next
	}
	return cmd
}

func (a App) readSecret(stdin bool) (string, error) {
	if stdin {
		if isTerminal(a.In) {
			return "", usage("config secret set --stdin requires non-TTY stdin")
		}
		return readSecretLine(a.In)
	}
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return "", errors.New("config secret set requires a controlling TTY; use --stdin for non-interactive input")
	}
	defer tty.Close()
	if !isTerminal(tty) {
		return "", errors.New("config secret set requires a controlling TTY; use --stdin for non-interactive input")
	}
	fmt.Fprint(a.Err, "Slack credential: ")
	first, err := readNoEcho(tty)
	if err != nil {
		return "", err
	}
	fmt.Fprint(a.Err, "Confirm Slack credential: ")
	second, err := readNoEcho(tty)
	if err != nil {
		return "", err
	}
	if first != second {
		return "", errors.New("Slack credential confirmation did not match")
	}
	return first, nil
}

func isTerminal(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func readSecretLine(r io.Reader) (string, error) {
	line, err := bufio.NewReader(io.LimitReader(r, 4097)).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", errors.New("could not read Slack credential")
	}
	if len(line) > 4096 || (!errors.Is(err, io.EOF) && !strings.HasSuffix(line, "\n")) {
		return "", errors.New("Slack credential input exceeds 4096 bytes")
	}
	return strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r"), nil
}

func readNoEcho(tty *os.File) (string, error) {
	old, err := unix.IoctlGetTermios(int(tty.Fd()), unix.TCGETS)
	if err != nil {
		return "", errors.New("could not disable terminal echo")
	}
	next := *old
	next.Lflag &^= unix.ECHO
	if err := unix.IoctlSetTermios(int(tty.Fd()), unix.TCSETS, &next); err != nil {
		return "", errors.New("could not disable terminal echo")
	}
	defer unix.IoctlSetTermios(int(tty.Fd()), unix.TCSETS, old)
	value, readErr := readSecretLine(tty)
	fmt.Fprintln(tty)
	if readErr != nil {
		return "", readErr
	}
	return value, nil
}

func (a App) renderDoctor(report doctor.Report, jsonOut bool) error {
	if jsonOut {
		return a.render(report, true)
	}

	color := colorEnabled(a.Out)
	fmt.Fprintf(a.Out, "Doctor: %s\n\n", formatDoctorStatus(report.Overall, color))
	nameWidth := 0
	for _, check := range report.Checks {
		if len(check.Name) > nameWidth {
			nameWidth = len(check.Name)
		}
	}
	for _, check := range report.Checks {
		name := formatDoctorName(check.Name, nameWidth, color)
		fmt.Fprintf(a.Out, "  %s  %s  %s\n", formatDoctorStatus(check.Status, color), name, check.Message)
		if check.Action != "" {
			action := "↳ Action:"
			if color {
				action = "\x1b[36m" + action + "\x1b[0m"
			}
			fmt.Fprintf(a.Out, "%s%s %s\n", strings.Repeat(" ", nameWidth+7), action, check.Action)
		}
	}
	return nil
}

func formatDoctorName(name string, width int, color bool) string {
	name = fmt.Sprintf("%-*s", width, name)
	if color {
		return "\x1b[1;34m" + name + "\x1b[0m"
	}
	return name
}

func formatDoctorStatus(status doctor.CheckStatus, color bool) string {
	icon, code := "?", "36"
	switch status {
	case doctor.Pass:
		icon, code = "✓", "32"
	case doctor.Warn:
		icon, code = "⚠", "33"
	case doctor.Fail:
		icon, code = "✗", "31"
	case doctor.Skipped:
		icon, code = "○", "2"
	}
	if color {
		return "\x1b[" + code + "m" + icon + "\x1b[0m"
	}
	return icon
}

func colorEnabled(w io.Writer) bool {
	if _, disabled := os.LookupEnv("NO_COLOR"); disabled || os.Getenv("TERM") == "dumb" {
		return false
	}
	f, ok := w.(*os.File)
	return ok && (isatty.IsTerminal(f.Fd()) || isatty.IsCygwinTerminal(f.Fd()))
}

// gatewayCall starts its timeout immediately before contacting the gateway, so
// local parsing and stdin consumption never consume the gateway budget.
func (a App) gatewayCall(c config.Config, command, requestID string, params any, requiredCapabilities ...string) (any, error) {
	ctx := a.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	timeout := a.timeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	if a.budget == nil {
		a.budget = &gatewayBudget{}
	}
	a.budget.once.Do(func() { a.budget.deadline = time.Now().Add(timeout) })
	ctx, cancel := context.WithDeadline(ctx, a.budget.deadline)
	defer cancel()
	if params == nil {
		params = protocol.EmptyParams{}
	}
	b, err := json.Marshal(params)
	if err != nil {
		return nil, usage("invalid request parameters")
	}
	request := protocol.Request{ProtocolVersion: protocol.Version, RequestID: requestID, ClientKind: "cli", ClientVersion: a.Version, Command: command, Params: b}
	if err := request.Validate(); err != nil {
		return nil, usage(err.Error())
	}
	cl, err := client.Dial(ctx, c.Paths.Endpoint, uint32(c.LocalProtocolMaximumFrame))
	if err != nil {
		return nil, &commandError{code: ExitUnavailable, message: err.Error()}
	}
	defer cl.Close()
	hello, err := cl.Hello(ctx, "req_hello_"+strings.ReplaceAll(command, ".", "_"), "cli", a.Version, nil)
	if err != nil {
		return nil, remote(err)
	}
	for _, requiredCapability := range requiredCapabilities {
		if requiredCapability != "" && !hasCapability(hello.Capabilities, requiredCapability) {
			return nil, unsupported("gateway does not support " + requiredCapability + "; upgrade the gateway before using this command")
		}
	}
	response, err := cl.Call(ctx, request)
	if err != nil {
		var submitted *client.RequestSubmittedError
		if errors.As(err, &submitted) && gatewayMutation(command) {
			return nil, &commandError{code: ExitAmbiguous, message: "gateway response was lost after request submission"}
		}
		return nil, remote(err)
	}
	if response.Error != nil {
		return nil, remote(&client.RemoteError{ProtocolError: *response.Error})
	}
	var out any
	if err := json.Unmarshal(response.Result, &out); err != nil {
		return nil, err
	}
	return out, nil
}
func localDoctorChecks(loc config.Location, c config.Config, loadErr error) []doctor.Check {
	checks := []doctor.Check{}
	if loadErr != nil {
		// Do not include parser errors: TOML parser diagnostics can quote a
		// token-bearing value. The fixed-endpoint checks below remain useful.
		checks = append(checks, doctor.Check{Name: "config", Status: doctor.Fail, Message: "staged configuration is malformed or obsolete", Action: "repair config.toml, then run gateway reload"})
	} else if err := config.Validate(c); err != nil {
		checks = append(checks, doctor.Check{Name: "config", Status: doctor.Fail, Message: "configuration is invalid", Action: "fix config.toml"})
	} else {
		checks = append(checks, doctor.Check{Name: "config", Status: doctor.Pass, Message: "configuration is valid"})
	}
	paths := []string{loc.Path, c.Paths.StateDir, c.Paths.Database, c.Paths.Endpoint, c.Paths.TmpDir}
	pathsSafe := true
	for _, path := range paths {
		if securepath.CheckAbsoluteComponents(path) != nil {
			pathsSafe = false
			break
		}
	}
	if !pathsSafe || securepath.CheckDir(c.Paths.StateDir) != nil || securepath.CheckDir(c.Paths.TmpDir) != nil || securepath.CheckFile(c.Paths.Database) != nil {
		checks = append(checks, doctor.Check{Name: "private_paths", Status: doctor.Fail, Message: "configured path components or state paths are not private and safe", Action: "repair configured path components, ownership, and modes"})
	} else {
		checks = append(checks, doctor.Check{Name: "private_paths", Status: doctor.Pass, Message: "configured path components and state paths are private and safe"})
	}
	var fs syscall.Statfs_t
	if err := syscall.Statfs(c.Paths.StateDir, &fs); err != nil {
		checks = append(checks, doctor.Check{Name: "storage_space", Status: doctor.Warn, Message: "state filesystem space is unavailable", Action: "restore the state directory and inspect available space"})
	} else {
		available := uint64(fs.Bavail) * uint64(fs.Bsize)
		if available < 10*1024*1024 {
			checks = append(checks, doctor.Check{Name: "storage_space", Status: doctor.Warn, Message: "state filesystem has less than 10 MiB available", Action: "free space before running the gateway"})
		} else {
			checks = append(checks, doctor.Check{Name: "storage_space", Status: doctor.Pass, Message: "state filesystem has available space"})
		}
	}
	// Redaction is structural: the view type has no token-value fields.
	_ = config.Redact(c)
	checks = append(checks, doctor.Check{Name: "secret_redaction", Status: doctor.Pass, Message: "diagnostics structurally omit Slack token values"})
	_ = loc
	return checks
}

func (a App) renderTrustedUsers(users protocol.TrustedUserListDTO, jsonOut bool) error {
	if jsonOut {
		return a.render(users, true)
	}

	color := colorEnabled(a.Out)
	fmt.Fprintf(a.Out, "Trusted Slack users: %d\n\n", len(users.SlackUserIDs))
	if len(users.SlackUserIDs) == 0 {
		fmt.Fprintln(a.Out, "  No users are trusted.")
		return nil
	}
	for _, userID := range users.SlackUserIDs {
		bullet, formattedID := "•", userID
		// User display names are Slack-provided text. Escape only for this
		// terminal projection; the JSON DTO preserves the original value.
		name := escapeTerminal(users.SlackUserNames[userID])
		formattedName := ""
		if name != "" {
			formattedName = "  @" + name
		}
		if color {
			bullet = "\x1b[32m" + bullet + "\x1b[0m"
			formattedID = "\x1b[1;34m" + formattedID + "\x1b[0m"
			if formattedName != "" {
				formattedName = "  \x1b[36m@" + name + "\x1b[0m"
			}
		}
		fmt.Fprintf(a.Out, "  %s  %s%s\n", bullet, formattedID, formattedName)
	}
	return nil
}

// gatewayMutation is intentionally conservative: a response lost after any
// request that can write durable state is not safe to replay automatically.
func gatewayMutation(command string) bool {
	switch command {
	case "config.reload", "metadata.purge", "trust.add", "trust.remove", "task.create", "task.update", "task.transition", "task.reply", "slack.send", "slack.file", "slack.edit", "slack.delete", "slack.react", "slack.unreact", "effect.retry", "file.get", "file.release":
		return true
	default:
		return false
	}
}

func hasCapability(capabilities []string, required string) bool {
	for _, capability := range capabilities {
		if capability == required {
			return true
		}
	}
	return false
}

func remote(err error) error {
	var re *client.RemoteError
	if errors.As(err, &re) {
		code := ExitUnsafe
		switch re.ProtocolError.Category {
		case "not_found":
			code = ExitNotFound
		case "conflict":
			code = ExitConflict
		case "usage":
			code = ExitUsage
		case "unsupported":
			code = ExitUnsupported
		case "unavailable":
			code = ExitUnavailable
		case "external_failed":
			code = ExitExternalFailed
		case "ambiguous":
			code = ExitAmbiguous
		}
		return &commandError{code: code, message: re.Error(), protocolError: &re.ProtocolError}
	}
	return &commandError{code: ExitUnavailable, message: err.Error()}
}

func (a App) render(v any, jsonOut bool) error {
	if jsonOut {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		fmt.Fprintln(a.Out, string(b))
		return nil
	}
	return renderHuman(a.Out, v)
}

// renderPage preserves the DTO in JSON mode while making bounded human reads
// directly resumable. continuation intentionally contains no prior cursor.
func (a App) renderPage(v any, jsonOut bool, continuation string) error {
	if err := a.render(v, jsonOut); err != nil || jsonOut {
		return err
	}
	return renderPaginationContinuation(a.Out, v, continuation)
}

type commandError struct {
	code          int
	message       string
	protocolError *protocol.Error
}

func (e *commandError) Error() string { return e.message }

// jsonRuntimeError marks an error from a successfully parsed --json leaf.
// It is deliberately created from Cobra flag state, never from argv.
type jsonRuntimeError struct {
	err       error
	requestID string
	partial   any
}

func (e *jsonRuntimeError) Error() string { return e.err.Error() }
func (e *jsonRuntimeError) Unwrap() error { return e.err }

type doctorPartialError struct {
	err    error
	report doctor.Report
}

func (e *doctorPartialError) Error() string { return e.err.Error() }
func (e *doctorPartialError) Unwrap() error { return e.err }

// runtimeJSONError makes the process contract apply to every runtime failure
// after Cobra has parsed a leaf's --json flag. Usage failures are intentionally
// left alone so they remain text on stderr with exit 2.
func (a App) runtimeJSONError(cmd *cobra.Command, err error) error {
	if err == nil {
		return nil
	}
	var ce *commandError
	if errors.As(err, &ce) && ce.code == ExitUsage {
		return err
	}
	jsonOut, _ := cmd.Flags().GetBool("json")
	if !jsonOut {
		return err
	}
	requestID, _ := cmd.Flags().GetString("request-id")
	var mutation *jsonMutationError
	if errors.As(err, &mutation) && mutation.requestID != "" {
		requestID = mutation.requestID
	}
	var partial *doctorPartialError
	if errors.As(err, &partial) {
		return &jsonRuntimeError{err: partial.err, requestID: requestID, partial: partial.report}
	}
	return &jsonRuntimeError{err: err, requestID: requestID}
}

type jsonMutationError struct {
	err       error
	requestID string
	jsonOut   bool
}

func (e *jsonMutationError) Error() string { return e.err.Error() }
func (e *jsonMutationError) Unwrap() error { return e.err }

// mutationJSONError opts in only parsed mutation commands. It deliberately
// does not inspect argv, so a value that happens to be "--json" cannot change
// the output protocol.
func mutationJSONError(err error, mutation, jsonOut bool, requestID string) error {
	if err == nil || !mutation {
		return err
	}
	return &jsonMutationError{err: err, requestID: requestID, jsonOut: jsonOut}
}

func exitCode(err error) int {
	var ce *commandError
	if errors.As(err, &ce) {
		return ce.code
	}
	return ExitUnsafe
}

type runtimeErrorJSON struct {
	Code          string                     `json:"code"`
	Category      string                     `json:"category"`
	Message       string                     `json:"message"`
	Retriable     bool                       `json:"retriable"`
	Details       map[string]string          `json:"details,omitempty"`
	Progress      *protocol.PurgeProgressDTO `json:"progress,omitempty"`
	PartialResult any                        `json:"partial_result,omitempty"`
	RequestID     string                     `json:"request_id,omitempty"`
	Guidance      string                     `json:"guidance,omitempty"`
}

type mutationErrorJSON = runtimeErrorJSON

func runtimeErrorDTO(err error, requestID string, partial any) runtimeErrorJSON {
	out := runtimeErrorJSON{Code: "local_error", Category: "local", Message: err.Error(), RequestID: requestID, PartialResult: partial}
	var ce *commandError
	if errors.As(err, &ce) && ce.protocolError != nil {
		out.Code, out.Category, out.Message = ce.protocolError.Code, ce.protocolError.Category, ce.protocolError.Message
		out.Retriable, out.Details, out.Progress = ce.protocolError.Retriable, ce.protocolError.Details, ce.protocolError.Progress
	} else {
		switch exitCode(err) {
		case ExitNotFound:
			out.Code, out.Category = "not_found", "not_found"
		case ExitConflict:
			out.Code, out.Category = "conflict", "conflict"
		case ExitUnavailable:
			out.Code, out.Category, out.Retriable = "unavailable", "unavailable", true
		case ExitExternalFailed:
			out.Code, out.Category = "external_failed", "external_failed"
		case ExitAmbiguous:
			out.Code, out.Category = "ambiguous", "ambiguous"
		case ExitUnsupported:
			out.Code, out.Category = "unsupported", "unsupported"
		}
	}
	switch {
	case out.Code == "request_id_conflict":
		out.Guidance = "This request ID is bound to another input; do not retry changed input with it."
	case out.Category == "unavailable":
		out.Guidance = "Replay the exact mutation with the same request ID."
	case out.Category == "ambiguous":
		out.Guidance = "Inspect the durable effect; never automatically resend this mutation."
	case out.Category == "external_failed":
		out.Guidance = "Inspect and revise the external failure; do not replay it as if uncertain."
	}
	return out
}

func (a App) renderRuntimeError(wrapped *jsonRuntimeError) {
	_ = json.NewEncoder(a.Out).Encode(runtimeErrorDTO(wrapped.err, wrapped.requestID, wrapped.partial))
}

func (a App) renderMutationError(wrapped *jsonMutationError) {
	// Compatibility renderer for callers that still opt in per mutation.
	// Parsed Cobra --json leaves use renderRuntimeError instead.
	out := runtimeErrorJSON{Code: "local_error", Category: "local", Message: wrapped.err.Error()}
	var ce *commandError
	if errors.As(wrapped.err, &ce) && ce.protocolError != nil {
		out.Code, out.Category, out.Message = ce.protocolError.Code, ce.protocolError.Category, ce.protocolError.Message
		out.Retriable, out.Details, out.Progress = ce.protocolError.Retriable, ce.protocolError.Details, ce.protocolError.Progress
	} else if exitCode(wrapped.err) == ExitUnavailable {
		out.Code, out.Category, out.Retriable = "unavailable", "unavailable", true
	}
	out.RequestID = wrapped.requestID
	switch {
	case out.Code == "request_id_conflict":
		out.Guidance = "This request ID is bound to another input; do not retry changed input with it."
	case out.Category == "unavailable":
		out.Guidance = "Replay the exact mutation with the same request ID."
	case out.Category == "ambiguous":
		out.Guidance = "Inspect the durable effect; never automatically resend this mutation."
	case out.Category == "external_failed":
		out.Guidance = "Inspect and revise the external failure; do not replay it as if uncertain."
	}
	if wrapped.jsonOut {
		_ = json.NewEncoder(a.Err).Encode(out)
		return
	}
	fmt.Fprintln(a.Err, wrapped.err)
	fmt.Fprintf(a.Err, "request_id: %s\n", wrapped.requestID)
	if out.Guidance != "" {
		fmt.Fprintf(a.Err, "guidance: %s\n", out.Guidance)
	}
}
func usage(m string) error       { return &commandError{code: ExitUsage, message: "usage: " + m} }
func unsupported(m string) error { return &commandError{code: ExitUnsupported, message: m} }

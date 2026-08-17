// Package app contains Mosscap's command tree and process-facing rendering.
package app

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/mattn/go-isatty"
	"golang.org/x/sys/unix"

	"github.com/stayradiated/mosscap/internal/client"
	"github.com/stayradiated/mosscap/internal/config"
	"github.com/stayradiated/mosscap/internal/doctor"
	"github.com/stayradiated/mosscap/internal/identity"
	"github.com/stayradiated/mosscap/internal/protocol"
	"github.com/stayradiated/mosscap/internal/securepath"
	"github.com/stayradiated/mosscap/internal/server"
	"github.com/stayradiated/mosscap/internal/slackruntime"
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

type App struct {
	Out     io.Writer
	Err     io.Writer
	In      io.Reader
	Version string
}

func (a App) Run(ctx context.Context, args []string) int {
	if err := a.Execute(ctx, args); err != nil {
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
func (a App) Execute(_ context.Context, args []string) error {
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
	if len(args) == 0 || isHelp(args[0]) {
		a.printRootHelp()
		return nil
	}
	if args[0] == "--version" || args[0] == "version" {
		if len(args) != 1 {
			return usage("version accepts no arguments")
		}
		fmt.Fprintln(a.Out, a.Version)
		return nil
	}
	switch args[0] {
	case "setup":
		return a.setup("", args[1:])
	case "config":
		return a.config("", args[1:])
	case "doctor":
		return a.doctorCommand("", args[1:])
	case "gateway":
		return a.gateway("", args[1:])
	case "trust":
		return a.trust(args[1:])
	case "purge":
		return a.purge(args[1:])
	case "task":
		return a.task("", args[1:])
	case "slack":
		return a.slack(args[1:])
	case "effect":
		return a.effect(args[1:])
	case "file":
		return a.file(args[1:])
	case "delivery":
		return a.delivery(args[1:])
	default:
		return usage(fmt.Sprintf("unknown command %q", args[0]))
	}
}
func (a App) setup(_ string, args []string) error {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	jsonOut := fs.Bool("json", false, "")
	help := fs.Bool("help", false, "")
	fs.BoolVar(help, "h", false, "")
	if err := fs.Parse(args); err != nil {
		return usage(err.Error())
	}
	if *help {
		fmt.Fprintln(a.Out, "Usage: mosscap setup [--json]\n\nCreate or validate private configuration and state directories.")
		return nil
	}
	if fs.NArg() != 0 {
		return usage("setup accepts no arguments")
	}
	loc, err := config.DefaultLocation()
	if err != nil {
		return err
	}
	c, err := config.Setup(loc)
	if err != nil {
		return err
	}
	return a.render(config.Redact(c), *jsonOut)
}
func (a App) config(override string, args []string) error {
	if len(args) == 0 || isHelp(args[0]) {
		a.printBranchHelp("config", []string{"show", "set", "secret set"})
		return nil
	}
	switch args[0] {
	case "show":
		return a.configShow(override, args[1:])
	case "set":
		return a.configSet(override, args[1:])
	case "secret":
		return a.configSecret(override, args[1:])
	default:
		return usage(fmt.Sprintf("unknown config command %q", args[0]))
	}
}
func (a App) configShow(_ string, args []string) error {
	fs := flag.NewFlagSet("config show", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	j := fs.Bool("json", false, "")
	h := fs.Bool("help", false, "")
	fs.BoolVar(h, "h", false, "")
	if err := fs.Parse(args); err != nil {
		return usage(err.Error())
	}
	if *h {
		fmt.Fprintln(a.Out, "Usage: mosscap config show [--json]")
		return nil
	}
	if fs.NArg() != 0 {
		return usage("config show accepts no arguments")
	}
	loc, err := config.DefaultLocation()
	if err != nil {
		return err
	}
	c, err := config.Load(loc)
	if err != nil {
		return err
	}
	return a.render(config.Redact(c), *j)
}
func (a App) configSet(_ string, args []string) error {
	fs := flag.NewFlagSet("config set", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	j := fs.Bool("json", false, "")
	h := fs.Bool("help", false, "")
	fs.BoolVar(h, "h", false, "")
	if err := fs.Parse(reorderTaskFlags(args)); err != nil {
		return usage(err.Error())
	}
	if *h {
		fmt.Fprintln(a.Out, "Usage: mosscap config set KEY VALUE [--json]\n\nStage one validated non-secret setting without changing the gateway's active snapshot. Set Slack credentials with `mosscap config secret set`; credentials are resolved only from private config.toml, never environment variables.")
		return nil
	}
	if fs.NArg() != 2 {
		return usage("config set requires KEY and VALUE")
	}
	if fs.Arg(0) == "slack_app_token" || fs.Arg(0) == "slack_bot_token" {
		return usage("Slack credentials must use `mosscap config secret set " + fs.Arg(0) + "`")
	}
	loc, err := config.DefaultLocation()
	if err != nil {
		return err
	}
	c, err := config.Set(loc, fs.Arg(0), fs.Arg(1))
	if err != nil {
		return err
	}
	return a.render(config.Redact(c), *j)
}

func (a App) configSecret(_ string, args []string) error {
	if len(args) == 0 || isHelp(args[0]) {
		fmt.Fprintln(a.Out, "Usage: mosscap config secret set (slack_app_token|slack_bot_token) [--stdin]\n\nRead one Slack credential without accepting it in argv. Without --stdin, read and confirm from the controlling TTY with echo disabled. Credentials are stored only in private config.toml; environment variables are not used.")
		return nil
	}
	if args[0] != "set" {
		return usage(fmt.Sprintf("unknown config secret command %q", args[0]))
	}
	fs := flag.NewFlagSet("config secret set", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	stdin := fs.Bool("stdin", false, "")
	help := fs.Bool("help", false, "")
	fs.BoolVar(help, "h", false, "")
	if err := fs.Parse(reorderTaskFlags(args[1:])); err != nil {
		return usage(err.Error())
	}
	if *help {
		fmt.Fprintln(a.Out, "Usage: mosscap config secret set (slack_app_token|slack_bot_token) [--stdin]")
		return nil
	}
	if fs.NArg() != 1 || (fs.Arg(0) != "slack_app_token" && fs.Arg(0) != "slack_bot_token") {
		return usage("config secret set requires slack_app_token or slack_bot_token")
	}
	value, err := a.readSecret(*stdin)
	if err != nil {
		return err
	}
	loc, err := config.DefaultLocation()
	if err != nil {
		return err
	}
	if _, err := config.SetSecret(loc, fs.Arg(0), value); err != nil {
		return err
	}
	fmt.Fprintln(a.Out, fs.Arg(0)+" updated")
	return nil
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
func (a App) gateway(override string, args []string) error {
	if len(args) == 0 || isHelp(args[0]) {
		a.printBranchHelp("gateway", []string{"run", "reload", "status"})
		return nil
	}
	if args[0] == "status" {
		return a.gatewayStatus(override, args[1:])
	}
	if args[0] == "reload" {
		return a.gatewayReload(args[1:])
	}
	if args[0] != "run" {
		return usage(fmt.Sprintf("unknown gateway command %q", args[0]))
	}
	fs := flag.NewFlagSet("gateway run", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	h := fs.Bool("help", false, "")
	fs.BoolVar(h, "h", false, "")
	if err := fs.Parse(args[1:]); err != nil {
		return usage(err.Error())
	}
	if *h {
		a.printLeafHelp("gateway run")
		return nil
	}
	if fs.NArg() != 0 {
		return usage("gateway run accepts no arguments")
	}
	loc, err := config.DefaultLocation()
	if err != nil {
		return err
	}
	c, err := config.Load(loc)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return unsupported("gateway run is not implemented until setup has completed")
		}
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return server.NewWithOptions(c, loc, a.Version, slog.New(slog.NewTextHandler(a.Err, nil)), server.Options{Factory: slackruntime.RealFactory{}}).Serve(ctx)
}

func (a App) gatewayReload(args []string) error {
	fs := flag.NewFlagSet("gateway reload", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	j := fs.Bool("json", false, "")
	h := fs.Bool("help", false, "")
	fs.BoolVar(h, "h", false, "")
	requestID := fs.String("request-id", "", "")
	if err := fs.Parse(args); err != nil {
		return usage(err.Error())
	}
	if *h {
		fmt.Fprintln(a.Out, "Usage: mosscap gateway reload [--request-id ID] [--json]\n\nValidate and atomically activate the complete staged configuration.")
		return nil
	}
	if fs.NArg() != 0 {
		return usage("gateway reload accepts no arguments")
	}
	if *requestID == "" {
		id, err := identity.NewGenerator(nil).NewRequestID()
		if err != nil {
			return err
		}
		*requestID = id
	}
	loc, err := config.DefaultLocation()
	if err != nil {
		return err
	}
	// The gateway, not the short-lived CLI, owns candidate parsing and atomic
	// activation. Use fixed XDG paths and the conservative default frame bound so
	// even a malformed staged file reaches the running gateway for a typed error.
	c, err := config.Default(loc)
	if err != nil {
		return err
	}
	result, err := a.gatewayCall(c, "config.reload", *requestID, nil, protocol.CapabilityConfigReload)
	if err != nil {
		return mutationJSONError(err, *j, *requestID)
	}
	return a.render(map[string]any{"request_id": *requestID, "result": result}, *j)
}

func (a App) gatewayStatus(_ string, args []string) error {
	fs := flag.NewFlagSet("gateway status", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	j := fs.Bool("json", false, "")
	h := fs.Bool("help", false, "")
	fs.BoolVar(h, "h", false, "")
	if err := fs.Parse(args); err != nil {
		return usage(err.Error())
	}
	if *h {
		fmt.Fprintln(a.Out, "Usage: mosscap gateway status [--json]")
		return nil
	}
	if fs.NArg() != 0 {
		return usage("gateway status accepts no arguments")
	}
	loc, err := config.DefaultLocation()
	if err != nil {
		return err
	}
	c, err := config.Default(loc)
	if err != nil {
		return err
	}
	result, err := a.gatewayCall(c, "status.get", "req_status", nil, "")
	if err != nil {
		return err
	}
	return a.render(result, *j)
}

func (a App) doctorCommand(_ string, args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	j := fs.Bool("json", false, "")
	h := fs.Bool("help", false, "")
	fs.BoolVar(h, "h", false, "")
	if err := fs.Parse(args); err != nil {
		return usage(err.Error())
	}
	if *h {
		fmt.Fprintln(a.Out, "Usage: mosscap doctor [--json]\n\nRun non-destructive local and gateway diagnostics.")
		return nil
	}
	if fs.NArg() != 0 {
		return usage("doctor accepts no arguments")
	}
	loc, err := config.DefaultLocation()
	if err != nil {
		return err
	}
	c, configErr := config.Load(loc)
	if configErr != nil {
		// A staged edit must not prevent doctor from reaching the fixed endpoint
		// of a healthy gateway still using its previous snapshot. Paths are not
		// configurable, so the default supplies only those fixed locations.
		c, err = config.Default(loc)
		if err != nil {
			return err
		}
	}
	checks := localDoctorChecks(loc, c, configErr)
	remoteResult, remoteErr := a.gatewayCall(c, "doctor.run", "req_doctor", nil, "")
	if remoteErr != nil {
		checks = append(checks, doctor.Check{Name: "gateway_diagnostics", Status: doctor.Skipped, Message: "gateway diagnostics unavailable", Action: "start mosscap gateway run"})
		report := doctor.Report{Overall: doctor.Overall(checks), Checks: checks}
		if err := a.renderDoctor(report, *j); err != nil {
			return err
		}
		return &commandError{code: ExitUnavailable, message: "gateway diagnostics unavailable"}
	}
	b, _ := json.Marshal(remoteResult)
	var remote doctor.Report
	if err := json.Unmarshal(b, &remote); err != nil {
		return err
	}
	checks = append(checks, remote.Checks...)
	return a.renderDoctor(doctor.Report{Overall: doctor.Overall(checks), Checks: checks}, *j)
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

func (a App) gatewayCall(c config.Config, command, requestID string, params any, requiredCapability string) (any, error) {
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
	cl, err := client.Dial(context.Background(), c.Paths.Endpoint, uint32(c.LocalProtocolMaximumFrame))
	if err != nil {
		return nil, &commandError{code: ExitUnavailable, message: err.Error()}
	}
	defer cl.Close()
	hello, err := cl.Hello(context.Background(), "req_hello_"+strings.ReplaceAll(command, ".", "_"), "cli", a.Version, nil)
	if err != nil {
		return nil, remote(err)
	}
	if requiredCapability != "" && !hasCapability(hello.Capabilities, requiredCapability) {
		return nil, unsupported("gateway does not support " + requiredCapability + "; upgrade the gateway before using this command")
	}
	response, err := cl.Call(context.Background(), request)
	if err != nil {
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

func (a App) purge(args []string) error {
	fs := flag.NewFlagSet("purge", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	before := fs.String("before", "", "")
	dryRun := fs.Bool("dry-run", false, "")
	confirm := fs.Bool("confirm", false, "")
	requestID := fs.String("request-id", "", "")
	jsonOut := fs.Bool("json", false, "")
	help := fs.Bool("help", false, "")
	fs.BoolVar(help, "h", false, "")
	if err := fs.Parse(args); err != nil {
		return usage(err.Error())
	}
	if *help {
		fmt.Fprintln(a.Out, "Usage: mosscap purge --before RFC3339 (--dry-run|--confirm) [--request-id ID] [--json]\n\nPreview is read-only. Confirmed removal is irreversible.")
		return nil
	}
	dryRuns, confirms := countFlag(args, "--dry-run"), countFlag(args, "--confirm")
	if fs.NArg() != 0 || *before == "" || dryRuns+confirms != 1 || (!*dryRun && !*confirm) || !validPurgeModeFlags(args) || countFlag(args, "--before") != 1 {
		return usage("purge requires --before and exactly one of --dry-run or --confirm")
	}
	cutoff, err := protocol.ParsePurgeCutoff(*before)
	if err != nil {
		return usage("--before must be RFC3339 with an offset and at most microsecond precision")
	}
	if *dryRun && *requestID != "" {
		return usage("--request-id is confirm-only")
	}
	if *confirm && *requestID == "" {
		*requestID, err = identity.NewGenerator(nil).NewRequestID()
		if err != nil {
			return err
		}
	}
	if *dryRun {
		*requestID = "req_purge_preview"
	}
	loc, err := config.DefaultLocation()
	if err != nil {
		return err
	}
	c, err := config.Default(loc)
	if err != nil {
		return err
	}
	mode := "dry_run"
	if *confirm {
		mode = "confirm"
		if !*jsonOut {
			fmt.Fprintln(a.Err, "Warning: confirmed metadata removal is irreversible.")
		}
	}
	result, err := a.gatewayCall(c, "metadata.purge", *requestID, protocol.MetadataPurgeParams{Cutoff: cutoff.Format("2006-01-02T15:04:05.000000Z"), Mode: mode}, protocol.CapabilityMetadataPurge)
	if err != nil {
		return mutationJSONError(err, *jsonOut && *confirm, *requestID)
	}
	if *confirm {
		result = map[string]any{"request_id": *requestID, "result": result}
	}
	return a.render(result, *jsonOut)
}

func countFlag(args []string, name string) int {
	count := 0
	for _, arg := range args {
		if arg == name || strings.HasPrefix(arg, name+"=") {
			count++
		}
	}
	return count
}

// Bool flags select an irreversible mode: spelling one with an explicit false
// value is not a harmless synonym for omitting it.
func validPurgeModeFlags(args []string) bool {
	for _, arg := range args {
		for _, name := range []string{"--dry-run", "--confirm"} {
			if strings.HasPrefix(arg, name+"=") && arg != name+"=true" {
				return false
			}
		}
	}
	return true
}

func (a App) trust(args []string) error {
	if len(args) == 0 || isHelp(args[0]) {
		a.printBranchHelp("trust", []string{"list", "add", "remove"})
		return nil
	}
	cmd := args[0]
	if cmd != "list" && cmd != "add" && cmd != "remove" {
		return usage(fmt.Sprintf("unknown trust command %q", cmd))
	}
	fs := flag.NewFlagSet("trust "+cmd, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	j := fs.Bool("json", false, "")
	h := fs.Bool("help", false, "")
	fs.BoolVar(h, "h", false, "")
	requestID := fs.String("request-id", "", "")
	if err := fs.Parse(reorderTaskFlags(args[1:])); err != nil {
		return usage(err.Error())
	}
	if *h {
		if cmd == "list" {
			fmt.Fprintln(a.Out, "Usage: mosscap trust list [--json]")
		} else {
			fmt.Fprintf(a.Out, "Usage: mosscap trust %s SLACK_USER_ID [--request-id ID] [--json]\n", cmd)
		}
		return nil
	}
	mutation := cmd != "list"
	if !mutation {
		if fs.NArg() != 0 || *requestID != "" {
			return usage("trust list accepts no arguments and no --request-id")
		}
		*requestID = "req_trust_list"
	} else {
		if fs.NArg() != 1 {
			return usage("trust " + cmd + " requires SLACK_USER_ID")
		}
		if *requestID == "" {
			id, err := identity.NewGenerator(nil).NewRequestID()
			if err != nil {
				return err
			}
			*requestID = id
		}
	}
	loc, err := config.DefaultLocation()
	if err != nil {
		return err
	}
	// Trust is live SQLite authority and remains operable independently of a
	// staged config edit; only the fixed endpoint and a bounded codec are needed.
	c, err := config.Default(loc)
	if err != nil {
		return err
	}
	var params any = protocol.EmptyParams{}
	command := "trust.list"
	if mutation {
		params = protocol.TrustMutationParams{SlackUserID: fs.Arg(0)}
		command = "trust." + cmd
	}
	result, err := a.gatewayCall(c, command, *requestID, params, protocol.CapabilityTrustedUsers)
	if err != nil {
		return mutationJSONError(err, *j && mutation, *requestID)
	}
	if mutation {
		result = map[string]any{"request_id": *requestID, "result": result}
		return a.render(result, *j)
	}
	b, err := json.Marshal(result)
	if err != nil {
		return err
	}
	var users protocol.TrustedUserListDTO
	if err := json.Unmarshal(b, &users); err != nil {
		return err
	}
	return a.renderTrustedUsers(users, *j)
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
		name := users.SlackUserNames[userID]
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

func (a App) task(_ string, args []string) error {
	if len(args) == 0 || isHelp(args[0]) {
		a.printBranchHelp("task", []string{"create", "list", "show", "update", "open", "working", "done", "reply"})
		return nil
	}
	cmd := args[0]
	if cmd == "reply" {
		return a.taskReply(args[1:])
	}
	if cmd != "create" && cmd != "list" && cmd != "show" && cmd != "update" && cmd != "open" && cmd != "working" && cmd != "done" {
		return usage(fmt.Sprintf("unknown task command %q", cmd))
	}
	fs := flag.NewFlagSet("task "+cmd, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	j := fs.Bool("json", false, "")
	h := fs.Bool("help", false, "")
	fs.BoolVar(h, "h", false, "")
	text := fs.String("text", "", "")
	textFile := fs.String("text-file", "", "")
	requestID := fs.String("request-id", "", "")
	note := fs.String("note", "", "")
	state := fs.String("state", "", "")
	all := fs.Bool("all", false, "")
	limit := fs.Int("limit", 0, "")
	if err := fs.Parse(reorderTaskFlags(args[1:])); err != nil {
		return usage(err.Error())
	}
	if *h {
		a.printTaskHelp(cmd)
		return nil
	}
	var params any
	mutation := cmd == "create" || cmd == "update" || cmd == "open" || cmd == "working" || cmd == "done"
	var loaded config.Config
	readText := func() (string, error) {
		if (*text == "") == (*textFile == "") {
			return "", usage("exactly one of --text or --text-file is required")
		}
		if *textFile != "" {
			loc, e := config.DefaultLocation()
			if e != nil {
				return "", e
			}
			loaded, e = config.Load(loc)
			if e != nil {
				return "", e
			}
			b, e := securepath.ReadRegularFile(*textFile, int64(loaded.LocalProtocolMaximumFrame))
			if e != nil {
				return "", fmt.Errorf("--text-file: %w", e)
			}
			return string(b), nil
		}
		return *text, nil
	}
	switch cmd {
	case "create":
		t, e := readText()
		if e != nil {
			return e
		}
		params = protocol.TaskCreateParams{Text: t}
	case "update":
		if fs.NArg() != 1 {
			return usage("task update requires TASK")
		}
		t, e := readText()
		if e != nil {
			return e
		}
		params = protocol.TaskUpdateParams{Task: fs.Arg(0), Text: t}
	case "show":
		if fs.NArg() != 1 {
			return usage("task show requires TASK")
		}
		params = protocol.TaskSelector{Task: fs.Arg(0)}
	case "list":
		if fs.NArg() != 0 {
			return usage("task list accepts no arguments")
		}
		if *state != "" && *all {
			return usage("--state and --all are mutually exclusive")
		}
		var sp *string
		if *state != "" {
			if *state != "open" && *state != "working" && *state != "done" {
				return usage("invalid --state")
			}
			sp = state
		}
		params = protocol.TaskListParams{State: sp, IncludeDone: *all, Limit: *limit}
	default:
		if fs.NArg() != 1 {
			return usage("task transition requires TASK")
		}
		st := cmd
		params = protocol.TaskTransitionParams{Task: fs.Arg(0), State: st, Note: note}
	}
	if !mutation && *requestID != "" {
		return usage("--request-id is mutation-only")
	}
	if mutation && *requestID == "" {
		id, e := identity.NewGenerator(nil).NewRequestID()
		if e != nil {
			return e
		}
		*requestID = id
	}
	if !mutation {
		*requestID = "req_read" + strings.ReplaceAll(cmd, ".", "_")
	}
	c := loaded
	if c.Paths.Endpoint == "" {
		loc, e := config.DefaultLocation()
		if e != nil {
			return e
		}
		c, e = config.Load(loc)
		if e != nil {
			return e
		}
	}
	b, _ := json.Marshal(params)
	request := protocol.Request{ProtocolVersion: protocol.Version, RequestID: *requestID, ClientKind: "cli", ClientVersion: a.Version, Command: map[string]string{"show": "task.get", "list": "task.list", "create": "task.create", "update": "task.update", "open": "task.transition", "working": "task.transition", "done": "task.transition"}[cmd], Params: b}
	if *textFile != "" {
		encoded, encodeErr := protocol.EncodeRequest(request)
		if encodeErr != nil {
			return encodeErr
		}
		if len(encoded) > c.LocalProtocolMaximumFrame {
			return usage("--text-file exceeds the protocol frame limit")
		}
	}
	cl, e := client.Dial(context.Background(), c.Paths.Endpoint, uint32(c.LocalProtocolMaximumFrame))
	if e != nil {
		return mutationJSONError(&commandError{code: ExitUnavailable, message: e.Error()}, *j && mutation, *requestID)
	}
	defer cl.Close()
	if _, e = cl.Hello(context.Background(), "req_hello_"+cmd, "cli", a.Version, nil); e != nil {
		return mutationJSONError(remote(e), *j && mutation, *requestID)
	}
	response, e := cl.Call(context.Background(), request)
	if e != nil {
		return mutationJSONError(remote(e), *j && mutation, *requestID)
	}
	if response.Error != nil {
		return mutationJSONError(remote(&client.RemoteError{ProtocolError: *response.Error}), *j && mutation, *requestID)
	}
	var out any
	if e := json.Unmarshal(response.Result, &out); e != nil {
		return e
	}
	if mutation {
		out = map[string]any{"request_id": *requestID, "result": out}
	}
	return a.render(out, *j)
}

// reorderTaskFlags allows the documented TASK-before-flags form while keeping
// the standard library flag parser for all option validation.
func reorderTaskFlags(args []string) []string {
	valueFlags := map[string]bool{"--text": true, "--text-file": true, "--comment": true, "--request-id": true, "--note": true, "--state": true, "--limit": true, "--thread": true, "--task": true, "--reason": true, "--file": true, "--output": true}
	flags := make([]string, 0, len(args))
	positionals := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if strings.HasPrefix(arg, "-") {
			flags = append(flags, arg)
			if valueFlags[arg] && i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
			continue
		}
		positionals = append(positionals, arg)
	}
	return append(flags, positionals...)
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
func (a App) taskReply(args []string) error {
	fs := newPhase1FlagSet("task reply")
	jsonOut := fs.Bool("json", false, "")
	text := fs.String("text", "", "")
	textFile := fs.String("text-file", "", "")
	done := fs.Bool("done", false, "")
	requestID := fs.String("request-id", "", "")
	help := fs.Bool("help", false, "")
	fs.BoolVar(help, "h", false, "")
	if err := fs.Parse(reorderTaskFlags(args)); err != nil {
		return usage(err.Error())
	}
	if *help {
		fmt.Fprintln(a.Out, "Usage: mosscap task reply TASK (--text TEXT | --text-file PATH) [--done] [--request-id ID] [--json]\n\nReply to the task's durable Slack source thread. --done completes only after Slack confirms the reply; this is resumable, not a cross-system transaction.")
		return nil
	}
	if err := phase1Only(fs, []string{"json", "text", "text-file", "done", "request-id"}); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return usage("task reply requires TASK")
	}
	t, err := a.phase1Text(*text, *textFile)
	if err != nil {
		return err
	}
	if *requestID == "" {
		*requestID, err = identity.NewGenerator(nil).NewRequestID()
		if err != nil {
			return err
		}
	}
	c, err := phase1Config()
	if err != nil {
		return err
	}
	params := protocol.TaskReplyParams{Task: fs.Arg(0), Text: t, Done: *done}
	if b, e := protocol.EncodeRequest(protocol.Request{ProtocolVersion: protocol.Version, RequestID: *requestID, ClientKind: "cli", ClientVersion: a.Version, Command: "task.reply", Params: mustJSON(params)}); e != nil || len(b) > c.LocalProtocolMaximumFrame {
		return usage("request exceeds the protocol frame limit")
	}
	out, err := a.gatewayCall(c, "task.reply", *requestID, params, protocol.CapabilityTaskReply)
	if err != nil {
		return mutationJSONError(err, *jsonOut, *requestID)
	}
	out = map[string]any{"request_id": *requestID, "result": out}
	return a.render(out, *jsonOut)
}

func (a App) printTaskHelp(cmd string) {
	if cmd == "reply" {
		fmt.Fprintln(a.Out, "Usage: mosscap task reply TASK (--text TEXT | --text-file PATH) [--done] [--request-id ID] [--json]")
		return
	}
	fmt.Fprintf(a.Out, "Usage: mosscap task %s [arguments]\n", cmd)
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
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	fmt.Fprintln(a.Out, string(b))
	return nil
}
func (a App) branch(name string, args, children []string) error {
	if len(args) == 0 || isHelp(args[0]) {
		a.printBranchHelp(name, children)
		return nil
	}
	for _, child := range children {
		if args[0] == child {
			return a.leaf(name+" "+child, args[1:])
		}
	}
	return usage(fmt.Sprintf("unknown %s command %q", name, args[0]))
}
func (a App) leaf(name string, args []string) error {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	help := flags.Bool("help", false, "")
	flags.BoolVar(help, "h", false, "")
	if err := flags.Parse(args); err != nil {
		return usage(err.Error())
	}
	if *help {
		a.printLeafHelp(name)
		return nil
	}
	return unsupported(fmt.Sprintf("%s is not implemented in this build", name))
}
func (a App) printRootHelp() {
	fmt.Fprint(a.Out, "Usage: mosscap <command> [arguments]\n\nMosscap is a durable local work coordinator.\n\nCommands:\n  setup       initialize private local state\n  config      inspect or stage configuration\n  gateway     run, reload, or inspect the local gateway\n  doctor      run non-destructive local diagnostics\n  trust       manage trusted Slack users\n  purge       preview or confirm bounded metadata purge\n  task        manage local tasks\n  slack       read and operate in the configured Slack channel\n  effect      inspect durable Slack effects\n  file        show incoming Slack file metadata\n  delivery    inspect and recover Pi delivery attempts\n\nRun \"mosscap <command> --help\" for command help.\n")
}
func (a App) printBranchHelp(name string, children []string) {
	fmt.Fprintf(a.Out, "Usage: mosscap %s <command> [arguments]\n\nCommands:\n", name)
	for _, child := range children {
		fmt.Fprintf(a.Out, "  %s\n", child)
	}
	fmt.Fprintf(a.Out, "\nRun \"mosscap %s <command> --help\" for command help.\n", name)
}
func (a App) printLeafHelp(name string) {
	fmt.Fprintf(a.Out, "Usage: mosscap %s [arguments]\n\n", name)
	fmt.Fprintln(a.Out, "This command is reserved for a later implementation phase.")
}

type commandError struct {
	code          int
	message       string
	protocolError *protocol.Error
}

func (e *commandError) Error() string { return e.message }

type jsonMutationError struct {
	err       error
	requestID string
}

func (e *jsonMutationError) Error() string { return e.err.Error() }
func (e *jsonMutationError) Unwrap() error { return e.err }

// mutationJSONError opts in only parsed mutation commands. It deliberately
// does not inspect argv, so a value that happens to be "--json" cannot change
// the output protocol.
func mutationJSONError(err error, jsonOut bool, requestID string) error {
	if !jsonOut || err == nil {
		return err
	}
	return &jsonMutationError{err: err, requestID: requestID}
}

func exitCode(err error) int {
	var ce *commandError
	if errors.As(err, &ce) {
		return ce.code
	}
	return ExitUnsafe
}

type mutationErrorJSON struct {
	Code      string                     `json:"code"`
	Category  string                     `json:"category"`
	Message   string                     `json:"message"`
	Retriable bool                       `json:"retriable"`
	Details   map[string]string          `json:"details,omitempty"`
	Progress  *protocol.PurgeProgressDTO `json:"progress,omitempty"`
	RequestID string                     `json:"request_id,omitempty"`
	Guidance  string                     `json:"guidance,omitempty"`
}

func (a App) renderMutationError(wrapped *jsonMutationError) {
	out := mutationErrorJSON{Code: "local_error", Category: "local", Message: wrapped.err.Error()}
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
	_ = json.NewEncoder(a.Err).Encode(out)
}
func usage(m string) error       { return &commandError{code: ExitUsage, message: "usage: " + m} }
func unsupported(m string) error { return &commandError{code: ExitUnsupported, message: m} }
func isHelp(a string) bool       { return a == "--help" || a == "-h" || a == "help" }

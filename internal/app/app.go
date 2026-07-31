// Package app contains Mosscap's command tree and process-facing rendering.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/stayradiated/mosscap/internal/config"
)

const (
	ExitSuccess     = 0
	ExitUsage       = 2
	ExitUnsafe      = 5
	ExitUnsupported = 9
)

type App struct {
	Out     io.Writer
	Err     io.Writer
	Version string
}

func (a App) Run(ctx context.Context, args []string) int {
	if err := a.Execute(ctx, args); err != nil {
		var ce *commandError
		if errors.As(err, &ce) {
			fmt.Fprintln(a.Err, ce.Error())
			return ce.code
		}
		fmt.Fprintln(a.Err, err)
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
	if a.Version == "" {
		a.Version = "devel"
	}
	var override string
	var err error
	args, override, err = stripGlobalConfig(args)
	if err != nil {
		return usage(err.Error())
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
		return a.setup(override, args[1:])
	case "config":
		return a.config(override, args[1:])
	case "doctor":
		return a.leaf("doctor", args[1:])
	case "gateway":
		return a.branch("gateway", args[1:], []string{"run", "status"})
	case "task":
		return a.branch("task", args[1:], []string{"create", "list", "show", "update", "open", "working", "done"})
	default:
		return usage(fmt.Sprintf("unknown command %q", args[0]))
	}
}
func (a App) setup(override string, args []string) error {
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
	loc, err := config.DefaultLocation(override)
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
		a.printBranchHelp("config", []string{"show", "set"})
		return nil
	}
	switch args[0] {
	case "show":
		return a.configShow(override, args[1:])
	case "set":
		return a.configSet(override, args[1:])
	default:
		return usage(fmt.Sprintf("unknown config command %q", args[0]))
	}
}
func (a App) configShow(override string, args []string) error {
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
	loc, err := config.DefaultLocation(override)
	if err != nil {
		return err
	}
	c, err := config.Load(loc)
	if err != nil {
		return err
	}
	return a.render(config.Redact(c), *j)
}
func (a App) configSet(override string, args []string) error {
	fs := flag.NewFlagSet("config set", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	j := fs.Bool("json", false, "")
	h := fs.Bool("help", false, "")
	fs.BoolVar(h, "h", false, "")
	if err := fs.Parse(args); err != nil {
		return usage(err.Error())
	}
	if *h {
		fmt.Fprintln(a.Out, "Usage: mosscap config set KEY VALUE [--json]\n\nChanges are permitted only while the configured gateway is offline.")
		return nil
	}
	if fs.NArg() != 2 {
		return usage("config set requires KEY and VALUE")
	}
	loc, err := config.DefaultLocation(override)
	if err != nil {
		return err
	}
	c, err := config.Set(loc, fs.Arg(0), fs.Arg(1))
	if err != nil {
		return err
	}
	return a.render(config.Redact(c), *j)
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
	fmt.Fprint(a.Out, "Usage: mosscap [--config PATH] <command> [arguments]\n\nMosscap is a durable local work coordinator.\n\nCommands:\n  setup       initialize private local state\n  config      inspect or change configuration\n  gateway     run or inspect the local gateway (not yet implemented)\n  doctor      diagnose local installation health (not yet implemented)\n  task        manage local tasks (not yet implemented)\n\nRun \"mosscap <command> --help\" for command help.\n")
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
	code    int
	message string
}

func (e *commandError) Error() string { return e.message }
func usage(m string) error            { return &commandError{ExitUsage, "usage: " + m} }
func unsupported(m string) error      { return &commandError{ExitUnsupported, m} }
func isHelp(a string) bool            { return a == "--help" || a == "-h" || a == "help" }
func stripGlobalConfig(args []string) ([]string, string, error) {
	var path string
	for len(args) > 0 {
		switch {
		case args[0] == "--config":
			if len(args) < 2 || args[1] == "" {
				return nil, "", errors.New("--config requires a path")
			}
			path = args[1]
			args = args[2:]
		case len(args[0]) > len("--config=") && args[0][:len("--config=")] == "--config=":
			path = args[0][len("--config="):]
			if path == "" {
				return nil, "", errors.New("--config requires a path")
			}
			args = args[1:]
		default:
			return args, path, nil
		}
	}
	return args, path, nil
}

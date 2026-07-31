// Package app contains Mosscap's command tree and process-facing rendering.
package app

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
)

const (
	ExitSuccess     = 0
	ExitUsage       = 2
	ExitUnsupported = 9
)

// App is the command dispatcher. Command implementations are added behind this
// tree in later phases; this phase intentionally performs no configuration or
// durable-state work.
type App struct {
	Out     io.Writer
	Err     io.Writer
	Version string
}

// Run dispatches one Mosscap command and returns its stable process exit code.
func (a App) Run(ctx context.Context, args []string) int {
	if err := a.Execute(ctx, args); err != nil {
		var commandErr *commandError
		if errors.As(err, &commandErr) {
			fmt.Fprintln(a.Err, commandErr.Error())
			return commandErr.code
		}
		fmt.Fprintln(a.Err, err)
		return ExitUsage
	}
	return ExitSuccess
}

// Execute dispatches one Mosscap command.
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

	var err error
	args, err = stripGlobalConfig(args)
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
	case "setup", "doctor":
		return a.leaf(args[0], args[1:])
	case "config":
		return a.branch("config", args[1:], []string{"show", "set"})
	case "gateway":
		return a.branch("gateway", args[1:], []string{"run", "status"})
	case "task":
		return a.branch("task", args[1:], []string{"create", "list", "show", "update", "open", "working", "done"})
	default:
		return usage(fmt.Sprintf("unknown command %q", args[0]))
	}
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
	help := flags.Bool("help", false, "show help")
	flags.BoolVar(help, "h", false, "show help")
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
	fmt.Fprint(a.Out, `Usage: mosscap [--config PATH] <command> [arguments]

Mosscap is a durable local work coordinator.

Commands:
  setup       initialize private local state (not yet implemented)
  config      inspect or change configuration (not yet implemented)
  gateway     run or inspect the local gateway (not yet implemented)
  doctor      diagnose local installation health (not yet implemented)
  task        manage local tasks (not yet implemented)

Run "mosscap <command> --help" for command help.
`)
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

func usage(message string) error       { return &commandError{code: ExitUsage, message: "usage: " + message} }
func unsupported(message string) error { return &commandError{code: ExitUnsupported, message: message} }

func isHelp(arg string) bool { return arg == "--help" || arg == "-h" || arg == "help" }

// stripGlobalConfig recognizes the global testability/config-location option.
// Configuration loading is intentionally deferred to the configuration phase.
func stripGlobalConfig(args []string) ([]string, error) {
	for len(args) > 0 {
		switch {
		case args[0] == "--config":
			if len(args) < 2 || args[1] == "" {
				return nil, errors.New("--config requires a path")
			}
			args = args[2:]
		case len(args[0]) > len("--config=") && args[0][:len("--config=")] == "--config=":
			args = args[1:]
		default:
			return args, nil
		}
	}
	return args, nil
}

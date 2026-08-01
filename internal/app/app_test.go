package app

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stayradiated/mosscap/internal/client"
	"github.com/stayradiated/mosscap/internal/protocol"
)

func TestHelpAtEveryCommandNode(t *testing.T) {
	tests := [][]string{
		nil,
		{"--help"},
		{"setup", "--help"},
		{"config", "--help"},
		{"config", "show", "--help"},
		{"config", "set", "--help"},
		{"gateway", "--help"},
		{"gateway", "run", "--help"},
		{"gateway", "status", "--help"},
		{"doctor", "--help"},
		{"task", "--help"},
		{"task", "create", "--help"},
		{"task", "list", "--help"},
		{"task", "show", "--help"},
		{"task", "update", "--help"},
		{"task", "open", "--help"},
		{"task", "working", "--help"},
		{"task", "done", "--help"},
	}
	for _, args := range tests {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			got := (App{Out: &stdout, Err: &stderr, Version: "test"}).Run(context.Background(), args)
			if got != ExitSuccess {
				t.Fatalf("Run(%q) exit = %d, stderr = %q", args, got, stderr.String())
			}
			if !strings.HasPrefix(stdout.String(), "Usage: mosscap") {
				t.Fatalf("help = %q, want usage", stdout.String())
			}
			if stderr.Len() != 0 {
				t.Fatalf("help stderr = %q, want empty", stderr.String())
			}
		})
	}
}

func TestGlobalConfigBeforeSubcommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	got := (App{Out: &stdout, Err: &stderr}).Run(context.Background(), []string{"--config", "/tmp/mosscap.toml", "task", "--help"})
	if got != ExitSuccess || !strings.Contains(stdout.String(), "Usage: mosscap task") || stderr.Len() != 0 {
		t.Fatalf("global config help: exit=%d stdout=%q stderr=%q", got, stdout.String(), stderr.String())
	}
}

func TestVersionAndUnimplementedCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	a := App{Out: &stdout, Err: &stderr, Version: "v1.2.3"}
	if got := a.Run(context.Background(), []string{"--version"}); got != ExitSuccess {
		t.Fatalf("version exit = %d", got)
	}
	if got, want := stdout.String(), "v1.2.3\n"; got != want {
		t.Fatalf("version = %q, want %q", got, want)
	}
	if got := a.Run(context.Background(), []string{"gateway", "run"}); got != ExitUnsupported {
		t.Fatalf("gateway run exit = %d, want %d", got, ExitUnsupported)
	}
	if !strings.Contains(stderr.String(), "not implemented") {
		t.Fatalf("gateway stderr = %q", stderr.String())
	}
}

func TestUnsupportedRemoteMapsToExit9(t *testing.T) {
	err := remote(&client.RemoteError{ProtocolError: protocol.Error{Code: "unsupported_protocol_version", Category: "unsupported", Message: "protocol version is unsupported"}})
	var command *commandError
	if !errors.As(err, &command) || command.code != ExitUnsupported {
		t.Fatalf("remote error = %#v", err)
	}
}

func TestUsageErrors(t *testing.T) {
	var stdout, stderr bytes.Buffer
	a := App{Out: &stdout, Err: &stderr}
	if got := a.Run(context.Background(), []string{"unknown"}); got != ExitUsage {
		t.Fatalf("unknown exit = %d, want %d", got, ExitUsage)
	}
	if !strings.HasPrefix(stderr.String(), "usage:") {
		t.Fatalf("stderr = %q, want usage error", stderr.String())
	}
}

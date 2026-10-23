package app

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCobraCutoverHonorsEndOfOptionsForPositionalSelectors(t *testing.T) {
	// Private XDG roots keep the outcome independent of host configuration: the
	// selector must reach the gateway call (unavailable), not a usage error.
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp(home, ".mosscap-cutover-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	if got := (App{Out: &bytes.Buffer{}, Err: &bytes.Buffer{}}).Run(context.Background(), []string{"setup"}); got != ExitSuccess {
		t.Fatalf("setup exit=%d", got)
	}
	var out, stderr bytes.Buffer
	code := (App{Out: &out, Err: &stderr}).Run(context.Background(), []string{"task", "show", "--", "--selector"})
	if code == ExitUsage || strings.Contains(stderr.String(), "flag provided but not defined") {
		t.Fatalf("end-of-options was reparsed as a flag: exit=%d stderr=%q", code, stderr.String())
	}
}

func TestSlackLeavesUseTypedCobraHandlers(t *testing.T) {
	for _, handler := range []string{"slack.history", "slack.thread", "slack.send", "slack.file", "slack.edit", "slack.delete", "slack.react", "slack.unreact"} {
		if !isTypedCobraHandler(handler) {
			t.Errorf("%s still uses the legacy Cobra bridge", handler)
		}
	}
}

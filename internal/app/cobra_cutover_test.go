package app

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestCobraCutoverHonorsEndOfOptionsForPositionalSelectors(t *testing.T) {
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

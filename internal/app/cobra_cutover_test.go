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

func TestCanonicalArgsPreservesCommaInRepeatedFiles(t *testing.T) {
	cmd := commandAt((App{}).NewCommandTree(), []string{"slack", "send"})
	if err := cmd.Flags().Set("file", "/tmp/a,b"); err != nil {
		t.Fatal(err)
	}
	got := canonicalArgs(cmd, nil)
	want := []string{"--file", "/tmp/a,b"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("canonical args=%q, want %q", got, want)
	}
}

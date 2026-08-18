package app

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stayradiated/mosscap/internal/doctor"
)

type runtimeJSONGolden struct {
	Name     string         `json:"name"`
	Args     []string       `json:"args"`
	Env      string         `json:"env"`
	EnvValue string         `json:"env_value"`
	Exit     int            `json:"exit"`
	Want     map[string]any `json:"want"`
}

func TestRuntimeJSONGolden(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "runtime-json-golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	var golden []runtimeJSONGolden
	if err := json.Unmarshal(b, &golden); err != nil {
		t.Fatal(err)
	}

	t.Run(golden[0].Name, func(t *testing.T) {
		t.Setenv(golden[0].Env, golden[0].EnvValue)
		var stdout, stderr bytes.Buffer
		gotExit := (App{Out: &stdout, Err: &stderr, Version: "test"}).Run(context.Background(), golden[0].Args)
		assertRuntimeJSON(t, stdout.Bytes(), stderr.Bytes(), gotExit, golden[0])
	})
	t.Run(golden[1].Name, func(t *testing.T) {
		var stdout bytes.Buffer
		a := App{Out: &stdout}
		cmd := &cobra.Command{Use: "doctor"}
		cmd.Flags().Bool("json", true, "")
		err := a.runtimeJSONError(cmd, &doctorPartialError{err: &commandError{code: ExitUnavailable, message: "gateway diagnostics unavailable"}, report: doctor.Report{Overall: doctor.Warn, Checks: []doctor.Check{{Name: "config", Status: doctor.Pass}}}})
		wrapped := err.(*jsonRuntimeError)
		a.renderRuntimeError(wrapped)
		assertRuntimeJSON(t, stdout.Bytes(), nil, exitCode(wrapped.err), golden[1])
	})
}

func TestRuntimeJSONUsageStaysTextOnStderr(t *testing.T) {
	var stdout, stderr bytes.Buffer
	got := (App{Out: &stdout, Err: &stderr, Version: "test"}).Run(context.Background(), []string{"setup", "--json", "extra"})
	if got != ExitUsage || stdout.Len() != 0 || !bytes.Contains(stderr.Bytes(), []byte("Usage:")) {
		t.Fatalf("exit=%d stdout=%q stderr=%q", got, stdout.String(), stderr.String())
	}
}

func assertRuntimeJSON(t *testing.T, stdout, stderr []byte, gotExit int, golden runtimeJSONGolden) {
	t.Helper()
	if gotExit != golden.Exit || len(stderr) != 0 || !bytes.HasSuffix(stdout, []byte("\n")) || bytes.Count(stdout, []byte("\n")) != 1 {
		t.Fatalf("exit=%d stdout=%q stderr=%q", gotExit, stdout, stderr)
	}
	var got map[string]any
	if err := json.Unmarshal(stdout, &got); err != nil {
		t.Fatalf("stdout is not JSON: %v: %q", err, stdout)
	}
	for key, want := range golden.Want {
		value, ok := got[key]
		if want == true {
			if !ok || value == nil {
				t.Fatalf("%s absent from %#v", key, got)
			}
		} else if value != want {
			t.Fatalf("%s=%#v, want %#v in %#v", key, value, want, got)
		}
	}
}

package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stayradiated/mosscap/internal/client"
	"github.com/stayradiated/mosscap/internal/config"
	"github.com/stayradiated/mosscap/internal/doctor"
	"github.com/stayradiated/mosscap/internal/fileio"
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
		{"config", "secret", "--help"},
		{"config", "secret", "set", "--help"},
		{"gateway", "--help"},
		{"gateway", "run", "--help"},
		{"gateway", "reload", "--help"},
		{"gateway", "status", "--help"},
		{"doctor", "--help"},
		{"trust", "--help"},
		{"purge", "--help"},
		{"trust", "list", "--help"},
		{"trust", "add", "--help"},
		{"trust", "remove", "--help"},
		{"task", "--help"},
		{"task", "create", "--help"},
		{"task", "list", "--help"},
		{"task", "show", "--help"},
		{"task", "update", "--help"},
		{"task", "open", "--help"},
		{"task", "working", "--help"},
		{"task", "done", "--help"},
		{"slack", "--help"},
		{"slack", "history", "--help"},
		{"slack", "thread", "--help"},
		{"slack", "send", "--help"},
		{"slack", "file", "--help"},
		{"slack", "edit", "--help"},
		{"slack", "delete", "--help"},
		{"slack", "react", "--help"},
		{"slack", "unreact", "--help"},
		{"effect", "--help"},
		{"effect", "list", "--help"},
		{"effect", "inspect", "--help"},
		{"effect", "retry", "--help"},
		{"file", "--help"},
		{"file", "show", "--help"},
		{"file", "get", "--help"},
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

func TestRenderDoctorIsHumanFriendlyUnlessJSONRequested(t *testing.T) {
	report := doctor.Report{Overall: doctor.Warn, Checks: []doctor.Check{
		{Name: "config", Status: doctor.Pass, Message: "configuration is valid"},
		{Name: "storage_space", Status: doctor.Warn, Message: "space is low", Action: "free some space"},
	}}

	var out bytes.Buffer
	a := App{Out: &out}
	if err := a.renderDoctor(report, false); err != nil {
		t.Fatal(err)
	}
	human := out.String()
	for _, want := range []string{"Doctor: ⚠", "✓  config         configuration is valid", "⚠  storage_space  space is low", "↳ Action: free some space"} {
		if !strings.Contains(human, want) {
			t.Fatalf("human doctor output missing %q: %q", want, human)
		}
	}
	if json.Valid([]byte(human)) || strings.Contains(human, `"overall"`) {
		t.Fatalf("default doctor output is JSON: %q", human)
	}
	if strings.Contains(human, "\x1b[") {
		t.Fatalf("non-terminal doctor output contains ANSI colors: %q", human)
	}
	if got := formatDoctorStatus(doctor.Fail, true); got != "\x1b[31m✗\x1b[0m" {
		t.Fatalf("colored failure status = %q", got)
	}
	if got := formatDoctorName("config", 10, true); got != "\x1b[1;34mconfig    \x1b[0m" {
		t.Fatalf("colored check name = %q", got)
	}

	out.Reset()
	if err := a.renderDoctor(report, true); err != nil {
		t.Fatal(err)
	}
	var decoded doctor.Report
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("--json output is invalid: %v; output=%q", err, out.String())
	}
	if decoded.Overall != report.Overall || len(decoded.Checks) != len(report.Checks) {
		t.Fatalf("--json output = %+v, want %+v", decoded, report)
	}
}

func TestRenderStatusPreservesBoundedVerificationRetentionFields(t *testing.T) {
	const secret = "xoxb-secret-canary"
	var out bytes.Buffer
	a := App{Out: &out}
	result := protocol.StatusDTO{Slack: &protocol.SlackRuntimeStatusDTO{
		State:                          "connected",
		LastVerificationDiagnosticCode: "channel_not_found",
		PriorRuntimeRetained:           true,
	}}
	if err := a.render(result, true); err != nil {
		t.Fatal(err)
	}
	rendered := out.String()
	for _, want := range []string{"channel_not_found", "prior_runtime_retained", "true"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("rendered status missing %q: %q", want, rendered)
		}
	}
	if strings.Contains(rendered, secret) {
		t.Fatalf("rendered status leaked secret: %q", rendered)
	}
}

func TestSecretCredentialEntryRejectsArgvAndWritesOnlyViaStdin(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp(home, ".mosscap-secret-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	var setupOut, setupErr bytes.Buffer
	if got := (App{Out: &setupOut, Err: &setupErr}).Run(context.Background(), []string{"setup"}); got != ExitSuccess {
		t.Fatalf("setup exit=%d stderr=%q", got, setupErr.String())
	}
	const secret = "xapp-secret-canary"
	var out, stderr bytes.Buffer
	if got := (App{Out: &out, Err: &stderr}).Run(context.Background(), []string{"config", "set", "slack_app_token", secret}); got != ExitUsage {
		t.Fatalf("positional secret exit=%d stderr=%q", got, stderr.String())
	}
	if strings.Contains(out.String()+stderr.String(), secret) {
		t.Fatal("positional secret leaked")
	}
	out.Reset()
	stderr.Reset()
	if got := (App{Out: &out, Err: &stderr, In: strings.NewReader(secret + "\n")}).Run(context.Background(), []string{"config", "secret", "set", "slack_app_token", "--stdin"}); got != ExitSuccess {
		t.Fatalf("stdin secret exit=%d stderr=%q", got, stderr.String())
	}
	if out.String() != "slack_app_token updated\n" || strings.Contains(out.String()+stderr.String(), secret) {
		t.Fatalf("unsafe secret output out=%q err=%q", out.String(), stderr.String())
	}
	loc, err := config.DefaultLocation()
	if err != nil {
		t.Fatal(err)
	}
	got, err := config.Load(loc)
	if err != nil || got.SlackAppToken != secret {
		t.Fatalf("stored secret mismatch: err=%v", err)
	}
	if secret == "" { // prevent accidental future logging of the canary.
		t.Fatal("empty secret")
	}
}

func TestSecretCredentialInputBoundsAndFieldValidation(t *testing.T) {
	if _, err := readSecretLine(strings.NewReader(strings.Repeat("x", 4097))); err == nil {
		t.Fatal("accepted oversized secret")
	}
	var out, stderr bytes.Buffer
	if got := (App{Out: &out, Err: &stderr, In: strings.NewReader("xapp-value\n")}).Run(context.Background(), []string{"config", "secret", "set", "unsupported", "--stdin"}); got != ExitUsage {
		t.Fatalf("unsupported field exit=%d stderr=%q", got, stderr.String())
	}
}

func TestAlternateConfigPathIsNotAuthority(t *testing.T) {
	var stdout, stderr bytes.Buffer
	got := (App{Out: &stdout, Err: &stderr}).Run(context.Background(), []string{"--config", "/tmp/mosscap.toml", "task", "--help"})
	if got != ExitUsage || stdout.Len() != 0 || !strings.Contains(stderr.String(), "unknown command") {
		t.Fatalf("alternate config path: exit=%d stdout=%q stderr=%q", got, stdout.String(), stderr.String())
	}
}

func TestVersionAndUnimplementedCommand(t *testing.T) {
	// gateway run is intentionally exercised before setup; isolate it from any
	// developer installation that might otherwise make the test block forever.
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", root+"/config")
	t.Setenv("XDG_STATE_HOME", root+"/state")
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

func TestPhase1ExitMappingsAndForbiddenFlags(t *testing.T) {
	for _, tc := range []struct {
		category string
		want     int
	}{{"external_failed", ExitExternalFailed}, {"ambiguous", ExitAmbiguous}} {
		t.Run(tc.category, func(t *testing.T) {
			err := remote(&client.RemoteError{ProtocolError: protocol.Error{Category: tc.category, Message: "safe"}})
			var command *commandError
			if !errors.As(err, &command) || command.code != tc.want {
				t.Fatalf("remote(%q) = %#v", tc.category, err)
			}
		})
	}
	for _, args := range [][]string{{"slack", "history", "--channel", "C123"}, {"file", "get", "fil_x", "--force"}, {"slack", "file", "--file", "one", "--file", "two"}} {
		var out, stderr bytes.Buffer
		if got := (App{Out: &out, Err: &stderr}).Run(context.Background(), args); got != ExitUsage {
			t.Fatalf("Run(%q) = %d, stderr=%q", args, got, stderr.String())
		}
	}
}

func TestFileOutputAndPathExitMappings(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want int
	}{
		{fileio.ErrDestinationExists, ExitConflict},
		{fileio.ErrChanged, ExitConflict},
		{fileio.ErrUnsafeFileType, ExitUnsafe},
		{fileio.ErrUnsupported, ExitUnsupported},
	} {
		var command *commandError
		if err := outputError(tc.err); !errors.As(err, &command) || command.code != tc.want {
			t.Fatalf("outputError(%v) = %#v, want exit %d", tc.err, err, tc.want)
		}
	}
	paths, err := absolutePaths([]string{"relative-file"})
	if err != nil || len(paths) != 1 || !strings.HasPrefix(paths[0], "/") {
		t.Fatalf("absolutePaths = %q, %v", paths, err)
	}
	transfer := protocol.FileGetDTO{TransferID: "xfr_AbCdEfGhIjKlMnOpQrStUv", FileID: "fil_AbCdEfGhIjKlMnOpQrStUv", TemporaryPath: "/tmp/mosscap/mosscap-xfr-" + strings.Repeat("a", 32), ValidatedSizeBytes: 1, SHA256: strings.Repeat("a", 64), ExpiresAt: "2026-01-01T00:00:00Z"}
	if !validFileTransferResult(config.Config{Paths: config.Paths{TmpDir: "/tmp/mosscap"}}, transfer) {
		t.Fatal("valid transfer result rejected")
	}
	transfer.TemporaryPath = "/tmp/other/mosscap-xfr-" + strings.Repeat("a", 32)
	if validFileTransferResult(config.Config{Paths: config.Paths{TmpDir: "/tmp/mosscap"}}, transfer) {
		t.Fatal("transfer outside configured tmp accepted")
	}
}

func TestPurgeRejectsAmbiguousModesAndInvalidCutoffsBeforeDialing(t *testing.T) {
	for _, args := range [][]string{
		{"purge", "--before", "2026-01-01T00:00:00Z"},
		{"purge", "--before", "2026-01-01T00:00:00Z", "--dry-run", "--confirm"},
		{"purge", "--before", "2026-01-01T00:00:00", "--dry-run"},
		{"purge", "--before", "2026-01-01T00:00:00Z", "--dry-run"},
		{"purge", "--before", "2026-01-01T00:00:00.1Z", "--dry-run"},
		{"purge", "--before", "2026-01-01T00:00:00.12345+01:00", "--dry-run"},
		{"purge", "--before", "2026-01-01T00:00:00,000000Z", "--dry-run"},
		{"purge", "--before", "2026-01-01T00:00:00.000000", "--dry-run"},
		{"purge", "--before", "2026-01-01T00:00:00Z", "--dry-run", "--dry-run"},
		{"purge", "--before", "2026-01-01T00:00:00.000000Z", "--dry-run=false"},
		{"purge", "--before", "2026-01-01T00:00:00.000000Z", "--dry-run", "--confirm=false"},
	} {
		var out, errOut bytes.Buffer
		if got := (App{Out: &out, Err: &errOut}).Run(context.Background(), args); got != ExitUsage {
			t.Fatalf("Run(%q) = %d, stderr=%q", args, got, errOut.String())
		}
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

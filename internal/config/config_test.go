package config

import (
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

func testConfig(home string) Config {
	state := filepath.Join(home, "state")
	return Config{Version: 1, Paths: Paths{StateDir: state, Database: filepath.Join(state, "mosscap.sqlite3"), Endpoint: filepath.Join(state, "control.sock"), TmpDir: filepath.Join(state, "tmp")}, Slack: Slack{TrustedUserIDs: []string{}}, Database: Database{BusyTimeout: "5s"}, Protocol: Protocol{MaxFrameBytes: 1048576}}
}
func privateDir(t *testing.T, path string) {
	t.Helper()
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
}
func writeConfig(t *testing.T, path string, c Config) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := toml.NewEncoder(f).Encode(c); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestLoadStrictAndRedacted(t *testing.T) {
	home := t.TempDir()
	privateDir(t, filepath.Join(home, "config"))
	p := filepath.Join(home, "config", "config.toml")
	c := testConfig(home)
	c.Slack.SecretsFile = filepath.Join(home, "secret")
	writeConfig(t, p, c)
	got, err := Load(Location{Home: home, Path: p})
	if err != nil {
		t.Fatal(err)
	}
	if Redact(got).Slack.SecretsFile != "<configured>" {
		t.Fatal("secret path not redacted")
	}
	if err := os.WriteFile(p, []byte("version = 1\nunknown = 1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(Location{Home: home, Path: p}); err == nil || !strings.Contains(err.Error(), "unknown key") {
		t.Fatalf("Load unknown error=%v", err)
	}
}
func TestSetValidatesAndAtomicallyReplaces(t *testing.T) {
	home := t.TempDir()
	privateDir(t, filepath.Join(home, "config"))
	p := filepath.Join(home, "config", "config.toml")
	c := testConfig(home)
	writeConfig(t, p, c)
	loc := Location{Home: home, Path: p}
	got, err := Set(loc, "protocol.max_frame_bytes", "65536")
	if err != nil {
		t.Fatal(err)
	}
	if got.Protocol.MaxFrameBytes != 65536 {
		t.Fatal(got.Protocol.MaxFrameBytes)
	}
	if _, err := Set(loc, "slack.app_token", "nope"); err == nil {
		t.Fatal("accepted token key")
	}
	if _, err := Set(loc, "paths.endpoint", "relative"); err == nil {
		t.Fatal("accepted relative path")
	}
	if _, err := Set(loc, "slack.trusted_user_ids", "[\"U1\", \"U2\"]"); err != nil {
		t.Fatal(err)
	}
	got, err = Load(loc)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Slack.TrustedUserIDs) != 2 {
		t.Fatal(got.Slack.TrustedUserIDs)
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("mode=%o", info.Mode().Perm())
	}
}
func TestSetupCreatesPrivateXDGLayout(t *testing.T) {
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp(u.HomeDir, ".mosscap-config-test-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	loc, err := DefaultLocation("")
	if err != nil {
		t.Fatal(err)
	}
	got, err := Setup(loc)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Load(loc); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(got.Paths.TmpDir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0700 {
		t.Fatalf("tmp mode = %o", info.Mode().Perm())
	}
}

func TestValidatePathsAndLimits(t *testing.T) {
	c := testConfig(t.TempDir())
	c.Paths.Endpoint = "relative"
	if err := Validate(c); err == nil {
		t.Fatal("relative accepted")
	}
	c = testConfig(t.TempDir())
	c.Protocol.MaxFrameBytes = 1
	if err := Validate(c); err == nil {
		t.Fatal("small frame accepted")
	}
}

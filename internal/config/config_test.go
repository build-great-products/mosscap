package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func testLocation(t *testing.T) Location {
	t.Helper()
	home := t.TempDir()
	configDir := filepath.Join(home, "config", "mosscap")
	stateDir := filepath.Join(home, "state", "mosscap")
	return Location{Home: home, Path: filepath.Join(configDir, FileName), Paths: Paths{
		ConfigDir: configDir,
		StateDir:  stateDir,
		Database:  filepath.Join(stateDir, "mosscap.sqlite3"),
		Endpoint:  filepath.Join(stateDir, "control.sock"),
		TmpDir:    filepath.Join(stateDir, "tmp"),
	}}
}

func setupConfig(t *testing.T) (Location, Config) {
	t.Helper()
	loc := testLocation(t)
	c, err := Setup(loc)
	if err != nil {
		t.Fatal(err)
	}
	return loc, c
}

func TestDefaultCompleteFixedPathsAndTemplate(t *testing.T) {
	loc, got := setupConfig(t)
	if got.Paths != loc.Paths {
		t.Fatalf("paths = %#v, want %#v", got.Paths, loc.Paths)
	}
	if got.SlackReconnectInitialBackoff != "1s" || got.SlackReconnectMaxBackoff != "60s" || !got.SlackReconnectJitter ||
		got.SQLiteBusyTimeout != "5s" || got.MaximumFileSize != 100<<20 || got.MaximumFilesPerSlackOperation != 20 ||
		got.TemporaryDownloadLifetime != "24h" || got.LocalProtocolMaximumFrame != 1<<20 {
		t.Fatalf("unexpected defaults: %#v", got)
	}
	b, err := os.ReadFile(loc.Path)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"version", "slack_app_token", "slack_bot_token", "slack_channel_allowlist", "slack_reconnect_initial_backoff", "slack_reconnect_max_backoff", "slack_reconnect_jitter", "sqlite_busy_timeout", "maximum_file_size", "maximum_files_per_slack_operation", "temporary_download_lifetime", "local_protocol_maximum_frame"} {
		if !strings.Contains(string(b), "\n"+key+" =") {
			t.Errorf("template lacks top-level %s", key)
		}
	}
	if !strings.Contains(string(b), "# Slack") {
		t.Error("template lacks explanatory comments")
	}
	info, err := os.Stat(loc.Path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("config mode: info=%v err=%v", info, err)
	}
	for _, forbidden := range []string{"paths =", "trusted_user_ids", "secrets_file"} {
		if strings.Contains(string(b), forbidden) {
			t.Errorf("template contains obsolete %s", forbidden)
		}
	}
}

func TestLoadStrictLegacyDiagnosticsAndNoEnvironmentSecrets(t *testing.T) {
	loc, _ := setupConfig(t)
	for _, tc := range []struct{ name, contents, want string }{
		{"unknown", "version = 1\nunknown = 1\n", "unknown key"},
		{"paths", "version = 1\n[paths]\nstate_dir = \"/x\"\n", "obsolete \"paths\""},
		{"trust", "version = 1\ntrusted_user_ids = []\n", "trusted users were removed"},
		{"secrets", "version = 1\nsecrets_file = \"/x\"\n", "put Slack tokens in config.toml"},
		{"obsolete team identity", "version = 1\nslack_team_id = \"T012ABC\"\n", "identity is derived from authenticated Slack tokens"},
		{"obsolete bot identity", "version = 1\nslack_bot_user_id = \"U012BOT\"\n", "identity is derived from authenticated Slack tokens"},
		{"obsolete single channel", "version = 1\nslack_channel_id = \"C012ABC\"\n", "replace it with slack_channel_allowlist"},
		{"obsolete delivery lease", "version = 1\ndelivery_lease = \"60s\"\n", "Pi delivery was removed"},
		{"obsolete leader expiry", "version = 1\nleader_expiry = \"30s\"\n", "Pi delivery was removed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(loc.Path, []byte(tc.contents), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(loc); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Load error = %v, want %q", err, tc.want)
			}
		})
	}
	def, err := Default(loc)
	if err != nil {
		t.Fatal(err)
	}
	incomplete := bytes.Replace(template(def), []byte("slack_reconnect_jitter = true\n"), nil, 1)
	if err := os.WriteFile(loc.Path, incomplete, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(loc); err == nil || !strings.Contains(err.Error(), "missing required key") {
		t.Fatalf("Load incomplete config error = %v", err)
	}
	// Recreate a valid complete file, then prove inherited environment values do not appear.
	if err := os.Remove(loc.Path); err != nil {
		t.Fatal(err)
	}
	if _, err := Setup(loc); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MOSSCAP_SLACK_APP_TOKEN", "xapp-from-environment")
	t.Setenv("MOSSCAP_SLACK_BOT_TOKEN", "xoxb-from-environment")
	got, err := Load(loc)
	if err != nil {
		t.Fatal(err)
	}
	if got.SlackAppToken != "" || got.SlackBotToken != "" || Redact(got).SlackAppTokenPresent || Redact(got).SlackBotTokenPresent {
		t.Fatal("environment token was imported")
	}
}

func TestValidateConstraints(t *testing.T) {
	loc := testLocation(t)
	base, err := Default(loc)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		mut  func(*Config)
	}{
		{"app-token", func(c *Config) { c.SlackAppToken = "bad" }},
		{"bot-token", func(c *Config) { c.SlackBotToken = "xapp-nope" }},
		{"channel", func(c *Config) { c.SlackChannelAllowlist = "D123" }},
		{"backoff-order", func(c *Config) { c.SlackReconnectInitialBackoff = "61s" }},
		{"busy", func(c *Config) { c.SQLiteBusyTimeout = "0s" }},
		{"file-size", func(c *Config) { c.MaximumFileSize = 0 }},
		{"file-count", func(c *Config) { c.MaximumFilesPerSlackOperation = 0 }},
		{"temporary", func(c *Config) { c.TemporaryDownloadLifetime = "no" }},
		{"frame", func(c *Config) { c.LocalProtocolMaximumFrame = 1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := base
			tc.mut(&c)
			if err := Validate(c); err == nil {
				t.Fatal("Validate accepted invalid candidate")
			}
		})
	}
	base.SlackAppToken, base.SlackBotToken = "xapp-token", "xoxb-token"
	base.SlackChannelAllowlist = "C123,G456"
	if err := Validate(base); err != nil {
		t.Fatalf("valid complete Slack config: %v", err)
	}
}

func TestSetPreservesFormattingAndOnlyChangesValue(t *testing.T) {
	loc, c := setupConfig(t)
	original := "# leading comment\r\nversion = 1\r\nslack_app_token = \"\" # token stays private\r\nslack_bot_token = \"\"\r\nslack_channel_allowlist = \"\"\r\nslack_reconnect_initial_backoff = \"1s\"\r\nslack_reconnect_max_backoff = \"60s\"\r\nslack_reconnect_jitter = true\r\nsqlite_busy_timeout = \"5s\"\r\nmaximum_file_size = 104857600\r\nmaximum_files_per_slack_operation = 20\r\ntemporary_download_lifetime = \"24h\"\r\nlocal_protocol_maximum_frame = 1048576 # frame\r\n# trailing comment\r\n"
	if err := os.WriteFile(loc.Path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := Set(loc, "local_protocol_maximum_frame", "65536")
	if err != nil {
		t.Fatal(err)
	}
	if got.LocalProtocolMaximumFrame != 65536 || got.Paths != c.Paths {
		t.Fatalf("Set returned %#v", got)
	}
	want := strings.Replace(original, "local_protocol_maximum_frame = 1048576 # frame", "local_protocol_maximum_frame = 65536 # frame", 1)
	actual, err := os.ReadFile(loc.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(actual, []byte(want)) {
		t.Fatalf("Set changed unrelated bytes:\n%s", actual)
	}
	if _, err := Set(loc, "version", "2"); err == nil {
		t.Fatal("Set changed version")
	}
	if _, err := Set(loc, "paths.endpoint", "/tmp/socket"); err == nil {
		t.Fatal("Set changed path")
	}
	if _, err := Set(loc, "maximum_file_size", "nope"); err == nil {
		t.Fatal("Set accepted bad byte size")
	}
	got, err = Set(loc, "maximum_file_size", "200MiB")
	if err != nil || got.MaximumFileSize != 200<<20 {
		t.Fatalf("Set binary byte size: config=%#v err=%v", got, err)
	}
	actual, err = os.ReadFile(loc.Path)
	if err != nil || !bytes.Contains(actual, []byte(`maximum_file_size = "200MiB"`)) {
		t.Fatalf("Set did not retain human byte size: %s (%v)", actual, err)
	}
	if _, err := Load(loc); err != nil {
		t.Fatalf("Load human byte size: %v", err)
	}
	const secret = "xapp-token-input-must-not-leak"
	if _, err := Set(loc, "slack_app_token", secret+" invalid"); err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("token Set error leaked input: %v", err)
	}
	if _, err := Set(loc, "slack_reconnect_initial_backoff", "61s"); err == nil {
		t.Fatal("Set accepted invalid full candidate")
	}
	if _, err := Set(loc, "slack_app_token", "xapp-new-token"); err == nil || !strings.Contains(err.Error(), "config secret set") {
		t.Fatalf("Set accepted positional token: %v", err)
	}
	got, err = SetSecret(loc, "slack_app_token", "xapp-new-token")
	if err != nil || got.SlackAppToken != "xapp-new-token" {
		t.Fatalf("SetSecret token mismatch: err=%v", err)
	}
	view := Redact(got)
	if !view.SlackAppTokenPresent || strings.Contains(fmt.Sprint(view), "xapp-new-token") {
		t.Fatal("token leaked through view")
	}
	info, err := os.Stat(loc.Path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("config mode: info=%v err=%v", info, err)
	}
}

func TestSetSerializesConcurrentEditsAndRejectsUnsafeFiles(t *testing.T) {
	loc, _ := setupConfig(t)
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key, value := "maximum_file_size", "104857600"
			if i%2 == 1 {
				key, value = "maximum_files_per_slack_operation", "20"
			}
			if _, err := Set(loc, key, value); err != nil {
				t.Errorf("Set: %v", err)
			}
		}(i)
	}
	wg.Wait()
	if _, err := Load(loc); err != nil {
		t.Fatalf("concurrent result not loadable: %v", err)
	}
	lock := filepath.Join(filepath.Dir(loc.Path), ".config.lock")
	if err := os.Remove(lock); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(loc.Path, lock); err != nil {
		t.Fatal(err)
	}
	if _, err := Set(loc, "maximum_file_size", "200MiB"); err == nil {
		t.Fatal("Set accepted symlink lock")
	}
}

func TestSetupDoesNotOverwriteExistingConfigOrLedger(t *testing.T) {
	loc, c := setupConfig(t)
	configBefore, err := os.ReadFile(loc.Path)
	if err != nil {
		t.Fatal(err)
	}
	dbBefore, err := os.Stat(c.Paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Setup(loc); err != nil {
		t.Fatal(err)
	}
	configAfter, _ := os.ReadFile(loc.Path)
	dbAfter, _ := os.Stat(c.Paths.Database)
	if !bytes.Equal(configBefore, configAfter) || !os.SameFile(dbBefore, dbAfter) {
		t.Fatal("Setup overwrote existing state")
	}
}

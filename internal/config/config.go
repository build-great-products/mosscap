// Package config owns Mosscap's strict, private TOML configuration.
package config

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/stayradiated/mosscap/internal/securepath"
	"github.com/stayradiated/mosscap/internal/store"
)

const FileName = "config.toml"

type Config struct {
	Version  int      `toml:"version"`
	Paths    Paths    `toml:"paths"`
	Slack    Slack    `toml:"slack"`
	Database Database `toml:"database"`
	Protocol Protocol `toml:"protocol"`
}
type Paths struct {
	StateDir string `toml:"state_dir"`
	Database string `toml:"database"`
	Endpoint string `toml:"endpoint"`
	TmpDir   string `toml:"tmp_dir"`
}
type Slack struct {
	TeamID         string   `toml:"team_id"`
	BotUserID      string   `toml:"bot_user_id"`
	ChannelID      string   `toml:"channel_id"`
	TrustedUserIDs []string `toml:"trusted_user_ids"`
	SecretsFile    string   `toml:"secrets_file"`
}
type Database struct {
	BusyTimeout string `toml:"busy_timeout"`
}
type Protocol struct {
	MaxFrameBytes int `toml:"max_frame_bytes"`
}

// Location contains the trusted effective home and selected config path.
type Location struct{ Home, Path string }

// DefaultLocation derives paths from the effective account, rejecting misleading HOME and XDG values.
func DefaultLocation(override string) (Location, error) {
	u, err := user.Current()
	if err != nil {
		return Location{}, fmt.Errorf("resolve effective user: %w", err)
	}
	if u.HomeDir == "" || !filepath.IsAbs(u.HomeDir) {
		return Location{}, errors.New("effective user has no absolute home directory")
	}
	home := filepath.Clean(u.HomeDir)
	if env := os.Getenv("HOME"); env != "" && filepath.Clean(env) != home {
		return Location{}, errors.New("HOME disagrees with effective user home")
	}
	if override != "" {
		if !filepath.IsAbs(override) || strings.IndexByte(override, 0) >= 0 {
			return Location{}, errors.New("config path must be absolute and contain no NUL")
		}
		return Location{Home: home, Path: filepath.Clean(override)}, nil
	}
	root, err := xdg("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	if err != nil {
		return Location{}, err
	}
	return Location{Home: home, Path: filepath.Join(root, "mosscap", FileName)}, nil
}

func xdg(name, fallback string) (string, error) {
	v, ok := os.LookupEnv(name)
	if !ok {
		return fallback, nil
	}
	if v == "" || !filepath.IsAbs(v) || strings.IndexByte(v, 0) >= 0 {
		return "", fmt.Errorf("%s must be an absolute non-empty path", name)
	}
	return filepath.Clean(v), nil
}

// Default returns an initial configuration for loc.
func Default(loc Location) (Config, error) {
	stateRoot, err := xdg("XDG_STATE_HOME", filepath.Join(loc.Home, ".local", "state"))
	if err != nil {
		return Config{}, err
	}
	state := filepath.Join(stateRoot, "mosscap")
	return Config{Version: 1, Paths: Paths{StateDir: state, Database: filepath.Join(state, "mosscap.sqlite3"), Endpoint: filepath.Join(state, "control.sock"), TmpDir: filepath.Join(state, "tmp")}, Slack: Slack{TrustedUserIDs: []string{}}, Database: Database{BusyTimeout: "5s"}, Protocol: Protocol{MaxFrameBytes: 1048576}}, nil
}

// Load strictly decodes and validates a config file.
func Load(loc Location) (Config, error) {
	if err := securepath.CheckDir(filepath.Dir(loc.Path)); err != nil {
		return Config{}, fmt.Errorf("config root: %w", err)
	}
	if err := securepath.CheckFile(loc.Path); err != nil {
		return Config{}, fmt.Errorf("config: %w", err)
	}
	var c Config
	md, err := toml.DecodeFile(loc.Path, &c)
	if err != nil {
		return Config{}, fmt.Errorf("decode config: %w", err)
	}
	if undecoded := md.Undecoded(); len(undecoded) != 0 {
		return Config{}, fmt.Errorf("decode config: unknown key %s", undecoded[0])
	}
	if err := Validate(c); err != nil {
		return Config{}, err
	}
	return c, nil
}

// Validate applies all semantic and lexical constraints independent of filesystem state.
func Validate(c Config) error {
	if c.Version != 1 {
		return fmt.Errorf("unsupported config version %d", c.Version)
	}
	if _, err := time.ParseDuration(c.Database.BusyTimeout); err != nil {
		return fmt.Errorf("database.busy_timeout: %w", err)
	}
	if c.Protocol.MaxFrameBytes < 64*1024 || c.Protocol.MaxFrameBytes > 16*1024*1024 {
		return errors.New("protocol.max_frame_bytes must be between 65536 and 16777216")
	}
	for name, p := range map[string]string{"paths.state_dir": c.Paths.StateDir, "paths.database": c.Paths.Database, "paths.endpoint": c.Paths.Endpoint, "paths.tmp_dir": c.Paths.TmpDir} {
		if p == "" || !filepath.IsAbs(p) || strings.IndexByte(p, 0) >= 0 {
			return fmt.Errorf("%s must be an absolute path without NUL", name)
		}
	}
	if !securepath.Descendant(c.Paths.StateDir, c.Paths.Database) || !securepath.Descendant(c.Paths.StateDir, c.Paths.Endpoint) || !securepath.Descendant(c.Paths.StateDir, c.Paths.TmpDir) {
		return errors.New("database, endpoint, and tmp_dir must be descendants of paths.state_dir")
	}
	if c.Slack.SecretsFile != "" && (!filepath.IsAbs(c.Slack.SecretsFile) || strings.IndexByte(c.Slack.SecretsFile, 0) >= 0) {
		return errors.New("slack.secrets_file must be an absolute path without NUL")
	}
	return nil
}

// Setup creates a private configuration/state layout and initializes its new ledger.
func Setup(loc Location) (Config, error) {
	def, err := Default(loc)
	if err != nil {
		return Config{}, err
	}
	if err := securepath.EnsurePrivateDir(loc.Home, filepath.Dir(loc.Path)); err != nil {
		return Config{}, err
	}
	if err := securepath.EnsurePrivateDir(loc.Home, def.Paths.StateDir); err != nil {
		return Config{}, err
	}
	if err := securepath.EnsurePrivateDir(loc.Home, def.Paths.TmpDir); err != nil {
		return Config{}, err
	}
	if _, err := os.Lstat(loc.Path); errors.Is(err, os.ErrNotExist) {
		if err := writeAtomic(loc.Path, def); err != nil {
			return Config{}, err
		}
		return initializeLedger(def)
	} else if err != nil {
		return Config{}, err
	}
	c, err := Load(loc)
	if err != nil {
		return Config{}, err
	}
	if err := securepath.EnsurePrivateDir(loc.Home, c.Paths.StateDir); err != nil {
		return Config{}, err
	}
	if err := securepath.EnsurePrivateDir(loc.Home, c.Paths.TmpDir); err != nil {
		return Config{}, err
	}
	if c.Slack.SecretsFile != "" {
		if err := securepath.CheckFile(c.Slack.SecretsFile); err != nil {
			return Config{}, fmt.Errorf("slack.secrets_file: %w", err)
		}
	}
	return initializeLedger(c)
}

func initializeLedger(c Config) (Config, error) {
	busy, err := time.ParseDuration(c.Database.BusyTimeout)
	if err != nil {
		return Config{}, err
	}
	var s *store.Store
	if _, err := os.Lstat(c.Paths.Database); errors.Is(err, os.ErrNotExist) {
		s, err = store.Initialize(context.Background(), c.Paths.Database, store.Options{BusyTimeout: busy})
	} else if err == nil {
		if err := securepath.CheckFile(c.Paths.Database); err != nil {
			return Config{}, fmt.Errorf("database: %w", err)
		}
		s, err = store.Open(context.Background(), c.Paths.Database, store.Options{BusyTimeout: busy})
	}
	if err != nil {
		return Config{}, fmt.Errorf("initialize ledger: %w", err)
	}
	if err := s.Close(); err != nil {
		return Config{}, err
	}
	if err := securepath.CheckFile(c.Paths.Database); err != nil {
		return Config{}, fmt.Errorf("database: %w", err)
	}
	return c, nil
}

var allowed = map[string]bool{"paths.state_dir": true, "paths.database": true, "paths.endpoint": true, "paths.tmp_dir": true, "slack.team_id": true, "slack.bot_user_id": true, "slack.channel_id": true, "slack.trusted_user_ids": true, "slack.secrets_file": true, "database.busy_timeout": true, "protocol.max_frame_bytes": true}

// Set changes one allowlisted setting after confirming the configured gateway is offline.
func Set(loc Location, key, value string) (Config, error) {
	if !allowed[key] {
		return Config{}, fmt.Errorf("config key %q is not allowed", key)
	}
	c, err := Load(loc)
	if err != nil {
		return Config{}, err
	}
	if live(c.Paths.Endpoint) {
		return Config{}, errors.New("config set requires the configured gateway to be offline")
	}
	switch key {
	case "paths.state_dir":
		c.Paths.StateDir = value
	case "paths.database":
		c.Paths.Database = value
	case "paths.endpoint":
		c.Paths.Endpoint = value
	case "paths.tmp_dir":
		c.Paths.TmpDir = value
	case "slack.team_id":
		c.Slack.TeamID = value
	case "slack.bot_user_id":
		c.Slack.BotUserID = value
	case "slack.channel_id":
		c.Slack.ChannelID = value
	case "slack.secrets_file":
		c.Slack.SecretsFile = value
	case "database.busy_timeout":
		c.Database.BusyTimeout = value
	case "protocol.max_frame_bytes":
		n, e := strconv.Atoi(value)
		if e != nil {
			return Config{}, fmt.Errorf("protocol.max_frame_bytes: %w", e)
		}
		c.Protocol.MaxFrameBytes = n
	case "slack.trusted_user_ids":
		var v struct {
			IDs []string `toml:"ids"`
		}
		if _, e := toml.Decode("ids = "+value, &v); e != nil {
			return Config{}, fmt.Errorf("slack.trusted_user_ids must be a TOML array: %w", e)
		}
		c.Slack.TrustedUserIDs = v.IDs
	}
	if err := Validate(c); err != nil {
		return Config{}, err
	}
	if err := writeAtomic(loc.Path, c); err != nil {
		return Config{}, err
	}
	return c, nil
}
func live(endpoint string) bool {
	c, err := net.DialTimeout("unix", endpoint, 150*time.Millisecond)
	if err == nil {
		_ = c.Close()
		return true
	}
	return false
}
func writeAtomic(path string, c Config) error {
	var suffix [16]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return err
	}
	tmp := filepath.Join(filepath.Dir(path), ".config."+fmt.Sprintf("%x", suffix)+".tmp")
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tmp)
		}
	}()
	if err := toml.NewEncoder(f).Encode(c); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return err
	}
	ok = true
	return nil
}

// View is safe for display: it never contains token values.
type View struct {
	Version  int       `json:"version"`
	Paths    Paths     `json:"paths"`
	Slack    SlackView `json:"slack"`
	Database Database  `json:"database"`
	Protocol Protocol  `json:"protocol"`
}
type SlackView struct {
	TeamID          string   `json:"team_id"`
	BotUserID       string   `json:"bot_user_id"`
	ChannelID       string   `json:"channel_id"`
	TrustedUserIDs  []string `json:"trusted_user_ids"`
	SecretsFile     string   `json:"secrets_file"`
	AppTokenPresent bool     `json:"app_token_present"`
	BotTokenPresent bool     `json:"bot_token_present"`
}

func Redact(c Config) View {
	secret := "<unset>"
	if c.Slack.SecretsFile != "" {
		secret = "<configured>"
	}
	return View{c.Version, c.Paths, SlackView{c.Slack.TeamID, c.Slack.BotUserID, c.Slack.ChannelID, c.Slack.TrustedUserIDs, secret, os.Getenv("MOSSCAP_SLACK_APP_TOKEN") != "", os.Getenv("MOSSCAP_SLACK_BOT_TOKEN") != ""}, c.Database, c.Protocol}
}

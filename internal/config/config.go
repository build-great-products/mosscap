// Package config owns Mosscap's strict, private TOML configuration.
package config

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/stayradiated/mosscap/internal/securepath"
	"github.com/stayradiated/mosscap/internal/store"
)

const (
	FileName       = "config.toml"
	maxConfigBytes = 1 << 20
)

// Config is the complete file-backed configuration. Paths are derived from
// XDG roots, never decoded from config.toml.
type Config struct {
	Version                       int      `toml:"version" json:"version"`
	SlackAppToken                 string   `toml:"slack_app_token" json:"-"`
	SlackBotToken                 string   `toml:"slack_bot_token" json:"-"`
	SlackChannelID                string   `toml:"slack_channel_id" json:"slack_channel_id"`
	DeliveryLease                 string   `toml:"delivery_lease" json:"delivery_lease"`
	LeaderHeartbeat               string   `toml:"leader_heartbeat" json:"leader_heartbeat"`
	LeaderExpiry                  string   `toml:"leader_expiry" json:"leader_expiry"`
	SlackReconnectInitialBackoff  string   `toml:"slack_reconnect_initial_backoff" json:"slack_reconnect_initial_backoff"`
	SlackReconnectMaxBackoff      string   `toml:"slack_reconnect_max_backoff" json:"slack_reconnect_max_backoff"`
	SlackReconnectJitter          bool     `toml:"slack_reconnect_jitter" json:"slack_reconnect_jitter"`
	SQLiteBusyTimeout             string   `toml:"sqlite_busy_timeout" json:"sqlite_busy_timeout"`
	MaximumFileSize               ByteSize `toml:"maximum_file_size" json:"maximum_file_size"`
	MaximumFilesPerSlackOperation int      `toml:"maximum_files_per_slack_operation" json:"maximum_files_per_slack_operation"`
	TemporaryDownloadLifetime     string   `toml:"temporary_download_lifetime" json:"temporary_download_lifetime"`
	LocalProtocolMaximumFrame     int      `toml:"local_protocol_maximum_frame" json:"local_protocol_maximum_frame"`
	Paths                         Paths    `toml:"-" json:"paths"`
}

// Paths are fixed XDG-derived locations exposed for consumers of Config.
// ByteSize is a non-negative binary byte quantity. It accepts legacy TOML
// integers and documented B, KiB, MiB, or GiB strings.
type ByteSize int64

func (s *ByteSize) UnmarshalTOML(value any) error {
	var text string
	switch v := value.(type) {
	case int64:
		if v < 0 {
			return errors.New("byte size must not be negative")
		}
		*s = ByteSize(v)
		return nil
	case string:
		text = v
	default:
		return errors.New("byte size must be an integer or B/KiB/MiB/GiB string")
	}
	parsed, err := parseByteSize(text)
	if err != nil {
		return err
	}
	*s = parsed
	return nil
}

func (s ByteSize) String() string {
	for _, unit := range []struct {
		name  string
		value ByteSize
	}{{"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10}} {
		if s >= unit.value && s%unit.value == 0 {
			return strconv.FormatInt(int64(s/unit.value), 10) + unit.name
		}
	}
	return strconv.FormatInt(int64(s), 10) + "B"
}

func parseByteSize(text string) (ByteSize, error) {
	unit := int64(1)
	number := text
	for _, suffix := range []struct {
		text string
		unit int64
	}{{"KiB", 1 << 10}, {"MiB", 1 << 20}, {"GiB", 1 << 30}, {"B", 1}} {
		if strings.HasSuffix(text, suffix.text) {
			number, unit = strings.TrimSuffix(text, suffix.text), suffix.unit
			break
		}
	}
	if number == "" {
		return 0, errors.New("byte size must use a decimal integer optionally followed by B, KiB, MiB, or GiB")
	}
	value, err := strconv.ParseInt(number, 10, 64)
	if err != nil || value < 0 || value > (1<<63-1)/unit {
		return 0, errors.New("byte size is invalid or overflows")
	}
	return ByteSize(value * unit), nil
}

type Paths struct {
	ConfigDir string `json:"config_dir"`
	StateDir  string `json:"state_dir"`
	Database  string `json:"database"`
	Endpoint  string `json:"endpoint"`
	TmpDir    string `json:"tmp_dir"`
}

// Location is the XDG-derived location set selected for the effective user.
// Call DefaultLocation rather than constructing one in application code.
type Location struct {
	Home, Path string
	Paths      Paths
}

// DefaultLocation derives all locations from the effective account's XDG
// roots. Configuration has no path override: use different XDG roots instead.
func DefaultLocation() (Location, error) {
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
	configRoot, err := xdg("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	if err != nil {
		return Location{}, err
	}
	stateRoot, err := xdg("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	if err != nil {
		return Location{}, err
	}
	configDir := filepath.Join(configRoot, "mosscap")
	stateDir := filepath.Join(stateRoot, "mosscap")
	return Location{Home: home, Path: filepath.Join(configDir, FileName), Paths: Paths{
		ConfigDir: configDir,
		StateDir:  stateDir,
		Database:  filepath.Join(stateDir, "mosscap.sqlite3"),
		Endpoint:  filepath.Join(stateDir, "control.sock"),
		TmpDir:    filepath.Join(stateDir, "tmp"),
	}}, nil
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

// Default returns the initial complete configuration for an XDG location.
func Default(loc Location) (Config, error) {
	paths, err := pathsFor(loc)
	if err != nil {
		return Config{}, err
	}
	return Config{
		Version:                       1,
		DeliveryLease:                 "60s",
		LeaderHeartbeat:               "10s",
		LeaderExpiry:                  "30s",
		SlackReconnectInitialBackoff:  "1s",
		SlackReconnectMaxBackoff:      "60s",
		SlackReconnectJitter:          true,
		SQLiteBusyTimeout:             "5s",
		MaximumFileSize:               ByteSize(100 << 20),
		MaximumFilesPerSlackOperation: 20,
		TemporaryDownloadLifetime:     "24h",
		LocalProtocolMaximumFrame:     1 << 20,
		Paths:                         paths,
	}, nil
}

// Load strictly decodes and validates config.toml. It never imports legacy
// trust or secrets sources.
func Load(loc Location) (Config, error) {
	c, _, err := LoadWithDigest(loc)
	return c, err
}

// LoadWithDigest strictly decodes config.toml and returns a SHA-256 digest of
// its exact staged bytes. The digest is safe to persist for request
// deduplication: it never contains token values, while still distinguishing a
// token-only staged edit from the prior candidate.
func LoadWithDigest(loc Location) (Config, string, error) {
	if _, err := pathsFor(loc); err != nil {
		return Config{}, "", err
	}
	b, err := readConfig(loc)
	if err != nil {
		return Config{}, "", err
	}
	h := sha256.Sum256(b)
	c, err := decode(loc, b)
	if err != nil {
		return Config{}, "sha256:" + hex.EncodeToString(h[:]), err
	}
	return c, "sha256:" + hex.EncodeToString(h[:]), nil
}

func pathsFor(loc Location) (Paths, error) {
	if loc.Home == "" || !filepath.IsAbs(loc.Home) || loc.Path == "" || !filepath.IsAbs(loc.Path) {
		return Paths{}, errors.New("invalid XDG location")
	}
	p := loc.Paths
	if p.ConfigDir == "" {
		p.ConfigDir = filepath.Dir(loc.Path)
	}
	if p.StateDir == "" {
		return Paths{}, errors.New("invalid XDG location: missing state directory")
	}
	p.ConfigDir, p.StateDir = filepath.Clean(p.ConfigDir), filepath.Clean(p.StateDir)
	if filepath.Clean(loc.Path) != filepath.Join(p.ConfigDir, FileName) {
		return Paths{}, errors.New("invalid XDG location: config path is not config.toml")
	}
	if p.Database == "" {
		p.Database = filepath.Join(p.StateDir, "mosscap.sqlite3")
	}
	if p.Endpoint == "" {
		p.Endpoint = filepath.Join(p.StateDir, "control.sock")
	}
	if p.TmpDir == "" {
		p.TmpDir = filepath.Join(p.StateDir, "tmp")
	}
	if p.Database != filepath.Join(p.StateDir, "mosscap.sqlite3") || p.Endpoint != filepath.Join(p.StateDir, "control.sock") || p.TmpDir != filepath.Join(p.StateDir, "tmp") {
		return Paths{}, errors.New("invalid XDG location: state paths must be fixed")
	}
	return p, nil
}

func readConfig(loc Location) ([]byte, error) {
	if err := securepath.CheckDir(filepath.Dir(loc.Path)); err != nil {
		return nil, fmt.Errorf("config root: %w", err)
	}
	f, err := securepath.OpenFileNoFollow(loc.Path, os.O_RDONLY, 0)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	defer f.Close()
	if err := securepath.CheckOpenFile(loc.Path, f); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	b, err := io.ReadAll(io.LimitReader(f, maxConfigBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	if len(b) > maxConfigBytes {
		return nil, errors.New("config exceeds 1 MiB")
	}
	if err := securepath.CheckOpenFile(loc.Path, f); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	return b, nil
}

func decode(loc Location, b []byte) (Config, error) {
	var c Config
	md, err := toml.Decode(string(b), &c)
	if err != nil {
		return Config{}, fmt.Errorf("decode config: %w", err)
	}
	if undecoded := md.Undecoded(); len(undecoded) != 0 {
		key := undecoded[0].String()
		return Config{}, unknownKeyError(key)
	}
	for _, key := range configKeys {
		if !md.IsDefined(key) {
			return Config{}, fmt.Errorf("decode config: missing required key %q", key)
		}
	}
	paths, err := pathsFor(loc)
	if err != nil {
		return Config{}, err
	}
	c.Paths = paths
	if err := Validate(c); err != nil {
		return Config{}, err
	}
	return c, nil
}

func unknownKeyError(key string) error {
	if key == "slack_team_id" || key == "slack_bot_user_id" {
		return fmt.Errorf("decode config: obsolete %q: Slack identity is derived from authenticated Slack tokens; remove it", key)
	}
	if key == "paths" || strings.HasPrefix(key, "paths.") {
		return fmt.Errorf("decode config: obsolete %q: paths are fixed by XDG_CONFIG_HOME and XDG_STATE_HOME; remove it", key)
	}
	if key == "trusted_user_ids" || strings.HasSuffix(key, ".trusted_user_ids") {
		return fmt.Errorf("decode config: obsolete %q: trust lives in SQLite; use `mosscap trust add`", key)
	}
	if key == "secrets_file" || strings.HasSuffix(key, ".secrets_file") {
		return fmt.Errorf("decode config: obsolete %q: put Slack tokens in config.toml", key)
	}
	return fmt.Errorf("decode config: unknown key %q", key)
}

var configKeys = []string{
	"version", "slack_app_token", "slack_bot_token", "slack_channel_id",
	"delivery_lease", "leader_heartbeat", "leader_expiry", "slack_reconnect_initial_backoff",
	"slack_reconnect_max_backoff", "slack_reconnect_jitter", "sqlite_busy_timeout", "maximum_file_size",
	"maximum_files_per_slack_operation", "temporary_download_lifetime", "local_protocol_maximum_frame",
}

var (
	slackID = regexp.MustCompile(`^[A-Z][A-Z0-9]{1,}$`)
	tokens  = map[string]*regexp.Regexp{
		"slack_app_token": regexp.MustCompile(`^xapp-[^\x00-\x20]+$`),
		"slack_bot_token": regexp.MustCompile(`^xoxb-[^\x00-\x20]+$`),
	}
)

// Validate applies lexical, range, and ordering constraints independently of
// filesystem state. Empty Slack credentials and IDs are intentionally allowed
// until Slack is enabled.
func Validate(c Config) error {
	if c.Version != 1 {
		return fmt.Errorf("unsupported config version %d", c.Version)
	}
	if err := optionalToken("slack_app_token", c.SlackAppToken); err != nil {
		return err
	}
	if err := optionalToken("slack_bot_token", c.SlackBotToken); err != nil {
		return err
	}
	if c.SlackChannelID != "" && (!slackID.MatchString(c.SlackChannelID) || (c.SlackChannelID[0] != 'C' && c.SlackChannelID[0] != 'G')) {
		return errors.New("slack_channel_id must be an empty Slack channel ID or begin with C/G")
	}
	lease, err := positiveDuration("delivery_lease", c.DeliveryLease)
	if err != nil {
		return err
	}
	heartbeat, err := positiveDuration("leader_heartbeat", c.LeaderHeartbeat)
	if err != nil {
		return err
	}
	expiry, err := positiveDuration("leader_expiry", c.LeaderExpiry)
	if err != nil {
		return err
	}
	if heartbeat >= expiry {
		return errors.New("leader_heartbeat must be less than leader_expiry")
	}
	if lease <= 0 { // retained for clarity if duration parsing changes.
		return errors.New("delivery_lease must be positive")
	}
	initial, err := positiveDuration("slack_reconnect_initial_backoff", c.SlackReconnectInitialBackoff)
	if err != nil {
		return err
	}
	maximum, err := positiveDuration("slack_reconnect_max_backoff", c.SlackReconnectMaxBackoff)
	if err != nil {
		return err
	}
	if initial > maximum {
		return errors.New("slack_reconnect_initial_backoff must not exceed slack_reconnect_max_backoff")
	}
	if _, err := positiveDuration("sqlite_busy_timeout", c.SQLiteBusyTimeout); err != nil {
		return err
	}
	if _, err := positiveDuration("temporary_download_lifetime", c.TemporaryDownloadLifetime); err != nil {
		return err
	}
	if c.MaximumFileSize < 1 || c.MaximumFileSize > ByteSize(1<<40) {
		return errors.New("maximum_file_size must be between 1 and 1099511627776")
	}
	if c.MaximumFilesPerSlackOperation < 1 || c.MaximumFilesPerSlackOperation > 20 {
		return errors.New("maximum_files_per_slack_operation must be between 1 and 20")
	}
	if c.LocalProtocolMaximumFrame < 64<<10 || c.LocalProtocolMaximumFrame > 16<<20 {
		return errors.New("local_protocol_maximum_frame must be between 65536 and 16777216")
	}
	return nil
}

func optionalToken(name, value string) error {
	if value != "" && !tokens[name].MatchString(value) {
		return fmt.Errorf("%s has invalid Slack token syntax", name)
	}
	return nil
}
func optionalID(name, value string, prefix byte) error {
	if value != "" && (!slackID.MatchString(value) || value[0] != prefix) {
		return fmt.Errorf("%s must be an empty Slack ID or begin with %c", name, prefix)
	}
	return nil
}
func positiveDuration(name, value string) (time.Duration, error) {
	d, err := time.ParseDuration(value)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%s must be a positive Go duration", name)
	}
	return d, nil
}

// Setup creates the private fixed XDG layout, writes a documented config only
// when absent, and initializes or verifies the fixed ledger without overwrite.
func Setup(loc Location) (Config, error) {
	def, err := Default(loc)
	if err != nil {
		return Config{}, err
	}
	if err := ensureXDGDir(loc.Home, def.Paths.ConfigDir); err != nil {
		return Config{}, err
	}
	if err := ensureXDGDir(loc.Home, def.Paths.StateDir); err != nil {
		return Config{}, err
	}
	if err := ensureXDGDir(loc.Home, def.Paths.TmpDir); err != nil {
		return Config{}, err
	}

	if _, err := os.Lstat(loc.Path); errors.Is(err, os.ErrNotExist) {
		if err := createConfig(loc.Path, template(def)); err != nil {
			return Config{}, err
		}
	} else if err != nil {
		return Config{}, fmt.Errorf("config: %w", err)
	}
	c, err := Load(loc)
	if err != nil {
		return Config{}, err
	}
	return initializeLedger(c)
}

func ensureXDGDir(home, path string) error {
	if path == filepath.Clean(home) || securepath.Descendant(home, path) {
		return securepath.EnsurePrivateDir(home, path)
	}
	return securepath.EnsurePrivateAbsoluteDir(path)
}

func initializeLedger(c Config) (Config, error) {
	busy, err := time.ParseDuration(c.SQLiteBusyTimeout)
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

func template(c Config) []byte {
	return []byte(fmt.Sprintf(`# Mosscap configuration. Paths are fixed by XDG_CONFIG_HOME and XDG_STATE_HOME.
# Keep this file private (mode 0600). Empty Slack fields are allowed until Slack is configured.
version = %d

# Slack Socket Mode app token and bot token. Both are redacted from all output.
slack_app_token = %q
slack_bot_token = %q
# One configured channel (C... or G...). Workspace and bot user IDs are derived from auth.test.
slack_channel_id = %q

# Delivery leader lease and heartbeat timings.
delivery_lease = %q
leader_heartbeat = %q
leader_expiry = %q

# Slack reconnect uses exponential backoff in this inclusive range.
slack_reconnect_initial_backoff = %q
slack_reconnect_max_backoff = %q
slack_reconnect_jitter = %t

# SQLite lock wait, Slack file limits, temporary-byte retention, and local protocol bound.
sqlite_busy_timeout = %q
maximum_file_size = %q
maximum_files_per_slack_operation = %d
temporary_download_lifetime = %q
local_protocol_maximum_frame = %d
`, c.Version, c.SlackAppToken, c.SlackBotToken, c.SlackChannelID,
		c.DeliveryLease, c.LeaderHeartbeat, c.LeaderExpiry, c.SlackReconnectInitialBackoff, c.SlackReconnectMaxBackoff,
		c.SlackReconnectJitter, c.SQLiteBusyTimeout, c.MaximumFileSize.String(), c.MaximumFilesPerSlackOperation,
		c.TemporaryDownloadLifetime, c.LocalProtocolMaximumFrame))
}

var setMu sync.Mutex

var settable = map[string]bool{
	"slack_channel_id": true, "delivery_lease": true, "leader_heartbeat": true, "leader_expiry": true,
	"slack_reconnect_initial_backoff": true, "slack_reconnect_max_backoff": true, "slack_reconnect_jitter": true,
	"sqlite_busy_timeout": true, "maximum_file_size": true, "maximum_files_per_slack_operation": true,
	"temporary_download_lifetime": true, "local_protocol_maximum_frame": true,
}

// Set validates a complete candidate then replaces only the selected TOML
// value bytes. It may run while the gateway is live. Slack credentials must
// use SetSecret so callers cannot accidentally pass them through argv.
func Set(loc Location, key, value string) (Config, error) {
	if isSecretKey(key) {
		return Config{}, fmt.Errorf("config key %q is secret; use `mosscap config secret set %s`", key, key)
	}
	return set(loc, key, value)
}

// SetSecret atomically writes one validated Slack credential while preserving
// the existing config's comments, formatting, and private mode.
func SetSecret(loc Location, key, value string) (Config, error) {
	if !isSecretKey(key) {
		return Config{}, fmt.Errorf("config key %q is not a supported secret", key)
	}
	return set(loc, key, value)
}

func isSecretKey(key string) bool { return key == "slack_app_token" || key == "slack_bot_token" }

func set(loc Location, key, value string) (Config, error) {
	if !settable[key] && !isSecretKey(key) {
		return Config{}, fmt.Errorf("config key %q is not allowed", key)
	}
	setMu.Lock()
	defer setMu.Unlock()
	unlock, err := lockConfig(loc)
	if err != nil {
		return Config{}, err
	}
	defer unlock()

	b, err := readConfig(loc)
	if err != nil {
		return Config{}, err
	}
	c, err := decode(loc, b)
	if err != nil {
		return Config{}, err
	}
	replacement, err := setValue(&c, key, value)
	if err != nil {
		return Config{}, err
	}
	if err := Validate(c); err != nil {
		return Config{}, err
	}
	start, end, err := valueRange(b, key)
	if err != nil {
		return Config{}, err
	}
	out := make([]byte, 0, len(b)-end+start+len(replacement))
	out = append(out, b[:start]...)
	out = append(out, replacement...)
	out = append(out, b[end:]...)
	if err := replaceConfig(loc.Path, out); err != nil {
		return Config{}, err
	}
	return c, nil
}

func setValue(c *Config, key, input string) ([]byte, error) {
	stringValue := func(dst *string) ([]byte, error) {
		v := input
		var parsed string
		if _, err := toml.Decode("v = "+input, &struct {
			V *string `toml:"v"`
		}{V: &parsed}); err == nil {
			v = parsed
		}
		*dst = v
		return []byte(strconv.Quote(v)), nil
	}
	durationValue := func(dst *string) ([]byte, error) {
		if _, err := positiveDuration(key, input); err != nil {
			return nil, err
		}
		*dst = input
		return []byte(strconv.Quote(input)), nil
	}
	intValue := func(dst *int) ([]byte, error) {
		v, err := strconv.Atoi(input)
		if err != nil {
			return nil, fmt.Errorf("%s must be an integer", key)
		}
		*dst = v
		return []byte(strconv.Itoa(v)), nil
	}
	switch key {
	case "slack_app_token":
		return stringValue(&c.SlackAppToken)
	case "slack_bot_token":
		return stringValue(&c.SlackBotToken)
	case "slack_channel_id":
		return stringValue(&c.SlackChannelID)
	case "delivery_lease":
		return durationValue(&c.DeliveryLease)
	case "leader_heartbeat":
		return durationValue(&c.LeaderHeartbeat)
	case "leader_expiry":
		return durationValue(&c.LeaderExpiry)
	case "slack_reconnect_initial_backoff":
		return durationValue(&c.SlackReconnectInitialBackoff)
	case "slack_reconnect_max_backoff":
		return durationValue(&c.SlackReconnectMaxBackoff)
	case "sqlite_busy_timeout":
		return durationValue(&c.SQLiteBusyTimeout)
	case "temporary_download_lifetime":
		return durationValue(&c.TemporaryDownloadLifetime)
	case "maximum_file_size":
		v, err := parseByteSize(input)
		if err != nil {
			return nil, errors.New("maximum_file_size must be a byte size such as 200MiB")
		}
		c.MaximumFileSize = v
		return []byte(strconv.Quote(input)), nil
	case "maximum_files_per_slack_operation":
		return intValue(&c.MaximumFilesPerSlackOperation)
	case "local_protocol_maximum_frame":
		return intValue(&c.LocalProtocolMaximumFrame)
	case "slack_reconnect_jitter":
		v, err := strconv.ParseBool(input)
		if err != nil {
			return nil, errors.New("slack_reconnect_jitter must be true or false")
		}
		c.SlackReconnectJitter = v
		return []byte(strconv.FormatBool(v)), nil
	}
	return nil, fmt.Errorf("config key %q is not allowed", key)
}

func lockConfig(loc Location) (func(), error) {
	if err := securepath.CheckDir(filepath.Dir(loc.Path)); err != nil {
		return nil, fmt.Errorf("config root: %w", err)
	}
	lock := filepath.Join(filepath.Dir(loc.Path), ".config.lock")
	f, err := securepath.OpenFileNoFollow(lock, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := f.Chmod(0600); err != nil {
		_ = f.Close()
		return nil, err
	}
	if err := securepath.CheckOpenFile(lock, f); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("config lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, err
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }, nil
}

func valueRange(b []byte, key string) (int, int, error) {
	for lineStart := 0; lineStart < len(b); {
		lineEnd := lineStart
		for lineEnd < len(b) && b[lineEnd] != '\n' {
			lineEnd++
		}
		line := b[lineStart:lineEnd]
		trimmed := strings.TrimLeft(string(line), " \t\r")
		if strings.HasPrefix(trimmed, "[") {
			return 0, 0, errors.New("config must use only top-level scalar keys")
		}
		if strings.HasPrefix(trimmed, key) {
			offset := lineStart + len(line) - len(trimmed)
			after := trimmed[len(key):]
			if len(after) > 0 && (after[0] == ' ' || after[0] == '\t' || after[0] == '=') {
				eq := strings.IndexByte(after, '=')
				if eq >= 0 {
					start := offset + len(key) + eq + 1
					for start < lineEnd && (b[start] == ' ' || b[start] == '\t') {
						start++
					}
					end := scalarEnd(b, start, lineEnd)
					for end > start && (b[end-1] == ' ' || b[end-1] == '\t' || b[end-1] == '\r') {
						end--
					}
					if start == end {
						return 0, 0, fmt.Errorf("config key %q has no scalar value", key)
					}
					return start, end, nil
				}
			}
		}
		lineStart = lineEnd + 1
	}
	return 0, 0, fmt.Errorf("config key %q is missing", key)
}

func scalarEnd(b []byte, start, lineEnd int) int {
	quote := byte(0)
	escaped := false
	for i := start; i < lineEnd; i++ {
		ch := b[i]
		if quote != 0 {
			if quote == '"' && ch == '\\' && !escaped {
				escaped = true
				continue
			}
			if ch == quote && !escaped {
				quote = 0
			}
			escaped = false
			continue
		}
		if ch == '"' || ch == '\'' {
			quote = ch
			continue
		}
		if ch == '#' {
			return i
		}
	}
	return lineEnd
}

func createConfig(path string, b []byte) error {
	tmp, err := writeTemp(filepath.Dir(path), b)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	if err := os.Link(tmp, path); err != nil {
		return fmt.Errorf("create config without overwrite: %w", err)
	}
	if err := syncDir(filepath.Dir(path)); err != nil {
		return err
	}
	return os.Remove(tmp)
}
func replaceConfig(path string, b []byte) error {
	if err := securepath.CheckFile(path); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	tmp, err := writeTemp(filepath.Dir(path), b)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tmp)
		}
	}()
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	if err := syncDir(filepath.Dir(path)); err != nil {
		return err
	}
	ok = true
	return nil
}
func writeTemp(dir string, b []byte) (string, error) {
	var suffix [16]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", err
	}
	path := filepath.Join(dir, ".config."+fmt.Sprintf("%x", suffix)+".tmp")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return "", err
	}
	if err := f.Chmod(0600); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return "", err
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return "", err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return "", err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	return path, nil
}
func syncDir(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// View is safe for all display and protocol output: token values are not part
// of its structure.
type View struct {
	Version                       int    `json:"version"`
	Paths                         Paths  `json:"paths"`
	SlackAppTokenPresent          bool   `json:"slack_app_token_present"`
	SlackBotTokenPresent          bool   `json:"slack_bot_token_present"`
	SlackChannelID                string `json:"slack_channel_id"`
	SlackIdentitySource           string `json:"slack_identity_source"`
	DeliveryLease                 string `json:"delivery_lease"`
	LeaderHeartbeat               string `json:"leader_heartbeat"`
	LeaderExpiry                  string `json:"leader_expiry"`
	SlackReconnectInitialBackoff  string `json:"slack_reconnect_initial_backoff"`
	SlackReconnectMaxBackoff      string `json:"slack_reconnect_max_backoff"`
	SlackReconnectJitter          bool   `json:"slack_reconnect_jitter"`
	SQLiteBusyTimeout             string `json:"sqlite_busy_timeout"`
	MaximumFileSize               int64  `json:"maximum_file_size"`
	MaximumFilesPerSlackOperation int    `json:"maximum_files_per_slack_operation"`
	TemporaryDownloadLifetime     string `json:"temporary_download_lifetime"`
	LocalProtocolMaximumFrame     int    `json:"local_protocol_maximum_frame"`
}

// Redact returns a structurally token-free configuration view.
func Redact(c Config) View {
	return View{c.Version, c.Paths, c.SlackAppToken != "", c.SlackBotToken != "", c.SlackChannelID, "verified_at_runtime",
		c.DeliveryLease, c.LeaderHeartbeat, c.LeaderExpiry, c.SlackReconnectInitialBackoff,
		c.SlackReconnectMaxBackoff, c.SlackReconnectJitter, c.SQLiteBusyTimeout, int64(c.MaximumFileSize),
		c.MaximumFilesPerSlackOperation, c.TemporaryDownloadLifetime, c.LocalProtocolMaximumFrame}
}

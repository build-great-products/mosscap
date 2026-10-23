package app

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/stayradiated/mosscap/internal/config"
	"github.com/stayradiated/mosscap/internal/protocol"
	"github.com/stayradiated/mosscap/internal/securepath"
)

// readCommandTextSource reads one complete message body before any gateway
// interaction. stdin is deliberately not subject to config secret's non-TTY
// rule: users may explicitly type a multiline body at a terminal.
func (a App) readCommandTextSource(text, path string, textSet, pathSet, stdin bool) (string, error) {
	sources := 0
	for _, set := range []bool{textSet, pathSet, stdin} {
		if set {
			sources++
		}
	}
	if sources != 1 {
		return "", usage("exactly one of --text, --text-file, or --stdin is required")
	}
	if textSet {
		return validateCommandText(text)
	}
	c, err := loadCommandConfig()
	if err != nil {
		return "", err
	}
	if stdin {
		b, err := io.ReadAll(io.LimitReader(a.In, int64(c.LocalProtocolMaximumFrame)+1))
		if err != nil {
			return "", fmt.Errorf("--stdin: could not read message text: %w", err)
		}
		if len(b) > c.LocalProtocolMaximumFrame {
			return "", usage("--stdin exceeds the protocol frame limit")
		}
		return validateCommandText(string(b))
	}
	b, err := securepath.ReadRegularFile(path, int64(c.LocalProtocolMaximumFrame))
	if err != nil {
		return "", fmt.Errorf("--text-file: %w", err)
	}
	return validateCommandText(string(b))
}

func validateCommandText(text string) (string, error) {
	if text == "" {
		return "", usage("message text must not be empty")
	}
	if strings.IndexByte(text, 0) >= 0 {
		return "", usage("message text must not contain NUL")
	}
	if !utf8.ValidString(text) {
		return "", usage("message text must be valid UTF-8")
	}
	return text, nil
}
func loadCommandConfig() (config.Config, error) {
	loc, err := config.DefaultLocation()
	if err != nil {
		return config.Config{}, err
	}
	return config.Load(loc)
}
func checkSlackTextFrame(c config.Config, path string, params any, cmd, requestID, version string) error {
	_ = path
	// Local text and file paths are resolved before the request is framed; check
	// the complete request rather than each individual input.
	if b, err := protocol.EncodeRequest(protocol.Request{ProtocolVersion: protocol.Version, RequestID: requestID, ClientKind: "cli", ClientVersion: version, Command: "slack." + cmd, Params: mustJSON(params)}); err != nil || len(b) > c.LocalProtocolMaximumFrame {
		return usage("request exceeds the protocol frame limit")
	}
	return nil
}
func (a App) renderSlackCommand(v any, jsonOut, explicitRead bool) error {
	if jsonOut {
		return a.render(v, true)
	}
	// Both reads and mutations use the same presentation-only renderer. Mutation
	// envelopes retain their request ID while effect bodies remain absent.
	_ = explicitRead
	return a.render(v, false)
}
func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }
func contains(xs []string, x string) bool {
	for _, y := range xs {
		if x == y {
			return true
		}
	}
	return false
}

func absolutePaths(paths []string) ([]string, error) {
	out := make([]string, len(paths))
	for i, value := range paths {
		path, err := filepath.Abs(value)
		if err != nil {
			return nil, usage("invalid file path")
		}
		if len(path) == 0 || len(path) > 4096 || strings.IndexByte(path, 0) >= 0 {
			return nil, usage("invalid file path")
		}
		out[i] = path
	}
	return out, nil
}

package slackapi

import (
	"errors"
	"sort"
	"strings"
)

const (
	ScopeAppMentionsRead  Scope = "app_mentions:read"
	ScopeChatWrite        Scope = "chat:write"
	ScopeReactionsRead    Scope = "reactions:read"
	ScopeReactionsWrite   Scope = "reactions:write"
	ScopeUsersRead        Scope = "users:read"
	ScopeTeamRead         Scope = "team:read"
	ScopeFilesRead        Scope = "files:read"
	ScopeFilesWrite       Scope = "files:write"
	ScopeChannelsRead     Scope = "channels:read"
	ScopeChannelsHistory  Scope = "channels:history"
	ScopeGroupsRead       Scope = "groups:read"
	ScopeGroupsHistory    Scope = "groups:history"
	ScopeConnectionsWrite Scope = "connections:write"
)

// Valid reports whether s has bounded Slack OAuth scope syntax. Unknown but
// syntactically valid scopes are retained so verification does not silently
// broaden authority.
func (s Scope) Valid() bool {
	if len(s) < 3 || len(s) > 100 {
		return false
	}
	colon := strings.IndexByte(string(s), ':')
	if colon == 0 || colon == len(s)-1 || colon >= 0 && strings.LastIndexByte(string(s), ':') != colon {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '-', r == ':':
		default:
			return false
		}
	}
	return true
}

// RequiredBotScopes returns the exact bot-scope set. Mosscap admits work from
// every public and private channel the bot is a member of, so both channel
// families are required. Socket Mode's app-level connections:write scope is
// verified separately because it belongs to a different token.
func RequiredBotScopes() []Scope {
	return []Scope{
		ScopeAppMentionsRead,
		ScopeChatWrite,
		ScopeReactionsRead,
		ScopeReactionsWrite,
		ScopeUsersRead,
		ScopeTeamRead,
		ScopeFilesRead,
		ScopeFilesWrite,
		ScopeChannelsRead,
		ScopeChannelsHistory,
		ScopeGroupsRead,
		ScopeGroupsHistory,
	}
}

// MissingScopes validates the granted set and returns required scopes not
// present, sorted for deterministic diagnostics.
func MissingScopes(granted, required []Scope) ([]Scope, error) {
	available := make(map[Scope]struct{}, len(granted))
	for _, scope := range granted {
		if !scope.Valid() {
			return nil, errors.New("invalid Slack OAuth scope")
		}
		available[scope] = struct{}{}
	}
	missing := make([]Scope, 0)
	seen := make(map[Scope]struct{}, len(required))
	for _, scope := range required {
		if !scope.Valid() {
			return nil, errors.New("invalid required Slack OAuth scope")
		}
		if _, duplicate := seen[scope]; duplicate {
			continue
		}
		seen[scope] = struct{}{}
		if _, found := available[scope]; !found {
			missing = append(missing, scope)
		}
	}
	sort.Slice(missing, func(i, j int) bool { return missing[i] < missing[j] })
	return missing, nil
}

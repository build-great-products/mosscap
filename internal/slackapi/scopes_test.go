package slackapi

import (
	"reflect"
	"testing"
)

func TestRequiredAndMissingScopes(t *testing.T) {
	public := RequiredBotScopes()
	if len(public) != 12 {
		t.Fatalf("scope count=%d", len(public))
	}
	missing, err := MissingScopes(
		[]Scope{ScopeChatWrite, ScopeAppMentionsRead, ScopeChannelsRead, "commands"},
		[]Scope{ScopeAppMentionsRead, ScopeChannelsHistory, ScopeChatWrite},
	)
	if err != nil {
		t.Fatal(err)
	}
	if want := []Scope{ScopeChannelsHistory}; !reflect.DeepEqual(missing, want) {
		t.Fatalf("missing = %v, want %v", missing, want)
	}
	if _, err := MissingScopes([]Scope{"INVALID SCOPE"}, public); err == nil {
		t.Fatal("invalid granted scope succeeded")
	}
}

package slackapi

import (
	"reflect"
	"testing"
)

func TestRequiredAndMissingScopes(t *testing.T) {
	public := RequiredBotScopes(false)
	private := RequiredBotScopes(true)
	if len(public) != 9 || len(private) != 9 {
		t.Fatalf("scope counts public=%d private=%d", len(public), len(private))
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

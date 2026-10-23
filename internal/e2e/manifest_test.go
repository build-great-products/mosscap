package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stayradiated/mosscap/internal/slackapi"
)

// TestManifestGrantsEveryVerifiedScopeAndChannelEvent keeps manifest.json in
// step with gateway verification: every required bot scope, and message
// events for both public and private channels.
func TestManifestGrantsEveryVerifiedScopeAndChannelEvent(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(sourceRoot(t), "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		OAuth struct {
			Scopes struct {
				Bot []string `json:"bot"`
			} `json:"scopes"`
		} `json:"oauth_config"`
		Settings struct {
			Events struct {
				Bot []string `json:"bot_events"`
			} `json:"event_subscriptions"`
		} `json:"settings"`
	}
	if err := json.Unmarshal(b, &manifest); err != nil {
		t.Fatal(err)
	}
	granted := map[string]bool{}
	for _, scope := range manifest.OAuth.Scopes.Bot {
		granted[scope] = true
	}
	for _, scope := range slackapi.RequiredBotScopes() {
		if !granted[string(scope)] {
			t.Errorf("manifest lacks required scope %s", scope)
		}
	}
	events := map[string]bool{}
	for _, event := range manifest.Settings.Events.Bot {
		events[event] = true
	}
	for _, event := range []string{"app_mention", "message.channels", "message.groups"} {
		if !events[event] {
			t.Errorf("manifest lacks event %s", event)
		}
	}
}

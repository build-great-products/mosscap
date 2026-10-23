package config

import (
	"strings"
	"testing"
)

func TestValidateSlackProductionRequiresCredentialsAndValidAllowlist(t *testing.T) {
	loc := Location{Home: "/home/test", Path: "/home/test/.config/mosscap/config.toml", Paths: Paths{StateDir: "/home/test/.local/state/mosscap"}}
	base, err := Default(loc)
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Config){
		"missing": func(c *Config) {},
		"bad app": func(c *Config) { c.SlackAppToken = "secret-token" },
		"bad allowlist": func(c *Config) {
			c.SlackAppToken = "xapp-safe"
			c.SlackBotToken = "xoxb-safe"
			c.SlackChannelAllowlist = "C012ABC,D012DM"
		},
	} {
		t.Run(name, func(t *testing.T) {
			c := base
			mutate(&c)
			err := ValidateSlackProduction(c)
			if err == nil {
				t.Fatal("expected rejection")
			}
			if strings.Contains(err.Error(), "secret-token") {
				t.Fatal("secret leaked")
			}
		})
	}
	base.SlackAppToken = "xapp-safe"
	base.SlackBotToken = "xoxb-safe"
	// An empty allowlist admits every member channel.
	if err := ValidateSlackProduction(base); err != nil {
		t.Fatal(err)
	}
	base.SlackChannelAllowlist = "C012ABC, G012PRV"
	if err := ValidateSlackProduction(base); err != nil {
		t.Fatal(err)
	}
	if got := ChannelAllowlist(base); len(got) != 2 || got[0] != "C012ABC" || got[1] != "G012PRV" {
		t.Fatalf("allowlist=%v", got)
	}
}

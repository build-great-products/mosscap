package config

import (
	"strings"
	"testing"
)

func TestValidateSlackProductionRequiresCredentials(t *testing.T) {
	loc := Location{Home: "/home/test", Path: "/home/test/.config/mosscap/config.toml", Paths: Paths{StateDir: "/home/test/.local/state/mosscap"}}
	base, err := Default(loc)
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Config){
		"missing": func(c *Config) {},
		"bad app": func(c *Config) { c.SlackAppToken = "secret-token" },
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
	if err := ValidateSlackProduction(base); err != nil {
		t.Fatal(err)
	}
}

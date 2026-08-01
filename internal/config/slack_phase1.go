package config

import (
	"errors"

	"github.com/stayradiated/mosscap/internal/slackapi"
)

// ValidateSlackProduction validates the complete Slack authority required by a
// production gateway. It is deliberately separate from Validate: setup writes
// empty placeholders before an operator configures Slack.
func ValidateSlackProduction(c Config) error {
	if err := Validate(c); err != nil {
		return err
	}
	if !tokens["slack_app_token"].MatchString(c.SlackAppToken) || !tokens["slack_bot_token"].MatchString(c.SlackBotToken) {
		return errors.New("production Slack credentials are required and invalid credentials are rejected")
	}
	if slackapi.ValidateTeamID(c.SlackTeamID) != nil || slackapi.ValidateUserID(c.SlackBotUserID) != nil || slackapi.ValidateChannelID(c.SlackChannelID) != nil {
		return errors.New("production Slack team, bot user, and channel IDs are required and invalid IDs are rejected")
	}
	return nil
}

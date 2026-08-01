package slackapi

import "errors"

func ValidatePermalinkRequest(request PermalinkRequest) error {
	if err := ValidateChannelID(request.ChannelID); err != nil {
		return err
	}
	if !request.MessageTS.Valid() {
		return errors.New("invalid Slack message timestamp")
	}
	return nil
}

func ValidateHistoryRequest(request HistoryRequest) error {
	if err := ValidateChannelID(request.ChannelID); err != nil {
		return err
	}
	if err := ValidateReadLimit(request.Limit); err != nil {
		return err
	}
	return validateOpaque("Slack cursor", request.Cursor, MaxCursorBytes, false)
}

func ValidateRepliesRequest(request RepliesRequest) error {
	if err := ValidateChannelID(request.ChannelID); err != nil {
		return err
	}
	if !request.ThreadTS.Valid() {
		return errors.New("invalid Slack thread timestamp")
	}
	if err := ValidateReadLimit(request.Limit); err != nil {
		return err
	}
	return validateOpaque("Slack cursor", request.Cursor, MaxCursorBytes, false)
}

func ValidatePostMessageRequest(request PostMessageRequest) error {
	if err := ValidateChannelID(request.ChannelID); err != nil {
		return err
	}
	if request.ThreadTS != "" && !request.ThreadTS.Valid() {
		return errors.New("invalid Slack thread timestamp")
	}
	if err := validateTextRequired(request.Text); err != nil {
		return err
	}
	return validateOpaque("Slack client message ID", request.ClientMessageID, MaxIDBytes, true)
}

func ValidateUpdateMessageRequest(request UpdateMessageRequest) error {
	if err := ValidateChannelID(request.ChannelID); err != nil {
		return err
	}
	if !request.MessageTS.Valid() {
		return errors.New("invalid Slack message timestamp")
	}
	return validateTextRequired(request.Text)
}

func ValidateDeleteMessageRequest(request DeleteMessageRequest) error {
	if err := ValidateChannelID(request.ChannelID); err != nil {
		return err
	}
	if !request.MessageTS.Valid() {
		return errors.New("invalid Slack message timestamp")
	}
	return nil
}

func ValidateReactionRequest(request ReactionRequest) error {
	if err := ValidateChannelID(request.ChannelID); err != nil {
		return err
	}
	if !request.MessageTS.Valid() {
		return errors.New("invalid Slack message timestamp")
	}
	normalized, err := NormalizeEmoji(request.Emoji)
	if err != nil {
		return err
	}
	if normalized != request.Emoji {
		return errors.New("Slack emoji name is not normalized")
	}
	return nil
}

func validateTextRequired(value string) error {
	return validateUTF8("Slack text", value, MaxTextBytes, true)
}

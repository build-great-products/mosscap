package slackapi

import (
	"errors"
	"net/url"
	"strings"
	"unicode/utf8"
)

const (
	MaxIDBytes        = 128
	MaxSubtypeBytes   = 128
	MaxCursorBytes    = 1024
	MaxTextBytes      = 40_000
	MaxFileNameBytes  = 1024
	MaxMIMETypeBytes  = 255
	MaxPermalinkBytes = 4096
	MaxFiles          = 1000
	MaxReadLimit      = 100
	MaxEmojiBytes     = 100
)

// ValidateTeamID validates an immutable Slack workspace ID.
func ValidateTeamID(value string) error { return validatePrefixedID(value, "T") }

// ValidateChannelID validates a public/private Slack channel ID.
func ValidateChannelID(value string) error { return validatePrefixedID(value, "CG") }

// ValidateUserID validates a workspace or enterprise Slack user ID.
func ValidateUserID(value string) error { return validatePrefixedID(value, "UW") }

// ValidateBotID validates Slack's bot object ID (not its U-prefixed bot user).
func ValidateBotID(value string) error { return validatePrefixedID(value, "B") }

// ValidateFileID validates a Slack file ID.
func ValidateFileID(value string) error { return validatePrefixedID(value, "F") }

func validatePrefixedID(value, prefixes string) error {
	if len(value) < 2 || len(value) > MaxIDBytes || !strings.ContainsRune(prefixes, rune(value[0])) {
		return errors.New("invalid Slack ID")
	}
	for i := range value {
		if (value[i] < 'A' || value[i] > 'Z') && (value[i] < '0' || value[i] > '9') {
			return errors.New("invalid Slack ID")
		}
	}
	return nil
}

// ValidateReadLimit applies the adapter-wide bound for one API page.
func ValidateReadLimit(limit int) error {
	if limit < 1 || limit > MaxReadLimit {
		return errors.New("Slack read limit must be between 1 and 100")
	}
	return nil
}

// NormalizeEmoji accepts a bare reaction name or one pair of surrounding
// colons and returns Slack's bare API representation.
func NormalizeEmoji(value string) (string, error) {
	if strings.HasPrefix(value, ":") && strings.HasSuffix(value, ":") && len(value) >= 3 {
		value = value[1 : len(value)-1]
	}
	if value == "" || len(value) > MaxEmojiBytes || !utf8.ValidString(value) {
		return "", errors.New("invalid Slack emoji name")
	}
	for _, r := range value {
		if r <= 0x20 || r == 0x7f || r == ':' && !strings.Contains(value, "::skin-tone-") {
			return "", errors.New("invalid Slack emoji name")
		}
	}
	if strings.Contains(value, "::") && !validSkinToneSuffix(value) {
		return "", errors.New("invalid Slack emoji name")
	}
	return value, nil
}

func validSkinToneSuffix(value string) bool {
	index := strings.LastIndex(value, "::skin-tone-")
	return index > 0 && index+len("::skin-tone-")+1 == len(value) && value[len(value)-1] >= '2' && value[len(value)-1] <= '6'
}

// ValidateEnvelope enforces transport-level shape and allocation bounds. It
// does not make trust or admission decisions.
func ValidateEnvelope(envelope Envelope) error {
	if !envelope.Kind.Valid() {
		return errors.New("unsupported Slack event kind")
	}
	if err := validateOpaque("envelope ID", envelope.EnvelopeID, MaxIDBytes, true); err != nil {
		return err
	}
	if err := validateOpaque("event ID", envelope.EventID, MaxIDBytes, false); err != nil {
		return err
	}
	if err := ValidateTeamID(envelope.TeamID); err != nil {
		return err
	}
	if err := ValidateChannelID(envelope.ChannelID); err != nil {
		return err
	}
	if envelope.UserID != "" {
		if err := ValidateUserID(envelope.UserID); err != nil {
			return err
		}
	}
	if envelope.BotID != "" {
		if err := ValidateBotID(envelope.BotID); err != nil {
			return err
		}
	}
	if err := validateSubtype(envelope.Subtype); err != nil {
		return err
	}
	if !envelope.MessageTS.Valid() || !envelope.EventTS.Valid() || envelope.ThreadTS != "" && !envelope.ThreadTS.Valid() {
		return errors.New("invalid Slack envelope timestamp")
	}
	if err := validateText(envelope.Text); err != nil {
		return err
	}
	if len(envelope.Files) > MaxFiles {
		return errors.New("too many Slack files")
	}
	if envelope.Permalink != "" {
		if err := validatePermalink(envelope.Permalink); err != nil {
			return err
		}
	}
	for _, file := range envelope.Files {
		if err := ValidateFile(file); err != nil {
			return err
		}
	}
	// Check acknowledgement last so malformed payloads retain their specific,
	// bounded validation result instead of being disguised as transport errors.
	if !envelope.Ack.ready() {
		return ErrAcknowledgementUnavailable
	}
	return nil
}

func validateSubtype(value string) error {
	if len(value) > MaxSubtypeBytes {
		return errors.New("invalid Slack event subtype")
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9') && r != '_' {
			return errors.New("invalid Slack event subtype")
		}
	}
	return nil
}

// ValidateFile checks bounded metadata only; MIME labels and names remain
// untrusted values.
func ValidateFile(file File) error {
	if err := ValidateFileID(file.ID); err != nil {
		return err
	}
	if err := validateUTF8("Slack filename", file.Name, MaxFileNameBytes, true); err != nil {
		return err
	}
	if err := validateUTF8("Slack MIME type", file.MIMEType, MaxMIMETypeBytes, false); err != nil {
		return err
	}
	if file.SizeBytes != nil && *file.SizeBytes < 0 {
		return errors.New("Slack file size must not be negative")
	}
	if file.Permalink != "" {
		return validatePermalink(file.Permalink)
	}
	return nil
}

func validateText(value string) error {
	return validateUTF8("Slack text", value, MaxTextBytes, false)
}

func validateUTF8(name, value string, maximum int, required bool) error {
	if required && value == "" {
		return errors.New(name + " is required")
	}
	if len(value) > maximum || !utf8.ValidString(value) || strings.IndexByte(value, 0) >= 0 {
		return errors.New(name + " is invalid or exceeds its limit")
	}
	return nil
}

func validateOpaque(name, value string, maximum int, required bool) error {
	if required && value == "" {
		return errors.New(name + " is required")
	}
	if len(value) > maximum || strings.IndexFunc(value, func(r rune) bool { return r <= 0x20 || r == 0x7f }) >= 0 {
		return errors.New(name + " is invalid or exceeds its limit")
	}
	return nil
}

func validatePermalink(value string) error {
	if len(value) > MaxPermalinkBytes || !utf8.ValidString(value) {
		return errors.New("invalid Slack permalink")
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil {
		return errors.New("invalid Slack permalink")
	}
	return nil
}

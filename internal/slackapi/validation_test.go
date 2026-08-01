package slackapi

import (
	"context"
	"strings"
	"testing"
)

func TestSlackIDValidation(t *testing.T) {
	valid := []struct {
		value string
		check func(string) error
	}{{"T012ABC", ValidateTeamID}, {"C012ABC", ValidateChannelID}, {"G012ABC", ValidateChannelID}, {"U012ABC", ValidateUserID}, {"W012ABC", ValidateUserID}, {"B012ABC", ValidateBotID}, {"F012ABC", ValidateFileID}}
	for _, test := range valid {
		if err := test.check(test.value); err != nil {
			t.Errorf("valid ID %q: %v", test.value, err)
		}
	}
	for _, value := range []string{"", "U", "u012ABC", "U01-ABC", "T012ABC"} {
		if err := ValidateUserID(value); err == nil {
			t.Errorf("ValidateUserID(%q) succeeded", value)
		}
	}
}

func TestNormalizeEmoji(t *testing.T) {
	for input, want := range map[string]string{
		"thumbsup":              "thumbsup",
		":party_parrot:":        "party_parrot",
		"thumbsup::skin-tone-3": "thumbsup::skin-tone-3",
	} {
		got, err := NormalizeEmoji(input)
		if err != nil || got != want {
			t.Errorf("NormalizeEmoji(%q) = %q, %v; want %q", input, got, err, want)
		}
	}
	for _, input := range []string{"", ":", "two words", "bad:colon", "thumbsup::skin-tone-9"} {
		if _, err := NormalizeEmoji(input); err == nil {
			t.Errorf("NormalizeEmoji(%q) succeeded", input)
		}
	}
}

func TestValidateEnvelopeBounds(t *testing.T) {
	size := int64(42)
	envelope := Envelope{
		EnvelopeID: "env-1",
		EventID:    "Ev01abc",
		Kind:       EventAppMention,
		TeamID:     "T012ABC",
		ChannelID:  "C012ABC",
		UserID:     "U012ABC",
		MessageTS:  "1712345678.000001",
		EventTS:    "1712345678.000002",
		Text:       "<@U999BOT> hello",
		Permalink:  "https://example.slack.com/archives/C012ABC/p1712345678000001",
		Files:      []File{{ID: "F012ABC", Name: "notes.txt", MIMEType: "text/plain", SizeBytes: &size}},
		Ack:        NewAcknowledgement(func(context.Context) error { return nil }),
	}
	if err := ValidateEnvelope(envelope); err != nil {
		t.Fatalf("ValidateEnvelope: %v", err)
	}

	invalid := envelope
	invalid.Text = strings.Repeat("x", MaxTextBytes+1)
	if err := ValidateEnvelope(invalid); err == nil {
		t.Fatal("oversized envelope text succeeded")
	}
	invalid = envelope
	invalid.Subtype = strings.Repeat("a", MaxSubtypeBytes+1)
	if err := ValidateEnvelope(invalid); err == nil {
		t.Fatal("oversized envelope subtype succeeded")
	}
	invalid.Subtype = "message-changed\n"
	if err := ValidateEnvelope(invalid); err == nil {
		t.Fatal("invalid envelope subtype succeeded")
	}
	invalid = envelope
	invalid.Ack = nil
	if err := ValidateEnvelope(invalid); err == nil || !strings.Contains(err.Error(), "acknowledgement") {
		t.Fatalf("envelope without acknowledgement error = %v", err)
	}

	// Shape errors remain available to the safe malformed-event path even when
	// a broken transport omitted its callback.
	invalid.Kind = "unknown"
	if err := ValidateEnvelope(invalid); err == nil || !strings.Contains(err.Error(), "event kind") {
		t.Fatalf("malformed envelope error = %v", err)
	}
}

func TestValidateReadLimit(t *testing.T) {
	for _, limit := range []int{1, MaxReadLimit} {
		if err := ValidateReadLimit(limit); err != nil {
			t.Errorf("ValidateReadLimit(%d): %v", limit, err)
		}
	}
	for _, limit := range []int{0, MaxReadLimit + 1} {
		if err := ValidateReadLimit(limit); err == nil {
			t.Errorf("ValidateReadLimit(%d) succeeded", limit)
		}
	}
}

package slackapi

import "testing"

func TestRequestValidation(t *testing.T) {
	validTS := Timestamp("1712345678.000001")
	tests := []struct {
		name    string
		valid   func() error
		invalid func() error
	}{
		{"permalink", func() error {
			return ValidatePermalinkRequest(PermalinkRequest{ChannelID: "C012ABC", MessageTS: validTS})
		}, func() error {
			return ValidatePermalinkRequest(PermalinkRequest{ChannelID: "C012ABC", MessageTS: "bad"})
		}},
		{"history", func() error { return ValidateHistoryRequest(HistoryRequest{ChannelID: "C012ABC", Limit: 20}) }, func() error { return ValidateHistoryRequest(HistoryRequest{ChannelID: "C012ABC", Limit: 0}) }},
		{"replies", func() error {
			return ValidateRepliesRequest(RepliesRequest{ChannelID: "C012ABC", ThreadTS: validTS, Limit: 20})
		}, func() error {
			return ValidateRepliesRequest(RepliesRequest{ChannelID: "C012ABC", ThreadTS: validTS, Limit: 101})
		}},
		{"post", func() error {
			return ValidatePostMessageRequest(PostMessageRequest{ChannelID: "C012ABC", Text: "hello", ClientMessageID: "eff_123"})
		}, func() error {
			return ValidatePostMessageRequest(PostMessageRequest{ChannelID: "C012ABC", Text: "", ClientMessageID: "eff_123"})
		}},
		{"update", func() error {
			return ValidateUpdateMessageRequest(UpdateMessageRequest{ChannelID: "C012ABC", MessageTS: validTS, Text: "hello"})
		}, func() error {
			return ValidateUpdateMessageRequest(UpdateMessageRequest{ChannelID: "C012ABC", MessageTS: "bad", Text: "hello"})
		}},
		{"delete", func() error {
			return ValidateDeleteMessageRequest(DeleteMessageRequest{ChannelID: "C012ABC", MessageTS: validTS})
		}, func() error {
			return ValidateDeleteMessageRequest(DeleteMessageRequest{ChannelID: "D012ABC", MessageTS: validTS})
		}},
		{"reaction", func() error {
			return ValidateReactionRequest(ReactionRequest{ChannelID: "C012ABC", MessageTS: validTS, Emoji: "thumbsup"})
		}, func() error {
			return ValidateReactionRequest(ReactionRequest{ChannelID: "C012ABC", MessageTS: validTS, Emoji: ":thumbsup:"})
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.valid(); err != nil {
				t.Fatalf("valid request: %v", err)
			}
			if err := test.invalid(); err == nil {
				t.Fatal("invalid request succeeded")
			}
		})
	}
}

package slackapi

import "testing"

func TestForwardedFromAttachmentExtractsOnlySafeProvenance(t *testing.T) {
	got, ok := ForwardedFromAttachment("original request", "https://workspace.slack.com/archives/C012ABC/p1700000000000001", "")
	if !ok || got.Text != "original request" || got.ChannelID != "C012ABC" || got.MessageTS != "1700000000.000001" {
		t.Fatalf("ForwardedFromAttachment = %#v, %v", got, ok)
	}
	if _, ok := ForwardedFromAttachment("untrusted unfurl", "https://example.invalid/post", "1700000000.000001"); ok {
		t.Fatal("accepted an attachment whose provenance was not a Slack message")
	}
}

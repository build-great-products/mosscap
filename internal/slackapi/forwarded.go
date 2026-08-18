package slackapi

import (
	"net/url"
	"regexp"
	"strings"
)

var archiveMessagePath = regexp.MustCompile(`^/archives/([CGD][A-Z0-9]+)/p([0-9]{10})([0-9]{6})$`)

// ForwardedFromAttachment returns the safe subset of a forwarded-message
// attachment. A text attachment without Slack message provenance is not a
// forwarded message and is deliberately ignored.
func ForwardedFromAttachment(text, sourceURL, timestamp string) (ForwardedMessage, bool) {
	if text == "" || len(text) > MaxTextBytes || !validText(text) {
		return ForwardedMessage{}, false
	}
	out := ForwardedMessage{Text: text}
	if timestamp != "" {
		parsed, err := ParseTimestamp(timestamp)
		if err != nil {
			return ForwardedMessage{}, false
		}
		out.MessageTS = parsed
	}
	if sourceURL != "" {
		channel, message, ok := forwardedSource(sourceURL)
		if !ok {
			return ForwardedMessage{}, false
		}
		out.ChannelID, out.MessageTS = channel, message
	}
	if out.MessageTS == "" {
		return ForwardedMessage{}, false
	}
	return out, true
}

func forwardedSource(value string) (string, Timestamp, bool) {
	u, err := url.Parse(value)
	if err != nil || u.User != nil {
		return "", "", false
	}
	if u.Scheme == "https" && strings.HasSuffix(strings.ToLower(u.Hostname()), ".slack.com") {
		m := archiveMessagePath.FindStringSubmatch(u.Path)
		if len(m) == 4 && ValidateChannelID(m[1]) == nil {
			ts, err := ParseTimestamp(m[2] + "." + m[3])
			return m[1], ts, err == nil
		}
	}
	if u.Scheme == "slack" && u.Host == "channel" {
		channel, message := u.Query().Get("id"), u.Query().Get("message")
		if ValidateChannelID(channel) == nil {
			ts, err := ParseTimestamp(message)
			return channel, ts, err == nil
		}
	}
	return "", "", false
}

func validText(value string) bool { return !strings.ContainsRune(value, 0) }

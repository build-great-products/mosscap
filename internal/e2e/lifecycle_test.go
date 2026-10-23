package e2e

import (
	"testing"

	"github.com/stayradiated/mosscap/internal/slackapi"
	"github.com/stayradiated/mosscap/internal/slackapi/fake"
)

// TestSlackLifecycleReactionsFollowTaskState proves the reaction projection:
// hourglass when admitted, eyes once claimed with task working, and no marker
// after task reply --done.
func TestSlackLifecycleReactionsFollowTaskState(t *testing.T) {
	const channel, ts = "C012ABC", "1700000000.000001"
	ack, acks := fake.NewAckRecorder(nil)
	mention := slackapi.Envelope{EnvelopeID: "env-1", EventID: "Ev-1", Kind: slackapi.EventAppMention, TeamID: harnessTeam, ChannelID: channel, UserID: harnessUser, MessageTS: ts, EventTS: ts, Text: "<@U012BOT> ship it", Ack: ack}
	h := newSlackHarness(t, nil, []fake.ReceiveStep{{Envelope: mention}}, steps(
		verificationSteps(),
		reactionSteps(fake.AddReaction, channel, ts, "hourglass_flowing_sand"),
		reactionSteps(fake.AddReaction, channel, ts, "eyes"),
		reactionSteps(fake.RemoveReaction, channel, ts, "hourglass_flowing_sand"),
		[]fake.Step{
			{Operation: fake.Permalink, Result: "https://example.invalid/thread"},
			{Operation: fake.PostMessage, Result: slackapi.MessageResult{ChannelID: channel, MessageTS: "1700000005.000001"}},
		},
		reactionSteps(fake.RemoveReaction, channel, ts, "eyes"),
	))
	waitAcks(t, acks)
	// Admission prepares the queued marker before ack and executes it after.
	h.waitWebRemaining(8)
	ids := h.taskIDs()
	if len(ids) != 1 {
		t.Fatalf("tasks=%v", ids)
	}
	h.run("", "task", "working", ids[0], "--json")
	h.waitWebRemaining(4)
	h.run("Done.\n", "task", "reply", ids[0], "--stdin", "--done", "--json")
	h.waitWebDone()
	if n := h.count("SELECT count(*) FROM slack_effects WHERE state <> 'confirmed'"); n != 0 {
		t.Fatalf("unconfirmed lifecycle effects=%d", n)
	}
	if n := h.count("SELECT count(*) FROM tasks WHERE state='done'"); n != 1 {
		t.Fatalf("done tasks=%d", n)
	}
}

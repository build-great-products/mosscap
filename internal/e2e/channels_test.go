package e2e

import (
	"strings"
	"testing"

	"github.com/stayradiated/mosscap/internal/slackapi"
	"github.com/stayradiated/mosscap/internal/slackapi/fake"
)

const (
	mainChannel     = "C0BAQM6JEAJ"
	pitchChannel    = "C0CC6JQ4A2U"
	privateChannel  = "G0DEVPRIVATE"
	mainMentionTS   = "1700000000.000001"
	pitchMentionTS  = "1700000100.000001"
	privateRemarkTS = "1700000200.000001"
)

func mentionIn(id, channel, ts, text string) (slackapi.Envelope, *fake.AckRecorder) {
	ack, recorder := fake.NewAckRecorder(nil)
	return slackapi.Envelope{EnvelopeID: "env-" + id, EventID: "Ev-" + id, Kind: slackapi.EventAppMention, TeamID: harnessTeam, ChannelID: channel, UserID: harnessUser, MessageTS: slackapi.Timestamp(ts), EventTS: slackapi.Timestamp(ts), Text: text, Ack: ack}, recorder
}

// TestEveryMemberChannelAdmitsAndCarriesItsChannel: mentions in two channels
// both become tasks, their reactions and replies go to their own channel, and
// task watch names each channel.
func TestEveryMemberChannelAdmitsAndCarriesItsChannel(t *testing.T) {
	main, mainAck := mentionIn("main", mainChannel, mainMentionTS, "<@U012BOT> main work")
	pitch, pitchAck := mentionIn("pitch", pitchChannel, pitchMentionTS, "<@U012BOT> pitch work")
	h := newSlackHarness(t, nil, []fake.ReceiveStep{{Envelope: main}, {Envelope: pitch}}, steps(
		verificationSteps(),
		reactionSteps(fake.AddReaction, mainChannel, mainMentionTS, "hourglass_flowing_sand"),
		reactionSteps(fake.AddReaction, pitchChannel, pitchMentionTS, "hourglass_flowing_sand"),
		[]fake.Step{
			{Operation: fake.PostMessage, Result: slackapi.MessageResult{ChannelID: pitchChannel, MessageTS: "1700000101.000001"}},
			{Operation: fake.Permalink, Request: slackapi.PermalinkRequest{ChannelID: mainChannel, MessageTS: mainMentionTS}, Result: "https://example.invalid/main"},
			{Operation: fake.PostMessage, Result: slackapi.MessageResult{ChannelID: mainChannel, MessageTS: "1700000002.000001"}},
		},
	))
	waitAcks(t, mainAck, pitchAck)
	h.waitWebRemaining(3)
	ids := h.taskIDs()
	if len(ids) != 2 {
		t.Fatalf("tasks=%v", ids)
	}
	// --task selects the task's channel; --channel names one explicitly.
	h.run("", "slack", "send", "--task", ids[1], "--text", "on it", "--json")
	h.run("", "slack", "send", "--channel", mainChannel, "--thread", mainMentionTS, "--text", "threaded", "--json")
	h.waitWebDone()
	calls := h.web.Calls()
	if got := calls[len(calls)-3].Request.(slackapi.PostMessageRequest); got.ChannelID != pitchChannel {
		t.Fatalf("--task send went to %s", got.ChannelID)
	}
	if got := calls[len(calls)-1].Request.(slackapi.PostMessageRequest); got.ChannelID != mainChannel || got.ThreadTS != mainMentionTS {
		t.Fatalf("--channel send=%+v", got)
	}
	// Without --channel or --task, a Slack command needs a channel.
	if out, code := h.runExit("", "slack", "history", "--json"); code != 2 || !strings.Contains(string(out), "channel_required") {
		t.Fatalf("history without channel exit=%d out=%s", code, out)
	}
	if n := h.count("SELECT count(*) FROM slack_effects WHERE channel_id=? AND kind='send'", pitchChannel); n != 1 {
		t.Fatalf("pitch sends=%d", n)
	}
	watch := startWatchProcess(t, h.binary, h.env, "--since", "0")
	for i, want := range []string{"source=slack channel=" + mainChannel + " thread=" + mainMentionTS + " parent= text=<@U012BOT> main work", "source=slack channel=" + pitchChannel + " thread=" + pitchMentionTS + " parent= text=<@U012BOT> pitch work"} {
		if got := watch.next(t); !strings.Contains(got, "task="+ids[i]+" ") || !strings.HasSuffix(got, want) {
			t.Fatalf("watch line %d=%q want suffix %q", i, got, want)
		}
	}
	watch.stop(t)
}

// TestPrivateChannelMentionIsAdmittedAndChannelIsAlwaysNamed: a mention in a
// private member channel is admitted like any other, a lone task does not make
// its channel the default, and a DM target is refused before any intent.
func TestPrivateChannelMentionIsAdmittedAndChannelIsAlwaysNamed(t *testing.T) {
	private, privateAck := mentionIn("private", privateChannel, privateRemarkTS, "<@U012BOT> private work")
	h := newSlackHarness(t, nil, []fake.ReceiveStep{{Envelope: private}}, steps(
		verificationSteps(),
		reactionSteps(fake.AddReaction, privateChannel, privateRemarkTS, "hourglass_flowing_sand"),
		[]fake.Step{{Operation: fake.History, Request: slackapi.HistoryRequest{ChannelID: privateChannel, Limit: 50}}},
	))
	waitAcks(t, privateAck)
	h.waitWebRemaining(1)
	if ids := h.taskIDs(); len(ids) != 1 {
		t.Fatalf("tasks=%v", ids)
	}
	if out, code := h.runExit("", "slack", "history", "--json"); code != 2 || !strings.Contains(string(out), "channel_required") {
		t.Fatalf("history without channel exit=%d out=%s", code, out)
	}
	h.run("", "slack", "history", "--channel", privateChannel, "--json")
	h.waitWebDone()
	if out, code := h.runExit("", "slack", "send", "--channel", "D012DM", "--text", "nope", "--json"); code != 2 {
		t.Fatalf("send to a DM exit=%d out=%s", code, out)
	}
	if n := h.count("SELECT count(*) FROM slack_effects WHERE kind='send'"); n != 0 {
		t.Fatalf("a refused target recorded %d sends", n)
	}
}

// TestExternalSlackConnectMentionIsIgnored: a mention from a user whose event
// names another workspace is acknowledged without a task or a reaction, while
// a mention from any member of our workspace is admitted.
func TestExternalSlackConnectMentionIsIgnored(t *testing.T) {
	external, externalAck := mentionIn("external", mainChannel, "1700000300.000001", "<@U012BOT> do our work")
	external.UserID, external.UserTeamID = "U0EXTERNAL", "T0EXTERNAL"
	member, memberAck := mentionIn("member", mainChannel, mainMentionTS, "<@U012BOT> member work")
	member.UserID, member.UserTeamID = "U0ANYONE1", harnessTeam
	h := newSlackHarness(t, nil, []fake.ReceiveStep{{Envelope: external}, {Envelope: member}}, steps(
		verificationSteps(),
		reactionSteps(fake.AddReaction, mainChannel, mainMentionTS, "hourglass_flowing_sand"),
	))
	waitAcks(t, externalAck, memberAck)
	h.waitWebDone()
	if n := h.count("SELECT count(*) FROM slack_source_events WHERE admission_result='ignored_workspace' AND user_id='U0EXTERNAL'"); n != 1 {
		t.Fatalf("external mention results=%d", n)
	}
	if n := h.count("SELECT count(*) FROM slack_task_sources WHERE author_user_id='U0ANYONE1'"); n != 1 {
		t.Fatalf("member tasks=%d", n)
	}
	if ids := h.taskIDs(); len(ids) != 1 {
		t.Fatalf("tasks=%v", ids)
	}
}

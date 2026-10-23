package e2e

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stayradiated/mosscap/internal/slackapi"
	"github.com/stayradiated/mosscap/internal/slackapi/fake"
)

func threadMessage(id, kind, channel, ts, threadTS, user, text string) (slackapi.Envelope, *fake.AckRecorder) {
	ack, recorder := fake.NewAckRecorder(nil)
	return slackapi.Envelope{EnvelopeID: "env-" + id, EventID: "Ev-" + id, Kind: slackapi.EventKind(kind), TeamID: harnessTeam, ChannelID: channel, UserID: user, MessageTS: slackapi.Timestamp(ts), ThreadTS: slackapi.Timestamp(threadTS), EventTS: slackapi.Timestamp(ts), Text: text, Ack: ack}, recorder
}

// TestUnmentionedThreadFollowUpsReachTheWatcher drives the Slack shape the
// orchestrator sees: a mention, then plain replies in its thread. Trusted
// replies become linked follow-up tasks with their own reaction; untrusted
// chatter and the bot's own messages are ignored; edits update a follow-up;
// and a reply to a thread the bot started is a follow-up too.
func TestUnmentionedThreadFollowUpsReachTheWatcher(t *testing.T) {
	const (
		rootTS     = "1700000000.000001"
		perfectTS  = "1700000010.000001"
		chatterTS  = "1700000020.000001"
		botReplyTS = "1700000030.000001"
		botRootTS  = "1700000100.000001"
		demoTS     = "1700000110.000001"
	)
	root, rootAck := threadMessage("root", "app_mention", mainChannel, rootTS, "", harnessUser, "<@U012BOT> render the deck")
	perfect, perfectAck := threadMessage("perfect", "message", mainChannel, perfectTS, rootTS, harnessUser, "Perfect")
	chatter, chatterAck := threadMessage("chatter", "message", mainChannel, chatterTS, rootTS, "U87654321", "looks good to me")
	botReply, botReplyAck := threadMessage("bot-reply", "message", mainChannel, botReplyTS, rootTS, "", "Done.")
	botReply.BotID = "B012BOT"
	edit, editAck := threadMessage("perfect-edit", "message_changed", mainChannel, perfectTS, rootTS, harnessUser, "Perfect, ship it")
	edit.EventTS = "1700000040.000001"
	demo, demoAck := threadMessage("demo", "message", mainChannel, demoTS, botRootTS, harnessUser, "Can you just put commented against all of them")
	demoGate := fake.NewBarrier()
	h := newSlackHarness(t, nil, []fake.ReceiveStep{{Envelope: root}, {Envelope: perfect}, {Envelope: chatter}, {Envelope: botReply}, {Envelope: edit}, {Envelope: demo, Barrier: demoGate}}, steps(
		verificationSteps(),
		reactionSteps(fake.AddReaction, mainChannel, rootTS, "hourglass_flowing_sand"),
		reactionSteps(fake.AddReaction, mainChannel, perfectTS, "hourglass_flowing_sand"),
		[]fake.Step{
			{Operation: fake.Replies, Request: slackapi.RepliesRequest{ChannelID: mainChannel, ThreadTS: rootTS, Limit: 20}, Result: slackapi.MessagePage{Messages: []slackapi.Message{{MessageTS: rootTS, ThreadTS: rootTS, AuthorID: harnessUser, Text: "<@U012BOT> render the deck"}, {MessageTS: perfectTS, ThreadTS: rootTS, AuthorID: harnessUser, Text: "Perfect, ship it"}}}},
			{Operation: fake.PostMessage, Result: slackapi.MessageResult{ChannelID: mainChannel, MessageTS: botRootTS}},
		},
		reactionSteps(fake.AddReaction, mainChannel, demoTS, "hourglass_flowing_sand"),
	))
	waitAcks(t, rootAck, perfectAck, chatterAck, botReplyAck, editAck)
	h.waitWebRemaining(4)
	ids := h.taskIDs()
	if len(ids) != 2 {
		t.Fatalf("tasks=%v; want root and one follow-up", ids)
	}
	rootID, followID := ids[0], ids[1]
	if n := h.count("SELECT count(*) FROM slack_task_sources WHERE task_id=? AND parent_task_id=? AND thread_ts=?", followID, rootID, rootTS); n != 1 {
		t.Fatalf("follow-up is not linked to its thread and parent")
	}
	if n := h.count("SELECT count(*) FROM slack_source_events WHERE admission_result='ignored_unmentioned' AND user_id='U87654321'"); n != 1 {
		t.Fatalf("untrusted chatter results=%d", n)
	}
	if n := h.count("SELECT count(*) FROM slack_source_events WHERE admission_result='ignored_bot'"); n != 1 {
		t.Fatalf("bot reply results=%d", n)
	}
	if n := h.count("SELECT count(*) FROM slack_effects WHERE emoji='no_entry'"); n != 0 {
		t.Fatalf("untrusted chatter earned %d no_entry reactions", n)
	}

	// task context on the follow-up reads the whole thread.
	var context struct {
		Task struct {
			Version     int `json:"version"`
			SlackSource struct {
				ThreadTS     string `json:"thread_ts"`
				ParentTaskID string `json:"parent_task_id"`
			} `json:"slack_source"`
		} `json:"task"`
		Thread struct {
			Status   string `json:"status"`
			Messages []any  `json:"messages"`
		} `json:"thread"`
	}
	if out := h.run("", "task", "context", followID, "--json"); json.Unmarshal(out, &context) != nil || context.Thread.Status != "available" || len(context.Thread.Messages) != 2 || context.Task.Version != 2 || context.Task.SlackSource.ParentTaskID != rootID {
		t.Fatalf("context=%s", out)
	}

	// The bot starts a new thread; a plain reply to it is a follow-up too.
	h.run("", "slack", "send", "--channel", mainChannel, "--text", "Here is the demo", "--json")
	demoGate.Release()
	waitAcks(t, demoAck)
	h.waitWebDone()
	ids = h.taskIDs()
	if len(ids) != 3 {
		t.Fatalf("tasks after bot-thread reply=%v", ids)
	}

	watch := startWatchProcess(t, h.binary, h.env, "--since", "0")
	want := []string{
		"task=" + rootID + " version=1 state=open source=slack channel=" + mainChannel + " thread=" + rootTS + " parent= text=<@U012BOT> render the deck",
		"task=" + followID + " version=2 state=open source=slack channel=" + mainChannel + " thread=" + rootTS + " parent=" + rootID + " text=Perfect, ship it",
		"task=" + ids[2] + " version=1 state=open source=slack channel=" + mainChannel + " thread=" + botRootTS + " parent= text=Can you just put commented against all of them",
	}
	for i := range want {
		if got := watch.next(t); !strings.HasSuffix(got, want[i]) {
			t.Fatalf("watch line %d=%q\nwant suffix %q", i, got, want[i])
		}
	}
	watch.stop(t)
}

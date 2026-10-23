package store

import (
	"context"
	"testing"
)

const (
	followupRootTS  = "1700000000.000001"
	followupBotRoot = "1700000500.000001"
)

// followupEvent is an unmentioned reply in the root task's thread.
func followupEvent(id, messageTS, threadTS, userID, text string, order int64) SlackSourceEvent {
	event := sourceTestEvent(id, SlackEventMessage, order)
	event.MessageTS, event.ThreadTS, event.UserID, event.Text = messageTS, threadTS, userID, text
	event.Permalink = ""
	return event
}

func commitFollowupEvent(t *testing.T, s *Store, event SlackSourceEvent) SlackEventResult {
	t.Helper()
	result, err := s.CommitSlackSourceEvent(context.Background(), sourceTestPolicy, event)
	if err != nil {
		t.Fatalf("%s: %v", event.SourceEventID, err)
	}
	return result
}

func TestUnmentionedThreadRepliesBecomeLinkedFollowUps(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	root := sourceTestEvent("Ev-root", SlackEventAppMention, 100)
	root.MessageTS, root.ThreadTS = followupRootTS, followupRootTS
	rootResult := commitFollowupEvent(t, s, root)
	if rootResult.AdmissionResult != SlackAdmitted {
		t.Fatalf("root=%+v", rootResult)
	}

	// An unmentioned reply in the thread is admitted as its own task.
	reply := commitFollowupEvent(t, s, followupEvent("Ev-perfect", "1700000001.000001", followupRootTS, "U12345678", "Perfect", 101))
	if reply.AdmissionResult != SlackAdmitted || reply.TaskID == rootResult.TaskID {
		t.Fatalf("follow-up=%+v", reply)
	}
	source, err := s.GetSlackSource(ctx, reply.TaskID)
	if err != nil || source.ParentTaskID != rootResult.TaskID || source.ThreadTS != followupRootTS || source.MessageTS != "1700000001.000001" || source.TaskText != "Perfect" {
		t.Fatalf("follow-up source=%+v err=%v", source, err)
	}
	// Later follow-ups link to the earliest task, not the previous follow-up.
	second := commitFollowupEvent(t, s, followupEvent("Ev-sync", "1700000002.000001", followupRootTS, "U12345678", "it's out of sync", 102))
	if source, err := s.GetSlackSource(ctx, second.TaskID); err != nil || source.ParentTaskID != rootResult.TaskID {
		t.Fatalf("second follow-up=%+v err=%v", source, err)
	}
	// Redelivery reuses the canonical result.
	if again := commitFollowupEvent(t, s, followupEvent("Ev-perfect", "1700000001.000001", followupRootTS, "U12345678", "Perfect", 101)); !again.DuplicateDelivery || again.TaskID != reply.TaskID {
		t.Fatalf("redelivery=%+v", again)
	}
	// Edits to a follow-up behave like edits to any admitted source.
	edit := followupEvent("Ev-perfect-edit", "1700000001.000001", followupRootTS, "U12345678", "Perfect, thanks", 110)
	edit.EventKind = SlackEventMessageChange
	if edited := commitFollowupEvent(t, s, edit); edited.AdmissionResult != SlackUpdated || edited.TaskVersion != 2 || edited.TaskID != reply.TaskID {
		t.Fatalf("edit=%+v", edited)
	}
	// The root task itself has no parent.
	if source, err := s.GetSlackSource(ctx, rootResult.TaskID); err != nil || source.ParentTaskID != "" {
		t.Fatalf("root source=%+v err=%v", source, err)
	}
}

func TestThreadRepliesThatAreNotFollowUps(t *testing.T) {
	s := testStore(t)
	root := sourceTestEvent("Ev-root", SlackEventAppMention, 100)
	root.MessageTS, root.ThreadTS = followupRootTS, followupRootTS
	commitFollowupEvent(t, s, root)

	untracked := followupEvent("Ev-untracked", "1700000003.000001", "1699999000.000001", "U12345678", "unrelated thread", 103)
	topLevel := followupEvent("Ev-top", "1700000004.000001", "1700000004.000001", "U12345678", "channel chatter", 104)
	bot := followupEvent("Ev-bot", "1700000005.000001", followupRootTS, "", "", 105)
	bot.BotID, bot.Text = "B12345678", "bot reply"
	self := followupEvent("Ev-self", "1700000006.000001", followupRootTS, "U99999999", "my own reply", 106)
	lateEdit := followupEvent("Ev-late-edit", "1699999999.000002", followupRootTS, "U12345678", "edited old reply", 107)
	lateEdit.EventKind = SlackEventMessageChange
	for _, tc := range []struct {
		event SlackSourceEvent
		want  SlackAdmissionResult
	}{
		{untracked, SlackIgnoredUnmentioned},
		{topLevel, SlackIgnoredUnmentioned},
		{bot, SlackIgnoredBot},
		{self, SlackIgnoredBot},
		// An edit never turns an unadmitted reply into a follow-up.
		{lateEdit, SlackIgnoredUnmentioned},
	} {
		if got := commitFollowupEvent(t, s, tc.event); got.AdmissionResult != tc.want || got.TaskID != "" {
			t.Errorf("%s result=%+v want %s", tc.event.SourceEventID, got, tc.want)
		}
	}
	var tasks int
	if err := s.DB().QueryRow("SELECT count(*) FROM tasks").Scan(&tasks); err != nil || tasks != 1 {
		t.Fatalf("tasks=%d err=%v", tasks, err)
	}
}

func TestRepliesInBotStartedThreadsAndAttachmentOnlyFollowUps(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	local, err := s.Create(ctx, "followup-local", "post the demo")
	if err != nil {
		t.Fatal(err)
	}
	// The bot posts a top-level message for a local task; replies to it are
	// follow-ups linked to that task.
	post := prepareEffect(t, s, "followup-bot-post", SlackEffectIntent{Kind: SlackEffectSend, Text: "Here is the demo", RelatedTaskID: local.TaskID})
	if _, err := s.ClaimSlackEffect(ctx, post.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CommitSlackEffectOutcome(ctx, post.ID, SlackEffectOutcome{State: SlackEffectConfirmed, ResponseMessageTS: followupBotRoot}); err != nil {
		t.Fatal(err)
	}
	reply := commitFollowupEvent(t, s, followupEvent("Ev-bot-thread", "1700000501.000001", followupBotRoot, "U12345678", "Can you just put commented against all of them", 100))
	source, err := s.GetSlackSource(ctx, reply.TaskID)
	if reply.AdmissionResult != SlackAdmitted || err != nil || source.ParentTaskID != local.TaskID {
		t.Fatalf("bot-thread reply=%+v source=%+v err=%v", reply, source, err)
	}
	// A file shared into the thread without text is still a follow-up.
	shared := followupEvent("Ev-file-share", "1700000502.000001", followupBotRoot, "U12345678", "", 101)
	shared.Subtype = "file_share"
	shared.Files = []SlackIncomingFile{{SlackFileID: "F12345678", OriginalName: "screenshot.png", DeclaredMIME: "image/png"}}
	sharedResult := commitFollowupEvent(t, s, shared)
	if sharedResult.AdmissionResult != SlackAdmitted {
		t.Fatalf("file share=%+v", sharedResult)
	}
	model, err := s.GetTaskReadModel(ctx, sharedResult.TaskID)
	if err != nil || model.Task.Text != attachmentOnlyText || len(model.Files) != 1 || model.SlackSource.ParentTaskID != local.TaskID {
		t.Fatalf("file share model=%+v err=%v", model, err)
	}
	// An unconfirmed bot post does not track its thread.
	pending := prepareEffect(t, s, "followup-bot-pending", SlackEffectIntent{Kind: SlackEffectSend, Text: "maybe", ThreadTS: "1700000900.000001"})
	_ = pending
	if got := commitFollowupEvent(t, s, followupEvent("Ev-pending-thread", "1700000901.000001", "1700000900.000001", "U12345678", "hello?", 102)); got.AdmissionResult != SlackIgnoredUnmentioned {
		t.Fatalf("pending-thread reply=%+v", got)
	}
}

// Every human member of the workspace may send work: a user nobody configured
// can mention the bot in a thread, and a different person can follow up.
func TestFollowUpsAndMentionsFromAnyWorkspaceMember(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	root := sourceTestEvent("Ev-anyone-root", SlackEventAppMention, 100)
	root.MessageTS, root.ThreadTS, root.UserID = followupRootTS, followupRootTS, "U87654321"
	rootResult := commitFollowupEvent(t, s, root)
	if rootResult.AdmissionResult != SlackAdmitted {
		t.Fatalf("mention from any member=%+v", rootResult)
	}
	reply := commitFollowupEvent(t, s, followupEvent("Ev-anyone-reply", "1700000001.000001", followupRootTS, "U55555555", "thanks, also check staging", 101))
	if reply.AdmissionResult != SlackAdmitted {
		t.Fatalf("follow-up from another member=%+v", reply)
	}
	source, err := s.GetSlackSource(ctx, reply.TaskID)
	if err != nil || source.ParentTaskID != rootResult.TaskID || source.AuthorUserID != "U55555555" {
		t.Fatalf("follow-up source=%+v err=%v", source, err)
	}
}

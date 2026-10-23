package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strconv"

	"github.com/stayradiated/mosscap/internal/slackops"
	"github.com/stayradiated/mosscap/internal/store"
	"github.com/stayradiated/mosscap/internal/task"
)

const (
	queuedTaskReaction  = "hourglass_flowing_sand"
	workingTaskReaction = "eyes"
)

// lifecycleReaction is the Slack marker that projects one task state onto the
// source message: queued while open, eyes while working, none once done. It is
// a projection only; SQLite task state stays authoritative.
func lifecycleReaction(state task.State) string {
	switch state {
	case task.Open:
		return queuedTaskReaction
	case task.Working:
		return workingTaskReaction
	default:
		return ""
	}
}

// prepareTaskReaction records a fixed lifecycle action under a stable request
// ID. The SQLite task mutation and the Slack effect intentionally are separate
// transactions: Slack cannot participate in either transaction.
func (s *Server) prepareTaskReaction(ctx context.Context, source store.SlackSource, version int64, stage string, kind store.SlackEffectKind, emoji string) (store.SlackEffect, error) {
	return s.prepareTaskReactionIn(ctx, s.store, source, version, stage, kind, emoji)
}

// prepareTaskReactionIn deliberately accepts the ledger that admitted the
// source event. During a config reload the candidate coordinator must not
// prepare an acknowledgement effect in the old store.
func (s *Server) prepareTaskReactionIn(ctx context.Context, ledger *store.Store, source store.SlackSource, version int64, stage string, kind store.SlackEffectKind, emoji string) (store.SlackEffect, error) {
	requestID := taskReactionRequestID(stage, source.TaskID, version)
	intent := store.SlackEffectIntent{Kind: kind, TargetMessageTS: source.MessageTS, Emoji: emoji, RelatedTaskID: source.TaskID}
	prepared, err := ledger.PrepareSlackEffect(ctx, requestID, store.SlackEffectTarget{TeamID: source.TeamID, ChannelID: source.ChannelID}, intent)
	if err != nil {
		return store.SlackEffect{}, err
	}
	return ledger.GetSlackEffect(ctx, prepared.EffectID)
}

// executeTaskReaction executes only the named prepared action. A confirmed,
// failed, or ambiguous action is returned as-is; it is never automatically
// retried. This gives duplicated envelopes and reconnects one durable intent,
// while preserving Slack's unavoidable uncertain-outcome semantics.
func (s *Server) executeTaskReaction(ctx context.Context, source store.SlackSource, version int64, stage string, kind store.SlackEffectKind, emoji string) (store.SlackEffect, error) {
	effect, err := s.prepareTaskReaction(ctx, source, version, stage, kind, emoji)
	if err != nil {
		return effect, err
	}
	if effect.State != store.SlackEffectPrepared {
		switch effect.State {
		case store.SlackEffectConfirmed:
			return effect, nil
		case store.SlackEffectFailed, store.SlackEffectAmbiguous:
			return effect, &slackops.FailureError{State: effect.State, Diagnostic: effect.DiagnosticCode}
		default:
			// In particular, never let a concurrent submitted add unblock the
			// remove half of a replacement before Slack confirms the add.
			return effect, slackops.ErrUnavailable
		}
	}
	op, release, err := s.ops()
	if err != nil {
		return effect, err
	}
	defer release()
	if !op.MatchesTarget(source.TeamID, source.ChannelID) {
		return effect, slackops.ErrUnavailable
	}
	return op.Execute(ctx, taskReactionRequestID(stage, source.TaskID, version), store.SlackEffectIntent{Kind: kind, TargetMessageTS: source.MessageTS, Emoji: emoji, RelatedTaskID: source.TaskID})
}

func taskReactionRequestID(stage, taskID string, version int64) string {
	sum := sha256.Sum256([]byte("mosscap.task-reaction.v1\x00" + stage + "\x00" + taskID + "\x00" + strconv.FormatInt(version, 10)))
	return "internal-task-reaction-" + hex.EncodeToString(sum[:])
}

// queuedStage is the stage of the admission marker. Admission always creates
// version 1, so the request ID is stable across envelope redelivery.
const queuedStage = "queued"

// queueTaskReaction executes the queued marker that admission prepared before
// acknowledging the Slack envelope.
func (s *Server) queueTaskReaction(ctx context.Context, taskID string) error {
	source, err := s.store.GetSlackSource(ctx, taskID)
	if err != nil || source.TaskState != task.Open {
		return err
	}
	_, err = s.executeTaskReaction(ctx, source, 1, queuedStage, store.SlackEffectReact, queuedTaskReaction)
	return err
}

// transitionStages names the two lifecycle effects of one committed state
// transition. Each transition has its own state event ID, so a later reopen
// gets new intents instead of reusing a completed one.
func transitionStages(event store.StateEvent) (add, remove string) {
	return "enter-" + event.ID, "leave-" + event.ID
}

// projectTransition replaces the lifecycle marker after a committed state
// change. The new marker is added first so a rejected or ambiguous removal
// still leaves a visible signal; the replacement is not atomic. Failures are
// logged and remain inspectable as durable effects.
func (s *Server) projectTransition(ctx context.Context, mutation store.MutationResult) {
	if !mutation.Changed {
		return
	}
	if err := s.projectLatestTransition(ctx, mutation.TaskID); err != nil && s.Log != nil {
		s.Log.Warn("automatic lifecycle reaction did not confirm", "task_id", mutation.TaskID, "task_state", mutation.State, "error", err)
	}
}

func (s *Server) projectLatestTransition(ctx context.Context, taskID string) error {
	source, event, ok, err := s.currentTransition(ctx, taskID)
	if err != nil || !ok {
		return err
	}
	addStage, removeStage := transitionStages(event)
	if emoji := lifecycleReaction(event.NewState); emoji != "" {
		if _, err := s.executeTaskReaction(ctx, source, 1, addStage, store.SlackEffectReact, emoji); err != nil {
			return err
		}
	}
	if emoji := lifecycleReaction(event.PriorState); emoji != "" {
		_, err = s.executeTaskReaction(ctx, source, 1, removeStage, store.SlackEffectUnreact, emoji)
	}
	return err
}

// currentTransition returns the latest state event of a Slack task only while
// the task is still in that event's state. A projection that races a newer
// transition is skipped; the newer transition projects the final state.
func (s *Server) currentTransition(ctx context.Context, taskID string) (store.SlackSource, store.StateEvent, bool, error) {
	row, err := s.store.Get(ctx, taskID)
	if err != nil || row.SourceKind != "slack" {
		return store.SlackSource{}, store.StateEvent{}, false, err
	}
	source, err := s.store.GetSlackSource(ctx, taskID)
	if err != nil {
		return store.SlackSource{}, store.StateEvent{}, false, err
	}
	event, found, err := s.store.LatestStateEvent(ctx, taskID, nil, nil)
	if err != nil || !found || event.NewState != source.TaskState {
		return source, event, false, err
	}
	return source, event, true, nil
}

// reconcileTaskReactions closes the crash window between a durable local
// transition and preparation of its Slack projection. It only prepares effects;
// the normal startup effect recovery performs their single submissions. Done
// tasks are skipped: their only projection is a removal, and replaying it for
// every historical task on upgrade would be noise rather than repair.
func (s *Server) reconcileTaskReactions(ctx context.Context) error {
	for cursor := ""; ; {
		tasks, next, err := s.store.List(ctx, nil, false, 100, cursor)
		if err != nil {
			return err
		}
		for _, row := range tasks {
			if row.SourceKind != "slack" {
				continue
			}
			if err := s.reconcileTaskReaction(ctx, row.ID); err != nil {
				return err
			}
		}
		if next == "" {
			return nil
		}
		cursor = next
	}
}

func (s *Server) reconcileTaskReaction(ctx context.Context, taskID string) error {
	source, err := s.store.GetSlackSource(ctx, taskID)
	if err != nil {
		return err
	}
	event, found, err := s.store.LatestStateEvent(ctx, taskID, nil, nil)
	if err != nil {
		return err
	}
	if !found {
		if source.TaskState == task.Open {
			_, err = s.prepareTaskReaction(ctx, source, 1, queuedStage, store.SlackEffectReact, queuedTaskReaction)
		}
		return err
	}
	if event.NewState != source.TaskState {
		return nil
	}
	addStage, removeStage := transitionStages(event)
	if emoji := lifecycleReaction(event.NewState); emoji != "" {
		// Prepare the remove half only after the add is confirmed. Startup
		// invokes reconciliation again after recovery to converge safely.
		add, err := s.prepareTaskReaction(ctx, source, 1, addStage, store.SlackEffectReact, emoji)
		if err != nil || add.State != store.SlackEffectConfirmed {
			return err
		}
	}
	if emoji := lifecycleReaction(event.PriorState); emoji != "" {
		_, err = s.prepareTaskReaction(ctx, source, 1, removeStage, store.SlackEffectUnreact, emoji)
	}
	return err
}

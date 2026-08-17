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
	queuedTaskReaction    = "hourglass_flowing_sand"
	deliveredTaskReaction = "eyes"
)

// prepareTaskReaction records a fixed lifecycle action under a stable request
// ID. The SQLite task/delivery mutation and the Slack effect intentionally are
// separate transactions: Slack cannot participate in either transaction.
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

func (s *Server) queueTaskReaction(ctx context.Context, taskID string, version int64) error {
	source, ok, err := s.activeSlackTaskSource(ctx, taskID, version)
	if err != nil || !ok {
		return err
	}
	// A new version replaces a previously delivered marker by adding the new
	// queued marker first. Initial/no-op admissions only need this first action.
	stage, err := s.activeMarkerStage(ctx, taskID, "queued")
	if err != nil {
		return err
	}
	if _, err = s.executeTaskReaction(ctx, source, version, stage, store.SlackEffectReact, queuedTaskReaction); err != nil {
		return err
	}
	if version > 1 {
		prior, err := s.hasPersistedVersion(ctx, taskID, version-1)
		if err != nil {
			return err
		}
		if prior {
			_, err = s.executeTaskReaction(ctx, source, version, "updated-clear-delivered", store.SlackEffectUnreact, deliveredTaskReaction)
		}
	}
	return err
}

func (s *Server) deliveredTaskReactions(ctx context.Context, taskID string, version int64) error {
	// A persisted delivery can arrive after a newer source update or completion.
	// Only the current, open Slack task version may change the projection.
	source, ok, err := s.activeSlackTaskSource(ctx, taskID, version)
	if err != nil || !ok {
		return err
	}
	stage, err := s.activeMarkerStage(ctx, taskID, "delivered")
	if err != nil {
		return err
	}
	// This is deliberately two ordered effects, not an atomic replacement.
	// Adding the new marker first retains a visible lifecycle signal if removal
	// is rejected or becomes ambiguous.
	if _, err = s.executeTaskReaction(ctx, source, version, stage, store.SlackEffectReact, deliveredTaskReaction); err != nil {
		return err
	}
	_, err = s.executeTaskReaction(ctx, source, version, "delivered-unqueue", store.SlackEffectUnreact, queuedTaskReaction)
	return err
}

func (s *Server) completedTaskReaction(ctx context.Context, taskID string, version int64) error {
	source, ok, err := s.doneSlackTaskSource(ctx, taskID, version)
	if err != nil || !ok {
		return err
	}
	persisted, err := s.hasPersistedVersion(ctx, taskID, version)
	if err != nil {
		return err
	}
	emoji, baseStage := queuedTaskReaction, "completed-unqueue"
	if persisted {
		emoji, baseStage = deliveredTaskReaction, "completed"
	}
	stage, err := s.completedMarkerStage(ctx, taskID, baseStage)
	if err != nil {
		return err
	}
	_, err = s.executeTaskReaction(ctx, source, version, stage, store.SlackEffectUnreact, emoji)
	return err
}

func (s *Server) hasPersistedVersion(ctx context.Context, taskID string, version int64) (bool, error) {
	return s.store.HasPersistedDelivery(ctx, taskID, version)
}

func (s *Server) activeSlackTaskSource(ctx context.Context, taskID string, version int64) (store.SlackSource, bool, error) {
	row, err := s.store.Get(ctx, taskID)
	if err != nil {
		return store.SlackSource{}, false, err
	}
	if row.SourceKind != "slack" || row.Version != version || row.State == "done" {
		return store.SlackSource{}, false, nil
	}
	source, err := s.store.GetSlackSource(ctx, taskID)
	return source, err == nil, err
}

func (s *Server) doneSlackTaskSource(ctx context.Context, taskID string, version int64) (store.SlackSource, bool, error) {
	row, err := s.store.Get(ctx, taskID)
	if err != nil {
		return store.SlackSource{}, false, err
	}
	// A completion callback that races with a reopen must not remove the
	// freshly restored marker.
	if row.SourceKind != "slack" || row.Version != version || row.State != task.Done {
		return store.SlackSource{}, false, nil
	}
	source, err := s.store.GetSlackSource(ctx, taskID)
	return source, err == nil, err
}

func (s *Server) activeMarkerStage(ctx context.Context, taskID, base string) (string, error) {
	return activeMarkerStageIn(ctx, s.store, taskID, base)
}

func activeMarkerStageIn(ctx context.Context, ledger *store.Store, taskID, base string) (string, error) {
	done := task.Done
	event, found, err := ledger.LatestStateEvent(ctx, taskID, &done, nil)
	if err != nil || !found {
		return base, err
	}
	return "reopened-" + base + "-" + event.ID, nil
}

func (s *Server) completedMarkerStage(ctx context.Context, taskID, base string) (string, error) {
	done := task.Done
	event, found, err := s.store.LatestStateEvent(ctx, taskID, nil, &done)
	if err != nil || !found {
		return base, err
	}
	return base + "-" + event.ID, nil
}

func (s *Server) restoreTaskReaction(ctx context.Context, taskID string, version int64) error {
	source, ok, err := s.activeSlackTaskSource(ctx, taskID, version)
	if err != nil || !ok {
		return err
	}
	persisted, err := s.hasPersistedVersion(ctx, taskID, version)
	if err != nil {
		return err
	}
	if persisted {
		stage, stageErr := s.activeMarkerStage(ctx, taskID, "delivered")
		if stageErr != nil {
			return stageErr
		}
		_, err = s.executeTaskReaction(ctx, source, version, stage, store.SlackEffectReact, deliveredTaskReaction)
	} else {
		stage, stageErr := s.activeMarkerStage(ctx, taskID, "queued")
		if stageErr != nil {
			return stageErr
		}
		_, err = s.executeTaskReaction(ctx, source, version, stage, store.SlackEffectReact, queuedTaskReaction)
	}
	return err
}

// reconcileTaskReactions closes the crash window between a durable local
// transition and preparation of its Slack projection. It only prepares effects;
// the normal startup effect recovery performs their single submissions.
func (s *Server) reconcileTaskReactions(ctx context.Context) error {
	// Page tasks to completion. Persisted evidence is queried by exact task
	// version, rather than inferred from the bounded operator delivery listing.
	for cursor := ""; ; {
		tasks, next, err := s.store.List(ctx, nil, true, 1000, cursor)
		if err != nil {
			return err
		}
		for _, row := range tasks {
			if row.SourceKind != "slack" {
				continue
			}
			source, err := s.store.GetSlackSource(ctx, row.ID)
			if err != nil {
				return err
			}
			persisted, err := s.hasPersistedVersion(ctx, row.ID, row.Version)
			if err != nil {
				return err
			}
			if row.State == "done" {
				emoji, baseStage := queuedTaskReaction, "completed-unqueue"
				if persisted {
					emoji, baseStage = deliveredTaskReaction, "completed"
				}
				stage, stageErr := s.completedMarkerStage(ctx, row.ID, baseStage)
				if stageErr != nil {
					return stageErr
				}
				if _, err = s.prepareTaskReaction(ctx, source, row.Version, stage, store.SlackEffectUnreact, emoji); err != nil {
					return err
				}
				continue
			}
			baseStage, emoji := "queued", queuedTaskReaction
			if persisted {
				baseStage, emoji = "delivered", deliveredTaskReaction
			}
			stage, stageErr := s.activeMarkerStage(ctx, row.ID, baseStage)
			if stageErr != nil {
				return stageErr
			}
			// Prepare the remove half only after the add is confirmed. Startup
			// invokes reconciliation again after recovery to converge safely.
			add, err := s.prepareTaskReaction(ctx, source, row.Version, stage, store.SlackEffectReact, emoji)
			if err != nil {
				return err
			}
			if add.State != store.SlackEffectConfirmed {
				continue
			}
			if persisted {
				if _, err = s.prepareTaskReaction(ctx, source, row.Version, "delivered-unqueue", store.SlackEffectUnreact, queuedTaskReaction); err != nil {
					return err
				}
				continue
			}
			if row.Version > 1 {
				prior, err := s.hasPersistedVersion(ctx, row.ID, row.Version-1)
				if err != nil {
					return err
				}
				if prior {
					if _, err = s.prepareTaskReaction(ctx, source, row.Version, "updated-clear-delivered", store.SlackEffectUnreact, deliveredTaskReaction); err != nil {
						return err
					}
				}
			}
		}
		if next == "" {
			return nil
		}
		cursor = next
	}
}

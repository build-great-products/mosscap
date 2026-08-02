// Package slackops provides configured-channel Slack reads and durable effects.
package slackops

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/stayradiated/mosscap/internal/slackapi"
	"github.com/stayradiated/mosscap/internal/store"
)

var (
	ErrUnavailable   = errors.New("Slack operations are unavailable")
	ErrInvalidTarget = errors.New("Slack target is not in the configured channel")
)

// EffectStore is the narrow durable boundary used by Service.
type EffectStore interface {
	PrepareSlackEffect(context.Context, string, store.SlackEffectTarget, store.SlackEffectIntent) (store.SlackEffectMutationResult, error)
	GetSlackEffect(context.Context, string) (store.SlackEffect, error)
	ClaimSlackEffect(context.Context, string) (store.SlackEffect, error)
	CommitSlackEffectOutcome(context.Context, string, store.SlackEffectOutcome) (store.SlackEffect, error)
	ReconcileSubmittedSlackEffects(context.Context) (int64, error)
	ListSlackEffects(context.Context, *store.SlackEffectState, int) ([]store.SlackEffect, error)
	RetrySlackEffect(context.Context, string, string, string, bool) (store.SlackEffectMutationResult, error)
}

// FailureError is a secret-safe operation failure. State is non-empty only
// after an effect was durably submitted and therefore reached a terminal state.
type FailureError struct {
	State      store.SlackEffectState
	Diagnostic store.SlackEffectDiagnosticCode
}

func (e *FailureError) Error() string {
	if e == nil || e.Diagnostic == "" {
		return "Slack operation failed"
	}
	return fmt.Sprintf("Slack operation failed (%s)", e.Diagnostic)
}

// Service is pinned to one verified runtime snapshot's Web client and target.
// The mutex serializes prepare/claim decisions in one process; ClaimSlackEffect
// remains the cross-process submission authority.
type Service struct {
	store  EffectStore
	web    slackapi.Web
	target store.SlackEffectTarget
	mu     sync.Mutex
}

func New(s EffectStore, web slackapi.Web, target store.SlackEffectTarget) *Service {
	return &Service{store: s, web: web, target: target}
}

// History returns one configured-channel page. A zero limit defaults to 50.
func (s *Service) History(ctx context.Context, limit int) (slackapi.MessagePage, error) {
	if limit == 0 {
		limit = 50
	}
	if err := slackapi.ValidateReadLimit(limit); err != nil {
		return slackapi.MessagePage{}, err
	}
	if err := s.ready(); err != nil {
		return slackapi.MessagePage{}, err
	}
	page, err := s.web.History(ctx, slackapi.HistoryRequest{ChannelID: s.target.ChannelID, Limit: limit})
	if err != nil {
		return slackapi.MessagePage{}, readError(err)
	}
	return page, nil
}

// Thread returns one configured-channel replies page of at most 100 messages.
func (s *Service) Thread(ctx context.Context, threadTS string) (slackapi.MessagePage, error) {
	ts, err := slackapi.ParseTimestamp(threadTS)
	if err != nil {
		return slackapi.MessagePage{}, err
	}
	if err := s.ready(); err != nil {
		return slackapi.MessagePage{}, err
	}
	page, err := s.web.Replies(ctx, slackapi.RepliesRequest{ChannelID: s.target.ChannelID, ThreadTS: ts, Limit: slackapi.MaxReadLimit})
	if err != nil {
		return slackapi.MessagePage{}, readError(err)
	}
	return page, nil
}

// Execute durably prepares, claims, and executes one public Slack mutation.
// It never retries a submitted effect, including after an uncertain response.
func (s *Service) Execute(ctx context.Context, requestID string, intent store.SlackEffectIntent) (store.SlackEffect, error) {
	if err := s.ready(); err != nil {
		return store.SlackEffect{}, err
	}
	if err := s.preflight(ctx, intent); err != nil {
		return store.SlackEffect{}, err
	}
	return s.executePrepared(ctx, requestID, intent)
}

// Retry creates a linked prepared effect through the store then executes it.
// The predecessor is never changed or re-submitted.
func (s *Service) Retry(ctx context.Context, requestID, predecessor, reason string, confirmDuplicateRisk bool) (store.SlackEffect, error) {
	if err := s.ready(); err != nil {
		return store.SlackEffect{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	result, err := s.store.RetrySlackEffect(ctx, requestID, predecessor, reason, confirmDuplicateRisk)
	if err != nil {
		return store.SlackEffect{}, err
	}
	effect, err := s.store.GetSlackEffect(ctx, result.EffectID)
	if err != nil {
		return store.SlackEffect{}, err
	}
	if effect.State != store.SlackEffectPrepared {
		return effect, terminalError(effect)
	}
	if err := s.preflightEffect(ctx, effect); err != nil {
		return effect, err
	}
	return s.claimAndCall(ctx, effect)
}

// Recover first turns every leftover submitted effect ambiguous, then executes
// prepared effects until none remain. It never calls Web for ambiguous work.
func (s *Service) Recover(ctx context.Context) (int64, error) {
	if err := s.ready(); err != nil {
		return 0, err
	}
	n, err := s.store.ReconcileSubmittedSlackEffects(ctx)
	if err != nil {
		return n, err
	}
	for {
		state := store.SlackEffectPrepared
		effects, err := s.store.ListSlackEffects(ctx, &state, 100)
		if err != nil {
			return n, err
		}
		if len(effects) == 0 {
			return n, nil
		}
		for _, effect := range effects {
			if _, err := s.executeExisting(ctx, effect); err != nil {
				// A prepared preflight failure is deliberately left for a later
				// explicit execution/recovery attempt; don't spin forever.
				if current, getErr := s.store.GetSlackEffect(ctx, effect.ID); getErr == nil && current.State == store.SlackEffectPrepared {
					return n, err
				}
			}
		}
	}
}

func (s *Service) executePrepared(ctx context.Context, requestID string, intent store.SlackEffectIntent) (store.SlackEffect, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	result, err := s.store.PrepareSlackEffect(ctx, requestID, s.target, intent)
	if err != nil {
		return store.SlackEffect{}, err
	}
	effect, err := s.store.GetSlackEffect(ctx, result.EffectID)
	if err != nil {
		return store.SlackEffect{}, err
	}
	// Execute already performed target preflight before preparation. Repeating
	// it here would add a second read and could make a prepared intent depend
	// on a changed remote target between validation and its claim.
	if effect.State != store.SlackEffectPrepared {
		return effect, terminalError(effect)
	}
	return s.claimAndCall(ctx, effect)
}
func (s *Service) executeExisting(ctx context.Context, effect store.SlackEffect) (store.SlackEffect, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.executeExistingLocked(ctx, effect)
}
func (s *Service) executeExistingLocked(ctx context.Context, effect store.SlackEffect) (store.SlackEffect, error) {
	if effect.State != store.SlackEffectPrepared {
		return effect, terminalError(effect)
	}
	if err := s.preflightEffect(ctx, effect); err != nil {
		return effect, err
	}
	return s.claimAndCall(ctx, effect)
}

func (s *Service) claimAndCall(ctx context.Context, effect store.SlackEffect) (store.SlackEffect, error) {
	if err := ctx.Err(); err != nil {
		return effect, err
	} // remains prepared
	claimed, err := s.store.ClaimSlackEffect(ctx, effect.ID)
	if errors.Is(err, store.ErrSlackEffectTransition) {
		current, getErr := s.store.GetSlackEffect(ctx, effect.ID)
		if getErr != nil {
			return store.SlackEffect{}, getErr
		}
		return current, terminalError(current)
	}
	if err != nil {
		return store.SlackEffect{}, err
	}
	outcome := s.call(ctx, claimed)
	completed, commitErr := s.store.CommitSlackEffectOutcome(context.WithoutCancel(ctx), claimed.ID, outcome)
	if commitErr != nil {
		return claimed, commitErr
	}
	return completed, terminalError(completed)
}

func (s *Service) preflight(ctx context.Context, intent store.SlackEffectIntent) error {
	if intent.Kind == store.SlackEffectSend && intent.ThreadTS == "" {
		return nil
	}
	ts := intent.TargetMessageTS
	if intent.Kind == store.SlackEffectSend {
		ts = intent.ThreadTS
	}
	parsed, err := slackapi.ParseTimestamp(ts)
	if err != nil {
		return err
	}
	_, err = s.web.Permalink(ctx, slackapi.PermalinkRequest{ChannelID: s.target.ChannelID, MessageTS: parsed})
	if err != nil {
		return readError(err)
	}
	return nil
}
func (s *Service) preflightEffect(ctx context.Context, effect store.SlackEffect) error {
	if effect.TeamID != s.target.TeamID || effect.ChannelID != s.target.ChannelID {
		return ErrInvalidTarget
	}
	return s.preflight(ctx, store.SlackEffectIntent{Kind: effect.Kind, TargetMessageTS: effect.TargetMessageTS, ThreadTS: effect.ThreadTS})
}

func (s *Service) call(ctx context.Context, effect store.SlackEffect) store.SlackEffectOutcome {
	var err error
	var result slackapi.MessageResult
	switch effect.Kind {
	case store.SlackEffectSend:
		result, err = s.web.PostMessage(ctx, slackapi.PostMessageRequest{ChannelID: s.target.ChannelID, ThreadTS: slackapi.Timestamp(effect.ThreadTS), Text: effect.Text, ClientMessageID: effect.IdempotencyKey})
	case store.SlackEffectEdit:
		result, err = s.web.UpdateMessage(ctx, slackapi.UpdateMessageRequest{ChannelID: s.target.ChannelID, MessageTS: slackapi.Timestamp(effect.TargetMessageTS), Text: effect.Text})
	case store.SlackEffectDelete:
		err = s.web.DeleteMessage(ctx, slackapi.DeleteMessageRequest{ChannelID: s.target.ChannelID, MessageTS: slackapi.Timestamp(effect.TargetMessageTS)})
	case store.SlackEffectReact:
		err = s.web.AddReaction(ctx, slackapi.ReactionRequest{ChannelID: s.target.ChannelID, MessageTS: slackapi.Timestamp(effect.TargetMessageTS), Emoji: effect.Emoji})
	case store.SlackEffectUnreact:
		err = s.web.RemoveReaction(ctx, slackapi.ReactionRequest{ChannelID: s.target.ChannelID, MessageTS: slackapi.Timestamp(effect.TargetMessageTS), Emoji: effect.Emoji})
	default:
		err = slackapi.NewExternalError(slackapi.ReasonInvalidResponse, slackapi.DiagnosticInvalidRequest, "", nil)
	}
	if err == nil && (effect.Kind == store.SlackEffectSend || effect.Kind == store.SlackEffectEdit) && (result.ChannelID != s.target.ChannelID || !result.MessageTS.Valid()) {
		err = slackapi.NewExternalError(slackapi.ReasonInvalidResponse, slackapi.DiagnosticMalformedResponse, "", nil)
	}
	if err == nil {
		return store.SlackEffectOutcome{State: store.SlackEffectConfirmed, ResponseMessageTS: string(result.MessageTS)}
	}
	failure := slackapi.Classify(err, slackapi.FailureAfterSubmit)
	state := store.SlackEffectAmbiguous
	if failure.Class == slackapi.FailureConclusive {
		state = store.SlackEffectFailed
	}
	return store.SlackEffectOutcome{State: state, DiagnosticCode: storeDiagnostic(failure.Diagnostic)}
}

func (s *Service) ready() error {
	if s == nil || s.store == nil || s.web == nil {
		return ErrUnavailable
	}
	if err := slackapi.ValidateTeamID(s.target.TeamID); err != nil {
		return ErrUnavailable
	}
	if err := slackapi.ValidateChannelID(s.target.ChannelID); err != nil {
		return ErrUnavailable
	}
	return nil
}
func readError(err error) error {
	if slackapi.Classify(err, slackapi.FailureRead).Class == slackapi.FailureConclusive {
		return &FailureError{State: store.SlackEffectFailed, Diagnostic: storeDiagnostic(slackapi.Classify(err, slackapi.FailureRead).Diagnostic)}
	}
	return ErrUnavailable
}
func terminalError(effect store.SlackEffect) error {
	switch effect.State {
	case store.SlackEffectFailed, store.SlackEffectAmbiguous:
		return &FailureError{State: effect.State, Diagnostic: effect.DiagnosticCode}
	default:
		return nil
	}
}
func storeDiagnostic(d slackapi.DiagnosticCode) store.SlackEffectDiagnosticCode {
	switch d {
	case slackapi.DiagnosticInvalidAuth:
		return store.SlackEffectDiagnosticInvalidAuth
	case slackapi.DiagnosticTokenRevoked:
		return store.SlackEffectDiagnosticTokenRevoked
	case slackapi.DiagnosticMissingScope:
		return store.SlackEffectDiagnosticMissingScope
	case slackapi.DiagnosticTeamMismatch:
		return store.SlackEffectDiagnosticTeamMismatch
	case slackapi.DiagnosticBotMismatch:
		return store.SlackEffectDiagnosticBotMismatch
	case slackapi.DiagnosticChannelNotFound:
		return store.SlackEffectDiagnosticChannelNotFound
	case slackapi.DiagnosticNotInChannel:
		return store.SlackEffectDiagnosticNotInChannel
	case slackapi.DiagnosticUserNotFound:
		return store.SlackEffectDiagnosticUserNotFound
	case slackapi.DiagnosticMessageNotFound:
		return store.SlackEffectDiagnosticMessageNotFound
	case slackapi.DiagnosticRateLimited:
		return store.SlackEffectDiagnosticRateLimited
	case slackapi.DiagnosticTimeout:
		return store.SlackEffectDiagnosticTimeout
	case slackapi.DiagnosticCanceled:
		return store.SlackEffectDiagnosticCanceled
	case slackapi.DiagnosticDisconnected:
		return store.SlackEffectDiagnosticDisconnected
	case slackapi.DiagnosticMalformedResponse:
		return store.SlackEffectDiagnosticMalformedResponse
	case slackapi.DiagnosticInvalidRequest:
		return store.SlackEffectDiagnosticInvalidRequest
	case slackapi.DiagnosticRejected:
		return store.SlackEffectDiagnosticExternalRejected
	default:
		return store.SlackEffectDiagnosticUnavailable
	}
}

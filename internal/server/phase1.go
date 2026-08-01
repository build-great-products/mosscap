package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/stayradiated/mosscap/internal/config"
	"github.com/stayradiated/mosscap/internal/doctor"
	"github.com/stayradiated/mosscap/internal/protocol"
	"github.com/stayradiated/mosscap/internal/slackapi"
	"github.com/stayradiated/mosscap/internal/slackingest"
	"github.com/stayradiated/mosscap/internal/slackops"
	"github.com/stayradiated/mosscap/internal/slackruntime"
	"github.com/stayradiated/mosscap/internal/store"
)

// Options controls the Phase 1 Slack runtime. Disabled is intended only for
// direct deterministic server tests; production callers must provide Factory.
type Options struct {
	Factory  slackruntime.Factory
	Disabled bool
}

func NewWithOptions(c config.Config, loc config.Location, version string, log *slog.Logger, options Options) *Server {
	// Keep New's logger contract while allowing callers to opt in without changing
	// the Phase 0 constructor used by existing local-only callers.
	s := New(c, loc, version, log)
	s.runtimeFactory, s.slackDisabled = options.Factory, options.Disabled
	if options.Factory != nil {
		s.slackDisabled = false
		s.slackState = slackruntime.StateConnecting
	}
	return s
}

func (s *Server) startSlack(ctx context.Context, c config.Config) error {
	if s.slackDisabled {
		s.slackState = slackruntime.StateDisabled
		return nil
	}
	if s.runtimeFactory == nil {
		return errors.New("production Slack runtime factory is required")
	}
	trusted, err := s.store.ListTrustedUsers(ctx)
	if err != nil {
		return err
	}
	policy := store.SlackAdmissionPolicy{TeamID: c.SlackTeamID, ChannelID: c.SlackChannelID, BotUserID: c.SlackBotUserID}
	coordinator := slackingest.New(s.store, policy, nil)
	rt, err := slackruntime.New(slackruntime.Options{Factory: s.runtimeFactory, TrustedUserIDs: trusted, Handler: func(ctx context.Context, e slackapi.Envelope) error { _, err := coordinator.Handle(ctx, e); return err }})
	if err != nil {
		return err
	}
	if err = rt.Start(ctx, c); err != nil {
		_ = rt.Close()
		return err
	}
	s.runtime, s.ingest = rt, coordinator
	s.slackState = slackruntime.StateConnected
	return nil
}
func (s *Server) closeSlack() {
	if s.runtime != nil {
		_ = s.runtime.Close()
		s.runtime = nil
	}
	if s.slackState != slackruntime.StateDisabled {
		s.slackState = slackruntime.StateClosed
	}
}
func (s *Server) ops() (*slackops.Service, func(), error) {
	if s.runtime == nil {
		return nil, func() {}, slackops.ErrUnavailable
	}
	snap, err := s.runtime.Acquire()
	if err != nil {
		return nil, func() {}, err
	}
	return slackops.New(s.store, snap.Web, store.SlackEffectTarget{TeamID: snap.TeamID, ChannelID: snap.ChannelID}), snap.Release, nil
}
func (s *Server) phase1Status(ctx context.Context) (*protocol.SlackRuntimeStatusDTO, error) {
	counts, err := s.store.SlackEffectCounts(ctx)
	if err != nil {
		return nil, err
	}
	out := &protocol.SlackRuntimeStatusDTO{State: string(s.slackState), Effects: protocol.SlackEffectCountsDTO{Total: counts.Total, ActionRequired: counts.ActionRequired, ByState: map[string]int64{}}}
	for k, v := range counts.ByState {
		out.Effects.ByState[string(k)] = v
	}
	if s.runtime != nil {
		o := s.runtime.Observations()
		out.State = string(o.State)
		out.TeamID = s.snapshot().SlackTeamID
		out.BotUserID = s.snapshot().SlackBotUserID
		out.ChannelID = s.snapshot().SlackChannelID
		out.ReconnectAttempt = o.ReconnectAttempt
		out.BackoffMilliseconds = o.Backoff.Milliseconds()
		out.LastDiagnosticCode = string(o.LastDiagnostic)
	}
	if s.ingest != nil {
		o := s.ingest.Observations()
		if !o.LastReceivedAt.IsZero() {
			v := stamp(o.LastReceivedAt)
			out.LastEnvelopeReceivedAt = &v
		}
		if !o.LastCommittedAt.IsZero() {
			v := stamp(o.LastCommittedAt)
			out.LastSourceCommittedAt = &v
		}
		out.LastDiagnosticCode = o.LastDiagnosticCode
		out.SourceResults = map[string]uint64{}
		for k, v := range o.Results {
			out.SourceResults[string(k)] = v
		}
	}
	return out, nil
}
func (s *Server) phase1Doctor(ctx context.Context) []doctor.Check {
	if s.slackDisabled {
		return []doctor.Check{{Name: "slack_runtime", Status: doctor.Skipped, Message: "Slack runtime is disabled for injected test mode"}}
	}
	if s.runtime == nil {
		return []doctor.Check{{Name: "slack_runtime", Status: doctor.Fail, Message: "Slack runtime is unavailable", Action: "restart gateway after correcting Slack configuration"}}
	}
	o := s.runtime.Observations()
	if o.State == slackruntime.StateConnected {
		return []doctor.Check{{Name: "slack_runtime", Status: doctor.Pass, Message: "Slack auth, access, scopes, and Socket readiness verified"}}
	}
	return []doctor.Check{{Name: "slack_runtime", Status: doctor.Warn, Message: "Slack runtime is reconnecting", Action: "inspect gateway status and Slack connectivity"}}
}
func effectIntent(command string, p protocol.Request) (store.SlackEffectIntent, error) {
	var i store.SlackEffectIntent
	switch command {
	case "slack.send":
		var x protocol.SlackSendParams
		if err := jsonUnmarshal(p.Params, &x); err != nil {
			return i, err
		}
		i = store.SlackEffectIntent{Kind: store.SlackEffectSend, ThreadTS: x.ThreadTS, Text: x.Text, RelatedTaskID: x.Task}
	case "slack.edit":
		var x protocol.SlackEditParams
		jsonUnmarshal(p.Params, &x)
		i = store.SlackEffectIntent{Kind: store.SlackEffectEdit, TargetMessageTS: x.MessageTS, Text: x.Text, RelatedTaskID: x.Task}
	case "slack.delete":
		var x protocol.SlackDeleteParams
		jsonUnmarshal(p.Params, &x)
		i = store.SlackEffectIntent{Kind: store.SlackEffectDelete, TargetMessageTS: x.MessageTS, OperatorReason: x.Reason, RelatedTaskID: x.Task}
	case "slack.react", "slack.unreact":
		var x protocol.SlackReactionParams
		jsonUnmarshal(p.Params, &x)
		k := store.SlackEffectReact
		if command == "slack.unreact" {
			k = store.SlackEffectUnreact
		}
		i = store.SlackEffectIntent{Kind: k, TargetMessageTS: x.MessageTS, Emoji: x.Emoji, RelatedTaskID: x.Task}
	default:
		return i, errors.New("unsupported command")
	}
	return i, nil
}
func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }
func phase1Stamp(t time.Time) *string {
	if t.IsZero() {
		return nil
	}
	v := stamp(t)
	return &v
}

package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/stayradiated/mosscap/internal/clock"
	"github.com/stayradiated/mosscap/internal/config"
	"github.com/stayradiated/mosscap/internal/doctor"
	"github.com/stayradiated/mosscap/internal/fileio"
	"github.com/stayradiated/mosscap/internal/filetransfer"
	"github.com/stayradiated/mosscap/internal/identity"
	"github.com/stayradiated/mosscap/internal/protocol"
	"github.com/stayradiated/mosscap/internal/slackapi"
	"github.com/stayradiated/mosscap/internal/slackingest"
	"github.com/stayradiated/mosscap/internal/slackops"
	"github.com/stayradiated/mosscap/internal/slackruntime"
	"github.com/stayradiated/mosscap/internal/store"
)

// Options controls the Slack runtime. Disabled is intended only for direct
// deterministic server tests; production callers must provide Factory.
type Options struct {
	Factory  slackruntime.Factory
	Disabled bool
}

func NewWithOptions(c config.Config, loc config.Location, version string, log *slog.Logger, options Options) *Server {
	// Keep New's logger contract while allowing callers to opt in without changing
	// the constructor used by existing local-only tests.
	s := New(c, loc, version, log)
	s.runtimeFactory, s.slackDisabled = options.Factory, options.Disabled
	if options.Factory != nil {
		s.slackDisabled = false
		s.slackState = slackruntime.StateConnecting
	}
	return s
}

func (s *Server) startSlack(ctx context.Context, c config.Config) error {
	rt, ingest, state, err := s.prepareSlack(ctx, c, s.store)
	if err != nil {
		s.recordSlackVerificationFailure(err, false)
		return s.reloadSlackVerificationError(err)
	}
	s.runtime, s.ingest, s.slackState = rt, ingest, state
	s.clearSlackVerificationFailure()
	return nil
}

// prepareSlack verifies and starts an unpublished runtime. Its caller owns rt
// until it is assigned to Server, and must Close it on a failed activation.
func (s *Server) prepareSlack(ctx context.Context, c config.Config, ledger *store.Store) (*slackruntime.Runtime, *slackingest.Coordinator, slackruntime.State, error) {
	if s.slackDisabled {
		return nil, nil, slackruntime.StateDisabled, nil
	}
	if s.runtimeFactory == nil {
		return nil, nil, "", errors.New("production Slack runtime factory is required")
	}
	trusted, err := ledger.ListTrustedUsers(ctx)
	if err != nil {
		return nil, nil, "", err
	}
	var coordinator *slackingest.Coordinator
	var rt *slackruntime.Runtime
	rt, err = slackruntime.New(slackruntime.Options{Factory: s.runtimeFactory, TrustedUserIDs: trusted, Handler: func(ctx context.Context, e slackapi.Envelope) error {
		if coordinator == nil {
			return errors.New("Slack admission coordinator is not ready")
		}
		result, handleErr := coordinator.Handle(ctx, e)
		s.logSlackAdmission(e, result)
		if result.AdmissionResult == store.SlackAdmitted {
			if reactionErr := s.queueTaskReaction(ctx, result.TaskID); reactionErr != nil && s.Log != nil {
				s.Log.Warn("automatic queued-task reaction did not confirm", "task_id", result.TaskID, "task_version", result.TaskVersion, "error", reactionErr)
			}
		}
		if result.AdmissionResult == store.SlackIgnoredUntrusted {
			effect, reactionErr := executeAutomaticUntrustedReaction(ctx, rt, ledger, e)
			if reactionErr != nil {
				s.Log.Warn("automatic untrusted-sender reaction did not confirm",
					"slack_user_id", e.UserID,
					"channel_id", e.ChannelID,
					"source_event_id", result.SourceEventID,
					"effect_id", effect.ID,
					"effect_state", effect.State,
					"error", reactionErr,
				)
			} else {
				s.Log.Info("automatic untrusted-sender reaction confirmed",
					"slack_user_id", e.UserID,
					"channel_id", e.ChannelID,
					"source_event_id", result.SourceEventID,
					"effect_id", effect.ID,
				)
			}
		}
		return handleErr
	}})
	if err != nil {
		return nil, nil, "", err
	}
	if err = rt.Start(ctx, c); err != nil {
		_ = rt.Close()
		return nil, nil, "", err
	}
	snapshot, err := rt.Acquire()
	if err != nil {
		_ = rt.Close()
		return nil, nil, "", err
	}
	policy := store.SlackAdmissionPolicy{TeamID: snapshot.TeamID, Channels: append([]string(nil), snapshot.Channels...), BotUserID: snapshot.BotUserID}
	coordinator = slackingest.NewWithOptions(ledger, policy, nil, slackingest.Options{
		BeforeAcknowledge: func(ctx context.Context, event store.SlackSourceEvent, result store.SlackEventResult) error {
			if result.AdmissionResult == store.SlackAdmitted {
				source, err := ledger.GetSlackSource(ctx, result.TaskID)
				if err != nil {
					return err
				}
				_, err = s.prepareTaskReactionIn(ctx, ledger, source, 1, queuedStage, store.SlackEffectReact, queuedTaskReaction)
				return err
			}
			if result.AdmissionResult != store.SlackIgnoredUntrusted {
				return nil
			}
			target := store.SlackEffectTarget{TeamID: event.TeamID, ChannelID: event.ChannelID}
			_, err := ledger.PrepareSlackEffect(ctx, automaticUntrustedReactionRequestID(event.TeamID, event.ChannelID, event.MessageTS), target, store.SlackEffectIntent{
				Kind:            store.SlackEffectReact,
				TargetMessageTS: event.MessageTS,
				Emoji:           untrustedSenderReaction,
			})
			return err
		},
	})
	snapshot.Release()
	return rt, coordinator, slackruntime.StateConnected, nil
}

// logSlackAdmission reports actionable admission failures without exposing
// message text or raw Slack payloads.
func (s *Server) logSlackAdmission(envelope slackapi.Envelope, result store.SlackEventResult) {
	if result.AdmissionResult != store.SlackIgnoredUntrusted {
		return
	}
	s.Log.Warn("Slack event ignored: sender is not trusted",
		"slack_user_id", envelope.UserID,
		"channel_id", envelope.ChannelID,
		"source_event_id", result.SourceEventID,
	)
}

const untrustedSenderReaction = "no_entry"

// executeAutomaticUntrustedReaction records the fixed reaction through the
// normal durable effect lifecycle. The request ID is stable per Slack message,
// so duplicate event types, edits, and redeliveries cannot submit it twice.
func executeAutomaticUntrustedReaction(ctx context.Context, rt *slackruntime.Runtime, ledger *store.Store, envelope slackapi.Envelope) (store.SlackEffect, error) {
	if rt == nil {
		return store.SlackEffect{}, slackops.ErrUnavailable
	}
	snapshot, err := rt.Acquire()
	if err != nil {
		return store.SlackEffect{}, err
	}
	defer snapshot.Release()
	service := slackops.New(ledger, snapshot.Web, slackops.Scope{TeamID: snapshot.TeamID, Channels: snapshot.Channels})
	if !service.Allows(envelope.TeamID, envelope.ChannelID) {
		return store.SlackEffect{}, slackops.ErrInvalidTarget
	}
	return service.Execute(ctx, automaticUntrustedReactionRequestID(envelope.TeamID, envelope.ChannelID, string(envelope.MessageTS)), envelope.ChannelID, store.SlackEffectIntent{
		Kind:            store.SlackEffectReact,
		TargetMessageTS: string(envelope.MessageTS),
		Emoji:           untrustedSenderReaction,
	})
}

func automaticUntrustedReactionRequestID(teamID, channelID, messageTS string) string {
	sum := sha256.Sum256([]byte(teamID + "\x00" + channelID + "\x00" + messageTS + "\x00" + untrustedSenderReaction))
	return "internal-untrusted-reaction-" + hex.EncodeToString(sum[:])
}

// prepareFileTransfer binds lazy transfers to one verified runtime snapshot.
// The adapter is never exposed through protocol results; only the service may
// stream it into a generated private file.
func (s *Server) prepareFileTransfer(ctx context.Context, rt *slackruntime.Runtime, ledger *store.Store, c config.Config) (*fileio.TempDir, *filetransfer.Service, error) {
	if rt == nil {
		return nil, nil, nil
	}
	tmp, err := fileio.OpenTempDir(c.Paths.TmpDir)
	if err != nil {
		return nil, nil, err
	}
	snapshot, err := rt.Acquire()
	if err != nil {
		_ = tmp.Close()
		return nil, nil, err
	}
	adapter, ok := snapshot.Web.(slackapi.FileTransfer)
	snapshot.Release()
	if !ok {
		_ = tmp.Close()
		// Deterministic Slack fakes need not implement the byte adapter.
		// Production slackgo does; capability advertisement remains disabled here.
		return nil, nil, nil
	}
	service, err := filetransfer.New(ledger, filetransfer.NewFileIOFiles(tmp), adapter, fileTransferSettings{server: s}, identity.NewGenerator(nil), clock.System{})
	if err != nil {
		_ = tmp.Close()
		return nil, nil, err
	}
	if err := service.Start(ctx); err != nil {
		service.Close()
		_ = tmp.Close()
		return nil, nil, err
	}
	return tmp, service, nil
}

func (s *Server) startFileTransfer(ctx context.Context, rt *slackruntime.Runtime, c config.Config) error {
	tmp, service, err := s.prepareFileTransfer(ctx, rt, s.store, c)
	if err != nil {
		return err
	}
	s.temporaryDir, s.fileTransfer = tmp, service
	return nil
}

func (s *Server) closeFileTransfer() {
	if s.fileTransfer != nil {
		s.fileTransfer.Close()
		s.fileTransfer = nil
	}
	if s.temporaryDir != nil {
		_ = s.temporaryDir.Close()
		s.temporaryDir = nil
	}
}

type fileTransferSettings struct{ server *Server }

func (s fileTransferSettings) Settings(context.Context) (filetransfer.Settings, error) {
	if s.server == nil {
		return filetransfer.Settings{}, errors.New("file transfer settings unavailable")
	}
	c := s.server.snapshot()
	lifetime, err := time.ParseDuration(c.TemporaryDownloadLifetime)
	if err != nil {
		return filetransfer.Settings{}, err
	}
	return filetransfer.Settings{MaximumBytes: int64(c.MaximumFileSize), MaximumLiveFiles: c.MaximumFilesPerSlackOperation, Lifetime: lifetime}, nil
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
	if files, ok := snap.Web.(slackapi.FileTransfer); ok {
		c := s.snapshot()
		return slackops.NewWithFileTransfer(s.store, snap.Web, files, slackops.Scope{TeamID: snap.TeamID, Channels: snap.Channels}, int64(c.MaximumFileSize), c.MaximumFilesPerSlackOperation), snap.Release, nil
	}
	return slackops.New(s.store, snap.Web, slackops.Scope{TeamID: snap.TeamID, Channels: snap.Channels}), snap.Release, nil
}
func (s *Server) slackStatus(ctx context.Context) (*protocol.SlackRuntimeStatusDTO, error) {
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
		snapshot, err := s.runtime.Acquire()
		if err == nil {
			out.TeamID = snapshot.TeamID
			out.BotUserID = snapshot.BotUserID
			out.Channels = append([]string(nil), snapshot.Channels...)
			out.IdentitySource = "auth_test"
			snapshot.Release()
		}
		out.ReconnectAttempt = o.ReconnectAttempt
		out.BackoffMilliseconds = o.Backoff.Milliseconds()
		out.LastDiagnosticCode = string(o.LastDiagnostic)
	}
	s.slackDiagnosticMu.RLock()
	out.LastVerificationDiagnosticCode = string(s.slackVerificationCode)
	out.PriorRuntimeRetained = s.slackVerificationHeld
	s.slackDiagnosticMu.RUnlock()
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
func (s *Server) fileTransferDoctor(ctx context.Context) []doctor.Check {
	if s.fileTransfer == nil || s.temporaryDir == nil {
		return []doctor.Check{{Name: "file_transfer", Status: doctor.Skipped, Message: "file transfer adapter is unavailable"}}
	}
	settings, err := (fileTransferSettings{server: s}).Settings(ctx)
	if err != nil || !settingsValid(settings) {
		return []doctor.Check{{Name: "file_transfer_configuration", Status: doctor.Fail, Message: "file transfer limits are invalid", Action: "repair configuration and reload gateway"}}
	}
	inventory, err := s.fileTransferStatus(ctx)
	if err != nil {
		return []doctor.Check{{Name: "file_transfer_temporary_inventory", Status: doctor.Warn, Message: "temporary transfer inventory cannot be read", Action: "inspect the private temporary directory"}}
	}
	inventoryCheck := doctor.Check{Name: "file_transfer_temporary_inventory", Status: doctor.Pass, Message: "temporary transfer inventory is readable without transfer I/O"}
	if inventory.UnsafeTemporaryEntries != 0 {
		inventoryCheck = doctor.Check{Name: "file_transfer_temporary_inventory", Status: doctor.Warn, Message: "temporary transfer inventory contains action-required entries", Action: "inspect the private temporary directory"}
	}
	return []doctor.Check{{Name: "file_transfer_configuration", Status: doctor.Pass, Message: "file transfer limits and retention are valid"}, inventoryCheck}
}
func settingsValid(settings filetransfer.Settings) bool {
	return settings.MaximumBytes > 0 && settings.MaximumLiveFiles > 0 && settings.Lifetime > 0
}

// slackUserNames resolves presentation-only usernames from the active Slack
// runtime. Trust remains entirely ID-based, and an unavailable lookup never
// prevents the committed allowlist from being returned.
func (s *Server) slackUserNames(ctx context.Context, userIDs []string) map[string]string {
	if s.runtime == nil || len(userIDs) == 0 {
		return nil
	}
	snapshot, err := s.runtime.Acquire()
	if err != nil {
		return nil
	}
	defer snapshot.Release()

	names := make(map[string]string, len(userIDs))
	for _, userID := range userIDs {
		user, err := snapshot.Web.User(ctx, userID)
		if err != nil || user.ID != userID || user.TeamID != snapshot.TeamID || user.Bot || user.Deactivated || slackapi.ValidateUserName(user.Name) != nil {
			continue
		}
		names[userID] = user.Name
	}
	if len(names) == 0 {
		return nil
	}
	return names
}

func (s *Server) slackDoctor(ctx context.Context) []doctor.Check {
	s.slackDiagnosticMu.RLock()
	verificationCode, priorRuntimeRetained := s.slackVerificationCode, s.slackVerificationHeld
	s.slackDiagnosticMu.RUnlock()
	if verificationCode != "" {
		message := "Slack candidate verification failed (" + string(verificationCode) + ")"
		if priorRuntimeRetained {
			message += "; the prior runtime remains active"
		}
		status := doctor.Fail
		if priorRuntimeRetained {
			status = doctor.Warn
		}
		return []doctor.Check{{Name: "slack_runtime", Status: status, Message: message, Action: slackapi.VerificationAdvice(verificationCode)}}
	}
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
func (s *Server) recordSlackVerificationFailure(err error, priorRuntimeRetained bool) {
	diagnostic, ok := slackruntime.VerificationDiagnosticFromError(err)
	if !ok {
		failure := slackapi.Classify(err, slackapi.FailureRead)
		diagnostic.Code = failure.Diagnostic
	}
	if !diagnostic.Code.Valid() {
		diagnostic.Code = slackapi.DiagnosticRejected
	}
	s.slackDiagnosticMu.Lock()
	s.slackVerificationCode = diagnostic.Code
	s.slackVerificationHeld = priorRuntimeRetained
	s.slackDiagnosticMu.Unlock()
}

func (s *Server) clearSlackVerificationFailure() {
	s.slackDiagnosticMu.Lock()
	s.slackVerificationCode = ""
	s.slackVerificationHeld = false
	s.slackDiagnosticMu.Unlock()
}

func (s *Server) reloadSlackVerificationMessage(err error) string {
	diagnostic, ok := slackruntime.VerificationDiagnosticFromError(err)
	if !ok {
		failure := slackapi.Classify(err, slackapi.FailureRead)
		diagnostic.Code = failure.Diagnostic
	}
	if !diagnostic.Code.Valid() {
		diagnostic.Code = slackapi.DiagnosticRejected
	}
	return "reload candidate Slack verification failed (" + string(diagnostic.Code) + "). " + slackapi.VerificationAdvice(diagnostic.Code)
}

func (s *Server) reloadSlackVerificationError(err error) error {
	diagnostic, ok := slackruntime.VerificationDiagnosticFromError(err)
	if !ok {
		failure := slackapi.Classify(err, slackapi.FailureRead)
		diagnostic.Code = failure.Diagnostic
	}
	if !diagnostic.Code.Valid() {
		diagnostic.Code = slackapi.DiagnosticRejected
	}
	return errors.New("Slack runtime verification failed (" + string(diagnostic.Code) + "). " + slackapi.VerificationAdvice(diagnostic.Code))
}

func hasUploadFiles(raw []byte) bool {
	var p protocol.SlackSendParams
	return json.Unmarshal(raw, &p) == nil && len(p.FilePaths) != 0
}

func fileUploadIntent(command string, p protocol.Request) (store.SlackFileUploadIntent, string, error) {
	var intent store.SlackFileUploadIntent
	var channel string
	var paths []string
	switch command {
	case "slack.file":
		var x protocol.SlackFileParams
		if err := jsonUnmarshal(p.Params, &x); err != nil {
			return intent, "", err
		}
		intent.ThreadTS, intent.Text, intent.RelatedTaskID, paths, channel = x.ThreadTS, x.Comment, x.Task, []string{x.FilePath}, x.Channel
	case "slack.send":
		var x protocol.SlackSendParams
		if err := jsonUnmarshal(p.Params, &x); err != nil {
			return intent, "", err
		}
		intent.ThreadTS, intent.Text, intent.RelatedTaskID, paths, channel = x.ThreadTS, x.Text, x.Task, x.FilePaths, x.Channel
	default:
		return intent, "", errors.New("unsupported command")
	}
	intent.Files = make([]store.SlackUploadFile, len(paths))
	for i, path := range paths {
		intent.Files[i].SourcePath = path
	}
	return intent, channel, nil
}

func effectIntent(command string, p protocol.Request) (store.SlackEffectIntent, string, error) {
	var i store.SlackEffectIntent
	var channel string
	switch command {
	case "slack.send":
		var x protocol.SlackSendParams
		if err := jsonUnmarshal(p.Params, &x); err != nil {
			return i, "", err
		}
		i, channel = store.SlackEffectIntent{Kind: store.SlackEffectSend, ThreadTS: x.ThreadTS, Text: x.Text, RelatedTaskID: x.Task}, x.Channel
	case "slack.edit":
		var x protocol.SlackEditParams
		jsonUnmarshal(p.Params, &x)
		i, channel = store.SlackEffectIntent{Kind: store.SlackEffectEdit, TargetMessageTS: x.MessageTS, Text: x.Text, RelatedTaskID: x.Task}, x.Channel
	case "slack.delete":
		var x protocol.SlackDeleteParams
		jsonUnmarshal(p.Params, &x)
		i, channel = store.SlackEffectIntent{Kind: store.SlackEffectDelete, TargetMessageTS: x.MessageTS, OperatorReason: x.Reason, RelatedTaskID: x.Task}, x.Channel
	case "slack.react", "slack.unreact":
		var x protocol.SlackReactionParams
		jsonUnmarshal(p.Params, &x)
		k := store.SlackEffectReact
		if command == "slack.unreact" {
			k = store.SlackEffectUnreact
		}
		i, channel = store.SlackEffectIntent{Kind: k, TargetMessageTS: x.MessageTS, Emoji: x.Emoji, RelatedTaskID: x.Task}, x.Channel
	default:
		return i, "", errors.New("unsupported command")
	}
	return i, channel, nil
}

// ErrChannelRequired means a Slack command named no channel and none could be
// derived from its task or a single-channel allowlist.
var ErrChannelRequired = errors.New("Slack channel is required: pass --channel, or --task for a Slack task")

// resolveChannel chooses the channel of a Slack command: the explicit one, else
// the related task's source channel, else the only allowlisted channel.
func (s *Server) resolveChannel(ctx context.Context, op *slackops.Service, channel, taskSelector string) (string, error) {
	if channel != "" {
		return channel, nil
	}
	if taskSelector != "" {
		source, err := s.store.GetSlackSource(ctx, taskSelector)
		if err == nil {
			return source.ChannelID, nil
		}
		if !errors.Is(err, store.ErrNotFound) {
			return "", err
		}
	}
	if channels := op.Channels(); len(channels) == 1 {
		return channels[0], nil
	}
	return "", ErrChannelRequired
}
func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

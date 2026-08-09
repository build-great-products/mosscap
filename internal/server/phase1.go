package server

import (
	"context"
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
	rt, ingest, state, err := s.prepareSlack(ctx, c, s.store)
	if err != nil {
		return err
	}
	s.runtime, s.ingest, s.slackState = rt, ingest, state
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
	rt, err := slackruntime.New(slackruntime.Options{Factory: s.runtimeFactory, TrustedUserIDs: trusted, Handler: func(ctx context.Context, e slackapi.Envelope) error {
		if coordinator == nil {
			return errors.New("Slack admission coordinator is not ready")
		}
		_, err := coordinator.Handle(ctx, e)
		return err
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
	coordinator = slackingest.New(ledger, store.SlackAdmissionPolicy{TeamID: snapshot.TeamID, ChannelID: snapshot.ChannelID, BotUserID: snapshot.BotUserID}, nil)
	snapshot.Release()
	return rt, coordinator, slackruntime.StateConnected, nil
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
		// Deterministic Phase 1 fakes need not implement the new byte adapter.
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
		return slackops.NewWithFileTransfer(s.store, snap.Web, files, store.SlackEffectTarget{TeamID: snap.TeamID, ChannelID: snap.ChannelID}, int64(c.MaximumFileSize), c.MaximumFilesPerSlackOperation), snap.Release, nil
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
		snapshot, err := s.runtime.Acquire()
		if err == nil {
			out.TeamID = snapshot.TeamID
			out.BotUserID = snapshot.BotUserID
			out.ChannelID = snapshot.ChannelID
			out.IdentitySource = "auth_test"
			snapshot.Release()
		}
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
func hasUploadFiles(raw []byte) bool {
	var p protocol.SlackSendParams
	return json.Unmarshal(raw, &p) == nil && len(p.FilePaths) != 0
}

func fileUploadIntent(command string, p protocol.Request) (store.SlackFileUploadIntent, error) {
	var intent store.SlackFileUploadIntent
	var paths []string
	switch command {
	case "slack.file":
		var x protocol.SlackFileParams
		if err := jsonUnmarshal(p.Params, &x); err != nil {
			return intent, err
		}
		intent.ThreadTS, intent.Text, intent.RelatedTaskID, paths = x.ThreadTS, x.Comment, x.Task, []string{x.FilePath}
	case "slack.send":
		var x protocol.SlackSendParams
		if err := jsonUnmarshal(p.Params, &x); err != nil {
			return intent, err
		}
		intent.ThreadTS, intent.Text, intent.RelatedTaskID, paths = x.ThreadTS, x.Text, x.Task, x.FilePaths
	default:
		return intent, errors.New("unsupported command")
	}
	intent.Files = make([]store.SlackUploadFile, len(paths))
	for i, path := range paths {
		intent.Files[i].SourcePath = path
	}
	return intent, nil
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

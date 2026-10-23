// Package slackops provides admitted-channel Slack reads and durable effects.
package slackops

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"path/filepath"
	"sync"
	"unicode/utf8"

	"github.com/stayradiated/mosscap/internal/fileio"
	"github.com/stayradiated/mosscap/internal/slackapi"
	"github.com/stayradiated/mosscap/internal/store"
)

const maxUploadFiles = 20

var (
	ErrUnavailable   = errors.New("Slack operations are unavailable")
	ErrInvalidTarget = errors.New("Slack target is not a C... or G... channel in the active workspace")
)

// EffectStore is the narrow durable boundary used by Service.
type EffectStore interface {
	PrepareSlackEffect(context.Context, string, store.SlackEffectTarget, store.SlackEffectIntent) (store.SlackEffectMutationResult, error)
	PrepareSlackFileUpload(context.Context, string, store.SlackEffectTarget, store.SlackFileUploadIntent) (store.SlackEffectMutationResult, error)
	GetSlackEffect(context.Context, string) (store.SlackEffect, error)
	ClaimSlackEffect(context.Context, string) (store.SlackEffect, error)
	CommitSlackEffectOutcome(context.Context, string, store.SlackEffectOutcome) (store.SlackEffect, error)
	CommitSlackFileUploadOutcome(context.Context, string, store.SlackEffectOutcome, []store.FileUploadOutcome) (store.SlackEffect, error)
	FailPreparedSlackFileUpload(context.Context, string, store.SlackEffectDiagnosticCode) (store.SlackEffect, error)
	ReconcileSubmittedSlackEffects(context.Context) (int64, error)
	ListSlackEffects(context.Context, *store.SlackEffectState, int) ([]store.SlackEffect, error)
	RetrySlackEffect(context.Context, string, string, string, bool) (store.SlackEffectMutationResult, error)
	InspectSlackFileUploadSources(context.Context, string) ([]store.SlackEffectOutgoingSource, error)
}

// uploadSource is the minimal race-safe source contract required by this
// package. fileio.Source is its production implementation.
type uploadSource interface {
	Path() string
	Device() uint64
	Inode() uint64
	MtimeNS() int64
	Close() error
	Revalidate() error
	RehashAndRevalidate() error
	StreamTo(io.Writer) error
	PreparedSize() int64
	PreparedSHA256() string
}

type fileSource struct{ *fileio.Source }

func (s fileSource) PreparedSize() int64    { return s.Size }
func (s fileSource) PreparedSHA256() string { return s.SHA256 }

type sourceOpener func(string, int64) (uploadSource, error)

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

// Scope is the authority of one verified runtime snapshot: its workspace.
// Every channel in it is a target; Slack itself enforces bot membership.
type Scope struct {
	TeamID string
}

// Allows reports whether channelID in teamID is within the scope.
func (s Scope) Allows(teamID, channelID string) bool {
	return teamID == s.TeamID && slackapi.ValidateChannelID(channelID) == nil
}

// Service is pinned to one verified runtime snapshot's Web client and scope.
// Every operation names its channel, which must be within the scope. The mutex
// serializes prepare/claim decisions in one process; ClaimSlackEffect remains
// the cross-process submission authority.
type Service struct {
	store        EffectStore
	web          slackapi.Web
	files        slackapi.FileTransfer
	scope        Scope
	maximumBytes int64
	maximumFiles int
	openSource   sourceOpener
	mu           sync.Mutex
}

func New(s EffectStore, web slackapi.Web, scope Scope) *Service {
	return &Service{store: s, web: web, scope: scope}
}

// NewWithFileTransfer adds the separately injectable, bounded upload adapter
// to a normal Slack operation service. Existing text/reaction callers remain
// on New; file uploads are unavailable until this constructor is used.
func NewWithFileTransfer(s EffectStore, web slackapi.Web, files slackapi.FileTransfer, scope Scope, maximumBytes int64, maximumFiles int) *Service {
	return &Service{
		store: s, web: web, files: files, scope: scope,
		maximumBytes: maximumBytes, maximumFiles: maximumFiles,
		openSource: func(path string, limit int64) (uploadSource, error) {
			source, err := fileio.OpenSource(path, limit)
			if err != nil {
				return nil, err
			}
			return fileSource{source}, nil
		},
	}
}

// History returns one page of an admitted channel. A zero limit defaults to 50.
func (s *Service) History(ctx context.Context, channelID string, limit int, cursor string) (slackapi.MessagePage, error) {
	if limit == 0 {
		limit = 50
	}
	if err := slackapi.ValidateReadLimit(limit); err != nil {
		return slackapi.MessagePage{}, err
	}
	if err := s.ready(); err != nil {
		return slackapi.MessagePage{}, err
	}
	if !s.Allows(s.scope.TeamID, channelID) {
		return slackapi.MessagePage{}, ErrInvalidTarget
	}
	page, err := s.web.History(ctx, slackapi.HistoryRequest{ChannelID: channelID, Limit: limit, Cursor: cursor})
	if err != nil {
		return slackapi.MessagePage{}, readError(err)
	}
	return page, nil
}

// Thread returns one replies page in an admitted channel. A zero limit
// defaults to 50.
func (s *Service) Thread(ctx context.Context, channelID, threadTS string, limit int, cursor string) (slackapi.MessagePage, error) {
	ts, err := slackapi.ParseTimestamp(threadTS)
	if err != nil {
		return slackapi.MessagePage{}, err
	}
	if limit == 0 {
		limit = 50
	}
	if err := slackapi.ValidateThreadReadLimit(limit); err != nil {
		return slackapi.MessagePage{}, err
	}
	if err := s.ready(); err != nil {
		return slackapi.MessagePage{}, err
	}
	if !s.Allows(s.scope.TeamID, channelID) {
		return slackapi.MessagePage{}, ErrInvalidTarget
	}
	page, err := s.web.Replies(ctx, slackapi.RepliesRequest{ChannelID: channelID, ThreadTS: ts, Limit: limit, Cursor: cursor})
	if err != nil {
		return slackapi.MessagePage{}, readError(err)
	}
	return page, nil
}

// Allows reports whether a durable source or target belongs to this active
// Slack workspace and channel scope. It prevents work from being redirected to
// a workspace or channel that a reload removed.
func (s *Service) Allows(teamID, channelID string) bool {
	return s != nil && s.scope.Allows(teamID, channelID)
}

// TeamID is the verified workspace of this service.
func (s *Service) TeamID() string { return s.scope.TeamID }

// Execute durably prepares, claims, and executes one public Slack mutation.
// It never retries a submitted effect, including after an uncertain response.
func (s *Service) Execute(ctx context.Context, requestID, channelID string, intent store.SlackEffectIntent) (store.SlackEffect, error) {
	if err := s.ready(); err != nil {
		return store.SlackEffect{}, err
	}
	if !s.Allows(s.scope.TeamID, channelID) {
		return store.SlackEffect{}, ErrInvalidTarget
	}
	if err := s.preflight(ctx, channelID, intent); err != nil {
		return store.SlackEffect{}, err
	}
	return s.executePrepared(ctx, requestID, channelID, intent)
}

// Retry creates a linked prepared effect through the store then executes it.
// The predecessor is never changed or re-submitted.
func (s *Service) Retry(ctx context.Context, requestID, predecessor, reason string, confirmDuplicateRisk bool) (store.SlackEffect, error) {
	if err := s.ready(); err != nil {
		return store.SlackEffect{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	prior, err := s.store.GetSlackEffect(ctx, predecessor)
	if err != nil {
		return store.SlackEffect{}, err
	}
	if !s.Allows(prior.TeamID, prior.ChannelID) {
		return prior, ErrInvalidTarget
	}
	var sources []preparedUploadSource
	if prior.Kind == store.SlackEffectFileUpload {
		if err := s.readyUpload(); err != nil {
			return store.SlackEffect{}, err
		}
		sources, err = s.openStoredUploadSources(ctx, prior.ID)
		if err != nil {
			return prior, err
		}
		defer closePreparedSources(sources)
	}
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
	if effect.Kind == store.SlackEffectFileUpload {
		if err := s.bindUploadSourceIDs(ctx, effect.ID, sources); err != nil {
			return effect, err
		}
		return s.claimAndUpload(ctx, effect, sources)
	}
	if err := s.preflightEffect(ctx, effect); err != nil {
		return effect, err
	}
	return s.claimAndCall(ctx, effect)
}

// ExecuteFileUpload securely opens and hashes every ordered input before it
// commits the exact file_upload intent. No Slack request can precede that
// transaction, and no completion can follow a changed source.
func (s *Service) ExecuteFileUpload(ctx context.Context, requestID, channelID string, intent store.SlackFileUploadIntent) (store.SlackEffect, error) {
	if err := s.readyUpload(); err != nil {
		return store.SlackEffect{}, err
	}
	if !s.Allows(s.scope.TeamID, channelID) {
		return store.SlackEffect{}, ErrInvalidTarget
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sources, err := s.openUploadIntent(intent)
	if err != nil {
		return store.SlackEffect{}, err
	}
	defer closePreparedSources(sources)
	intent.Files = uploadFiles(sources)
	result, err := s.store.PrepareSlackFileUpload(ctx, requestID, store.SlackEffectTarget{TeamID: s.scope.TeamID, ChannelID: channelID}, intent)
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
	if err := s.bindUploadSourceIDs(ctx, effect.ID, sources); err != nil {
		return effect, err
	}
	return s.claimAndUpload(ctx, effect, sources)
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

func (s *Service) executePrepared(ctx context.Context, requestID, channelID string, intent store.SlackEffectIntent) (store.SlackEffect, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	result, err := s.store.PrepareSlackEffect(ctx, requestID, store.SlackEffectTarget{TeamID: s.scope.TeamID, ChannelID: channelID}, intent)
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
	if !s.Allows(effect.TeamID, effect.ChannelID) {
		return effect, ErrInvalidTarget
	}
	if effect.Kind == store.SlackEffectFileUpload {
		if err := s.readyUpload(); err != nil {
			return effect, err
		}
		sources, err := s.openStoredUploadSources(ctx, effect.ID)
		if err != nil {
			if errors.Is(err, ErrUnavailable) {
				return effect, err
			}
			// A recovered prepared upload has not crossed a Slack boundary. Record
			// its local mismatch conclusively and keep recovering unrelated work.
			failed, failErr := s.store.FailPreparedSlackFileUpload(context.WithoutCancel(ctx), effect.ID, store.SlackEffectDiagnosticFileChanged)
			if failErr != nil {
				return effect, failErr
			}
			return failed, terminalError(failed)
		}
		defer closePreparedSources(sources)
		return s.claimAndUpload(ctx, effect, sources)
	}
	if err := s.preflightEffect(ctx, effect); err != nil {
		return effect, err
	}
	return s.claimAndCall(ctx, effect)
}

func (s *Service) claimAndUpload(ctx context.Context, effect store.SlackEffect, sources []preparedUploadSource) (store.SlackEffect, error) {
	if err := ctx.Err(); err != nil {
		return effect, err // remains prepared
	}
	for _, source := range sources {
		if err := source.source.Revalidate(); err != nil {
			failed, failErr := s.store.FailPreparedSlackFileUpload(context.WithoutCancel(ctx), effect.ID, store.SlackEffectDiagnosticFileChanged)
			if failErr != nil {
				return effect, failErr
			}
			return failed, terminalError(failed)
		}
	}
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

	readers := make([]*sourceReader, len(sources))
	request := slackapi.UploadRequest{ChannelID: claimed.ChannelID, ThreadTS: slackapi.Timestamp(claimed.ThreadTS), InitialComment: claimed.Text, Files: make([]slackapi.UploadSource, len(sources))}
	for index, prepared := range sources {
		reader := newSourceReader(prepared.source)
		readers[index] = reader
		request.Files[index] = slackapi.UploadSource{
			Name: prepared.file.OriginalName, SizeBytes: prepared.file.SizeBytes, Reader: reader,
			ValidateAfter: func(context.Context) error {
				return reader.Wait()
			},
		}
	}
	result, uploadErr := s.files.Upload(ctx, request)
	for _, reader := range readers {
		reader.Close()
	}
	outcome := store.SlackEffectOutcome{State: store.SlackEffectConfirmed}
	var uploaded []store.FileUploadOutcome
	if uploadErr != nil {
		if errors.Is(uploadErr, fileio.ErrChanged) || errors.Is(uploadErr, fileio.ErrUnsafePath) || errors.Is(uploadErr, fileio.ErrUnsafeFileType) {
			outcome = store.SlackEffectOutcome{State: store.SlackEffectFailed, DiagnosticCode: store.SlackEffectDiagnosticFileChanged}
		} else {
			failure := slackapi.Classify(uploadErr, slackapi.FailureAfterSubmit)
			state := store.SlackEffectAmbiguous
			if failure.Class == slackapi.FailureConclusive {
				state = store.SlackEffectFailed
			}
			outcome = store.SlackEffectOutcome{State: state, DiagnosticCode: storeDiagnostic(failure.Diagnostic)}
		}
	} else if len(result.Files) != len(sources) {
		outcome = store.SlackEffectOutcome{State: store.SlackEffectAmbiguous, DiagnosticCode: store.SlackEffectDiagnosticMalformedResponse}
	} else {
		uploaded = make([]store.FileUploadOutcome, len(sources))
		for index, file := range result.Files {
			uploaded[index] = store.FileUploadOutcome{FileID: sources[index].file.FileID, SlackFileID: file.ID, Permalink: file.Permalink}
		}
	}
	completed, commitErr := s.store.CommitSlackFileUploadOutcome(context.WithoutCancel(ctx), claimed.ID, outcome, uploaded)
	if commitErr != nil {
		return claimed, commitErr
	}
	return completed, terminalError(completed)
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

func (s *Service) preflight(ctx context.Context, channelID string, intent store.SlackEffectIntent) error {
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
	_, err = s.web.Permalink(ctx, slackapi.PermalinkRequest{ChannelID: channelID, MessageTS: parsed})
	if err != nil {
		return readError(err)
	}
	return nil
}
func (s *Service) preflightEffect(ctx context.Context, effect store.SlackEffect) error {
	if !s.Allows(effect.TeamID, effect.ChannelID) {
		return ErrInvalidTarget
	}
	return s.preflight(ctx, effect.ChannelID, store.SlackEffectIntent{Kind: effect.Kind, TargetMessageTS: effect.TargetMessageTS, ThreadTS: effect.ThreadTS})
}

func (s *Service) call(ctx context.Context, effect store.SlackEffect) store.SlackEffectOutcome {
	var err error
	var result slackapi.MessageResult
	switch effect.Kind {
	case store.SlackEffectSend:
		result, err = s.web.PostMessage(ctx, slackapi.PostMessageRequest{ChannelID: effect.ChannelID, ThreadTS: slackapi.Timestamp(effect.ThreadTS), Text: effect.Text, ClientMessageID: effect.IdempotencyKey})
	case store.SlackEffectEdit:
		result, err = s.web.UpdateMessage(ctx, slackapi.UpdateMessageRequest{ChannelID: effect.ChannelID, MessageTS: slackapi.Timestamp(effect.TargetMessageTS), Text: effect.Text})
	case store.SlackEffectDelete:
		err = s.web.DeleteMessage(ctx, slackapi.DeleteMessageRequest{ChannelID: effect.ChannelID, MessageTS: slackapi.Timestamp(effect.TargetMessageTS)})
	case store.SlackEffectReact:
		err = s.web.AddReaction(ctx, slackapi.ReactionRequest{ChannelID: effect.ChannelID, MessageTS: slackapi.Timestamp(effect.TargetMessageTS), Emoji: effect.Emoji})
	case store.SlackEffectUnreact:
		err = s.web.RemoveReaction(ctx, slackapi.ReactionRequest{ChannelID: effect.ChannelID, MessageTS: slackapi.Timestamp(effect.TargetMessageTS), Emoji: effect.Emoji})
	default:
		err = slackapi.NewExternalError(slackapi.ReasonInvalidResponse, slackapi.DiagnosticInvalidRequest, "", nil)
	}
	if err == nil && (effect.Kind == store.SlackEffectSend || effect.Kind == store.SlackEffectEdit) && (result.ChannelID != effect.ChannelID || !result.MessageTS.Valid()) {
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
	return s.validTarget()
}

func (s *Service) readyUpload() error {
	if s == nil || s.store == nil || s.files == nil || s.openSource == nil || s.maximumBytes < 1 || s.maximumFiles < 1 {
		return ErrUnavailable
	}
	return s.validTarget()
}

func (s *Service) validTarget() error {
	if err := slackapi.ValidateTeamID(s.scope.TeamID); err != nil {
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

// preparedUploadSource is held from pre-prepare until the attempt finishes.
// Keeping that descriptor is what binds staging to the exact hashed inode.
type preparedUploadSource struct {
	file   store.SlackUploadFile
	source uploadSource
}

func (s *Service) openUploadIntent(intent store.SlackFileUploadIntent) ([]preparedUploadSource, error) {
	if len(intent.Files) == 0 || len(intent.Files) > s.maximumFiles || len(intent.Files) > maxUploadFiles {
		return nil, fmt.Errorf("invalid upload file count")
	}
	result := make([]preparedUploadSource, 0, len(intent.Files))
	for _, input := range intent.Files {
		source, err := s.openSource(input.SourcePath, s.maximumBytes)
		if err != nil {
			closePreparedSources(result)
			return nil, err
		}
		name := filepath.Base(source.Path())
		if name == "." || name == string(filepath.Separator) || name == "" || len(name) > 1024 || !utf8.ValidString(name) {
			source.Close()
			closePreparedSources(result)
			return nil, fmt.Errorf("invalid upload file name")
		}
		device, inode := source.Device(), source.Inode()
		if device > math.MaxInt64 || inode > math.MaxInt64 {
			source.Close()
			closePreparedSources(result)
			return nil, fmt.Errorf("invalid source identity")
		}
		d, i, m := int64(device), int64(inode), source.MtimeNS()
		result = append(result, preparedUploadSource{source: source, file: store.SlackUploadFile{
			SourcePath: source.Path(), OriginalName: name, SizeBytes: source.PreparedSize(), SHA256: source.PreparedSHA256(),
			SourceDevice: &d, SourceInode: &i, SourceMTimeNS: &m,
		}})
	}
	return result, nil
}

func (s *Service) bindUploadSourceIDs(ctx context.Context, effectID string, sources []preparedUploadSource) error {
	files, err := s.outgoingFiles(ctx, effectID)
	if err != nil {
		return err
	}
	if len(files) != len(sources) {
		return fmt.Errorf("stored upload file count mismatch")
	}
	for index, file := range files {
		if file.FileID == "" || file.SourcePath != sources[index].file.SourcePath || file.OriginalName != sources[index].file.OriginalName || file.SizeBytes != sources[index].file.SizeBytes || file.SHA256 != sources[index].file.SHA256 {
			return fmt.Errorf("stored upload file mismatch")
		}
		sources[index].file.FileID = file.FileID
	}
	return nil
}

func uploadFiles(sources []preparedUploadSource) []store.SlackUploadFile {
	files := make([]store.SlackUploadFile, len(sources))
	for index, source := range sources {
		files[index] = source.file
	}
	return files
}

func closePreparedSources(sources []preparedUploadSource) {
	for _, source := range sources {
		_ = source.source.Close()
	}
}

// openStoredUploadSources is used only for recovery and retry. Recovery is
// intentionally content-based across process death: inode/device changes are
// accepted only when the same lexical path still yields the prepared size and
// hash. A live attempt still calls Revalidate before submission/completion.
func (s *Service) openStoredUploadSources(ctx context.Context, effectID string) ([]preparedUploadSource, error) {
	files, err := s.outgoingFiles(ctx, effectID)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 || len(files) > s.maximumFiles || len(files) > maxUploadFiles {
		return nil, fmt.Errorf("invalid stored upload files")
	}
	result := make([]preparedUploadSource, 0, len(files))
	for _, file := range files {
		if file.FileID == "" || file.SHA256 == "" || file.SourcePath == "" {
			closePreparedSources(result)
			return nil, fmt.Errorf("invalid stored upload file")
		}
		source, err := s.openSource(file.SourcePath, s.maximumBytes)
		if err != nil {
			closePreparedSources(result)
			return nil, err
		}
		if source.PreparedSize() != file.SizeBytes || source.PreparedSHA256() != file.SHA256 {
			source.Close()
			closePreparedSources(result)
			return nil, fileio.ErrChanged
		}
		prepared := preparedUploadSource{source: source, file: store.SlackUploadFile{
			FileID: file.FileID, SourcePath: file.SourcePath, OriginalName: file.OriginalName,
			SizeBytes: file.SizeBytes, SHA256: file.SHA256,
			SourceDevice: file.SourceDevice, SourceInode: file.SourceInode, SourceMTimeNS: file.SourceMTimeNS,
		}}
		result = append(result, prepared)
	}
	return result, nil
}

// outgoingFiles delegates exact, ordered source inspection to the durable
// store boundary. The returned data contains no bytes or remote URLs.
func (s *Service) outgoingFiles(ctx context.Context, effectID string) ([]store.SlackEffectOutgoingSource, error) {
	return s.store.InspectSlackFileUploadSources(ctx, effectID)
}

// sourceReader starts its pipe only if an adapter actually stages the source.
// Closing unconsumed readers prevents a reserve failure from leaking a blocked
// goroutine. Wait performs the post-stage content and identity validation that
// must finish immediately before the adapter completes the upload.
type sourceReader struct {
	source uploadSource
	once   sync.Once
	reader *io.PipeReader
	writer *io.PipeWriter
	done   chan struct{}
	err    error
}

func newSourceReader(source uploadSource) *sourceReader {
	return &sourceReader{source: source, done: make(chan struct{})}
}

func (r *sourceReader) start() {
	r.once.Do(func() {
		r.reader, r.writer = io.Pipe()
		go func() {
			r.err = r.source.StreamTo(r.writer)
			_ = r.writer.CloseWithError(r.err)
			close(r.done)
		}()
	})
}

func (r *sourceReader) Read(p []byte) (int, error) {
	r.start()
	return r.reader.Read(p)
}

func (r *sourceReader) Wait() error {
	r.start()
	<-r.done
	if r.err != nil {
		return r.err
	}
	return r.source.RehashAndRevalidate()
}

func (r *sourceReader) Close() {
	r.start()
	_ = r.reader.Close()
	<-r.done
}

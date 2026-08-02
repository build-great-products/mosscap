// Package fake provides deterministic, in-memory slackapi implementations.
package fake

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"

	"github.com/stayradiated/mosscap/internal/slackapi"
)

// Operation names one Web contract method.
type Operation string

const (
	Auth           Operation = "auth"
	Team           Operation = "team"
	User           Operation = "user"
	Channel        Operation = "channel"
	Scopes         Operation = "scopes"
	Permalink      Operation = "permalink"
	History        Operation = "history"
	Replies        Operation = "replies"
	PostMessage    Operation = "post_message"
	UpdateMessage  Operation = "update_message"
	DeleteMessage  Operation = "delete_message"
	AddReaction    Operation = "add_reaction"
	RemoveReaction Operation = "remove_reaction"
)

// CallFault is a safe, deterministic failure injected around one Web call.
type CallFault string

const (
	// Timeout returns context.DeadlineExceeded. It models a call deadline
	// without retaining any request or response body.
	Timeout CallFault = "timeout"
	// Disconnect returns a classified Slack disconnect error.
	Disconnect CallFault = "disconnect"
)

// CallScript delays a call at its phase and then injects Fault. Barrier is
// optional; when supplied, it reports that the phase was reached and blocks
// until released or the call context is canceled. A zero-valued script passes.
type CallScript struct {
	Barrier *Barrier
	Fault   CallFault
}

func (s CallScript) apply(ctx context.Context) error {
	if err := s.Barrier.Wait(ctx); err != nil {
		return err
	}
	switch s.Fault {
	case "":
		return nil
	case Timeout:
		return context.DeadlineExceeded
	case Disconnect:
		return slackapi.NewExternalError(slackapi.ReasonDisconnected, slackapi.DiagnosticDisconnected, "", nil)
	default:
		return fmt.Errorf("fake Slack Web: unknown call fault %q", s.Fault)
	}
}

// Step is one expected call and scripted result. A nil Request accepts any
// request. Result must be assignable to the method's result DTO. PreCall runs
// after this step has been selected but before the request is recorded or the
// step is consumed; a pre-call fault therefore proves no Web call occurred.
// PostCall runs after exact request capture and consumption, modelling an
// unknown outcome after submission. Both scripts preserve strict FIFO order.
type Step struct {
	Operation Operation
	Request   any
	Result    any
	Err       error
	PreCall   CallScript
	PostCall  CallScript
}

// Call is one observed SDK-free request.
type Call struct {
	Operation Operation
	Request   any
}

// Web executes Steps in strict FIFO order and is safe for concurrent tests.
type Web struct {
	mu    sync.Mutex
	steps []Step
	calls []Call
}

var _ slackapi.Web = (*Web)(nil)

func NewWeb(steps ...Step) *Web {
	return &Web{steps: append([]Step(nil), steps...)}
}

func (w *Web) Auth(ctx context.Context) (slackapi.AuthIdentity, error) {
	var result slackapi.AuthIdentity
	err := w.invoke(ctx, Auth, struct{}{}, &result)
	return result, err
}

func (w *Web) Team(ctx context.Context, id string) (slackapi.Team, error) {
	var result slackapi.Team
	err := w.invoke(ctx, Team, id, &result)
	return result, err
}

func (w *Web) User(ctx context.Context, id string) (slackapi.User, error) {
	var result slackapi.User
	err := w.invoke(ctx, User, id, &result)
	return result, err
}

func (w *Web) Channel(ctx context.Context, id string) (slackapi.Channel, error) {
	var result slackapi.Channel
	err := w.invoke(ctx, Channel, id, &result)
	return result, err
}

func (w *Web) Scopes(ctx context.Context) ([]slackapi.Scope, error) {
	var result []slackapi.Scope
	err := w.invoke(ctx, Scopes, struct{}{}, &result)
	return result, err
}

func (w *Web) Permalink(ctx context.Context, request slackapi.PermalinkRequest) (string, error) {
	var result string
	err := w.invoke(ctx, Permalink, request, &result)
	return result, err
}

func (w *Web) History(ctx context.Context, request slackapi.HistoryRequest) (slackapi.MessagePage, error) {
	var result slackapi.MessagePage
	err := w.invoke(ctx, History, request, &result)
	return result, err
}

func (w *Web) Replies(ctx context.Context, request slackapi.RepliesRequest) (slackapi.MessagePage, error) {
	var result slackapi.MessagePage
	err := w.invoke(ctx, Replies, request, &result)
	return result, err
}

func (w *Web) PostMessage(ctx context.Context, request slackapi.PostMessageRequest) (slackapi.MessageResult, error) {
	var result slackapi.MessageResult
	err := w.invoke(ctx, PostMessage, request, &result)
	return result, err
}

func (w *Web) UpdateMessage(ctx context.Context, request slackapi.UpdateMessageRequest) (slackapi.MessageResult, error) {
	var result slackapi.MessageResult
	err := w.invoke(ctx, UpdateMessage, request, &result)
	return result, err
}

func (w *Web) DeleteMessage(ctx context.Context, request slackapi.DeleteMessageRequest) error {
	return w.invoke(ctx, DeleteMessage, request, nil)
}

func (w *Web) AddReaction(ctx context.Context, request slackapi.ReactionRequest) error {
	return w.invoke(ctx, AddReaction, request, nil)
}

func (w *Web) RemoveReaction(ctx context.Context, request slackapi.ReactionRequest) error {
	return w.invoke(ctx, RemoveReaction, request, nil)
}

func (w *Web) invoke(ctx context.Context, operation Operation, request, output any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	w.mu.Lock()
	if len(w.steps) == 0 {
		w.mu.Unlock()
		return fmt.Errorf("fake Slack Web: unexpected %s call", operation)
	}
	step := w.steps[0]
	if step.Operation != operation {
		w.mu.Unlock()
		return fmt.Errorf("fake Slack Web: got %s call, expected %s", operation, step.Operation)
	}
	if step.Request != nil && !reflect.DeepEqual(step.Request, request) {
		w.mu.Unlock()
		return fmt.Errorf("fake Slack Web: %s request differs from script", operation)
	}
	// Keep the lock through the pre-call script so a later caller cannot pass
	// this unconsumed FIFO step. Once captured, later calls may proceed while a
	// post-call script models the uncertain response boundary.
	if err := step.PreCall.apply(ctx); err != nil {
		w.mu.Unlock()
		return err
	}
	w.calls = append(w.calls, Call{Operation: operation, Request: request})
	w.steps = w.steps[1:]
	w.mu.Unlock()
	if err := step.PostCall.apply(ctx); err != nil {
		return err
	}
	if step.Err != nil {
		return step.Err
	}
	if output == nil {
		if step.Result != nil {
			return fmt.Errorf("fake Slack Web: %s script has an unexpected result", operation)
		}
		return nil
	}
	if step.Result == nil {
		return nil
	}
	destination := reflect.ValueOf(output)
	source := reflect.ValueOf(step.Result)
	if destination.Kind() != reflect.Pointer || !source.Type().AssignableTo(destination.Elem().Type()) {
		return errors.New("fake Slack Web: scripted result has wrong type")
	}
	destination.Elem().Set(source)
	return nil
}

// Calls returns a snapshot of observed calls.
func (w *Web) Calls() []Call {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]Call(nil), w.calls...)
}

// Remaining returns the number of unconsumed scripted calls.
func (w *Web) Remaining() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.steps)
}

// AssertDone reports an error if scripted calls remain.
func (w *Web) AssertDone() error {
	if remaining := w.Remaining(); remaining != 0 {
		return fmt.Errorf("fake Slack Web: %d scripted calls remain", remaining)
	}
	return nil
}

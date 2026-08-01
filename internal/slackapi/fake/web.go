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

// Step is one expected call and scripted result. A nil Request accepts any
// request. Result must be assignable to the method's result DTO.
type Step struct {
	Operation Operation
	Request   any
	Result    any
	Err       error
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
	defer w.mu.Unlock()
	w.calls = append(w.calls, Call{Operation: operation, Request: request})
	if len(w.steps) == 0 {
		return fmt.Errorf("fake Slack Web: unexpected %s call", operation)
	}
	step := w.steps[0]
	w.steps = w.steps[1:]
	if step.Operation != operation {
		return fmt.Errorf("fake Slack Web: got %s call, expected %s", operation, step.Operation)
	}
	if step.Request != nil && !reflect.DeepEqual(step.Request, request) {
		return fmt.Errorf("fake Slack Web: %s request differs from script", operation)
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

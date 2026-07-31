// Package protocol defines Mosscap's version 1 local wire contract.
package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/stayradiated/mosscap/internal/identity"
)

const Version = 1

const (
	CapabilityStatus       = "status"
	CapabilityDoctor       = "doctor"
	CapabilityLocalTasks   = "local_tasks"
	CapabilityRequestDedup = "request_dedup"
)

var ServerCapabilities = []string{CapabilityStatus, CapabilityDoctor, CapabilityLocalTasks, CapabilityRequestDedup}

var (
	ErrEmptyFrame    = errors.New("protocol: empty frame")
	ErrFrameTooLarge = errors.New("protocol: frame exceeds maximum size")
	ErrInvalidUTF8   = errors.New("protocol: frame is not valid UTF-8")
	ErrInvalidJSON   = errors.New("protocol: invalid JSON object")
)

// Request is the common request envelope. Params is decoded according to Command.
type Request struct {
	ProtocolVersion int             `json:"protocol_version"`
	RequestID       string          `json:"request_id"`
	ClientKind      string          `json:"client_kind"`
	ClientVersion   string          `json:"client_version"`
	Command         string          `json:"command"`
	Params          json.RawMessage `json:"params"`
}

// Response is the common response envelope. Exactly one of Result and Error is present.
type Response struct {
	ProtocolVersion int             `json:"protocol_version"`
	RequestID       string          `json:"request_id"`
	ServerVersion   string          `json:"server_version"`
	ResultType      string          `json:"result_type,omitempty"`
	Result          json.RawMessage `json:"result,omitempty"`
	Error           *Error          `json:"error,omitempty"`
}

type Error struct {
	Code      string            `json:"code"`
	Category  string            `json:"category"`
	Message   string            `json:"message"`
	Retriable bool              `json:"retriable"`
	Details   map[string]string `json:"details,omitempty"`
}

// HelloParams initiates a connection. SupportedVersions must include Version.
type HelloParams struct {
	SupportedVersions []int    `json:"supported_versions"`
	Capabilities      []string `json:"capabilities"`
}

type HelloResult struct {
	SelectedVersion int      `json:"selected_version"`
	Capabilities    []string `json:"capabilities"`
}

type EmptyParams struct{}
type TaskSelector struct {
	Task string `json:"task"`
}
type TaskCreateParams struct {
	Text string `json:"text"`
}
type TaskUpdateParams struct {
	Task string `json:"task"`
	Text string `json:"text"`
}
type TaskTransitionParams struct {
	Task  string  `json:"task"`
	State string  `json:"state"`
	Note  *string `json:"note,omitempty"`
}
type TaskListParams struct {
	State       *string `json:"state,omitempty"`
	IncludeDone bool    `json:"include_done,omitempty"`
	Limit       int     `json:"limit,omitempty"`
	Cursor      string  `json:"cursor,omitempty"`
}

// TaskDTO deliberately represents current task text only; mutation results omit text.
type TaskDTO struct {
	TaskID     string  `json:"task_id"`
	Version    int64   `json:"version"`
	State      string  `json:"state"`
	Text       string  `json:"text"`
	SourceKind string  `json:"source_kind"`
	CreatedAt  string  `json:"created_at"`
	UpdatedAt  string  `json:"updated_at"`
	DoneAt     *string `json:"done_at"`
}
type TaskMutationDTO struct {
	TaskID    string `json:"task_id"`
	Version   int64  `json:"version"`
	State     string `json:"state"`
	Changed   bool   `json:"changed"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}
type TaskListDTO struct {
	Tasks      []TaskDTO `json:"tasks"`
	NextCursor string    `json:"next_cursor,omitempty"`
}

func (r Request) Validate() error {
	if r.ProtocolVersion != Version {
		return fmt.Errorf("protocol: unsupported version %d", r.ProtocolVersion)
	}
	if !identity.ValidRequestID(r.RequestID) {
		return errors.New("protocol: invalid request_id")
	}
	if r.ClientKind != "cli" && r.ClientKind != "extension" {
		return errors.New("protocol: invalid client_kind")
	}
	if r.ClientVersion == "" || len(r.ClientVersion) > 128 {
		return errors.New("protocol: invalid client_version")
	}
	if !validCommand(r.Command) {
		return fmt.Errorf("protocol: unknown command %q", r.Command)
	}
	if len(r.Params) == 0 {
		return errors.New("protocol: missing params")
	}
	return ValidateParams(r.Command, r.Params)
}

func (r Response) Validate() error {
	if r.ProtocolVersion != Version {
		return fmt.Errorf("protocol: unsupported version %d", r.ProtocolVersion)
	}
	if !identity.ValidRequestID(r.RequestID) {
		return errors.New("protocol: invalid request_id")
	}
	if r.ServerVersion == "" || len(r.ServerVersion) > 128 {
		return errors.New("protocol: invalid server_version")
	}
	result := len(r.Result) != 0
	if result == (r.Error != nil) {
		return errors.New("protocol: response must contain exactly one result or error")
	}
	if result && r.ResultType == "" {
		return errors.New("protocol: result missing result_type")
	}
	if !result && r.ResultType != "" {
		return errors.New("protocol: error response has result_type")
	}
	if result && !json.Valid(r.Result) {
		return errors.New("protocol: invalid result JSON")
	}
	if r.Error != nil {
		return r.Error.Validate()
	}
	return nil
}
func (e Error) Validate() error {
	if e.Code == "" || len(e.Code) > 128 || e.Category == "" || len(e.Category) > 32 || e.Message == "" || len(e.Message) > 1024 {
		return errors.New("protocol: invalid error")
	}
	switch e.Category {
	case "usage", "not_found", "conflict", "unsafe", "unavailable", "external_failed", "ambiguous", "unsupported":
	default:
		return errors.New("protocol: invalid error category")
	}
	if len(e.Details) > 16 {
		return errors.New("protocol: too many error details")
	}
	for k, v := range e.Details {
		if k == "" || len(k) > 64 || len(v) > 256 {
			return errors.New("protocol: invalid error details")
		}
	}
	return nil
}

func ValidateParams(command string, raw json.RawMessage) error {
	var target any
	switch command {
	case "hello":
		target = &HelloParams{}
	case "status.get", "doctor.run":
		target = &EmptyParams{}
	case "task.create":
		target = &TaskCreateParams{}
	case "task.list":
		target = &TaskListParams{}
	case "task.get":
		target = &TaskSelector{}
	case "task.update":
		target = &TaskUpdateParams{}
	case "task.transition":
		target = &TaskTransitionParams{}
	default:
		return fmt.Errorf("protocol: unknown command %q", command)
	}
	if err := decodeStrict(raw, target); err != nil {
		return fmt.Errorf("protocol: invalid %s params: %w", command, err)
	}
	switch p := target.(type) {
	case *HelloParams:
		if len(p.SupportedVersions) == 0 || len(p.SupportedVersions) > 16 || len(p.Capabilities) > 32 {
			return errors.New("invalid hello parameters")
		}
		for _, v := range p.SupportedVersions {
			if v <= 0 {
				return errors.New("invalid hello version")
			}
		}
	case *TaskCreateParams:
		if p.Text == "" {
			return errors.New("missing task text")
		}
	case *TaskSelector:
		if p.Task == "" || len(p.Task) > 128 {
			return errors.New("invalid task selector")
		}
	case *TaskUpdateParams:
		if p.Task == "" || p.Text == "" {
			return errors.New("invalid task update")
		}
	case *TaskTransitionParams:
		if p.Task == "" || (p.State != "open" && p.State != "working" && p.State != "done") || (p.Note != nil && len(*p.Note) > 4096) {
			return errors.New("invalid task transition")
		}
	case *TaskListParams:
		if p.State != nil && *p.State != "open" && *p.State != "working" && *p.State != "done" {
			return errors.New("invalid list state")
		}
		if p.Limit < 0 || p.Limit > 1000 || (p.State != nil && p.IncludeDone) {
			return errors.New("invalid task list")
		}
	}
	return nil
}

// DecodeRequest and DecodeResponse reject unknown fields and trailing JSON values.
func DecodeRequest(payload []byte) (Request, error) {
	var r Request
	if err := decodeStrict(payload, &r); err != nil {
		return r, err
	}
	return r, r.Validate()
}
func DecodeResponse(payload []byte) (Response, error) {
	var r Response
	if err := decodeStrict(payload, &r); err != nil {
		return r, err
	}
	return r, r.Validate()
}
func EncodeRequest(r Request) ([]byte, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(r)
}
func EncodeResponse(r Response) ([]byte, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(r)
}

func decodeStrict(b []byte, v any) error {
	if len(bytes.TrimSpace(b)) == 0 || bytes.TrimSpace(b)[0] != '{' {
		return ErrInvalidJSON
	}
	if !utf8.Valid(b) {
		return ErrInvalidUTF8
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidJSON, err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return ErrInvalidJSON
	}
	return nil
}
func validCommand(c string) bool {
	switch c {
	case "hello", "status.get", "doctor.run", "task.create", "task.list", "task.get", "task.update", "task.transition":
		return true
	}
	return false
}

// NewErrorResponse builds a valid typed error envelope.
func NewErrorResponse(requestID, serverVersion, code, category, message string, retriable bool) Response {
	return Response{ProtocolVersion: Version, RequestID: requestID, ServerVersion: serverVersion, Error: &Error{Code: code, Category: category, Message: strings.TrimSpace(message), Retriable: retriable}}
}

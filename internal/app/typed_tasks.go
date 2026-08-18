package app

import (
	"fmt"
	"strings"

	"github.com/stayradiated/mosscap/internal/config"
	"github.com/stayradiated/mosscap/internal/identity"
	"github.com/stayradiated/mosscap/internal/protocol"
)

// taskTyped binds Cobra's already-parsed task leaves directly to protocol DTOs.
// It intentionally retains the legacy task request IDs, text-file safety, and
// mutation JSON envelope while avoiding a second flag.FlagSet parse.
func (a App) taskTyped(command, selector, text, textFile string, textSet, textFileSet, stdin bool, state string, all bool, limit int, note string, done bool, requestID, cursor string, jsonOut bool) error {
	mutation := command == "create" || command == "update" || command == "open" || command == "working" || command == "done" || command == "reply"
	var params any

	switch command {
	case "create":
		body, err := a.readCommandTextSource(text, textFile, textSet, textFileSet, stdin)
		if err != nil {
			return err
		}
		params = protocol.TaskCreateParams{Text: body}
	case "update":
		body, err := a.readCommandTextSource(text, textFile, textSet, textFileSet, stdin)
		if err != nil {
			return err
		}
		params = protocol.TaskUpdateParams{Task: selector, Text: body}
	case "show":
		params = protocol.TaskSelector{Task: selector}
	case "list":
		if state != "" && all {
			return usage("--state and --all are mutually exclusive")
		}
		var selectedState *string
		if state != "" {
			if state != "open" && state != "working" && state != "done" {
				return usage("invalid --state")
			}
			selectedState = &state
		}
		params = protocol.TaskListParams{State: selectedState, IncludeDone: all, Limit: limit, Cursor: cursor}
	case "open", "working", "done":
		// The legacy flag parser always supplied a note pointer, including for
		// an omitted --note; retain that precise wire DTO shape.
		params = protocol.TaskTransitionParams{Task: selector, State: command, Note: &note}
	case "reply":
		body, err := a.readCommandTextSource(text, textFile, textSet, textFileSet, stdin)
		if err != nil {
			return err
		}
		params = protocol.TaskReplyParams{Task: selector, Text: body, Done: done}
	default:
		return fmt.Errorf("unbound typed task command %q", command)
	}

	if !mutation && requestID != "" {
		return usage("--request-id is mutation-only")
	}
	if mutation && requestID == "" {
		id, err := identity.NewGenerator(nil).NewRequestID()
		if err != nil {
			return err
		}
		requestID = id
	}
	if !mutation {
		requestID = "req_read" + strings.ReplaceAll(command, ".", "_")
	}

	c, err := loadCommandConfig()
	if err != nil {
		return err
	}
	if err := checkTaskFrame(c, command, requestID, params, textFileSet, stdin, a.Version); err != nil {
		return err
	}
	capability := protocol.CapabilityLocalTasks
	if command == "reply" {
		capability = protocol.CapabilityTaskReply
	}
	out, err := a.gatewayCall(c, taskProtocolCommand(command), requestID, params, capability)
	if err != nil {
		return mutationJSONError(err, mutation, jsonOut, requestID)
	}
	if mutation {
		out = map[string]any{"request_id": requestID, "result": out}
	}
	return a.render(out, jsonOut)
}

// checkTaskFrame preserves the legacy text-file diagnostic for ordinary task
// mutations. Reply historically guarded every text source, so it must preflight
// literal text too before a client handshake can misclassify it as unavailable.
func checkTaskFrame(c config.Config, command, requestID string, params any, textFile, stdin bool, version string) error {
	if command != "reply" && !textFile && !stdin {
		return nil
	}
	request := protocol.Request{ProtocolVersion: protocol.Version, RequestID: requestID, ClientKind: "cli", ClientVersion: version, Command: taskProtocolCommand(command), Params: mustJSON(params)}
	encoded, err := protocol.EncodeRequest(request)
	if err != nil {
		return err
	}
	if len(encoded) <= c.LocalProtocolMaximumFrame {
		return nil
	}
	if command == "reply" || stdin {
		return usage("request exceeds the protocol frame limit")
	}
	return usage("--text-file exceeds the protocol frame limit")
}

func taskProtocolCommand(command string) string {
	switch command {
	case "show":
		return "task.get"
	case "open", "working", "done":
		return "task.transition"
	default:
		return "task." + command
	}
}

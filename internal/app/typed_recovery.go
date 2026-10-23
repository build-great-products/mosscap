package app

import (
	"fmt"

	"github.com/stayradiated/mosscap/internal/identity"
	"github.com/stayradiated/mosscap/internal/protocol"
)

// effectTyped directly binds Cobra values to the recovery protocol requests.
func (a App) effectTyped(command, effect, state, task string, limit int, cursor, reason string, confirmDuplicateRisk bool, requestID string, jsonOut bool) error {
	mutation := command == "retry"
	var params any
	switch command {
	case "list":
		if state != "" && !contains([]string{"prepared", "submitted", "confirmed", "ambiguous", "failed"}, state) {
			return usage("invalid --state")
		}
		var selected *string
		if state != "" {
			selected = &state
		}
		params = protocol.EffectListParams{State: selected, Task: task, Limit: limit, Cursor: cursor}
	case "inspect":
		params = protocol.EffectSelector{Effect: effect}
	case "retry":
		if reason == "" {
			return usage("effect retry requires EFFECT and --reason TEXT")
		}
		params = protocol.EffectRetryParams{Effect: effect, Reason: reason, ConfirmDuplicateRisk: confirmDuplicateRisk}
	default:
		return usage("unknown effect command " + command)
	}
	if mutation && requestID == "" {
		id, err := identity.NewGenerator(nil).NewRequestID()
		if err != nil {
			return err
		}
		requestID = id
	}
	if !mutation {
		requestID = "req_effect_" + command
	}
	c, err := loadCommandConfig()
	if err != nil {
		return err
	}
	out, err := a.gatewayCall(c, "effect."+command, requestID, params, protocol.CapabilitySlackEffects)
	if err != nil {
		return mutationJSONError(err, mutation, jsonOut, requestID)
	}
	if mutation {
		out = map[string]any{"request_id": requestID, "effect": out}
	}
	if command == "list" {
		continuation := "mosscap effect list"
		if state != "" {
			continuation += " --state " + shellQuote(state)
		}
		if task != "" {
			continuation += " --task " + shellQuote(task)
		}
		if limit == 0 {
			limit = 50
		}
		return a.renderPage(out, jsonOut, continuation+fmt.Sprintf(" --limit %d", limit))
	}
	return a.renderSlackCommand(out, jsonOut, command == "inspect")
}

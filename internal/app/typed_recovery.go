package app

import (
	"github.com/stayradiated/mosscap/internal/config"
	"github.com/stayradiated/mosscap/internal/identity"
	"github.com/stayradiated/mosscap/internal/protocol"
)

// effectTyped directly binds Cobra values to the recovery protocol requests.
// It deliberately does not pass through effect's legacy flag.FlagSet adapter.
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
	return a.renderSlackCommand(out, jsonOut, command == "inspect")
}

// deliveryTyped directly binds Cobra values to the delivery recovery requests.
// The CLI remains gateway-mediated so ownership and duplicate-risk protections
// stay enforced by the gateway.
func (a App) deliveryTyped(command, delivery, state, task string, limit int, cursor, reason, requestID string, jsonOut bool) error {
	mutation := command == "requeue"
	var params any
	switch command {
	case "list":
		if state != "" && !deliveryState(state) {
			return usage("invalid --state")
		}
		var selected *string
		if state != "" {
			selected = &state
		}
		params = protocol.DeliveryListParams{State: selected, Task: task, Limit: limit, Cursor: cursor}
	case "inspect":
		params = protocol.DeliverySelector{Delivery: delivery}
	case "requeue":
		if reason == "" {
			return usage("delivery requeue requires DELIVERY and --reason")
		}
		params = protocol.DeliveryRequeueParams{Delivery: delivery, Reason: reason}
	default:
		return usage("unknown delivery command " + command)
	}
	if mutation && requestID == "" {
		id, err := identity.NewGenerator(nil).NewRequestID()
		if err != nil {
			return err
		}
		requestID = id
	}
	if !mutation {
		requestID = "req_delivery_" + command
	}
	loc, err := config.DefaultLocation()
	if err != nil {
		return err
	}
	c, err := config.Default(loc)
	if err != nil {
		return err
	}
	result, err := a.gatewayCall(c, "delivery."+command, requestID, params, protocol.CapabilityPiDeliveryMVP)
	if err != nil {
		return mutationJSONError(err, mutation, jsonOut, requestID)
	}
	if mutation {
		result = map[string]any{"request_id": requestID, "result": result, "warning": "duplicate insertion is possible"}
	}
	return a.render(result, jsonOut)
}

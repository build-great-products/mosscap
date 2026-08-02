package app

import (
	"flag"
	"fmt"
	"io"

	"github.com/stayradiated/mosscap/internal/config"
	"github.com/stayradiated/mosscap/internal/identity"
	"github.com/stayradiated/mosscap/internal/protocol"
)

// delivery is intentionally gateway-mediated: the short-lived CLI never opens
// SQLite and therefore cannot bypass connection/session ownership checks.
func (a App) delivery(args []string) error {
	if len(args) == 0 || isHelp(args[0]) {
		a.printBranchHelp("delivery", []string{"list", "inspect", "requeue"})
		return nil
	}
	cmd := args[0]
	if cmd != "list" && cmd != "inspect" && cmd != "requeue" {
		return usage(fmt.Sprintf("unknown delivery command %q", cmd))
	}
	fs := newDeliveryFlagSet("delivery " + cmd)
	jsonOut := fs.Bool("json", false, "")
	help := fs.Bool("help", false, "")
	fs.BoolVar(help, "h", false, "")
	state := fs.String("state", "", "")
	reason := fs.String("reason", "", "")
	requestID := fs.String("request-id", "", "")
	if err := fs.Parse(reorderTaskFlags(args[1:])); err != nil {
		return usage(err.Error())
	}
	if *help {
		switch cmd {
		case "list":
			fmt.Fprintln(a.Out, "Usage: mosscap delivery list [--state pending|inserting|persisted|uncertain|rejected] [--json]")
		case "inspect":
			fmt.Fprintln(a.Out, "Usage: mosscap delivery inspect DELIVERY [--json]")
		case "requeue":
			fmt.Fprintln(a.Out, "Usage: mosscap delivery requeue DELIVERY --reason TEXT [--request-id ID] [--json]\n\nWarning: duplicate insertion is possible.")
		}
		return nil
	}
	var params any
	var command string
	mutation := cmd == "requeue"
	switch cmd {
	case "list":
		if fs.NArg() != 0 || *reason != "" || *requestID != "" {
			return usage("delivery list accepts no arguments, --reason, or --request-id")
		}
		var selected *string
		if *state != "" {
			if !deliveryState(*state) {
				return usage("invalid --state")
			}
			selected = state
		}
		params, command = protocol.DeliveryListParams{State: selected}, "delivery.list"
	case "inspect":
		if fs.NArg() != 1 || *state != "" || *reason != "" || *requestID != "" {
			return usage("delivery inspect requires DELIVERY only")
		}
		params, command = protocol.DeliverySelector{Delivery: fs.Arg(0)}, "delivery.inspect"
	case "requeue":
		if fs.NArg() != 1 || *state != "" || *reason == "" {
			return usage("delivery requeue requires DELIVERY and --reason")
		}
		params, command = protocol.DeliveryRequeueParams{Delivery: fs.Arg(0), Reason: *reason}, "delivery.requeue"
	}
	if mutation && *requestID == "" {
		id, err := identity.NewGenerator(nil).NewRequestID()
		if err != nil {
			return err
		}
		*requestID = id
	}
	if !mutation {
		*requestID = "req_delivery_" + cmd
	}
	loc, err := config.DefaultLocation()
	if err != nil {
		return err
	}
	c, err := config.Default(loc)
	if err != nil {
		return err
	}
	result, err := a.gatewayCall(c, command, *requestID, params, protocol.CapabilityPiDeliveryMVP)
	if err != nil {
		return err
	}
	if mutation {
		// Preserve explicit risk wording in both human and JSON modes.
		result = map[string]any{"request_id": *requestID, "result": result, "warning": "duplicate insertion is possible"}
	}
	return a.render(result, *jsonOut)
}

func newDeliveryFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

func deliveryState(v string) bool {
	return v == "pending" || v == "inserting" || v == "persisted" || v == "uncertain" || v == "rejected"
}

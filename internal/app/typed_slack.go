package app

import (
	"github.com/spf13/cobra"

	"github.com/stayradiated/mosscap/internal/identity"
	"github.com/stayradiated/mosscap/internal/protocol"
)

// slackTyped binds Cobra-parsed Slack leaves directly to protocol requests.
// It deliberately shares the established frame check, durable mutation error,
// and renderer behavior with the legacy implementation.
func (a App) slackTyped(command string, cmd *cobra.Command, args []string) error {
	jsonOut, _ := cmd.Flags().GetBool("json")
	mutation := command != "history" && command != "thread"
	requestID := "req_slack_" + command
	if mutation {
		requestID, _ = cmd.Flags().GetString("request-id")
		if requestID == "" {
			id, err := identity.NewGenerator(nil).NewRequestID()
			if err != nil {
				return err
			}
			requestID = id
		}
	}

	var params any
	capability := protocol.CapabilitySlackPagination
	switch command {
	case "history":
		limit, _ := cmd.Flags().GetInt("limit")
		// Cobra's zero default represents the legacy FlagSet's omitted default.
		if !cmd.Flags().Changed("limit") {
			limit = 50
		}
		cursor, _ := cmd.Flags().GetString("cursor")
		params = protocol.SlackHistoryParams{Limit: limit, Cursor: cursor}
	case "thread":
		limit, _ := cmd.Flags().GetInt("limit")
		if !cmd.Flags().Changed("limit") {
			limit = 50
		}
		cursor, _ := cmd.Flags().GetString("cursor")
		params = protocol.SlackThreadParams{ThreadTS: args[0], Limit: limit, Cursor: cursor}
	case "send":
		text, err := a.slackTypedText(cmd)
		if err != nil {
			return err
		}
		thread, _ := cmd.Flags().GetString("thread")
		task, _ := cmd.Flags().GetString("task")
		files, _ := cmd.Flags().GetStringArray("file")
		paths, err := absolutePaths(files)
		if err != nil {
			return err
		}
		params = protocol.SlackSendParams{ThreadTS: thread, Text: text, Task: task, FilePaths: paths}
		if len(files) > 0 {
			capability = protocol.CapabilityFileTransfer
		}
	case "edit":
		text, err := a.slackTypedText(cmd)
		if err != nil {
			return err
		}
		task, _ := cmd.Flags().GetString("task")
		params = protocol.SlackEditParams{MessageTS: args[0], Text: text, Task: task}
	case "delete":
		reason, _ := cmd.Flags().GetString("reason")
		if reason == "" {
			return usage("slack delete requires MESSAGE_TS and --reason TEXT")
		}
		task, _ := cmd.Flags().GetString("task")
		params = protocol.SlackDeleteParams{MessageTS: args[0], Reason: reason, Task: task}
	case "react", "unreact":
		task, _ := cmd.Flags().GetString("task")
		params = protocol.SlackReactionParams{MessageTS: args[0], Emoji: args[1], Task: task}
	default:
		return usage("unknown slack command " + command)
	}
	if mutation {
		capability = protocol.CapabilitySlackEffects
		if command == "send" {
			files, _ := cmd.Flags().GetStringArray("file")
			if len(files) > 0 {
				capability = protocol.CapabilityFileTransfer
			}
		}
	}
	c, err := loadCommandConfig()
	if err != nil {
		return err
	}
	if err := checkSlackTextFrame(c, "", params, command, requestID, a.Version); err != nil {
		return err
	}
	out, err := a.gatewayCall(c, "slack."+command, requestID, params, capability)
	if err != nil {
		return mutationJSONError(err, mutation, jsonOut, requestID)
	}
	if mutation {
		out = map[string]any{"request_id": requestID, "effect": out}
	}
	return a.renderSlackCommand(out, jsonOut, !mutation)
}

func (a App) slackTypedText(cmd *cobra.Command) (string, error) {
	text, _ := cmd.Flags().GetString("text")
	textFile, _ := cmd.Flags().GetString("text-file")
	return a.readCommandText(text, textFile)
}

package app

import (
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/stayradiated/mosscap/internal/config"
	"github.com/stayradiated/mosscap/internal/identity"
	"github.com/stayradiated/mosscap/internal/protocol"
)

// The typed methods below bind administrative Cobra leaves without routing
// through the legacy flag.FlagSet adapters.
func (a App) purgeTyped(before string, dryRun, confirm bool, beforeCount, dryRunCount, confirmCount int, requestID string, jsonOut bool) error {
	if before == "" || beforeCount != 1 || dryRunCount+confirmCount != 1 || (!dryRun && !confirm) {
		return usage("purge requires --before and exactly one of --dry-run or --confirm")
	}
	cutoff, err := protocol.ParsePurgeCutoff(before)
	if err != nil {
		return usage("--before must be RFC3339 with an offset and at most microsecond precision")
	}
	if dryRun && requestID != "" {
		return usage("--request-id is confirm-only")
	}
	if confirm && requestID == "" {
		requestID, err = identity.NewGenerator(nil).NewRequestID()
		if err != nil {
			return err
		}
	}
	if dryRun {
		requestID = "req_purge_preview"
	}
	loc, err := config.DefaultLocation()
	if err != nil {
		return err
	}
	c, err := config.Default(loc)
	if err != nil {
		return err
	}
	mode := "dry_run"
	if confirm {
		mode = "confirm"
		if !jsonOut {
			fmt.Fprintln(a.Err, "Warning: confirmed metadata removal is irreversible.")
		}
	}
	result, err := a.gatewayCall(c, "metadata.purge", requestID, protocol.MetadataPurgeParams{Cutoff: cutoff.Format("2006-01-02T15:04:05.000000Z"), Mode: mode}, protocol.CapabilityMetadataPurge)
	if err != nil {
		return mutationJSONError(err, confirm, jsonOut, requestID)
	}
	if confirm {
		result = map[string]any{"request_id": requestID, "result": result}
	}
	return a.render(result, jsonOut)
}

type trackedFlag struct {
	boolean     bool
	value       string
	occurrences int
}

func newTrackedFlag(boolean bool) *trackedFlag {
	value := ""
	if boolean {
		value = "false"
	}
	return &trackedFlag{boolean: boolean, value: value}
}
func (f *trackedFlag) Set(value string) error {
	if f.boolean {
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			return err
		}
		f.value = strconv.FormatBool(parsed)
	} else {
		f.value = value
	}
	f.occurrences++
	return nil
}
func (f *trackedFlag) String() string { return f.value }
func (f *trackedFlag) Type() string {
	if f.boolean {
		return "bool"
	}
	return "string"
}
func (f *trackedFlag) IsBoolFlag() bool { return f.boolean }

func trackedFlagValue(cmd *cobra.Command, name string) *trackedFlag {
	value, ok := cmd.Flags().Lookup(name).Value.(*trackedFlag)
	if !ok {
		panic("untracked Cobra flag " + name)
	}
	return value
}
func trackedOccurrences(cmd *cobra.Command, name string) int {
	return trackedFlagValue(cmd, name).occurrences
}
func trackedString(cmd *cobra.Command, name string) string { return trackedFlagValue(cmd, name).value }
func trackedBool(cmd *cobra.Command, name string) bool {
	value, _ := strconv.ParseBool(trackedFlagValue(cmd, name).value)
	return value
}

func (a App) trustTyped(command, slackUserID, requestID string, jsonOut bool) error {
	mutation := command != "list"
	if mutation && requestID == "" {
		id, err := identity.NewGenerator(nil).NewRequestID()
		if err != nil {
			return err
		}
		requestID = id
	}
	if !mutation {
		requestID = "req_trust_list"
	}
	loc, err := config.DefaultLocation()
	if err != nil {
		return err
	}
	// Trust remains operable from its fixed endpoint independently of staged
	// configuration edits.
	c, err := config.Default(loc)
	if err != nil {
		return err
	}
	var params any = protocol.EmptyParams{}
	gatewayCommand := "trust.list"
	if mutation {
		params = protocol.TrustMutationParams{SlackUserID: slackUserID}
		gatewayCommand = "trust." + command
	}
	result, err := a.gatewayCall(c, gatewayCommand, requestID, params, protocol.CapabilityTrustedUsers)
	if err != nil {
		return mutationJSONError(err, mutation, jsonOut, requestID)
	}
	if mutation {
		return a.render(map[string]any{"request_id": requestID, "result": result}, jsonOut)
	}
	bytes, err := json.Marshal(result)
	if err != nil {
		return err
	}
	var users protocol.TrustedUserListDTO
	if err := json.Unmarshal(bytes, &users); err != nil {
		return err
	}
	return a.renderTrustedUsers(users, jsonOut)
}

package app

import (
	"encoding/json"
	"fmt"

	"github.com/stayradiated/mosscap/internal/fileio"
	"github.com/stayradiated/mosscap/internal/identity"
	"github.com/stayradiated/mosscap/internal/protocol"
)

// fileShowTyped directly binds the selector Cobra has already parsed. Metadata
// is deliberately rendered through the existing safe Slack read renderer.
func (a App) fileShowTyped(file string, jsonOut bool) error {
	if err := protocol.ValidateParams("file.show", mustJSON(protocol.FileSelector{File: file})); err != nil {
		return usage("invalid file selector")
	}
	c, err := loadCommandConfig()
	if err != nil {
		return err
	}
	out, err := a.gatewayCall(c, "file.show", "req_file_show", protocol.FileSelector{File: file}, protocol.CapabilitySlackSourceMetadata)
	if err != nil {
		return err
	}
	return a.renderSlackCommand(out, jsonOut, false)
}

// fileGetTyped directly binds parsed Cobra values while retaining gateway-owned
// temporary-byte lifecycle and safe explicit output placement.
func (a App) fileGetTyped(file, output string, force bool, requestID string, jsonOut bool) error {
	if force && output == "" {
		return usage("--force requires --output")
	}
	if err := protocol.ValidateParams("file.get", mustJSON(protocol.FileGetParams{File: file})); err != nil {
		return usage("invalid file selector")
	}
	var destination string
	if output != "" {
		paths, err := absolutePaths([]string{output})
		if err != nil {
			return err
		}
		destination = paths[0]
	}
	if requestID == "" {
		id, err := identity.NewGenerator(nil).NewRequestID()
		if err != nil {
			return err
		}
		requestID = id
	}
	c, err := loadCommandConfig()
	if err != nil {
		return err
	}
	result, err := a.gatewayCall(c, "file.get", requestID, protocol.FileGetParams{File: file}, protocol.CapabilityFileTransfer)
	if err != nil {
		return mutationJSONError(err, true, jsonOut, requestID)
	}
	var transfer protocol.FileGetDTO
	encoded, err := json.Marshal(result)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(encoded, &transfer); err != nil || !validFileTransferResult(c, transfer) {
		return &commandError{code: ExitUnsafe, message: "gateway returned an invalid file transfer result"}
	}
	if destination == "" {
		if jsonOut {
			return a.render(map[string]any{"request_id": requestID, "result": transfer}, true)
		}
		fmt.Fprintf(a.Out, "file_id: %s\ntransfer_id: %s\ntemporary_path: %q\nvalidated_size_bytes: %d\nsha256: %s\nexpires_at: %s\nretention: release this private temporary file when finished; it expires at the time shown\n", transfer.FileID, transfer.TransferID, transfer.TemporaryPath, transfer.ValidatedSizeBytes, transfer.SHA256, transfer.ExpiresAt)
		return nil
	}
	if err := fileio.PlaceOutput(transfer.TemporaryPath, destination, transfer.ValidatedSizeBytes, transfer.SHA256, force); err != nil {
		// A successful gateway get is durable even if local placement fails, so
		// retain its request ID and honor the parsed mutation JSON contract.
		return mutationJSONError(outputError(err), true, jsonOut, requestID)
	}
	if releaseID, err := identity.NewGenerator(nil).NewRequestID(); err == nil {
		_, _ = a.gatewayCall(c, "file.release", releaseID, protocol.FileReleaseParams{TransferID: transfer.TransferID}, protocol.CapabilityFileTransfer)
	}
	out := map[string]any{"request_id": requestID, "file_id": transfer.FileID, "destination": destination, "validated_size_bytes": transfer.ValidatedSizeBytes, "sha256": transfer.SHA256}
	if jsonOut {
		return a.render(out, true)
	}
	fmt.Fprintf(a.Out, "file_id: %s\ndestination: %q\nvalidated_size_bytes: %d\nsha256: %s\n", transfer.FileID, destination, transfer.ValidatedSizeBytes, transfer.SHA256)
	return nil
}

// slackFileTyped accepts exactly one already-parsed repeated --file value.
func (a App) slackFileTyped(thread string, files []string, comment, task, requestID string, jsonOut bool) error {
	if len(files) != 1 {
		return usage("slack file requires exactly one --file")
	}
	paths, err := absolutePaths(files)
	if err != nil {
		return err
	}
	if requestID == "" {
		id, err := identity.NewGenerator(nil).NewRequestID()
		if err != nil {
			return err
		}
		requestID = id
	}
	c, err := loadCommandConfig()
	if err != nil {
		return err
	}
	params := protocol.SlackFileParams{ThreadTS: thread, FilePath: paths[0], Comment: comment, Task: task}
	if err := checkSlackTextFrame(c, "", params, "file", requestID, a.Version); err != nil {
		return err
	}
	out, err := a.gatewayCall(c, "slack.file", requestID, params, protocol.CapabilityFileTransfer)
	if err != nil {
		return mutationJSONError(err, true, jsonOut, requestID)
	}
	return a.renderSlackCommand(map[string]any{"request_id": requestID, "effect": out}, jsonOut, false)
}

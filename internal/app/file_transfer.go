package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/stayradiated/mosscap/internal/config"
	"github.com/stayradiated/mosscap/internal/fileio"
	"github.com/stayradiated/mosscap/internal/identity"
	"github.com/stayradiated/mosscap/internal/protocol"
)

// fileGet retrieves one admitted incoming attachment. The gateway owns the
// temporary byte lifecycle; the CLI only performs an explicit safe placement
// when the operator supplies --output.
func (a App) fileGet(args []string) error {
	fs := newCommandFlagSet("file get")
	jsonOut := fs.Bool("json", false, "")
	output := fs.String("output", "", "")
	force := fs.Bool("force", false, "")
	requestID := fs.String("request-id", "", "")
	help := fs.Bool("help", false, "")
	fs.BoolVar(help, "h", false, "")
	if err := fs.Parse(reorderTaskFlags(args)); err != nil {
		return usage(err.Error())
	}
	if *help {
		fmt.Fprintln(a.Out, "Usage: mosscap file get FILE [--output PATH] [--force] [--request-id ID] [--json]\n\nRetrieve an admitted Slack file. Without --output, retain and release the private temporary path before its expiry.")
		return nil
	}
	if fs.NArg() != 1 {
		return usage("file get requires FILE")
	}
	if *force && *output == "" {
		return usage("--force requires --output")
	}
	if err := protocol.ValidateParams("file.get", mustJSON(protocol.FileGetParams{File: fs.Arg(0)})); err != nil {
		return usage("invalid file selector")
	}
	var destination string
	if *output != "" {
		paths, err := absolutePaths([]string{*output})
		if err != nil {
			return err
		}
		destination = paths[0]
	}
	if *requestID == "" {
		id, err := identity.NewGenerator(nil).NewRequestID()
		if err != nil {
			return err
		}
		*requestID = id
	}
	c, err := loadCommandConfig()
	if err != nil {
		return err
	}
	result, err := a.gatewayCall(c, "file.get", *requestID, protocol.FileGetParams{File: fs.Arg(0)}, protocol.CapabilityFileTransfer)
	if err != nil {
		return mutationJSONError(err, true, *jsonOut, *requestID)
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
		if *jsonOut {
			return a.render(map[string]any{"request_id": *requestID, "result": transfer}, true)
		}
		// The temporary path is operator-requested output only on this explicit
		// get. State/status/effect output never receives it.
		fmt.Fprintf(a.Out, "file_id: %s\ntransfer_id: %s\ntemporary_path: %q\nvalidated_size_bytes: %d\nsha256: %s\nexpires_at: %s\nretention: release this private temporary file when finished; it expires at the time shown\n", transfer.FileID, transfer.TransferID, transfer.TemporaryPath, transfer.ValidatedSizeBytes, transfer.SHA256, transfer.ExpiresAt)
		return nil
	}
	if err := fileio.PlaceOutput(transfer.TemporaryPath, destination, transfer.ValidatedSizeBytes, transfer.SHA256, *force); err != nil {
		return outputError(err)
	}
	// The final destination is durably installed before release. A release error
	// only extends private temporary retention and cannot invalidate the output.
	if releaseID, err := identity.NewGenerator(nil).NewRequestID(); err == nil {
		_, _ = a.gatewayCall(c, "file.release", releaseID, protocol.FileReleaseParams{TransferID: transfer.TransferID}, protocol.CapabilityFileTransfer)
	}
	out := map[string]any{"request_id": *requestID, "file_id": transfer.FileID, "destination": destination, "validated_size_bytes": transfer.ValidatedSizeBytes, "sha256": transfer.SHA256}
	if *jsonOut {
		return a.render(out, true)
	}
	fmt.Fprintf(a.Out, "file_id: %s\ndestination: %q\nvalidated_size_bytes: %d\nsha256: %s\n", transfer.FileID, destination, transfer.ValidatedSizeBytes, transfer.SHA256)
	return nil
}

func validFileTransferResult(c config.Config, transfer protocol.FileGetDTO) bool {
	if protocol.ValidateParams("file.get", mustJSON(protocol.FileGetParams{File: transfer.FileID})) != nil || protocol.ValidateParams("file.release", mustJSON(protocol.FileReleaseParams{TransferID: transfer.TransferID})) != nil || transfer.ValidatedSizeBytes < 0 || !validSHA256(transfer.SHA256) || transfer.ExpiresAt == "" || !filepath.IsAbs(transfer.TemporaryPath) || filepath.Clean(transfer.TemporaryPath) != transfer.TemporaryPath || !fileio.IsGeneratedName(filepath.Base(transfer.TemporaryPath)) {
		return false
	}
	root, err := filepath.Abs(c.Paths.TmpDir)
	return err == nil && filepath.Dir(transfer.TemporaryPath) == root
}

func validSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, r := range value {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return false
		}
	}
	return true
}

func outputError(err error) error {
	switch {
	case errors.Is(err, fileio.ErrDestinationExists), errors.Is(err, fileio.ErrChanged):
		return &commandError{code: ExitConflict, message: err.Error()}
	case errors.Is(err, fileio.ErrUnsupported):
		return &commandError{code: ExitUnsupported, message: err.Error()}
	case errors.Is(err, fileio.ErrUnsafePath), errors.Is(err, fileio.ErrUnsafeFileType), errors.Is(err, fileio.ErrInvalidTemporary):
		return &commandError{code: ExitUnsafe, message: err.Error()}
	default:
		// Destination placement must fail closed: an unexpected filesystem error
		// is never treated as a successful or retry-safe replacement.
		return &commandError{code: ExitUnsafe, message: "safe output placement failed"}
	}
}

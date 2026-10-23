package app

import (
	"errors"
	"path/filepath"
	"strings"

	"github.com/stayradiated/mosscap/internal/config"
	"github.com/stayradiated/mosscap/internal/fileio"
	"github.com/stayradiated/mosscap/internal/protocol"
)

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

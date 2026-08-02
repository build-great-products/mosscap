// Package identity creates opaque Mosscap identifiers.
package identity

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
)

const randomBytes = 16

// Generator creates IDs using cryptographically secure random bytes.
type Generator struct {
	reader io.Reader
}

// NewGenerator returns a generator backed by reader. A nil reader uses
// crypto/rand.Reader.
func NewGenerator(reader io.Reader) Generator {
	if reader == nil {
		reader = rand.Reader
	}
	return Generator{reader: reader}
}

// IsZero reports whether the generator has not been initialized.
func (g Generator) IsZero() bool { return g.reader == nil }

// NewID returns prefix followed by 128 random bits encoded as raw base64url.
func (g Generator) NewID(prefix string) (string, error) {
	if prefix == "" {
		return "", errors.New("identity prefix must not be empty")
	}

	bytes := make([]byte, randomBytes)
	if _, err := io.ReadFull(g.reader, bytes); err != nil {
		return "", err
	}
	return prefix + base64.RawURLEncoding.EncodeToString(bytes), nil
}

// NewTaskID returns a task ID.
func (g Generator) NewTaskID() (string, error) { return g.NewID("tsk_") }

// NewStateEventID returns a task-state-event ID.
func (g Generator) NewStateEventID() (string, error) { return g.NewID("ste_") }

// NewEffectID returns a Slack effect ID.
func (g Generator) NewEffectID() (string, error) { return g.NewID("eff_") }

// NewDeliveryID returns an opaque durable Pi delivery ID.
func (g Generator) NewDeliveryID() (string, error) { return g.NewID("dlv_") }

// NewFileID returns a durable file-metadata ID.
func (g Generator) NewFileID() (string, error) { return g.NewID("fil_") }

// NewRequestID returns a request ID.
func (g Generator) NewRequestID() (string, error) { return g.NewID("req_") }

// ValidRequestID reports whether id is valid for a caller-supplied request ID.
func ValidRequestID(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '.' || c == '_' || c == '~' || c == '-' {
			continue
		}
		return false
	}
	return true
}

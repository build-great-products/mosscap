package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/stayradiated/mosscap/internal/identity"
)

// ConfigReloadResult is the durable, token-free outcome of a gateway
// configuration activation.
type ConfigReloadResult struct {
	Activated                        bool   `json:"activated"`
	SlackNetworkVerificationDeferred bool   `json:"slack_network_verification_deferred"`
	ActiveConfigGeneration           int64  `json:"active_config_generation"`
	ActiveConfigDigest               string `json:"active_config_digest"`
}

// ConfigReloadError is a deliberately sanitized, deterministic reload error.
// Its Message is persisted so retrying a request ID receives its original
// typed failure even if the staged file is subsequently edited.
type ConfigReloadError struct{ Message string }

func (e *ConfigReloadError) Error() string { return e.Message }

// ReloadConfig executes one verified activation decision once for a staged
// configuration digest. It persists only the SHA-256 digest and a token-free
// result/error; neither TOML bytes nor token values enter SQLite.
//
// apply must not mutate the server's active in-memory configuration. The
// caller publishes it only after this method commits a successful outcome.
func (s *Store) ReloadConfig(ctx context.Context, requestID, digest string, apply func() (ConfigReloadResult, error)) (ConfigReloadResult, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !identity.ValidRequestID(requestID) {
		return ConfigReloadResult{}, false, errors.New("invalid request ID")
	}
	if len(digest) != len("sha256:")+64 {
		return ConfigReloadResult{}, false, errors.New("invalid configuration digest")
	}
	req := request{operation: "config.reload", hash: hash("config.reload", digest)}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ConfigReloadResult{}, false, err
	}
	defer tx.Rollback()
	var operation, inputHash, outcome, resultType, resultJSON, errorCode, errorMessage string
	err = tx.QueryRowContext(ctx, "SELECT operation,input_hash,outcome,coalesce(result_type,''),coalesce(result_json,''),coalesce(error_code,''),coalesce(error_message,'') FROM mutation_requests WHERE request_id=?", requestID).Scan(&operation, &inputHash, &outcome, &resultType, &resultJSON, &errorCode, &errorMessage)
	if err == nil {
		if operation != req.operation || inputHash != req.hash {
			return ConfigReloadResult{}, true, ErrRequestConflict
		}
		if outcome != "success" {
			if errorCode == "config_reload_failed" {
				return ConfigReloadResult{}, true, &ConfigReloadError{Message: errorMessage}
			}
			return ConfigReloadResult{}, true, errors.New("prior reload error")
		}
		if resultType != "config_reload" {
			return ConfigReloadResult{}, true, errors.New("prior mutation result type mismatch")
		}
		var result ConfigReloadResult
		if err := json.Unmarshal([]byte(resultJSON), &result); err != nil {
			return ConfigReloadResult{}, true, err
		}
		return result, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return ConfigReloadResult{}, false, err
	}

	result, applyErr := apply()
	now := s.now()
	expiry := now + RequestRetention.Microseconds()
	if applyErr != nil {
		message := "reload candidate could not be activated"
		var reloadErr *ConfigReloadError
		if errors.As(applyErr, &reloadErr) && reloadErr.Message != "" {
			message = reloadErr.Message
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO mutation_requests(request_id,operation,input_hash,outcome,error_code,error_message,created_at,completed_at,expiry_eligible_at) VALUES(?,?,?,'error','config_reload_failed',?,?,?,?)", requestID, req.operation, req.hash, message, now, now, expiry); err != nil {
			return ConfigReloadResult{}, false, err
		}
		if err := tx.Commit(); err != nil {
			return ConfigReloadResult{}, false, err
		}
		return ConfigReloadResult{}, false, &ConfigReloadError{Message: message}
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return ConfigReloadResult{}, false, err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO mutation_requests(request_id,operation,input_hash,outcome,result_type,result_json,created_at,completed_at,expiry_eligible_at) VALUES(?,?,?,'success','config_reload',?,?,?,?)", requestID, req.operation, req.hash, string(encoded), now, now, expiry); err != nil {
		return ConfigReloadResult{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return ConfigReloadResult{}, false, err
	}
	return result, false, nil
}

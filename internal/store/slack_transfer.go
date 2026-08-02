package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/stayradiated/mosscap/internal/identity"
)

const (
	maxSlackUploadFiles = 20
	maxFilePathBytes    = 4096
)

var (
	ErrFileTransferNotFound = errors.New("file transfer not found")
	ErrFileTransferExpired  = errors.New("file transfer expired")
)

// SlackFileUploadIntent is the complete ordered source intent committed before
// an upload is claimed. Paths are intentionally available only through explicit
// effect inspection APIs, never list/status responses.
type SlackFileUploadIntent struct {
	ThreadTS      string
	Text          string
	RelatedTaskID string
	Files         []SlackUploadFile
}

// FileUploadOutcome atomically records Slack's conclusive child identities.
type FileUploadOutcome struct {
	FileID      string
	SlackFileID string
	Permalink   string
}

func validateUploadFile(file SlackUploadFile) error {
	if file.FileID != "" && !validFullFileID(file.FileID) || !filepath.IsAbs(file.SourcePath) || len(file.SourcePath) > maxFilePathBytes || !utf8.ValidString(file.SourcePath) || strings.IndexByte(file.SourcePath, 0) >= 0 || file.OriginalName == "" || len(file.OriginalName) > 1024 || !utf8.ValidString(file.OriginalName) || strings.IndexByte(file.OriginalName, 0) >= 0 || file.SizeBytes < 0 || !validSHA256(file.SHA256) {
		return invalidInput("invalid Slack upload file intent")
	}
	return nil
}

func validSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	for i := range value {
		if (value[i] < '0' || value[i] > '9') && (value[i] < 'a' || value[i] > 'f') {
			return false
		}
	}
	return true
}

func validFullFileID(fileID string) bool {
	return len(fileID) == 26 && validTypedSelector(fileID, "fil_")
}

func canonicalSlackFileUpload(target SlackEffectTarget, intent SlackFileUploadIntent) error {
	if !validSlackID(target.TeamID, "T") || !validSlackID(target.ChannelID, "CG") || len(intent.Files) == 0 || len(intent.Files) > maxSlackUploadFiles {
		return invalidInput("invalid Slack file upload intent")
	}
	if intent.ThreadTS != "" && !validSlackTimestamp(intent.ThreadTS) {
		return invalidInput("invalid Slack file upload thread timestamp")
	}
	if intent.RelatedTaskID != "" && !validTaskSelector(intent.RelatedTaskID) {
		return invalidInput("invalid related task selector")
	}
	if intent.Text != "" {
		if _, err := normalizeSlackEffectText(intent.Text); err != nil {
			return err
		}
	}
	for _, file := range intent.Files {
		if err := validateUploadFile(file); err != nil {
			return err
		}
	}
	return nil
}

// PrepareSlackFileUpload inserts one file_upload effect, all ordered outgoing
// rows, and the request replay record in one transaction.
func (s *Store) PrepareSlackFileUpload(ctx context.Context, requestID string, target SlackEffectTarget, intent SlackFileUploadIntent) (SlackEffectMutationResult, error) {
	validationErr := canonicalSlackFileUpload(target, intent)
	inputHash := hashSlackFileUploadInput(target, intent)
	return s.mutateSlackEffect(ctx, request{operation: "slack.file_upload.prepare", hash: inputHash}, requestID, func(tx *sql.Tx, now int64) (SlackEffectMutationResult, error) {
		if validationErr != nil {
			return SlackEffectMutationResult{}, validationErr
		}
		resolvedTaskID, err := resolveOptionalTaskID(ctx, tx, intent.RelatedTaskID)
		if err != nil {
			return SlackEffectMutationResult{}, err
		}
		text := intent.Text
		if text != "" {
			text, err = normalizeSlackEffectText(text)
			if err != nil {
				return SlackEffectMutationResult{}, err
			}
		}
		payloadHash := hashSlackFileUploadPayload(target, intent.ThreadTS, text, resolvedTaskID, intent.Files)
		effectID, err := s.ids.NewEffectID()
		if err != nil {
			return SlackEffectMutationResult{}, err
		}
		idempotencyKey := hashCanonicalSlackEffect("slack.effect.idempotency.v1", requestID, payloadHash)
		_, err = tx.ExecContext(ctx, `INSERT INTO slack_effects(effect_id,idempotency_key,related_task_id,kind,team_id,channel_id,thread_ts,text,payload_hash,state,attempt_count,prepared_at_us,created_at_us,updated_at_us) VALUES(?,?,?,?,?,?,?,?,?,'prepared',0,?,?,?)`, effectID, idempotencyKey, nullString(resolvedTaskID), SlackEffectFileUpload, target.TeamID, target.ChannelID, nullString(intent.ThreadTS), nullString(text), payloadHash, now, now, now)
		if err != nil {
			return SlackEffectMutationResult{}, err
		}
		for ordinal, source := range intent.Files {
			if source.FileID == "" {
				source.FileID, err = s.ids.NewFileID()
				if err != nil {
					return SlackEffectMutationResult{}, err
				}
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO files(file_id,direction,original_name,validated_size_bytes,sha256,related_effect_id,effect_ordinal,source_attached,source_path,source_device,source_inode,source_mtime_ns,transfer_state,created_at_us,updated_at_us) VALUES(?,'to_slack',?,?,?,?,?,0,?,?,?,?, 'prepared',?,?)`, source.FileID, source.OriginalName, source.SizeBytes, source.SHA256, effectID, ordinal, source.SourcePath, source.SourceDevice, source.SourceInode, source.SourceMTimeNS, now, now); err != nil {
				return SlackEffectMutationResult{}, err
			}
		}
		return SlackEffectMutationResult{EffectID: effectID, State: SlackEffectPrepared, Kind: SlackEffectFileUpload, TeamID: target.TeamID, ChannelID: target.ChannelID}, nil
	})
}

func hashSlackFileUploadInput(target SlackEffectTarget, intent SlackFileUploadIntent) string {
	values := []string{target.TeamID, target.ChannelID, intent.ThreadTS, intent.Text, intent.RelatedTaskID}
	for _, file := range intent.Files {
		values = append(values, file.SourcePath, file.OriginalName, int64String(file.SizeBytes), file.SHA256, optionalIntString(file.SourceDevice), optionalIntString(file.SourceInode), optionalIntString(file.SourceMTimeNS))
	}
	return hashSlackEffectFields("slack.file_upload.prepare.v1", values...)
}

func hashSlackFileUploadPayload(target SlackEffectTarget, threadTS, text, taskID string, files []SlackUploadFile) string {
	values := []string{target.TeamID, target.ChannelID, threadTS, text, taskID}
	for _, file := range files {
		values = append(values, file.SourcePath, file.OriginalName, int64String(file.SizeBytes), file.SHA256)
	}
	return hashSlackEffectFields("slack.file_upload.payload.v1", values...)
}

func int64String(v int64) string { return strconv.FormatInt(v, 10) }
func optionalIntString(v *int64) string {
	if v == nil {
		return ""
	}
	return int64String(*v)
}

func dereferenceInt(v *int64) int64 {
	if v == nil {
		return 0
	}
	return *v
}

// FailPreparedSlackFileUpload records a conclusively local pre-submit failure.
func (s *Store) FailPreparedSlackFileUpload(ctx context.Context, effectID string, code SlackEffectDiagnosticCode) (SlackEffect, error) {
	if !validFullSlackEffectID(effectID) {
		return SlackEffect{}, ErrSlackEffectNotFound
	}
	if !code.valid() {
		return SlackEffect{}, invalidInput("invalid Slack effect diagnostic code")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return SlackEffect{}, err
	}
	defer tx.Rollback()
	now := s.now()
	result, err := tx.ExecContext(ctx, `UPDATE slack_effects SET state='failed',completed_at_us=max(?,prepared_at_us,updated_at_us),diagnostic_code=?,updated_at_us=max(?,prepared_at_us,updated_at_us) WHERE effect_id=? AND kind='file_upload' AND state='prepared' AND attempt_count=0`, now, code, now, effectID)
	if err != nil {
		return SlackEffect{}, err
	}
	changed, err := rowsChanged(result)
	if err != nil {
		return SlackEffect{}, err
	}
	if !changed {
		if _, err := getSlackEffectExact(ctx, tx, effectID); err != nil {
			return SlackEffect{}, err
		}
		return SlackEffect{}, ErrSlackEffectTransition
	}
	if _, err := tx.ExecContext(ctx, `UPDATE files SET transfer_state='failed',diagnostic_code=?,updated_at_us=? WHERE related_effect_id=? AND direction='to_slack' AND transfer_state='prepared'`, code, now, effectID); err != nil {
		return SlackEffect{}, err
	}
	effect, err := getSlackEffectExact(ctx, tx, effectID)
	if err != nil {
		return SlackEffect{}, err
	}
	if err := tx.Commit(); err != nil {
		return SlackEffect{}, err
	}
	return effect, nil
}

// CommitSlackFileUploadOutcome records child Slack IDs/permalinks with a
// submitted effect. No partial child outcome can become durable.
func (s *Store) CommitSlackFileUploadOutcome(ctx context.Context, effectID string, outcome SlackEffectOutcome, files []FileUploadOutcome) (SlackEffect, error) {
	if !validFullSlackEffectID(effectID) {
		return SlackEffect{}, ErrSlackEffectNotFound
	}
	if err := validateSlackEffectOutcome(outcome); err != nil {
		return SlackEffect{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return SlackEffect{}, err
	}
	defer tx.Rollback()
	effect, err := getSlackEffectExact(ctx, tx, effectID)
	if err != nil {
		return SlackEffect{}, err
	}
	if effect.Kind != SlackEffectFileUpload || effect.State != SlackEffectSubmitted {
		return SlackEffect{}, ErrSlackEffectTransition
	}
	if outcome.ResponseFileID != "" || outcome.ResponseMessageTS != "" {
		return SlackEffect{}, invalidInput("upload outcome uses child file identities")
	}
	current, err := listOutgoingFilesForEffectTx(ctx, tx, effectID)
	if err != nil {
		return SlackEffect{}, err
	}
	if outcome.State == SlackEffectConfirmed {
		if len(files) != len(current) {
			return SlackEffect{}, invalidInput("upload completion file count mismatch")
		}
		byID := make(map[string]FileUploadOutcome, len(files))
		for _, file := range files {
			if !validFullFileID(file.FileID) || !validSlackID(file.SlackFileID, "F") || (file.Permalink != "" && len(file.Permalink) > 4096) {
				return SlackEffect{}, invalidInput("invalid upload completion file")
			}
			if _, exists := byID[file.FileID]; exists {
				return SlackEffect{}, invalidInput("duplicate upload completion file")
			}
			byID[file.FileID] = file
		}
		for _, currentFile := range current {
			file, ok := byID[currentFile.FileID]
			if !ok {
				return SlackEffect{}, invalidInput("upload completion file identity mismatch")
			}
			if _, err := tx.ExecContext(ctx, `UPDATE files SET slack_file_id=?,permalink=?,transfer_state='confirmed',diagnostic_code=NULL,updated_at_us=? WHERE file_id=? AND related_effect_id=? AND transfer_state='submitted'`, file.SlackFileID, nullString(file.Permalink), s.now(), file.FileID, effectID); err != nil {
				return SlackEffect{}, err
			}
		}
	} else if len(files) != 0 {
		return SlackEffect{}, invalidInput("unsuccessful upload cannot contain file identities")
	}
	now := s.now()
	result, err := tx.ExecContext(ctx, `UPDATE slack_effects SET state=?,completed_at_us=max(?,submitted_at_us,updated_at_us),diagnostic_code=?,updated_at_us=max(?,submitted_at_us,updated_at_us) WHERE effect_id=? AND state='submitted' AND kind='file_upload'`, outcome.State, now, nullString(string(outcome.DiagnosticCode)), now, effectID)
	if err != nil {
		return SlackEffect{}, err
	}
	changed, err := rowsChanged(result)
	if err != nil {
		return SlackEffect{}, err
	}
	if !changed {
		return SlackEffect{}, ErrSlackEffectTransition
	}
	if outcome.State != SlackEffectConfirmed {
		if _, err := tx.ExecContext(ctx, `UPDATE files SET transfer_state=?,diagnostic_code=?,updated_at_us=? WHERE related_effect_id=? AND direction='to_slack' AND transfer_state='submitted'`, outcome.State, nullString(string(outcome.DiagnosticCode)), now, effectID); err != nil {
			return SlackEffect{}, err
		}
	}
	updated, err := getSlackEffectExact(ctx, tx, effectID)
	if err != nil {
		return SlackEffect{}, err
	}
	if err := tx.Commit(); err != nil {
		return SlackEffect{}, err
	}
	return updated, nil
}

func listOutgoingFilesForEffectTx(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, effectID string) ([]SlackFile, error) {
	rows, err := q.QueryContext(ctx, slackFileSelect+" WHERE f.direction='to_slack' AND f.related_effect_id=? ORDER BY f.effect_ordinal,f.file_id", effectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var files []SlackFile
	for rows.Next() {
		file, err := scanSlackFile(rows)
		if err != nil {
			return nil, err
		}
		files = append(files, file)
	}
	return files, rows.Err()
}

// InsertFileTemporaryTransfer persists an already-published private transfer.
// The caller must have fsynced the matching generated entry before this call.
func (s *Store) InsertFileTemporaryTransfer(ctx context.Context, transfer FileTemporaryTransfer) error {
	if !validTemporaryTransfer(transfer) {
		return invalidInput("invalid temporary file transfer")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, `INSERT INTO file_temporary_transfers(transfer_id,file_id,generated_name,size_bytes,sha256,expires_at_us,created_at_us) VALUES(?,?,?,?,?,?,?)`, transfer.TransferID, transfer.FileID, transfer.GeneratedName, transfer.SizeBytes, transfer.SHA256, transfer.ExpiresAt.UnixMicro(), transfer.CreatedAt.UnixMicro())
	return err
}

func validTemporaryTransfer(transfer FileTemporaryTransfer) bool {
	return len(transfer.TransferID) == 26 && validTypedSelector(transfer.TransferID, "xfr_") && validFullFileID(transfer.FileID) && validTemporaryGeneratedName(transfer.GeneratedName) && transfer.SizeBytes >= 0 && validSHA256(transfer.SHA256) && !transfer.CreatedAt.IsZero() && !transfer.ExpiresAt.Before(transfer.CreatedAt)
}

// validTemporaryGeneratedName accepts only the basename created by fileio.TempDir:
// mosscap-xfr- followed by 32 lowercase hexadecimal characters (128 bits).
func validTemporaryGeneratedName(name string) bool {
	if len(name) != 44 || !strings.HasPrefix(name, "mosscap-xfr-") {
		return false
	}
	for i := len("mosscap-xfr-"); i < len(name); i++ {
		if (name[i] < '0' || name[i] > '9') && (name[i] < 'a' || name[i] > 'f') {
			return false
		}
	}
	return true
}

func (s *Store) GetFileTemporaryTransfer(ctx context.Context, transferID string) (FileTemporaryTransfer, error) {
	if len(transferID) != 26 || !validTypedSelector(transferID, "xfr_") {
		return FileTemporaryTransfer{}, ErrFileTransferNotFound
	}
	var transfer FileTemporaryTransfer
	var expires, created int64
	err := s.db.QueryRowContext(ctx, `SELECT transfer_id,file_id,generated_name,size_bytes,sha256,expires_at_us,created_at_us FROM file_temporary_transfers WHERE transfer_id=?`, transferID).Scan(&transfer.TransferID, &transfer.FileID, &transfer.GeneratedName, &transfer.SizeBytes, &transfer.SHA256, &expires, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return FileTemporaryTransfer{}, ErrFileTransferNotFound
	}
	if err != nil {
		return FileTemporaryTransfer{}, err
	}
	transfer.ExpiresAt = time.UnixMicro(expires).UTC()
	transfer.CreatedAt = time.UnixMicro(created).UTC()
	return transfer, nil
}

// SlackFileTransferCounts returns only bounded transfer-state inventory; it
// intentionally includes no paths, names, URLs, hashes, or bytes themselves.
func (s *Store) SlackFileTransferCounts(ctx context.Context) (SlackFileTransferCounts, error) {
	counts := SlackFileTransferCounts{Incoming: make(map[string]int64), Outgoing: make(map[string]int64)}
	for _, state := range []string{"metadata_only", "validated", "unavailable", "oversized", "content_changed"} {
		counts.Incoming[state] = 0
	}
	for _, state := range []string{"prepared", "submitted", "confirmed", "failed", "ambiguous"} {
		counts.Outgoing[state] = 0
	}
	rows, err := s.db.QueryContext(ctx, `SELECT direction,transfer_state,count(*) FROM files GROUP BY direction,transfer_state`)
	if err != nil {
		return counts, err
	}
	defer rows.Close()
	for rows.Next() {
		var direction, state string
		var count int64
		if err := rows.Scan(&direction, &state, &count); err != nil {
			return counts, err
		}
		if direction == "from_slack" {
			counts.Incoming[state] = count
		} else {
			counts.Outgoing[state] = count
		}
	}
	if err := rows.Err(); err != nil {
		return counts, err
	}
	err = s.db.QueryRowContext(ctx, `SELECT count(*),coalesce(sum(size_bytes),0) FROM file_temporary_transfers`).Scan(&counts.TemporaryCount, &counts.TemporaryBytes)
	return counts, err
}

func (s *Store) ListExpiredFileTemporaryTransfers(ctx context.Context, before time.Time, limit int) ([]FileTemporaryTransfer, error) {
	if limit < 1 || limit > 1000 {
		return nil, invalidInput("invalid temporary transfer list limit")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT transfer_id,file_id,generated_name,size_bytes,sha256,expires_at_us,created_at_us FROM file_temporary_transfers WHERE expires_at_us<=? ORDER BY expires_at_us,transfer_id LIMIT ?`, before.UnixMicro(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var transfers []FileTemporaryTransfer
	for rows.Next() {
		var t FileTemporaryTransfer
		var expires, created int64
		if err := rows.Scan(&t.TransferID, &t.FileID, &t.GeneratedName, &t.SizeBytes, &t.SHA256, &expires, &created); err != nil {
			return nil, err
		}
		t.ExpiresAt = time.UnixMicro(expires).UTC()
		t.CreatedAt = time.UnixMicro(created).UTC()
		transfers = append(transfers, t)
	}
	return transfers, rows.Err()
}

// DeleteFileTemporaryTransfer removes only the row; lifecycle code must unlink
// the verified generated file first.
func (s *Store) DeleteFileTemporaryTransfer(ctx context.Context, transferID string) error {
	if len(transferID) != 26 || !validTypedSelector(transferID, "xfr_") {
		return ErrFileTransferNotFound
	}
	result, err := s.db.ExecContext(ctx, `DELETE FROM file_temporary_transfers WHERE transfer_id=?`, transferID)
	if err != nil {
		return err
	}
	changed, err := rowsChanged(result)
	if err != nil {
		return err
	}
	if !changed {
		return ErrFileTransferNotFound
	}
	return nil
}

// CommitIncomingFileTransfer atomically records validated incoming identity,
// the ephemeral transfer row, and compact file.get replay evidence.
func (s *Store) CommitIncomingFileTransfer(ctx context.Context, requestID string, transfer FileTemporaryTransfer) (FileGetRequestResult, error) {
	if !identity.ValidRequestID(requestID) || !validTemporaryTransfer(transfer) {
		return FileGetRequestResult{}, invalidInput("invalid incoming file transfer")
	}
	inputHash := hashSlackEffectFields("file.get.v1", transfer.FileID)
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return FileGetRequestResult{}, err
	}
	defer tx.Rollback()
	result, replay, err := replayFileGetRequest(ctx, tx, requestID, inputHash)
	if err != nil {
		return FileGetRequestResult{}, err
	}
	if replay {
		var live bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM file_temporary_transfers WHERE transfer_id=? AND file_id=?)`, result.TransferID, transfer.FileID).Scan(&live); err != nil {
			return FileGetRequestResult{}, err
		}
		if !live {
			return FileGetRequestResult{}, ErrFileTransferExpired
		}
		return result, tx.Commit()
	}
	file, err := scanSlackFile(tx.QueryRowContext(ctx, slackFileSelect+" WHERE f.file_id=? AND f.direction='from_slack'", transfer.FileID))
	if errors.Is(err, sql.ErrNoRows) {
		return FileGetRequestResult{}, ErrNotFound
	}
	if err != nil {
		return FileGetRequestResult{}, err
	}
	if file.SHA256 != "" && file.SHA256 != transfer.SHA256 {
		return FileGetRequestResult{}, invalidInput("incoming file content changed")
	}
	now := s.now()
	if _, err := tx.ExecContext(ctx, `UPDATE files SET validated_size_bytes=?,sha256=?,transfer_state='validated',diagnostic_code=NULL,updated_at_us=max(?,updated_at_us) WHERE file_id=? AND direction='from_slack'`, transfer.SizeBytes, transfer.SHA256, now, transfer.FileID); err != nil {
		return FileGetRequestResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO file_temporary_transfers(transfer_id,file_id,generated_name,size_bytes,sha256,expires_at_us,created_at_us) VALUES(?,?,?,?,?,?,?)`, transfer.TransferID, transfer.FileID, transfer.GeneratedName, transfer.SizeBytes, transfer.SHA256, transfer.ExpiresAt.UnixMicro(), transfer.CreatedAt.UnixMicro()); err != nil {
		return FileGetRequestResult{}, err
	}
	result = FileGetRequestResult{FileID: transfer.FileID, TransferID: transfer.TransferID}
	encoded, err := json.Marshal(result)
	if err != nil {
		return FileGetRequestResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO mutation_requests(request_id,operation,input_hash,outcome,result_type,result_json,created_at,completed_at,expiry_eligible_at) VALUES(?,?,?,'success','file_get',?,?,?,?)`, requestID, "file.get", inputHash, string(encoded), now, now, now+RequestRetention.Microseconds()); err != nil {
		return FileGetRequestResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return FileGetRequestResult{}, err
	}
	return result, nil
}

// ReplayIncomingFileTransfer returns the live replay target. A request record
// whose transfer has been released or expired is intentionally a typed expiry,
// rather than a path to a nonexistent file.
func (s *Store) ReplayIncomingFileTransfer(ctx context.Context, requestID, fileID string) (FileGetRequestResult, error) {
	if !identity.ValidRequestID(requestID) || !validFullFileID(fileID) {
		return FileGetRequestResult{}, invalidInput("invalid file get request")
	}
	inputHash := hashSlackEffectFields("file.get.v1", fileID)
	var result FileGetRequestResult
	var operation, hashValue, resultType, resultJSON string
	err := s.db.QueryRowContext(ctx, `SELECT operation,input_hash,coalesce(result_type,''),coalesce(result_json,'') FROM mutation_requests WHERE request_id=?`, requestID).Scan(&operation, &hashValue, &resultType, &resultJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return FileGetRequestResult{}, ErrFileTransferNotFound
	}
	if err != nil {
		return FileGetRequestResult{}, err
	}
	if operation != "file.get" || hashValue != inputHash {
		return FileGetRequestResult{}, ErrRequestConflict
	}
	if resultType != "file_get" || json.Unmarshal([]byte(resultJSON), &result) != nil || result.FileID != fileID {
		return FileGetRequestResult{}, errors.New("prior mutation result type mismatch")
	}
	if _, err := s.GetFileTemporaryTransfer(ctx, result.TransferID); errors.Is(err, ErrFileTransferNotFound) {
		return FileGetRequestResult{}, ErrFileTransferExpired
	} else if err != nil {
		return FileGetRequestResult{}, err
	}
	return result, nil
}

func replayFileGetRequest(ctx context.Context, tx *sql.Tx, requestID, inputHash string) (FileGetRequestResult, bool, error) {
	var operation, hashValue, resultType, resultJSON string
	err := tx.QueryRowContext(ctx, `SELECT operation,input_hash,coalesce(result_type,''),coalesce(result_json,'') FROM mutation_requests WHERE request_id=?`, requestID).Scan(&operation, &hashValue, &resultType, &resultJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return FileGetRequestResult{}, false, nil
	}
	if err != nil {
		return FileGetRequestResult{}, false, err
	}
	if operation != "file.get" || hashValue != inputHash {
		return FileGetRequestResult{}, false, ErrRequestConflict
	}
	var result FileGetRequestResult
	if resultType != "file_get" || json.Unmarshal([]byte(resultJSON), &result) != nil {
		return FileGetRequestResult{}, false, errors.New("prior mutation result type mismatch")
	}
	return result, true, nil
}

// FileGetRequestResult is the compact replay-safe record for a mutation that
// created bytes. The path is deliberately derived by the service, never stored.
type FileGetRequestResult struct {
	FileID     string `json:"file_id"`
	TransferID string `json:"transfer_id"`
}

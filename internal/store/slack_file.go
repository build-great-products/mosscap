package store

import (
	"context"
	"database/sql"
	"time"
)

// GetSlackFile resolves a typed fil_ selector. Prefix matches must be unique.
// Only metadata-only incoming files attached to a Slack source are exposed by
// this Phase 1 repository.
func (s *Store) GetSlackFile(ctx context.Context, selector string) (SlackFile, error) {
	if !validTypedSelector(selector, "fil_") {
		return SlackFile{}, ErrNotFound
	}
	rows, err := s.db.QueryContext(ctx, slackFileSelect+" WHERE f.file_id=? OR substr(f.file_id,1,length(?))=? ORDER BY f.file_id LIMIT 2", selector, selector, selector)
	if err != nil {
		return SlackFile{}, err
	}
	defer rows.Close()
	var files []SlackFile
	for rows.Next() {
		file, err := scanSlackFile(rows)
		if err != nil {
			return SlackFile{}, err
		}
		files = append(files, file)
	}
	if err := rows.Err(); err != nil {
		return SlackFile{}, err
	}
	if len(files) == 0 {
		return SlackFile{}, ErrNotFound
	}
	if len(files) > 1 {
		return SlackFile{}, ErrAmbiguous
	}
	return files[0], nil
}

// ListSlackFiles returns current incoming metadata for one Slack task in
// stable Slack-file-ID order. A local task selector is not a Slack source.
func (s *Store) ListSlackFiles(ctx context.Context, taskSelector string) ([]SlackFile, error) {
	source, err := s.GetSlackSource(ctx, taskSelector)
	if err != nil {
		return nil, err
	}
	return listSlackFilesForTaskTx(ctx, s.db, source.TaskID)
}

const slackFileSelect = `SELECT f.file_id,f.slack_file_id,f.permalink,f.original_name,f.declared_mime,f.size_bytes,f.related_task_id,f.transfer_state,f.created_at_us,f.updated_at_us
	FROM files f JOIN tasks t ON t.task_id=f.related_task_id JOIN slack_task_sources s ON s.task_id=t.task_id
	AND f.direction='from_slack' AND f.transfer_state='metadata_only' AND f.related_effect_id IS NULL AND f.sha256 IS NULL AND f.temporary_path IS NULL AND t.source_kind='slack'`

type rowsScanner interface {
	Scan(...any) error
}

func scanSlackFile(row rowsScanner) (SlackFile, error) {
	var file SlackFile
	var permalink, mime sql.NullString
	var size sql.NullInt64
	var created, updated int64
	if err := row.Scan(&file.FileID, &file.SlackFileID, &permalink, &file.OriginalName, &mime, &size, &file.RelatedTaskID, &file.TransferState, &created, &updated); err != nil {
		return SlackFile{}, err
	}
	file.Permalink = permalink.String
	file.DeclaredMIME = mime.String
	if size.Valid {
		value := size.Int64
		file.SizeBytes = &value
	}
	file.CreatedAt = time.UnixMicro(created).UTC()
	file.UpdatedAt = time.UnixMicro(updated).UTC()
	return file, nil
}

func listSlackFilesForTaskTx(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, taskID string) ([]SlackFile, error) {
	rows, err := q.QueryContext(ctx, slackFileSelect+" WHERE f.related_task_id=? ORDER BY f.slack_file_id,f.file_id", taskID)
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

func sameIncomingFiles(current []SlackFile, incoming []SlackIncomingFile) bool {
	if len(current) != len(incoming) {
		return false
	}
	incoming = sortedIncomingFiles(incoming)
	for i := range current {
		if current[i].SlackFileID != incoming[i].SlackFileID || current[i].Permalink != incoming[i].Permalink || current[i].OriginalName != incoming[i].OriginalName || current[i].DeclaredMIME != incoming[i].DeclaredMIME || !sameOptionalInt(current[i].SizeBytes, incoming[i].SizeBytes) {
			return false
		}
	}
	return true
}

func sameOptionalInt(a, b *int64) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}

func (s *Store) reconcileIncomingFiles(ctx context.Context, tx *sql.Tx, taskID string, incoming []SlackIncomingFile, now int64) error {
	current, err := listSlackFilesForTaskTx(ctx, tx, taskID)
	if err != nil {
		return err
	}
	bySlackID := make(map[string]SlackFile, len(current))
	for _, file := range current {
		bySlackID[file.SlackFileID] = file
	}
	incoming = sortedIncomingFiles(incoming)
	keep := make(map[string]struct{}, len(incoming))
	for _, file := range incoming {
		keep[file.SlackFileID] = struct{}{}
		if existing, ok := bySlackID[file.SlackFileID]; ok {
			if existing.Permalink != file.Permalink || existing.OriginalName != file.OriginalName || existing.DeclaredMIME != file.DeclaredMIME || !sameOptionalInt(existing.SizeBytes, file.SizeBytes) {
				result, err := tx.ExecContext(ctx, `UPDATE files SET permalink=?,original_name=?,declared_mime=?,size_bytes=?,updated_at_us=?
					WHERE file_id=? AND direction='from_slack' AND transfer_state='metadata_only' AND related_task_id=? AND related_effect_id IS NULL AND sha256 IS NULL AND temporary_path IS NULL`, nullable(file.Permalink), file.OriginalName, nullable(file.DeclaredMIME), file.SizeBytes, now, existing.FileID, taskID)
				if err != nil {
					return err
				}
				changed, err := rowsChanged(result)
				if err != nil {
					return err
				}
				if !changed {
					return ErrSlackInvariant
				}
			}
			continue
		}
		fileID, err := s.ids.NewFileID()
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO files(file_id,direction,slack_file_id,permalink,original_name,declared_mime,size_bytes,sha256,related_task_id,related_effect_id,transfer_state,diagnostic_code,temporary_path,created_at_us,updated_at_us)
			VALUES(?,'from_slack',?,?,?,?,?,NULL,?,NULL,'metadata_only',NULL,NULL,?,?)`, fileID, file.SlackFileID, nullable(file.Permalink), file.OriginalName, nullable(file.DeclaredMIME), file.SizeBytes, taskID, now, now); err != nil {
			return err
		}
	}
	for _, file := range current {
		if _, ok := keep[file.SlackFileID]; ok {
			continue
		}
		result, err := tx.ExecContext(ctx, `DELETE FROM files WHERE file_id=? AND direction='from_slack' AND transfer_state='metadata_only' AND related_task_id=? AND related_effect_id IS NULL AND sha256 IS NULL AND temporary_path IS NULL`, file.FileID, taskID)
		if err != nil {
			return err
		}
		changed, err := rowsChanged(result)
		if err != nil {
			return err
		}
		if !changed {
			return ErrSlackInvariant
		}
	}
	return nil
}

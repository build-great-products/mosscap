package store

import (
	"context"
	"database/sql"
	"time"
)

// GetSlackFile resolves a typed fil_ selector. Detached incoming records remain
// explicitly inspectable; task listings intentionally expose attached records only.
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

// ListSlackFiles returns attached incoming metadata for one Slack task in
// stable Slack-file-ID order. Detached metadata remains available via file ID.
func (s *Store) ListSlackFiles(ctx context.Context, taskSelector string) ([]SlackFile, error) {
	source, err := s.GetSlackSource(ctx, taskSelector)
	if err != nil {
		return nil, err
	}
	return listSlackFilesForTaskTx(ctx, s.db, source.TaskID)
}

const slackFileSelect = `SELECT
	f.file_id,f.direction,coalesce(f.slack_file_id,''),coalesce(f.permalink,''),f.original_name,
	coalesce(f.declared_mime,''),f.declared_size_bytes,f.validated_size_bytes,coalesce(f.sha256,''),
	coalesce(f.related_task_id,''),coalesce(f.related_effect_id,''),f.effect_ordinal,
	f.source_attached,f.source_detached_at_us,coalesce(f.source_path,''),f.source_device,f.source_inode,f.source_mtime_ns,
	f.transfer_state,coalesce(f.diagnostic_code,''),f.created_at_us,f.updated_at_us
FROM files f`

type rowsScanner interface{ Scan(...any) error }

func scanSlackFile(row rowsScanner) (SlackFile, error) {
	var file SlackFile
	var declared, validated, ordinal, detached, device, inode, mtime sql.NullInt64
	var created, updated int64
	if err := row.Scan(&file.FileID, &file.Direction, &file.SlackFileID, &file.Permalink, &file.OriginalName,
		&file.DeclaredMIME, &declared, &validated, &file.SHA256, &file.RelatedTaskID, &file.RelatedEffectID, &ordinal,
		&file.SourceAttached, &detached, &file.SourcePath, &device, &inode, &mtime,
		&file.TransferState, &file.DiagnosticCode, &created, &updated); err != nil {
		return SlackFile{}, err
	}
	file.DeclaredSizeBytes = optionalInt64(declared)
	file.SizeBytes = file.DeclaredSizeBytes
	file.ValidatedSizeBytes = optionalInt64(validated)
	file.EffectOrdinal = optionalInt64(ordinal)
	file.SourceDevice = optionalInt64(device)
	file.SourceInode = optionalInt64(inode)
	file.SourceMTimeNS = optionalInt64(mtime)
	file.CreatedAt = time.UnixMicro(created).UTC()
	file.UpdatedAt = time.UnixMicro(updated).UTC()
	if detached.Valid {
		t := time.UnixMicro(detached.Int64).UTC()
		file.SourceDetachedAt = &t
	}
	return file, nil
}

func optionalInt64(value sql.NullInt64) *int64 {
	if !value.Valid {
		return nil
	}
	v := value.Int64
	return &v
}

func listSlackFilesForTaskTx(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, taskID string) ([]SlackFile, error) {
	return listIncomingSlackFilesForTaskTx(ctx, q, taskID, true)
}

func listAllIncomingSlackFilesForTaskTx(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, taskID string) ([]SlackFile, error) {
	return listIncomingSlackFilesForTaskTx(ctx, q, taskID, false)
}

func listIncomingSlackFilesForTaskTx(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, taskID string, attachedOnly bool) ([]SlackFile, error) {
	query := slackFileSelect + " WHERE f.direction='from_slack' AND f.related_task_id=?"
	if attachedOnly {
		query += " AND f.source_attached=1"
	}
	query += " ORDER BY f.slack_file_id,f.file_id"
	rows, err := q.QueryContext(ctx, query, taskID)
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
		if current[i].SlackFileID != incoming[i].SlackFileID || current[i].Permalink != incoming[i].Permalink || current[i].OriginalName != incoming[i].OriginalName || current[i].DeclaredMIME != incoming[i].DeclaredMIME || !sameOptionalInt(current[i].DeclaredSizeBytes, incoming[i].SizeBytes) {
			return false
		}
	}
	return true
}

func sameOptionalInt(a, b *int64) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}

// reconcileIncomingFiles updates/re-attaches existing identity rows and marks
// removed attachments detached. It never deletes validated outcome evidence.
func (s *Store) reconcileIncomingFiles(ctx context.Context, tx *sql.Tx, taskID string, incoming []SlackIncomingFile, now int64) error {
	current, err := listAllIncomingSlackFilesForTaskTx(ctx, tx, taskID)
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
			result, err := tx.ExecContext(ctx, `UPDATE files SET permalink=?,original_name=?,declared_mime=?,declared_size_bytes=?,source_attached=1,source_detached_at_us=NULL,updated_at_us=? WHERE file_id=? AND direction='from_slack' AND related_task_id=?`, nullable(file.Permalink), file.OriginalName, nullable(file.DeclaredMIME), file.SizeBytes, now, existing.FileID, taskID)
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
			continue
		}
		fileID, err := s.ids.NewFileID()
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO files(file_id,direction,slack_file_id,permalink,original_name,declared_mime,declared_size_bytes,related_task_id,source_attached,transfer_state,created_at_us,updated_at_us) VALUES(?,'from_slack',?,?,?,?,?,?,1,'metadata_only',?,?)`, fileID, file.SlackFileID, nullable(file.Permalink), file.OriginalName, nullable(file.DeclaredMIME), file.SizeBytes, taskID, now, now); err != nil {
			return err
		}
	}
	for _, file := range current {
		if _, ok := keep[file.SlackFileID]; ok || !file.SourceAttached {
			continue
		}
		result, err := tx.ExecContext(ctx, `UPDATE files SET source_attached=0,source_detached_at_us=?,updated_at_us=? WHERE file_id=? AND direction='from_slack' AND related_task_id=? AND source_attached=1`, now, now, file.FileID, taskID)
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

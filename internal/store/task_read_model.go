package store

import (
	"context"
	"database/sql"
	"time"
)

// TaskReadModel is the current task together with optional Slack-origin
// metadata. Local tasks retain their normal Task shape and have no source or
// incoming files.
type TaskReadModel struct {
	Task              Task         `json:"task"`
	SlackSource       *SlackSource `json:"slack_source,omitempty"`
	Files             []SlackFile  `json:"files,omitempty"`
	LatestReplyEffect *SlackEffect `json:"latest_reply_effect,omitempty"`
}

// GetTaskReadModel resolves an exact task ID or an unambiguous prefix and
// includes current Slack source and metadata-only incoming file details when
// the task originated in Slack.
func (s *Store) GetTaskReadModel(ctx context.Context, selector string) (TaskReadModel, error) {
	canonical, ok := canonicalTaskSelector(selector)
	if !ok {
		return TaskReadModel{}, ErrNotFound
	}
	rows, err := s.db.QueryContext(ctx, `SELECT t.task_id,t.version,t.state,t.text,t.source_kind,t.created_at,t.updated_at,t.done_at,
		s.initial_source_event_id,s.team_id,s.channel_id,s.author_user_id,s.message_ts,s.thread_ts,s.permalink,s.latest_event_at_us,s.source_deleted_at_us,s.created_at_us,s.updated_at_us
		FROM tasks t LEFT JOIN slack_task_sources s ON s.task_id=t.task_id
		WHERE t.task_id=? OR substr(t.task_id,1,length(?))=?
		ORDER BY t.task_id LIMIT 2`, canonical, canonical, canonical)
	if err != nil {
		return TaskReadModel{}, err
	}
	defer rows.Close()

	var models []TaskReadModel
	for rows.Next() {
		model, err := scanTaskReadModel(rows)
		if err != nil {
			return TaskReadModel{}, err
		}
		models = append(models, model)
	}
	if err := rows.Err(); err != nil {
		return TaskReadModel{}, err
	}
	if len(models) == 0 {
		return TaskReadModel{}, ErrNotFound
	}
	if len(models) > 1 {
		return TaskReadModel{}, ErrAmbiguous
	}
	model := models[0]
	if model.SlackSource == nil {
		return model, nil
	}
	files, err := listSlackFilesForTaskTx(ctx, s.db, model.Task.ID)
	if err != nil {
		return TaskReadModel{}, err
	}
	model.Files = files
	effect, err := latestSlackReplyEffect(ctx, s.db, model.Task.ID, model.SlackSource)
	if err != nil {
		return TaskReadModel{}, err
	}
	model.LatestReplyEffect = effect
	return model, nil
}

// latestSlackReplyEffect selects only durable send effects addressed to this
// task's exact source thread. "Latest" is updated_at_us descending then
// effect_id descending, so ties are stable and visible via ReplyEffectID.
func latestSlackReplyEffect(ctx context.Context, q rowQueryer, taskID string, source *SlackSource) (*SlackEffect, error) {
	effect, err := scanSlackEffect(q.QueryRowContext(ctx, slackEffectSelect+` WHERE related_task_id=? AND kind='send' AND team_id=? AND channel_id=? AND thread_ts=? ORDER BY updated_at_us DESC,effect_id DESC LIMIT 1`, taskID, source.TeamID, source.ChannelID, source.ThreadTS))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &effect, nil
}

func scanTaskReadModel(row rowsScanner) (TaskReadModel, error) {
	var model TaskReadModel
	var done sql.NullInt64
	var initialEvent, teamID, channelID, authorUserID, messageTS, threadTS, permalink sql.NullString
	var latestEvent, deletedAt, sourceCreated, sourceUpdated sql.NullInt64
	var created, updated int64
	if err := row.Scan(&model.Task.ID, &model.Task.Version, &model.Task.State, &model.Task.Text, &model.Task.SourceKind, &created, &updated, &done,
		&initialEvent, &teamID, &channelID, &authorUserID, &messageTS, &threadTS, &permalink, &latestEvent, &deletedAt, &sourceCreated, &sourceUpdated); err != nil {
		return TaskReadModel{}, err
	}
	model.Task.CreatedAt = time.UnixMicro(created).UTC()
	model.Task.UpdatedAt = time.UnixMicro(updated).UTC()
	if done.Valid {
		value := time.UnixMicro(done.Int64).UTC()
		model.Task.DoneAt = &value
	}
	if !initialEvent.Valid {
		return model, nil
	}
	if !teamID.Valid || !channelID.Valid || !authorUserID.Valid || !messageTS.Valid || !threadTS.Valid || !latestEvent.Valid || !sourceCreated.Valid || !sourceUpdated.Valid || model.Task.SourceKind != "slack" {
		return TaskReadModel{}, ErrSlackInvariant
	}
	source := &SlackSource{
		TaskID: model.Task.ID, InitialSourceEventID: initialEvent.String,
		TeamID: teamID.String, ChannelID: channelID.String, AuthorUserID: authorUserID.String,
		MessageTS: messageTS.String, ThreadTS: threadTS.String, Permalink: permalink.String,
		LatestEventAtUS: latestEvent.Int64, CreatedAt: time.UnixMicro(sourceCreated.Int64).UTC(), UpdatedAt: time.UnixMicro(sourceUpdated.Int64).UTC(),
		TaskVersion: model.Task.Version, TaskState: model.Task.State, TaskText: model.Task.Text,
	}
	if deletedAt.Valid {
		value := time.UnixMicro(deletedAt.Int64).UTC()
		source.SourceDeletedAt = &value
	}
	model.SlackSource = source
	return model, nil
}

// SlackEffectCounts is bounded operational inventory. It deliberately omits
// effect intents, text, reasons, and external response bodies.
type SlackEffectCounts struct {
	Total          int64                      `json:"total"`
	ActionRequired int64                      `json:"action_required"`
	ByState        map[SlackEffectState]int64 `json:"by_state"`
}

// SlackEffectCounts returns counts for every durable state. ActionRequired is
// the subset requiring an explicit operator decision: failed or ambiguous.
func (s *Store) SlackEffectCounts(ctx context.Context) (SlackEffectCounts, error) {
	counts := SlackEffectCounts{ByState: make(map[SlackEffectState]int64)}
	for _, state := range []SlackEffectState{SlackEffectPrepared, SlackEffectSubmitted, SlackEffectConfirmed, SlackEffectFailed, SlackEffectAmbiguous} {
		counts.ByState[state] = 0
	}
	rows, err := s.db.QueryContext(ctx, "SELECT state,count(*) FROM slack_effects GROUP BY state")
	if err != nil {
		return counts, err
	}
	defer rows.Close()
	for rows.Next() {
		var state SlackEffectState
		var count int64
		if err := rows.Scan(&state, &count); err != nil {
			return counts, err
		}
		if !state.valid() {
			return counts, ErrSlackInvariant
		}
		counts.ByState[state] = count
		counts.Total += count
		if state == SlackEffectFailed || state == SlackEffectAmbiguous {
			counts.ActionRequired += count
		}
	}
	if err := rows.Err(); err != nil {
		return counts, err
	}
	return counts, nil
}

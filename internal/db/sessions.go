package db

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/timae/ses/internal/model"
)

func (db *DB) InsertSession(s *model.Session, messages []model.Message) error {
	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	res, err := tx.Exec(`
		INSERT OR REPLACE INTO sessions (
			short_id, source_type, source_id, pid, project, cwd,
			git_branch, git_commit, started_at, ended_at,
			message_count, tool_call_count, first_prompt, last_assistant,
			model, transcript_path, scanned_at, transcript_mtime, transcript_size,
			input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens, api_call_count
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		s.ShortID, s.SourceType, s.SourceID, s.PID, s.Project, s.CWD,
		s.GitBranch, s.GitCommit, s.StartedAt, s.EndedAt,
		s.MessageCount, s.ToolCallCount, s.FirstPrompt, s.LastAssistant,
		s.Model, s.TranscriptPath, time.Now(), s.TranscriptMtime, s.TranscriptSize,
		s.InputTokens, s.OutputTokens, s.CacheCreationTokens, s.CacheReadTokens, s.APICallCount,
	)
	if err != nil {
		return fmt.Errorf("inserting session: %w", err)
	}

	sessionID, _ := res.LastInsertId()

	// Delete old related data (for OR REPLACE case)
	for _, table := range []string{"user_prompts", "session_files", "session_tags", "tool_outputs", "session_usage_daily"} {
		tx.Exec("DELETE FROM "+table+" WHERE session_id = ?", sessionID)
	}

	// Insert per-day token usage rollups
	for _, d := range s.UsageDays {
		tx.Exec(`INSERT OR REPLACE INTO session_usage_daily
			(session_id, day, model, input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens, api_calls)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			sessionID, d.Day, d.Model, d.InputTokens, d.OutputTokens, d.CacheCreationTokens, d.CacheReadTokens, d.APICalls)
	}

	// Insert user prompts
	var promptTexts []string
	for i, p := range s.UserPrompts {
		tx.Exec(`INSERT INTO user_prompts (session_id, ordinal, content) VALUES (?, ?, ?)`,
			sessionID, i, p)
		promptTexts = append(promptTexts, p)
	}

	// Insert tool outputs (tool_result messages) — opt-in surface for
	// recovering what Claude saw from Bash/Read/Grep etc.
	toolOrdinal := 0
	for _, m := range messages {
		if m.Role != "tool_result" || m.Content == "" {
			continue
		}
		tx.Exec(`INSERT INTO tool_outputs (session_id, ordinal, tool_name, file_path, content, size) VALUES (?, ?, ?, ?, ?, ?)`,
			sessionID, toolOrdinal, m.ToolName, m.FilePath, m.Content, len(m.Content))
		toolOrdinal++
	}

	// Insert files
	for _, f := range s.Files {
		tx.Exec(`INSERT OR IGNORE INTO session_files (session_id, file_path, action) VALUES (?, ?, ?)`,
			sessionID, f.FilePath, f.Action)
	}

	// Update FTS
	allPrompts := strings.Join(promptTexts, "\n")
	tx.Exec(`INSERT OR REPLACE INTO session_fts (rowid, short_id, project, first_prompt, last_assistant, prompts_text)
		VALUES (?, ?, ?, ?, ?, ?)`,
		sessionID, s.ShortID, s.Project, s.FirstPrompt, s.LastAssistant, allPrompts)

	return tx.Commit()
}

func (db *DB) GetBySourceID(sourceType model.Source, sourceID string) (*model.Session, error) {
	s := &model.Session{}
	err := db.conn.QueryRow(`
		SELECT id, short_id, source_type, source_id, pid, project, cwd,
			git_branch, git_commit, started_at, ended_at,
			message_count, tool_call_count, first_prompt, last_assistant,
			model, transcript_path, scanned_at, transcript_mtime, transcript_size,
			input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens, api_call_count
		FROM sessions WHERE source_type = ? AND source_id = ?`,
		sourceType, sourceID,
	).Scan(
		&s.ID, &s.ShortID, &s.SourceType, &s.SourceID, &s.PID, &s.Project, &s.CWD,
		&s.GitBranch, &s.GitCommit, &s.StartedAt, &s.EndedAt,
		&s.MessageCount, &s.ToolCallCount, &s.FirstPrompt, &s.LastAssistant,
		&s.Model, &s.TranscriptPath, &s.ScannedAt, &s.TranscriptMtime, &s.TranscriptSize,
		&s.InputTokens, &s.OutputTokens, &s.CacheCreationTokens, &s.CacheReadTokens, &s.APICallCount,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return s, err
}

func (db *DB) DeleteSession(id int64) error {
	_, err := db.conn.Exec("DELETE FROM sessions WHERE id = ?", id)
	return err
}

type ListFilter struct {
	Since   *time.Time
	Until   *time.Time
	Project string
	Source  string
	Tag     string
	Limit   int
}

func (db *DB) ListSessions(f ListFilter) ([]model.Session, error) {
	query := `SELECT s.id, s.short_id, s.source_type, s.source_id, s.project, s.cwd,
		s.git_branch, s.started_at, s.ended_at, s.message_count, s.tool_call_count,
		s.first_prompt, s.model, s.transcript_path
		FROM sessions s`

	var conditions []string
	var args []any

	if f.Tag != "" {
		query += ` JOIN session_tags t ON s.id = t.session_id`
		conditions = append(conditions, "t.tag = ?")
		args = append(args, f.Tag)
	}
	if f.Since != nil {
		conditions = append(conditions, "s.started_at >= ?")
		args = append(args, *f.Since)
	}
	if f.Until != nil {
		conditions = append(conditions, "s.started_at <= ?")
		args = append(args, *f.Until)
	}
	if f.Project != "" {
		conditions = append(conditions, "s.project LIKE ?")
		args = append(args, "%"+f.Project+"%")
	}
	if f.Source != "" {
		conditions = append(conditions, "s.source_type = ?")
		args = append(args, f.Source)
	}

	if len(conditions) > 0 {
		query += " WHERE " + strings.Join(conditions, " AND ")
	}

	query += " ORDER BY s.started_at DESC"

	if f.Limit > 0 {
		query += fmt.Sprintf(" LIMIT %d", f.Limit)
	}

	rows, err := db.conn.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var sessions []model.Session
	for rows.Next() {
		var s model.Session
		if err := rows.Scan(
			&s.ID, &s.ShortID, &s.SourceType, &s.SourceID, &s.Project, &s.CWD,
			&s.GitBranch, &s.StartedAt, &s.EndedAt, &s.MessageCount, &s.ToolCallCount,
			&s.FirstPrompt, &s.Model, &s.TranscriptPath,
		); err != nil {
			return nil, err
		}
		sessions = append(sessions, s)
	}

	// Load tags for each session
	for i := range sessions {
		sessions[i].Tags, _ = db.GetTags(sessions[i].ID)
	}

	return sessions, nil
}

func (db *DB) GetSession(shortID string) (*model.Session, error) {
	s := &model.Session{}
	err := db.conn.QueryRow(`
		SELECT id, short_id, source_type, source_id, pid, project, cwd,
			git_branch, git_commit, started_at, ended_at,
			message_count, tool_call_count, first_prompt, last_assistant,
			model, transcript_path,
			input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens, api_call_count
		FROM sessions WHERE short_id LIKE ?`,
		shortID+"%",
	).Scan(
		&s.ID, &s.ShortID, &s.SourceType, &s.SourceID, &s.PID, &s.Project, &s.CWD,
		&s.GitBranch, &s.GitCommit, &s.StartedAt, &s.EndedAt,
		&s.MessageCount, &s.ToolCallCount, &s.FirstPrompt, &s.LastAssistant,
		&s.Model, &s.TranscriptPath,
		&s.InputTokens, &s.OutputTokens, &s.CacheCreationTokens, &s.CacheReadTokens, &s.APICallCount,
	)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("no session matching %q", shortID)
	}
	if err != nil {
		return nil, err
	}

	s.Tags, _ = db.GetTags(s.ID)
	s.Files, _ = db.GetFiles(s.ID)
	s.UserPrompts, _ = db.GetUserPrompts(s.ID)

	return s, nil
}

func (db *DB) GetTags(sessionID int64) ([]string, error) {
	rows, err := db.conn.Query("SELECT tag FROM session_tags WHERE session_id = ?", sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tags []string
	for rows.Next() {
		var tag string
		rows.Scan(&tag)
		tags = append(tags, tag)
	}
	return tags, nil
}

func (db *DB) GetFiles(sessionID int64) ([]model.SessionFile, error) {
	rows, err := db.conn.Query("SELECT file_path, action FROM session_files WHERE session_id = ?", sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var files []model.SessionFile
	for rows.Next() {
		var f model.SessionFile
		rows.Scan(&f.FilePath, &f.Action)
		files = append(files, f)
	}
	return files, nil
}

func (db *DB) GetUserPrompts(sessionID int64) ([]string, error) {
	rows, err := db.conn.Query("SELECT content FROM user_prompts WHERE session_id = ? ORDER BY ordinal", sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var prompts []string
	for rows.Next() {
		var p string
		rows.Scan(&p)
		prompts = append(prompts, p)
	}
	return prompts, nil
}

func (db *DB) AddTags(shortID string, tags []string) error {
	s, err := db.GetSession(shortID)
	if err != nil {
		return err
	}
	for _, tag := range tags {
		db.conn.Exec("INSERT OR IGNORE INTO session_tags (session_id, tag) VALUES (?, ?)", s.ID, tag)
	}
	return nil
}

func (db *DB) RemoveTags(shortID string, tags []string) error {
	s, err := db.GetSession(shortID)
	if err != nil {
		return err
	}
	for _, tag := range tags {
		db.conn.Exec("DELETE FROM session_tags WHERE session_id = ? AND tag = ?", s.ID, tag)
	}
	return nil
}

func (db *DB) Search(query string, limit int) ([]model.Session, error) {
	rows, err := db.conn.Query(`
		SELECT s.id, s.short_id, s.source_type, s.source_id, s.project, s.cwd,
			s.git_branch, s.started_at, s.ended_at, s.message_count, s.tool_call_count,
			s.first_prompt, s.model, s.transcript_path
		FROM session_fts f
		JOIN sessions s ON s.id = f.rowid
		WHERE session_fts MATCH ?
		ORDER BY rank
		LIMIT ?`,
		query, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var sessions []model.Session
	for rows.Next() {
		var s model.Session
		if err := rows.Scan(
			&s.ID, &s.ShortID, &s.SourceType, &s.SourceID, &s.Project, &s.CWD,
			&s.GitBranch, &s.StartedAt, &s.EndedAt, &s.MessageCount, &s.ToolCallCount,
			&s.FirstPrompt, &s.Model, &s.TranscriptPath,
		); err != nil {
			return nil, err
		}
		sessions = append(sessions, s)
	}
	return sessions, nil
}

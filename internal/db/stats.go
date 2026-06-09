package db

import (
	"fmt"
	"time"
)

type Stats struct {
	Total          int
	ThisWeek       int
	AvgDurationSec float64
	AvgMessages    float64
	TotalToolCalls int
	BySource       map[string]int
	ByModel        map[string]int
	TopProjects    []CountEntry
	TopTags        []CountEntry
	DailyActivity  []DayCount

	Tokens        TokenTotals
	TokensByModel map[string]TokenTotals
	TokensByDay   []DailyUsage // last 7 days
}

type CountEntry struct {
	Name  string
	Count int
}

type DayCount struct {
	Day   string // "Mon", "Tue", etc.
	Date  string // "2026-03-31"
	Count int
}

func (db *DB) GetStats(f ListFilter) (*Stats, error) {
	s := &Stats{
		BySource: make(map[string]int),
		ByModel:  make(map[string]int),
	}

	// Build WHERE clause
	where, args := buildStatsWhere(f)

	// Total + averages (duration computed in Go due to timezone format in DB)
	row := db.conn.QueryRow(fmt.Sprintf(`
		SELECT
			COUNT(*),
			COALESCE(AVG(message_count), 0),
			COALESCE(SUM(tool_call_count), 0)
		FROM sessions %s`, where), args...)
	row.Scan(&s.Total, &s.AvgMessages, &s.TotalToolCalls)

	// Compute avg duration in Go
	durRows, _ := db.conn.Query(fmt.Sprintf(
		"SELECT started_at, ended_at FROM sessions %s", where), args...)
	if durRows != nil {
		var totalDur float64
		var durCount int
		for durRows.Next() {
			var startedAt, endedAt time.Time
			durRows.Scan(&startedAt, &endedAt)
			if !endedAt.IsZero() && endedAt.After(startedAt) {
				totalDur += endedAt.Sub(startedAt).Seconds()
				durCount++
			}
		}
		durRows.Close()
		if durCount > 0 {
			s.AvgDurationSec = totalDur / float64(durCount)
		}
	}

	// This week
	weekAgo := time.Now().AddDate(0, 0, -7)
	argsWeek := append(args, weekAgo)
	weekWhere := where
	if weekWhere == "" {
		weekWhere = "WHERE started_at >= ?"
	} else {
		weekWhere += " AND started_at >= ?"
	}
	db.conn.QueryRow(fmt.Sprintf("SELECT COUNT(*) FROM sessions %s", weekWhere), argsWeek...).Scan(&s.ThisWeek)

	// By source
	rows, err := db.conn.Query(fmt.Sprintf(
		"SELECT source_type, COUNT(*) FROM sessions %s GROUP BY source_type ORDER BY COUNT(*) DESC", where), args...)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var name string
			var count int
			rows.Scan(&name, &count)
			s.BySource[name] = count
		}
	}

	// By model
	rows, err = db.conn.Query(fmt.Sprintf(
		"SELECT model, COUNT(*) FROM sessions %s GROUP BY model ORDER BY COUNT(*) DESC", where), args...)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var name string
			var count int
			rows.Scan(&name, &count)
			if name == "" {
				name = "(unknown)"
			}
			s.ByModel[name] = count
		}
	}

	// Top projects
	rows, err = db.conn.Query(fmt.Sprintf(
		"SELECT project, COUNT(*) FROM sessions %s GROUP BY project ORDER BY COUNT(*) DESC LIMIT 5", where), args...)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var e CountEntry
			rows.Scan(&e.Name, &e.Count)
			s.TopProjects = append(s.TopProjects, e)
		}
	}

	// Top tags
	rows, err = db.conn.Query(fmt.Sprintf(
		`SELECT t.tag, COUNT(*) FROM session_tags t
		 JOIN sessions s ON s.id = t.session_id %s
		 GROUP BY t.tag ORDER BY COUNT(*) DESC LIMIT 10`,
		where), args...)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var e CountEntry
			rows.Scan(&e.Name, &e.Count)
			s.TopTags = append(s.TopTags, e)
		}
	}

	// Token totals (filter-aware via join to sessions)
	db.conn.QueryRow(fmt.Sprintf(`
		SELECT COALESCE(SUM(u.input_tokens), 0), COALESCE(SUM(u.output_tokens), 0),
			COALESCE(SUM(u.cache_creation_tokens), 0), COALESCE(SUM(u.cache_read_tokens), 0),
			COALESCE(SUM(u.api_calls), 0)
		FROM session_usage_daily u JOIN sessions ON sessions.id = u.session_id %s`, where), args...).
		Scan(&s.Tokens.InputTokens, &s.Tokens.OutputTokens,
			&s.Tokens.CacheCreationTokens, &s.Tokens.CacheReadTokens, &s.Tokens.APICalls)

	// Tokens by model
	s.TokensByModel = make(map[string]TokenTotals)
	rows, err = db.conn.Query(fmt.Sprintf(`
		SELECT u.model, SUM(u.input_tokens), SUM(u.output_tokens),
			SUM(u.cache_creation_tokens), SUM(u.cache_read_tokens), SUM(u.api_calls)
		FROM session_usage_daily u JOIN sessions ON sessions.id = u.session_id %s
		GROUP BY u.model ORDER BY SUM(u.input_tokens) + SUM(u.output_tokens) DESC`, where), args...)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var name string
			var t TokenTotals
			rows.Scan(&name, &t.InputTokens, &t.OutputTokens,
				&t.CacheCreationTokens, &t.CacheReadTokens, &t.APICalls)
			if name == "" {
				name = "(unknown)"
			}
			s.TokensByModel[name] = t
		}
	}

	// Tokens per day (last 7 days)
	weekStart := time.Now().AddDate(0, 0, -6).Format("2006-01-02")
	s.TokensByDay, _ = db.UsageByDay(weekStart, "")

	// Daily activity (last 7 days)
	for i := 6; i >= 0; i-- {
		day := time.Now().AddDate(0, 0, -i)
		dayStart := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, day.Location())
		dayEnd := dayStart.AddDate(0, 0, 1)

		var count int
		db.conn.QueryRow(
			"SELECT COUNT(*) FROM sessions WHERE started_at >= ? AND started_at < ?",
			dayStart, dayEnd).Scan(&count)

		s.DailyActivity = append(s.DailyActivity, DayCount{
			Day:   day.Format("Mon"),
			Date:  day.Format("01/02"),
			Count: count,
		})
	}

	return s, nil
}

func buildStatsWhere(f ListFilter) (string, []any) {
	var conditions []string
	var args []any

	if f.Since != nil {
		conditions = append(conditions, "started_at >= ?")
		args = append(args, *f.Since)
	}
	if f.Until != nil {
		conditions = append(conditions, "started_at <= ?")
		args = append(args, *f.Until)
	}
	if f.Project != "" {
		conditions = append(conditions, "project LIKE ?")
		args = append(args, "%"+f.Project+"%")
	}
	if f.Source != "" {
		conditions = append(conditions, "source_type = ?")
		args = append(args, f.Source)
	}

	if len(conditions) == 0 {
		return "", nil
	}

	where := "WHERE "
	for i, c := range conditions {
		if i > 0 {
			where += " AND "
		}
		where += c
	}
	return where, args
}

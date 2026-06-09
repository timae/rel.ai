package db

// TokenTotals is a sum of token usage over some slice of sessions/days.
type TokenTotals struct {
	InputTokens         int64
	OutputTokens        int64
	CacheCreationTokens int64
	CacheReadTokens     int64
	APICalls            int
}

// DailyUsage is the per-day rollup across all sessions.
type DailyUsage struct {
	Day string // "2006-01-02"
	TokenTotals
	Sessions int
}

// usageWhere builds a day-range filter. since/until are inclusive
// "2006-01-02" strings; empty means unbounded.
func usageWhere(since, until string) (string, []any) {
	where := ""
	var args []any
	if since != "" {
		where += " AND u.day >= ?"
		args = append(args, since)
	}
	if until != "" {
		where += " AND u.day <= ?"
		args = append(args, until)
	}
	return where, args
}

func (db *DB) UsageByDay(since, until string) ([]DailyUsage, error) {
	where, args := usageWhere(since, until)
	rows, err := db.conn.Query(`
		SELECT u.day,
			SUM(u.input_tokens), SUM(u.output_tokens),
			SUM(u.cache_creation_tokens), SUM(u.cache_read_tokens),
			SUM(u.api_calls), COUNT(DISTINCT u.session_id)
		FROM session_usage_daily u
		WHERE u.day != ''`+where+`
		GROUP BY u.day ORDER BY u.day`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []DailyUsage
	for rows.Next() {
		var d DailyUsage
		if err := rows.Scan(&d.Day, &d.InputTokens, &d.OutputTokens,
			&d.CacheCreationTokens, &d.CacheReadTokens, &d.APICalls, &d.Sessions); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (db *DB) UsageTotals(since, until string) (TokenTotals, error) {
	where, args := usageWhere(since, until)
	var t TokenTotals
	err := db.conn.QueryRow(`
		SELECT COALESCE(SUM(u.input_tokens), 0), COALESCE(SUM(u.output_tokens), 0),
			COALESCE(SUM(u.cache_creation_tokens), 0), COALESCE(SUM(u.cache_read_tokens), 0),
			COALESCE(SUM(u.api_calls), 0)
		FROM session_usage_daily u
		WHERE 1=1`+where,
		args...,
	).Scan(&t.InputTokens, &t.OutputTokens, &t.CacheCreationTokens, &t.CacheReadTokens, &t.APICalls)
	return t, err
}

func (db *DB) UsageByModel(since, until string) (map[string]TokenTotals, error) {
	where, args := usageWhere(since, until)
	rows, err := db.conn.Query(`
		SELECT u.model,
			SUM(u.input_tokens), SUM(u.output_tokens),
			SUM(u.cache_creation_tokens), SUM(u.cache_read_tokens), SUM(u.api_calls)
		FROM session_usage_daily u
		WHERE 1=1`+where+`
		GROUP BY u.model`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]TokenTotals)
	for rows.Next() {
		var name string
		var t TokenTotals
		if err := rows.Scan(&name, &t.InputTokens, &t.OutputTokens,
			&t.CacheCreationTokens, &t.CacheReadTokens, &t.APICalls); err != nil {
			return nil, err
		}
		if name == "" {
			name = "(unknown)"
		}
		out[name] = t
	}
	return out, rows.Err()
}

func (db *DB) GetMeta(key string) (string, error) {
	var v string
	err := db.conn.QueryRow("SELECT value FROM meta WHERE key = ?", key).Scan(&v)
	if err != nil {
		return "", nil // missing key reads as empty
	}
	return v, nil
}

func (db *DB) SetMeta(key, value string) error {
	_, err := db.conn.Exec("INSERT OR REPLACE INTO meta (key, value) VALUES (?, ?)", key, value)
	return err
}

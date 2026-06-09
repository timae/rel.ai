package fleet

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

var (
	ErrNotFound     = errors.New("not found")
	ErrUnauthorized = errors.New("unauthorized")
)

const storeSchema = `
CREATE TABLE IF NOT EXISTS users (
    id                   INTEGER PRIMARY KEY AUTOINCREMENT,
    email                TEXT UNIQUE NOT NULL,
    name                 TEXT NOT NULL DEFAULT '',
    api_key_hash         TEXT UNIQUE NOT NULL,
    weekly_budget_tokens INTEGER NOT NULL DEFAULT 0,
    is_admin             INTEGER NOT NULL DEFAULT 0,
    disabled             INTEGER NOT NULL DEFAULT 0,
    created_at           TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS seat_status (
    user_id        INTEGER NOT NULL REFERENCES users(id),
    hostname       TEXT NOT NULL,
    last_seen      TEXT NOT NULL,
    ses_version    TEXT NOT NULL DEFAULT '',
    worker_enabled INTEGER NOT NULL DEFAULT 0,
    worker_state   TEXT NOT NULL DEFAULT 'off',
    running_task   TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (user_id, hostname)
);

CREATE TABLE IF NOT EXISTS usage_days (
    user_id               INTEGER NOT NULL REFERENCES users(id),
    day                   TEXT NOT NULL,
    input_tokens          INTEGER NOT NULL DEFAULT 0,
    output_tokens         INTEGER NOT NULL DEFAULT 0,
    cache_creation_tokens INTEGER NOT NULL DEFAULT 0,
    cache_read_tokens     INTEGER NOT NULL DEFAULT 0,
    api_calls             INTEGER NOT NULL DEFAULT 0,
    sessions              INTEGER NOT NULL DEFAULT 0,
    updated_at            TEXT NOT NULL,
    PRIMARY KEY (user_id, day)
);

CREATE TABLE IF NOT EXISTS tasks (
    id               TEXT PRIMARY KEY,
    created_by       INTEGER NOT NULL REFERENCES users(id),
    title            TEXT NOT NULL DEFAULT '',
    prompt           TEXT NOT NULL,
    repo_url         TEXT NOT NULL,
    base_branch      TEXT NOT NULL DEFAULT '',
    handoff_url      TEXT NOT NULL DEFAULT '',
    max_tokens       INTEGER NOT NULL,
    max_turns        INTEGER NOT NULL DEFAULT 30,
    priority         INTEGER NOT NULL DEFAULT 5,
    status           TEXT NOT NULL DEFAULT 'queued',
    attempts         INTEGER NOT NULL DEFAULT 0,
    max_attempts     INTEGER NOT NULL DEFAULT 2,
    cancel_requested INTEGER NOT NULL DEFAULT 0,
    claimed_by       INTEGER,
    claimed_host     TEXT NOT NULL DEFAULT '',
    lease_expires_at TEXT,
    created_at       TEXT NOT NULL,
    expires_at       TEXT,
    started_at       TEXT,
    finished_at      TEXT,
    result_branch    TEXT NOT NULL DEFAULT '',
    result_share_url TEXT NOT NULL DEFAULT '',
    tokens_used      INTEGER NOT NULL DEFAULT 0,
    num_turns        INTEGER NOT NULL DEFAULT 0,
    error            TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_tasks_claim ON tasks(status, priority, created_at);

CREATE TABLE IF NOT EXISTS task_events (
    id      INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id TEXT NOT NULL REFERENCES tasks(id),
    at      TEXT NOT NULL,
    actor   TEXT NOT NULL,
    event   TEXT NOT NULL,
    detail  TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_task_events_task ON task_events(task_id, id);
`

type Store struct {
	db *sql.DB
}

func OpenStore(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("creating data dir: %w", err)
	}
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(wal)&_pragma=foreign_keys(on)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(storeSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("applying schema: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// --- users -----------------------------------------------------------------

type User struct {
	ID                 int64  `json:"id"`
	Email              string `json:"email"`
	Name               string `json:"name"`
	WeeklyBudgetTokens int64  `json:"weekly_budget_tokens"`
	IsAdmin            bool   `json:"is_admin"`
	Disabled           bool   `json:"disabled"`
}

// NewAPIKey returns (rawKey, sha256hex). The raw key is shown once at
// provisioning time; only the hash is stored.
func NewAPIKey() (string, string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", "", err
	}
	raw := "sesf_" + base64.RawURLEncoding.EncodeToString(buf)
	return raw, hashKey(raw), nil
}

func hashKey(raw string) string {
	h := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(h[:])
}

func (s *Store) CreateUser(email, name string, weeklyBudget int64, isAdmin bool) (User, string, error) {
	raw, hash, err := NewAPIKey()
	if err != nil {
		return User{}, "", err
	}
	res, err := s.db.Exec(`INSERT INTO users (email, name, api_key_hash, weekly_budget_tokens, is_admin, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		email, name, hash, weeklyBudget, boolInt(isAdmin), now())
	if err != nil {
		return User{}, "", err
	}
	id, _ := res.LastInsertId()
	return User{ID: id, Email: email, Name: name, WeeklyBudgetTokens: weeklyBudget, IsAdmin: isAdmin}, raw, nil
}

// UpdateUser applies non-nil fields. Rotating the key returns a new raw key.
func (s *Store) UpdateUser(email string, weeklyBudget *int64, disabled *bool, rotateKey bool) (string, error) {
	var newRaw string
	if weeklyBudget != nil {
		if _, err := s.db.Exec("UPDATE users SET weekly_budget_tokens = ? WHERE email = ?", *weeklyBudget, email); err != nil {
			return "", err
		}
	}
	if disabled != nil {
		if _, err := s.db.Exec("UPDATE users SET disabled = ? WHERE email = ?", boolInt(*disabled), email); err != nil {
			return "", err
		}
	}
	if rotateKey {
		raw, hash, err := NewAPIKey()
		if err != nil {
			return "", err
		}
		if _, err := s.db.Exec("UPDATE users SET api_key_hash = ? WHERE email = ?", hash, email); err != nil {
			return "", err
		}
		newRaw = raw
	}
	var n int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM users WHERE email = ?", email).Scan(&n); err != nil {
		return "", err
	}
	if n == 0 {
		return "", ErrNotFound
	}
	return newRaw, nil
}

// AuthUser resolves a raw API key to its user. Disabled users do not resolve —
// that is the server-side per-seat kill switch.
func (s *Store) AuthUser(rawKey string) (User, error) {
	var u User
	var disabled, isAdmin int
	err := s.db.QueryRow(`SELECT id, email, name, weekly_budget_tokens, is_admin, disabled
		FROM users WHERE api_key_hash = ?`, hashKey(rawKey)).
		Scan(&u.ID, &u.Email, &u.Name, &u.WeeklyBudgetTokens, &isAdmin, &disabled)
	if err == sql.ErrNoRows || (err == nil && disabled == 1) {
		return User{}, ErrUnauthorized
	}
	if err != nil {
		return User{}, err
	}
	u.IsAdmin = isAdmin == 1
	return u, nil
}

// --- heartbeats / usage ------------------------------------------------------

type UsageDay struct {
	Day                 string `json:"day"`
	InputTokens         int64  `json:"input_tokens"`
	OutputTokens        int64  `json:"output_tokens"`
	CacheCreationTokens int64  `json:"cache_creation_tokens"`
	CacheReadTokens     int64  `json:"cache_read_tokens"`
	APICalls            int    `json:"api_calls"`
	Sessions            int    `json:"sessions"`
}

type WorkerStatus struct {
	Enabled     bool   `json:"enabled"`
	State       string `json:"state"` // off|idle|running
	RunningTask string `json:"running_task"`
}

type Heartbeat struct {
	Hostname   string       `json:"hostname"`
	SesVersion string       `json:"ses_version"`
	UsageDays  []UsageDay   `json:"usage_days"`
	Worker     WorkerStatus `json:"worker"`
}

func (s *Store) RecordHeartbeat(userID int64, hb Heartbeat) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`INSERT OR REPLACE INTO seat_status
		(user_id, hostname, last_seen, ses_version, worker_enabled, worker_state, running_task)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		userID, hb.Hostname, now(), hb.SesVersion,
		boolInt(hb.Worker.Enabled), defaultStr(hb.Worker.State, "off"), hb.Worker.RunningTask); err != nil {
		return err
	}

	// Heartbeats carry absolute per-day rollups from the seat's local index,
	// so an upsert is idempotent and self-correcting across rescans.
	for _, d := range hb.UsageDays {
		if d.Day == "" {
			continue
		}
		if _, err := tx.Exec(`INSERT OR REPLACE INTO usage_days
			(user_id, day, input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens, api_calls, sessions, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			userID, d.Day, d.InputTokens, d.OutputTokens, d.CacheCreationTokens, d.CacheReadTokens,
			d.APICalls, d.Sessions, now()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// --- capacity board ----------------------------------------------------------

type SeatCapacity struct {
	Email           string `json:"email"`
	Name            string `json:"name"`
	Hostname        string `json:"hostname"`
	LastSeen        string `json:"last_seen"`
	WeeklyBudget    int64  `json:"weekly_budget"`
	Tokens7d        int64  `json:"tokens_7d"` // weighted
	TokensToday     int64  `json:"tokens_today"`
	AvailableTokens int64  `json:"available_tokens"`
	WorkerEnabled   bool   `json:"worker_enabled"`
	WorkerState     string `json:"worker_state"`
	RunningTask     string `json:"running_task"`
}

func (s *Store) Capacity() ([]SeatCapacity, error) {
	weekStart := time.Now().AddDate(0, 0, -6).Format("2006-01-02")
	today := time.Now().Format("2006-01-02")

	rows, err := s.db.Query(`
		SELECT u.email, u.name, u.weekly_budget_tokens,
			COALESCE(st.hostname, ''), COALESCE(st.last_seen, ''),
			COALESCE(st.worker_enabled, 0), COALESCE(st.worker_state, 'off'), COALESCE(st.running_task, ''),
			COALESCE((SELECT SUM(input_tokens + output_tokens + cache_creation_tokens + cache_read_tokens / 10)
				FROM usage_days d WHERE d.user_id = u.id AND d.day >= ?), 0),
			COALESCE((SELECT SUM(input_tokens + output_tokens + cache_creation_tokens + cache_read_tokens / 10)
				FROM usage_days d WHERE d.user_id = u.id AND d.day = ?), 0)
		FROM users u
		LEFT JOIN seat_status st ON st.user_id = u.id
			AND st.last_seen = (SELECT MAX(last_seen) FROM seat_status WHERE user_id = u.id)
		WHERE u.disabled = 0
		ORDER BY u.email`, weekStart, today)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []SeatCapacity
	for rows.Next() {
		var c SeatCapacity
		var enabled int
		if err := rows.Scan(&c.Email, &c.Name, &c.WeeklyBudget,
			&c.Hostname, &c.LastSeen, &enabled, &c.WorkerState, &c.RunningTask,
			&c.Tokens7d, &c.TokensToday); err != nil {
			return nil, err
		}
		c.WorkerEnabled = enabled == 1
		c.AvailableTokens = c.WeeklyBudget - c.Tokens7d
		if c.AvailableTokens < 0 {
			c.AvailableTokens = 0
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// --- usage report -------------------------------------------------------------

type UserUsageReport struct {
	Email          string `json:"email"`
	Name           string `json:"name"`
	WeeklyBudget   int64  `json:"weekly_budget"`
	WeightedTokens int64  `json:"weighted_tokens"`
	RawTokens      int64  `json:"raw_tokens"`
	ActiveDays     int    `json:"active_days"`
	Sessions       int    `json:"sessions"`
	UtilizationPct int    `json:"utilization_pct"` // vs budget pro-rated over the range
}

func (s *Store) UsageReport(since, until string) ([]UserUsageReport, error) {
	rows, err := s.db.Query(`
		SELECT u.email, u.name, u.weekly_budget_tokens,
			COALESCE(SUM(d.input_tokens + d.output_tokens + d.cache_creation_tokens + d.cache_read_tokens / 10), 0),
			COALESCE(SUM(d.input_tokens + d.output_tokens + d.cache_creation_tokens + d.cache_read_tokens), 0),
			COUNT(d.day), COALESCE(SUM(d.sessions), 0)
		FROM users u
		LEFT JOIN usage_days d ON d.user_id = u.id AND d.day >= ? AND d.day <= ?
		WHERE u.disabled = 0
		GROUP BY u.id ORDER BY 4 DESC`, since, until)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	from, _ := time.Parse("2006-01-02", since)
	to, _ := time.Parse("2006-01-02", until)
	rangeDays := int(to.Sub(from).Hours()/24) + 1
	if rangeDays < 1 {
		rangeDays = 7
	}

	var out []UserUsageReport
	for rows.Next() {
		var r UserUsageReport
		if err := rows.Scan(&r.Email, &r.Name, &r.WeeklyBudget, &r.WeightedTokens, &r.RawTokens, &r.ActiveDays, &r.Sessions); err != nil {
			return nil, err
		}
		if r.WeeklyBudget > 0 {
			budgetForRange := r.WeeklyBudget * int64(rangeDays) / 7
			if budgetForRange > 0 {
				r.UtilizationPct = int(r.WeightedTokens * 100 / budgetForRange)
			}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// --- helpers -------------------------------------------------------------------

func now() string { return time.Now().UTC().Format(time.RFC3339) }

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func defaultStr(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

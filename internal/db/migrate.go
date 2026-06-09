package db

import "fmt"

// baselineVersion is the schema established by schemaSQL. Databases created
// before versioned migrations existed report user_version 0 but already hold
// the baseline schema (schemaSQL is idempotent), so 0 is treated as baseline.
const baselineVersion = 1

// migrations[i] upgrades the schema from version baselineVersion+i to
// baselineVersion+i+1. Statements run inside a single transaction per step.
var migrations = []string{
	// v1 -> v2: token telemetry
	`
	ALTER TABLE sessions ADD COLUMN input_tokens          INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE sessions ADD COLUMN output_tokens         INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE sessions ADD COLUMN cache_creation_tokens INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE sessions ADD COLUMN cache_read_tokens     INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE sessions ADD COLUMN api_call_count        INTEGER NOT NULL DEFAULT 0;

	CREATE TABLE IF NOT EXISTS session_usage_daily (
	    session_id            INTEGER REFERENCES sessions(id) ON DELETE CASCADE,
	    day                   TEXT NOT NULL,
	    model                 TEXT NOT NULL DEFAULT '',
	    input_tokens          INTEGER NOT NULL DEFAULT 0,
	    output_tokens         INTEGER NOT NULL DEFAULT 0,
	    cache_creation_tokens INTEGER NOT NULL DEFAULT 0,
	    cache_read_tokens     INTEGER NOT NULL DEFAULT 0,
	    api_calls             INTEGER NOT NULL DEFAULT 0,
	    PRIMARY KEY (session_id, day, model)
	);
	CREATE INDEX IF NOT EXISTS idx_usage_daily_day ON session_usage_daily(day);

	CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
	INSERT OR REPLACE INTO meta (key, value) VALUES ('token_backfill', 'pending');
	`,
}

func (db *DB) runMigrations() error {
	var version int
	if err := db.conn.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("reading user_version: %w", err)
	}
	if version == 0 {
		version = baselineVersion
	}

	for ; version < baselineVersion+len(migrations); version++ {
		tx, err := db.conn.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[version-baselineVersion]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration to v%d: %w", version+1, err)
		}
		if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", version+1)); err != nil {
			tx.Rollback()
			return fmt.Errorf("setting user_version %d: %w", version+1, err)
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

package store

import (
	"fmt"
	"time"
)

// migrations are applied in order; each entry runs inside one transaction and
// records its version so re-running Migrate is idempotent.
var migrations = []struct {
	version int
	stmts   []string
}{
	{
		version: 1,
		stmts: []string{
			`CREATE TABLE accounts (
				id TEXT PRIMARY KEY,
				name TEXT NOT NULL,
				credential_path TEXT NOT NULL UNIQUE,
				upstream_account_id TEXT NOT NULL DEFAULT '',
				plan_type TEXT NOT NULL DEFAULT '',
				enabled INTEGER NOT NULL DEFAULT 0 CHECK (enabled IN (0, 1)),
				weight INTEGER NOT NULL DEFAULT 1 CHECK (weight BETWEEN 1 AND 100),
				token_expires_at INTEGER,
				last_error_code TEXT NOT NULL DEFAULT '',
				cooldown_until INTEGER,
				created_at INTEGER NOT NULL,
				updated_at INTEGER NOT NULL
			)`,
			`CREATE UNIQUE INDEX idx_accounts_upstream_identity
				ON accounts(upstream_account_id)
				WHERE upstream_account_id <> ''`,
			`CREATE TABLE api_keys (
				id TEXT PRIMARY KEY,
				name TEXT NOT NULL,
				prefix TEXT NOT NULL,
				key_hash TEXT NOT NULL UNIQUE,
				enabled INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0, 1)),
				expires_at INTEGER,
				revoked_at INTEGER,
				last_used_at INTEGER,
				created_at INTEGER NOT NULL,
				updated_at INTEGER NOT NULL
			)`,
			`CREATE TABLE usage_requests (
				id TEXT PRIMARY KEY,
				api_key_id TEXT REFERENCES api_keys(id),
				account_id TEXT REFERENCES accounts(id),
				requested_model TEXT NOT NULL DEFAULT '',
				reported_model TEXT NOT NULL DEFAULT '',
				reasoning_effort TEXT NOT NULL DEFAULT '',
				status TEXT NOT NULL,
				http_status INTEGER NOT NULL DEFAULT 0,
				upstream_status INTEGER NOT NULL DEFAULT 0,
				terminal_event TEXT NOT NULL DEFAULT '',
				error_code TEXT NOT NULL DEFAULT '',
				input_tokens INTEGER,
				output_tokens INTEGER,
				total_tokens INTEGER,
				cached_tokens INTEGER,
				reasoning_tokens INTEGER,
				started_at INTEGER NOT NULL,
				finished_at INTEGER NOT NULL,
				duration_ms INTEGER NOT NULL
			)`,
			`CREATE INDEX idx_usage_started_at ON usage_requests(started_at)`,
			`CREATE INDEX idx_usage_account_time ON usage_requests(account_id, started_at)`,
			`CREATE INDEX idx_usage_key_time ON usage_requests(api_key_id, started_at)`,
			`CREATE INDEX idx_usage_model_time ON usage_requests(requested_model, started_at)`,
		},
	},
	{
		version: 2,
		stmts: []string{
			// Settings is a small key/value table for runtime configuration that
			// must survive restarts and be editable from the admin console.
			`CREATE TABLE settings (
				key TEXT PRIMARY KEY,
				value TEXT NOT NULL,
				updated_at INTEGER NOT NULL
			)`,
		},
	},
	{
		version: 3,
		stmts: []string{
			// Sealed key material lets the console show a saved key again
			// without storing plaintext. Empty means the key predates this
			// column and cannot be revealed.
			`ALTER TABLE api_keys ADD COLUMN key_sealed TEXT NOT NULL DEFAULT ''`,
		},
	},
	{
		version: 4,
		stmts: []string{
			// One snapshot of the upstream model catalog, refreshed on demand
			// from the console. Storing the whole document keeps /v1/models a
			// pure read instead of a live upstream call on every request.
			`CREATE TABLE model_catalog (
				id TEXT PRIMARY KEY,
				payload TEXT NOT NULL,
				synced_at INTEGER NOT NULL,
				source_account TEXT NOT NULL DEFAULT ''
			)`,
			// Exact-match model aliasing applied to every LLM request. The
			// source model is the primary key, so a model maps to one target.
			`CREATE TABLE model_mappings (
				source_model TEXT PRIMARY KEY,
				target_model TEXT NOT NULL,
				created_at INTEGER NOT NULL,
				updated_at INTEGER NOT NULL
			)`,
		},
	},
	{
		version: 5,
		stmts: []string{
			// Which model each account actually serves, as measured by
			// fingerprint attribution. Routing reads this so a request for a
			// model an account silently downgrades never lands there.
			`CREATE TABLE account_models (
				account_id TEXT NOT NULL,
				model TEXT NOT NULL,
				top_model TEXT NOT NULL DEFAULT '',
				self_prob REAL NOT NULL DEFAULT 0,
				verified INTEGER NOT NULL DEFAULT 0 CHECK (verified IN (0, 1)),
				checked_at INTEGER NOT NULL,
				last_error TEXT NOT NULL DEFAULT '',
				PRIMARY KEY (account_id, model)
			)`,
			`CREATE INDEX idx_account_models_model ON account_models(model, verified)`,
			// Probe scheduling configuration lives with the other settings.
			`INSERT INTO settings (key, value, updated_at) VALUES
				('probe_interval_minutes', '0', 0),
				('probe_last_run', '0', 0)
				ON CONFLICT(key) DO NOTHING`,
		},
	},
}

// Migrate brings the schema up to the latest version. It fails closed on the
// first statement error so a partially applied migration never hides itself.
func (d *DB) Migrate() error {
	if _, err := d.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY,
		applied_at INTEGER NOT NULL
	)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}
	var current int
	if err := d.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&current); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	for _, m := range migrations {
		if m.version <= current {
			continue
		}
		tx, err := d.Begin()
		if err != nil {
			return fmt.Errorf("begin migration %d: %w", m.version, err)
		}
		for _, stmt := range m.stmts {
			if _, err := tx.Exec(stmt); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("migration %d: %w", m.version, err)
			}
		}
		if _, err := tx.Exec(`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`,
			m.version, time.Now().UnixMilli()); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("record migration %d: %w", m.version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %d: %w", m.version, err)
		}
	}
	return nil
}

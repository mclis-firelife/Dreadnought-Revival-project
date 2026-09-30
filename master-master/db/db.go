package db

import (
	"database/sql"
	"fmt"

	_ "github.com/mattn/go-sqlite3"
)

var migrations = []string{
	`CREATE TABLE IF NOT EXISTS clusters (
		id              TEXT PRIMARY KEY,
		name            TEXT NOT NULL UNIQUE,
		web_url         TEXT NOT NULL,
		battle_ip       TEXT NOT NULL,
		version         TEXT NOT NULL DEFAULT '',
		motd            TEXT NOT NULL DEFAULT '',
		ca_cert         TEXT NOT NULL DEFAULT '',
		ca_fingerprint  TEXT NOT NULL DEFAULT '',
		players         INTEGER NOT NULL DEFAULT 0,
		servers         INTEGER NOT NULL DEFAULT 0,
		status          TEXT NOT NULL DEFAULT 'online',
		last_heartbeat  TEXT NOT NULL DEFAULT (datetime('now')),
		registered_at   TEXT NOT NULL DEFAULT (datetime('now'))
	)`,
	// Operator blocklist: a blocked name is refused at register time, so a
	// kicked cluster cannot simply re-list itself on the next heartbeat.
	`ALTER TABLE clusters ADD COLUMN blocked INTEGER NOT NULL DEFAULT 0`,
	// Stage 3 (account roaming): who to mail the secret, the secret's hash
	// (plaintext is shown once at generation and never stored), and where to
	// push it automatically (https only, enforced at send time).
	`ALTER TABLE clusters ADD COLUMN contact_email TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE clusters ADD COLUMN secret_hash TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE clusters ADD COLUMN agent_url TEXT NOT NULL DEFAULT ''`,
	// Central account mirror. users carry identity (password hashes included:
	// the same login must work on every cluster); snapshots carry one full
	// per-user bundle plus headline columns for the stats views.
	`CREATE TABLE IF NOT EXISTS sync_users (
		user_id        TEXT PRIMARY KEY,
		username       TEXT NOT NULL,
		email          TEXT NOT NULL DEFAULT '',
		password_hash  TEXT NOT NULL DEFAULT '',
		created_at     TEXT NOT NULL DEFAULT (datetime('now')),
		updated_at     TEXT NOT NULL DEFAULT (datetime('now')),
		source_cluster TEXT NOT NULL DEFAULT ''
	)`,
	`CREATE TABLE IF NOT EXISTS sync_bans (
		id             TEXT PRIMARY KEY,
		user_id        TEXT NOT NULL,
		reason         TEXT NOT NULL DEFAULT '',
		banned_by      TEXT NOT NULL DEFAULT '',
		expires_at     TEXT,
		created_at     TEXT NOT NULL DEFAULT (datetime('now'))
	)`,
	`CREATE TABLE IF NOT EXISTS sync_snapshots (
		user_id        TEXT PRIMARY KEY,
		updated_at     TEXT NOT NULL DEFAULT (datetime('now')),
		source_cluster TEXT NOT NULL DEFAULT '',
		credits        INTEGER NOT NULL DEFAULT 0,
		rank           INTEGER NOT NULL DEFAULT 1,
		ships          INTEGER NOT NULL DEFAULT 0,
		data           TEXT NOT NULL DEFAULT '{}'
	)`,
	// Every sync communication, in and out. This table is the audit trail:
	// who sent what, when, and whether it was accepted.
	`CREATE TABLE IF NOT EXISTS sync_log (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		time       TEXT NOT NULL DEFAULT (datetime('now')),
		cluster_id TEXT NOT NULL DEFAULT '',
		direction  TEXT NOT NULL DEFAULT '',
		endpoint   TEXT NOT NULL DEFAULT '',
		users      INTEGER NOT NULL DEFAULT 0,
		status     TEXT NOT NULL DEFAULT '',
		detail     TEXT NOT NULL DEFAULT ''
	)`,
	// Live presence per account per cluster: is this user right now in a
	// match there? Refreshed by every push (freshness is the point, so it is
	// keyed separately from snapshots). Rows older than the browser window
	// below count as gone; no cleanup needed for correctness.
	`CREATE TABLE IF NOT EXISTS sync_presence (
		user_id    TEXT NOT NULL,
		cluster_id TEXT NOT NULL,
		in_match   INTEGER NOT NULL DEFAULT 0,
		updated_at TEXT NOT NULL DEFAULT (datetime('now')),
		PRIMARY KEY (user_id, cluster_id)
	)`,
	// Operator settings (key/value): currently the main cluster for manual
	// rollouts — the one whose state "Sync from main" copies everywhere.
	`CREATE TABLE IF NOT EXISTS settings (
		key   TEXT PRIMARY KEY,
		value TEXT NOT NULL DEFAULT ''
	)`,
	// Temporary blocks: reason shown in the dashboard, optional expiry after
	// which the cluster may list again without operator action. blocked=1
	// with an empty until is indefinite (the old behaviour).
	`ALTER TABLE clusters ADD COLUMN blocked_until TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE clusters ADD COLUMN blocked_reason TEXT NOT NULL DEFAULT ''`,
	// Heartbeat transitions for the flap timeline: 'online' when a cluster
	// returns, 'offline' when the sweeper marks it stale. Capped to 30 days
	// by the same sweeper.
	`CREATE TABLE IF NOT EXISTS heartbeat_events (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		cluster_id TEXT NOT NULL DEFAULT '',
		time       TEXT NOT NULL DEFAULT (datetime('now')),
		event      TEXT NOT NULL DEFAULT ''
	)`,
	// Operator note per cluster (owner contact, maintenance window, quirks).
	`ALTER TABLE clusters ADD COLUMN note TEXT NOT NULL DEFAULT ''`,
	// When the sync secret was last (re)generated: rotation age display.
	`ALTER TABLE clusters ADD COLUMN secret_set_at TEXT NOT NULL DEFAULT ''`,
	// MOTD expiry: empty means permanent (the old behaviour).
	`ALTER TABLE clusters ADD COLUMN motd_until TEXT NOT NULL DEFAULT ''`,
	// One-time pairing tokens: the operator generates one per cluster (shown
	// once), the cluster operator pastes it exactly once, the agent
	// exchanges it for the cluster secret. Burned on use, expired after a
	// day. After pairing, rotation is authenticated by the secret itself.
	`CREATE TABLE IF NOT EXISTS pairing_tokens (
		token_hash TEXT PRIMARY KEY,
		cluster_id TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL DEFAULT (datetime('now')),
		expires_at TEXT NOT NULL DEFAULT (datetime('now','+1 day')),
		used_at    TEXT NOT NULL DEFAULT ''
	)`,
	// Roaming tickets (single sign-on): a cluster asks for one on behalf of
	// its logged-in user (delegate, authed by cluster secret); another
	// cluster redeems it for a local session (verify, authed by its own
	// secret). Opaque random, 5 minutes, reusable within expiry (one user
	// may roam to several clusters), pruned by the sweeper.
	`CREATE TABLE IF NOT EXISTS roam_tickets (
		token_hash TEXT PRIMARY KEY,
		user_id    TEXT NOT NULL DEFAULT '',
		username   TEXT NOT NULL DEFAULT '',
		email      TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL DEFAULT (datetime('now')),
		expires_at TEXT NOT NULL DEFAULT (datetime('now','+5 minutes'))
	)`,
}

func Open(path string) (*sql.DB, error) {
	dsn := fmt.Sprintf("file:%s?_journal_mode=WAL&_foreign_keys=on&_busy_timeout=5000", path)
	database, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	database.SetMaxOpenConns(1)
	if err := database.Ping(); err != nil {
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	if err := migrate(database); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return database, nil
}

func migrate(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_versions (
		version INTEGER PRIMARY KEY,
		applied_at TEXT NOT NULL DEFAULT (datetime('now'))
	)`); err != nil {
		return err
	}
	var current int
	_ = db.QueryRow(`SELECT COALESCE(MAX(version),0) FROM schema_versions`).Scan(&current)
	for i, ddl := range migrations {
		v := i + 1
		if v <= current {
			continue
		}
		if _, err := db.Exec(ddl); err != nil {
			return fmt.Errorf("migration %d: %w", v, err)
		}
		if _, err := db.Exec(`INSERT INTO schema_versions(version) VALUES(?)`, v); err != nil {
			return fmt.Errorf("record schema version %d: %w", v, err)
		}
	}
	return nil
}

package db

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

type Store struct {
	DB *sql.DB
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_journal_mode=WAL&_foreign_keys=on&_busy_timeout=5000")
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}

	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("ping db: %w", err)
	}

	if err := migrate(db); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}

	return &Store{DB: db}, nil
}

func (s *Store) Close() error {
	return s.DB.Close()
}

func migrate(db *sql.DB) error {
	if _, err := db.Exec(schema); err != nil {
		return err
	}
	// Incremental migrations for existing databases.
	// Each ALTER TABLE is idempotent — it silently fails if the column already exists.
	alterStmts := []string{
		`ALTER TABLE containers ADD COLUMN health TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE containers ADD COLUMN restart_count INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE containers ADD COLUMN started_at TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE containers ADD COLUMN exit_code INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE containers ADD COLUMN oom_killed INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE hosts ADD COLUMN agent_version TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE sessions ADD COLUMN user_id INTEGER NOT NULL DEFAULT 0`,
		// Data-isolation pass:
		`ALTER TABLE share_links ADD COLUMN user_id INTEGER NOT NULL DEFAULT 0`,
	}
	for _, stmt := range alterStmts {
		db.Exec(stmt) // ignore "duplicate column" errors
	}

	// Backfill share_links.user_id to the first admin (once). Idempotent: only touches rows with user_id=0.
	db.Exec(`UPDATE share_links SET user_id = COALESCE((SELECT id FROM users WHERE role='admin' AND status='active' ORDER BY id LIMIT 1), 0) WHERE user_id = 0`)

	// Rebuild ignored_entities to attach user_id (per-user scope). SQLite can't
	// drop the legacy UNIQUE(entity_type, entity_key) constraint via ALTER, so
	// we detect the old schema and rebuild the table exactly once.
	if err := migrateIgnoredEntitiesToPerUser(db); err != nil {
		return fmt.Errorf("migrate ignored_entities: %w", err)
	}

	return nil
}

// migrateIgnoredEntitiesToPerUser adds user_id scoping to ignored_entities.
// Existing global rows are assigned to the first admin (so their view is
// preserved). If no admin exists yet (fresh install mid-boot), existing rows
// are dropped — safer than leaving orphans referencing a non-user.
func migrateIgnoredEntitiesToPerUser(db *sql.DB) error {
	// Detect old schema: if user_id column is absent, rebuild.
	rows, err := db.Query(`PRAGMA table_info(ignored_entities)`)
	if err != nil {
		return err
	}
	hasUserID := false
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			rows.Close()
			return err
		}
		if name == "user_id" {
			hasUserID = true
		}
	}
	rows.Close()
	if hasUserID {
		return nil
	}

	var adminID int64
	_ = db.QueryRow(`SELECT COALESCE((SELECT id FROM users WHERE role='admin' AND status='active' ORDER BY id LIMIT 1), 0)`).Scan(&adminID)

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`
		CREATE TABLE ignored_entities_new (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			entity_type TEXT NOT NULL,
			entity_key TEXT NOT NULL,
			created_at TEXT NOT NULL DEFAULT (datetime('now')),
			UNIQUE(user_id, entity_type, entity_key)
		)`); err != nil {
		return err
	}
	if adminID > 0 {
		if _, err := tx.Exec(
			`INSERT INTO ignored_entities_new (user_id, entity_type, entity_key, created_at)
			 SELECT ?, entity_type, entity_key, created_at FROM ignored_entities`,
			adminID,
		); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`DROP TABLE ignored_entities`); err != nil {
		return err
	}
	if _, err := tx.Exec(`ALTER TABLE ignored_entities_new RENAME TO ignored_entities`); err != nil {
		return err
	}
	if _, err := tx.Exec(`CREATE INDEX IF NOT EXISTS idx_ignored_user ON ignored_entities(user_id)`); err != nil {
		return err
	}
	return tx.Commit()
}

const schema = `
CREATE TABLE IF NOT EXISTS hosts (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	hostname TEXT NOT NULL UNIQUE,
	os TEXT NOT NULL DEFAULT '',
	kernel TEXT NOT NULL DEFAULT '',
	uptime_seconds INTEGER NOT NULL DEFAULT 0,
	agent_version TEXT NOT NULL DEFAULT '',
	last_seen TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS containers (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	host_id INTEGER NOT NULL REFERENCES hosts(id) ON DELETE CASCADE,
	name TEXT NOT NULL,
	image TEXT NOT NULL DEFAULT '',
	state TEXT NOT NULL DEFAULT 'unknown',
	health TEXT NOT NULL DEFAULT '',
	restart_count INTEGER NOT NULL DEFAULT 0,
	started_at TEXT NOT NULL DEFAULT '',
	exit_code INTEGER NOT NULL DEFAULT 0,
	oom_killed INTEGER NOT NULL DEFAULT 0,
	last_seen TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_containers_host ON containers(host_id);

CREATE TABLE IF NOT EXISTS container_events (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	host_id INTEGER NOT NULL REFERENCES hosts(id) ON DELETE CASCADE,
	container_name TEXT NOT NULL,
	event_type TEXT NOT NULL,
	exit_code INTEGER NOT NULL DEFAULT 0,
	oom_killed INTEGER NOT NULL DEFAULT 0,
	ts TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX IF NOT EXISTS idx_container_events_host_name ON container_events(host_id, container_name, ts);

CREATE TABLE IF NOT EXISTS agents (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	host_id INTEGER NOT NULL REFERENCES hosts(id) ON DELETE CASCADE,
	name TEXT NOT NULL,
	agent_type TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL DEFAULT 'unknown',
	last_seen TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_agents_host ON agents(host_id);

CREATE TABLE IF NOT EXISTS sessions (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	token TEXT NOT NULL UNIQUE,
	created_at TEXT NOT NULL DEFAULT (datetime('now')),
	expires_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_sessions_token ON sessions(token);

CREATE TABLE IF NOT EXISTS tokens (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	hostname TEXT NOT NULL UNIQUE,
	token_hash TEXT NOT NULL,
	created_at TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX IF NOT EXISTS idx_tokens_hash ON tokens(token_hash);

CREATE TABLE IF NOT EXISTS share_links (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	token TEXT NOT NULL UNIQUE,
	label TEXT NOT NULL DEFAULT '',
	user_id INTEGER NOT NULL DEFAULT 0,
	created_at TEXT NOT NULL DEFAULT (datetime('now')),
	expires_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_share_links_token ON share_links(token);

CREATE TABLE IF NOT EXISTS status_samples (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	entity_type TEXT NOT NULL,
	entity_key TEXT NOT NULL,
	ts TEXT NOT NULL,
	status TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_samples_entity_ts ON status_samples(entity_type, entity_key, ts);
CREATE INDEX IF NOT EXISTS idx_samples_ts ON status_samples(ts);

CREATE TABLE IF NOT EXISTS settings (
	key TEXT PRIMARY KEY,
	value TEXT NOT NULL
);

INSERT OR IGNORE INTO settings (key, value) VALUES ('heartbeat_interval', '60');

CREATE TABLE IF NOT EXISTS ignored_entities (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	entity_type TEXT NOT NULL,
	entity_key TEXT NOT NULL,
	created_at TEXT NOT NULL DEFAULT (datetime('now')),
	UNIQUE(user_id, entity_type, entity_key)
);
CREATE INDEX IF NOT EXISTS idx_ignored_user ON ignored_entities(user_id);

CREATE TABLE IF NOT EXISTS image_presets (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	name TEXT NOT NULL,
	mime_type TEXT NOT NULL DEFAULT 'image/png',
	data BLOB NOT NULL,
	created_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS host_configs (
	hostname TEXT PRIMARY KEY,
	image_preset_id INTEGER REFERENCES image_presets(id) ON DELETE SET NULL,
	comment TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS users (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	email TEXT NOT NULL UNIQUE,
	password_hash TEXT NOT NULL,
	role TEXT NOT NULL DEFAULT 'user' CHECK(role IN ('admin','user')),
	status TEXT NOT NULL DEFAULT 'active' CHECK(status IN ('active','inactive','deleted')),
	totp_secret TEXT NOT NULL DEFAULT '',
	totp_enabled INTEGER NOT NULL DEFAULT 0,
	created_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS totp_pending (
	token TEXT PRIMARY KEY,
	user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	expires_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS password_reset_tokens (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	token_hash TEXT NOT NULL UNIQUE,
	created_at TEXT NOT NULL,
	expires_at TEXT NOT NULL,
	used_at TEXT,
	ip_address TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_prt_user ON password_reset_tokens(user_id);

CREATE TABLE IF NOT EXISTS user_host_access (
	user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	host_id INTEGER NOT NULL REFERENCES hosts(id) ON DELETE CASCADE,
	PRIMARY KEY (user_id, host_id)
);
`

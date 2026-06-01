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
	// FLEET-155: use modernc.org/sqlite's `_pragma` query param so
	// busy_timeout is applied to *every* new pool connection. The older
	// `_busy_timeout=5000` form is unreliable across the pool (FLEET-135);
	// relying on a single db.Exec(`PRAGMA …`) only set the timeout on one
	// connection, so concurrent writers (e.g. updateAllAgents fan-out)
	// raced past it and hit SQLITE_BUSY in 1-9ms.
	db, err := sql.Open("sqlite", path+"?_journal_mode=WAL&_foreign_keys=on&_pragma=busy_timeout(5000)")
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
		// Hardware/metadata pass:
		`ALTER TABLE hosts ADD COLUMN hw_static TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE hosts ADD COLUMN hw_live TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE hosts ADD COLUMN hw_live_at TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE hosts ADD COLUMN fastfetch_json TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE hosts ADD COLUMN fastfetch_at TEXT NOT NULL DEFAULT ''`,
		// Auto-update / server-triggered update request:
		`ALTER TABLE hosts ADD COLUMN update_requested_at TEXT NOT NULL DEFAULT ''`,
		// FLEET-84 deployment shape — drives universal Update button gating.
		`ALTER TABLE hosts ADD COLUMN deployment_shape TEXT NOT NULL DEFAULT ''`,
		// FLEET-369.1 host.reboot — boot_id from /proc/sys/kernel/random/boot_id is
		// the canonical "did this host actually reboot?" signal; allow_reboot is a
		// per-host kill switch the operator flips when a host shouldn't be rebooted
		// even if the global feature flag is on (e.g. hsb2 mid-bake).
		`ALTER TABLE hosts ADD COLUMN boot_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE hosts ADD COLUMN allow_reboot INTEGER NOT NULL DEFAULT 1`,
		// User avatars (FLEET-487) — stored as a data URL string (data:image/...;base64,...).
		`ALTER TABLE users ADD COLUMN avatar TEXT NOT NULL DEFAULT ''`,
		// FLEET-113: per-row OOB confirmation-code state. The hash is
		// SHA-256(code || pubkey_fp) so a leaked code cannot approve a
		// different bridge. Attempts is bumped on each failed /approve;
		// at 5 the row auto-rejects (deletes) and the bridge must re-
		// register from scratch.
		`ALTER TABLE bridge_pairings ADD COLUMN confirmation_code_hash TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE bridge_pairings ADD COLUMN confirmation_code_expires_at TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE bridge_pairings ADD COLUMN confirmation_attempts INTEGER NOT NULL DEFAULT 0`,
		// FLEET-113: per-gateway OOB-delivery toggle. Default OFF until
		// the OpenClaw RFC for `bridge.confirmation_code` lands on the
		// gateway side; flipping ON enables strict OOB enforcement on
		// /approve for that gateway's bridges.
		`ALTER TABLE openclaw_gateways ADD COLUMN oob_delivery_enabled INTEGER NOT NULL DEFAULT 0`,
		// FLEET-114: per-gateway attestation enforcement + the gateway's
		// raw Ed25519 pubkey (b64url-no-padding). The pubkey populates
		// when an operator pastes it (or when the OpenClaw RFC for
		// pair-time pubkey exchange lands and we capture it during
		// MarkGatewayPaired). Until the column is non-empty for a given
		// gateway, attestation falls through to "skipped" regardless of
		// the env / per-gateway flag — verification cannot proceed
		// without the verifier's key.
		`ALTER TABLE openclaw_gateways ADD COLUMN attestation_required INTEGER NOT NULL DEFAULT 1`,
		`ALTER TABLE openclaw_gateways ADD COLUMN gateway_pubkey_b64 TEXT NOT NULL DEFAULT ''`,
		// FLEET-114: per-row attestation outcome. One of:
		//   'unknown'  — pre-Phase-3 row OR registration didn't include a sig
		//   'verified' — sig present + valid; gateway endorsed this (host,agent,fp)
		//   'skipped'  — registration succeeded under attestation_skipped
		`ALTER TABLE bridge_pairings ADD COLUMN attestation_status TEXT NOT NULL DEFAULT 'unknown'`,
		// Agent observability (FLEET-36) — see PPM Knowledge
		// FLEET/guideline/agent_observability.
		// New tables are created by the schema const; nothing to ALTER here
		// unless we evolve existing tables later.
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

	// Index is created here (not in the schema const) so that the column
	// user_id is guaranteed to exist — either via fresh-install schema or via
	// the rebuild above.
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_ignored_user ON ignored_entities(user_id)`)

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
	created_at TEXT NOT NULL DEFAULT (datetime('now')),
	hw_static TEXT NOT NULL DEFAULT '',
	hw_live TEXT NOT NULL DEFAULT '',
	hw_live_at TEXT NOT NULL DEFAULT '',
	fastfetch_json TEXT NOT NULL DEFAULT '',
	fastfetch_at TEXT NOT NULL DEFAULT '',
	update_requested_at TEXT NOT NULL DEFAULT '',
	deployment_shape TEXT NOT NULL DEFAULT '',
	boot_id TEXT NOT NULL DEFAULT '',
	allow_reboot INTEGER NOT NULL DEFAULT 1
);

CREATE TABLE IF NOT EXISTS host_metrics (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	host_id INTEGER NOT NULL REFERENCES hosts(id) ON DELETE CASCADE,
	ts TEXT NOT NULL,
	cpu_load REAL NOT NULL DEFAULT 0,
	mem_used_pct REAL NOT NULL DEFAULT 0,
	cpu_temp_c REAL,
	gpu_temp_c REAL
);
CREATE INDEX IF NOT EXISTS idx_host_metrics_host_ts ON host_metrics(host_id, ts);
CREATE INDEX IF NOT EXISTS idx_host_metrics_ts ON host_metrics(ts);

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

CREATE TABLE IF NOT EXISTS backups (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	host_id INTEGER NOT NULL REFERENCES hosts(id) ON DELETE CASCADE,
	name TEXT NOT NULL,
	kind TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL DEFAULT '',
	container_name TEXT NOT NULL DEFAULT '',
	last_success_at TEXT NOT NULL DEFAULT '',
	last_checked_at TEXT NOT NULL DEFAULT '',
	snapshot_id TEXT NOT NULL DEFAULT '',
	snapshot_host TEXT NOT NULL DEFAULT '',
	paths_json TEXT NOT NULL DEFAULT '[]',
	error TEXT NOT NULL DEFAULT '',
	updated_at TEXT NOT NULL DEFAULT '',
	UNIQUE(host_id, name)
);
CREATE INDEX IF NOT EXISTS idx_backups_host ON backups(host_id);
CREATE INDEX IF NOT EXISTS idx_backups_status ON backups(status, last_success_at);

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
-- idx_ignored_user is created in migrate() after migrateIgnoredEntitiesToPerUser
-- runs, because on upgrade the column "user_id" does not yet exist when the
-- schema SQL is first executed.

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

-- FLEET-79: user-issued API tokens for read-only programmatic access.
-- Token shape is "fleet_pat_<64 hex>"; only the SHA-256 of the full string
-- is stored. The plaintext "prefix" column ("fleet_pat_<first 8 hex>") is
-- the only piece of the token surfaced anywhere — UI display, audit logs,
-- rate-limit identity. expires_at NULL means "never" (the UI surfaces a
-- prominent warning when the operator picks that). revoked_at is sticky:
-- once set, the token never validates again. last_used_at write is
-- throttled in the middleware to one update per 60s per token.
CREATE TABLE IF NOT EXISTS user_api_tokens (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	token_hash TEXT NOT NULL UNIQUE,
	prefix TEXT NOT NULL,
	label TEXT NOT NULL DEFAULT '',
	scopes TEXT NOT NULL DEFAULT '[]',
	last_used_at TEXT,
	expires_at TEXT,
	revoked_at TEXT,
	created_at TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX IF NOT EXISTS idx_user_api_tokens_user ON user_api_tokens(user_id);
CREATE INDEX IF NOT EXISTS idx_user_api_tokens_hash ON user_api_tokens(token_hash);

-- FLEET-108: operator activity log. Every async user-initiated action goes
-- through busy() in the browser, which POSTs a row here on completion.
-- Drives the left-edge activity drawer and gives the operator a "what did
-- I just do" view that survives page reloads. Admins see all rows; regular
-- users see only their own (host-scoped filtering is a v2 refinement).
CREATE TABLE IF NOT EXISTS activity_events (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	ts TEXT NOT NULL DEFAULT (datetime('now')),
	user_id INTEGER NOT NULL DEFAULT 0,
	verb TEXT NOT NULL DEFAULT '',
	target_type TEXT NOT NULL DEFAULT '',
	target_key TEXT NOT NULL DEFAULT '',
	outcome TEXT NOT NULL DEFAULT 'ok',
	duration_ms INTEGER NOT NULL DEFAULT 0,
	error TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_activity_ts ON activity_events(ts DESC);
CREATE INDEX IF NOT EXISTS idx_activity_user_ts ON activity_events(user_id, ts DESC);

-- FLEET-167: alert rule state. The evaluator writes one row per
-- (rule, entity) so notifications dedupe across ticks and can send a
-- recovery when the condition returns to OK.
CREATE TABLE IF NOT EXISTS alert_states (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	rule_key TEXT NOT NULL,
	entity_type TEXT NOT NULL,
	entity_key TEXT NOT NULL,
	status TEXT NOT NULL DEFAULT 'ok',
	title TEXT NOT NULL DEFAULT '',
	summary TEXT NOT NULL DEFAULT '',
	active_since TEXT NOT NULL DEFAULT '',
	resolved_at TEXT NOT NULL DEFAULT '',
	last_sent_at TEXT NOT NULL DEFAULT '',
	last_error TEXT NOT NULL DEFAULT '',
	updated_at TEXT NOT NULL DEFAULT (datetime('now')),
	UNIQUE(rule_key, entity_key)
);
CREATE INDEX IF NOT EXISTS idx_alert_states_status ON alert_states(status, updated_at);
CREATE INDEX IF NOT EXISTS idx_alert_states_entity ON alert_states(entity_type, entity_key);

-- OpenClaw gateway pairing + bridge registry (FLEET-51) —
-- see PPM Knowledge FLEET/guideline/agent_bridge_pairing.
CREATE TABLE IF NOT EXISTS openclaw_gateways (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	host TEXT NOT NULL UNIQUE,
	url TEXT NOT NULL,
	fc_pubkey_b64 TEXT NOT NULL DEFAULT '',
	fc_device_token_hash TEXT NOT NULL DEFAULT '',
	paired_at TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL DEFAULT 'unpaired',
	-- FLEET-112: default OFF for new gateways. Existing rows keep their
	-- current value (no migration on this column). Operators see a one-
	-- time advisory toast in the dashboard pointing at the new posture.
	auto_approve_bridges INTEGER NOT NULL DEFAULT 0,
	-- FLEET-113: per-gateway OOB-delivery toggle (default OFF until the
	-- gateway-side OpenClaw RFC ships the bridge.confirmation_code RPC).
	oob_delivery_enabled INTEGER NOT NULL DEFAULT 0,
	-- FLEET-114: per-gateway attestation enforcement + the gateway's
	-- own pubkey for signature verification. See migrate() for the
	-- ALTER fallback and the column semantics.
	attestation_required INTEGER NOT NULL DEFAULT 1,
	gateway_pubkey_b64 TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS bridge_pairings (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	host TEXT NOT NULL,
	agent TEXT NOT NULL,
	pubkey_fp TEXT NOT NULL,
	pubkey_pem TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL DEFAULT 'pending',
	approved_at TEXT NOT NULL DEFAULT '',
	request_id TEXT NOT NULL DEFAULT '',
	last_seen_at TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL DEFAULT (datetime('now')),
	-- FLEET-113: OOB confirmation-code state. See migrate() for the
	-- ALTER fallback path used by existing databases.
	confirmation_code_hash TEXT NOT NULL DEFAULT '',
	confirmation_code_expires_at TEXT NOT NULL DEFAULT '',
	confirmation_attempts INTEGER NOT NULL DEFAULT 0,
	-- FLEET-114: per-row attestation outcome (unknown | verified | skipped).
	attestation_status TEXT NOT NULL DEFAULT 'unknown',
	UNIQUE(host, agent)
);
CREATE INDEX IF NOT EXISTS idx_bridge_pairings_fp ON bridge_pairings(pubkey_fp);

-- Agent observability (FLEET-36) — see PPM Knowledge FLEET/guideline/agent_observability.
CREATE TABLE IF NOT EXISTS agents_obs (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	host_id INTEGER NOT NULL REFERENCES hosts(id) ON DELETE CASCADE,
	name TEXT NOT NULL,
	agent_type TEXT NOT NULL DEFAULT '',
	snapshot_json TEXT NOT NULL DEFAULT '',
	snapshot_at TEXT NOT NULL DEFAULT '',
	UNIQUE(host_id, name)
);
CREATE INDEX IF NOT EXISTS idx_agents_obs_host ON agents_obs(host_id);

CREATE TABLE IF NOT EXISTS agent_turns (
	id TEXT PRIMARY KEY,
	agent_id INTEGER NOT NULL REFERENCES agents_obs(id) ON DELETE CASCADE,
	chat_id TEXT NOT NULL DEFAULT '',
	chat_name TEXT NOT NULL DEFAULT '',
	started_at TEXT NOT NULL,
	first_token_at TEXT,
	replied_at TEXT,
	status TEXT NOT NULL DEFAULT '',
	model TEXT NOT NULL DEFAULT '',
	tokens_prompt INTEGER,
	tokens_completion INTEGER,
	duration_ms INTEGER,
	error_class TEXT,
	excerpt TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_agent_turns_agent_started ON agent_turns(agent_id, started_at DESC);
CREATE INDEX IF NOT EXISTS idx_agent_turns_chat ON agent_turns(agent_id, chat_id, started_at DESC);

CREATE TABLE IF NOT EXISTS agent_tools (
	id TEXT PRIMARY KEY,
	turn_id TEXT NOT NULL REFERENCES agent_turns(id) ON DELETE CASCADE,
	name TEXT NOT NULL,
	target TEXT NOT NULL DEFAULT '',
	started_at TEXT NOT NULL,
	completed_at TEXT,
	exit_code INTEGER,
	duration_ms INTEGER
);
CREATE INDEX IF NOT EXISTS idx_agent_tools_turn ON agent_tools(turn_id);

CREATE TABLE IF NOT EXISTS agent_events (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	agent_id INTEGER NOT NULL REFERENCES agents_obs(id) ON DELETE CASCADE,
	ts TEXT NOT NULL,
	kind TEXT NOT NULL,
	turn_id TEXT,
	payload_json TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_agent_events_agent_ts ON agent_events(agent_id, ts DESC);
CREATE INDEX IF NOT EXISTS idx_agent_events_ts ON agent_events(ts);

-- Bosun command channel (FLEET-59/60). Admins enqueue work for a host;
-- bosun picks it up via the heartbeat response and POSTs the result to
-- /api/command-results. kind is validated against bosun's compiled-in
-- allowlist — unknown kinds fail fast with status='failed'.
CREATE TABLE IF NOT EXISTS host_commands (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	host TEXT NOT NULL,
	kind TEXT NOT NULL,
	params TEXT NOT NULL DEFAULT '{}',
	status TEXT NOT NULL DEFAULT 'pending',
	issued_by_user_id INTEGER,
	issued_at TEXT NOT NULL DEFAULT (datetime('now')),
	picked_at TEXT,
	completed_at TEXT,
	result TEXT,
	error TEXT
);
CREATE INDEX IF NOT EXISTS idx_host_commands_host_status ON host_commands(host, status);
CREATE INDEX IF NOT EXISTS idx_host_commands_issued_at ON host_commands(issued_at DESC);
`

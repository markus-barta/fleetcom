package db

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// FLEET-117 posture-name → flag-triple mapping. The wizard surfaces three
// canonical combinations of the three FLEET-111 flags as named "postures"
// so the operator picks one card instead of three independent toggles.
//
//	Auto-pair  →  auto_approve=ON,  oob=OFF, attest=OFF  (1-of-3 trust)
//	Reviewed   →  auto_approve=OFF, oob=OFF, attest=ON   (2-of-3 — production default)
//	Hardened   →  auto_approve=OFF, oob=ON,  attest=ON   (3-of-3 — gated on pubkey)
//
// Non-canonical flag combinations (e.g. operators flipping individual
// toggles via the "Advanced" disclosure) read as Custom on the frontend
// and don't match any posture exactly.
const (
	PostureAutoPair = "auto-pair"
	PostureReviewed = "reviewed"
	PostureHardened = "hardened"
)

// ErrUnknownPosture means the operator passed something other than the
// three canonical posture names. The handler maps this to a 400.
var ErrUnknownPosture = errors.New("unknown posture: must be auto-pair, reviewed, or hardened")

// ErrPostureLocked means hardened was requested but the gateway pubkey
// is empty — without it FleetCom can't actually verify the attestation
// signatures the posture promises, so flipping the flags would be a lie.
// The handler maps this to a 422.
var ErrPostureLocked = errors.New("hardened posture requires the gateway pubkey — paste it via PUT /api/gateways/{host}/pubkey first")

// OpenClawGateway is the server-side record of one OpenClaw gateway
// FleetCom pairs with. One row per host that's running OpenClaw.
type OpenClawGateway struct {
	ID                 int64  `json:"id"`
	Host               string `json:"host"`
	URL                string `json:"url"`
	FCPubkeyB64        string `json:"fc_pubkey_b64,omitempty"`
	PairedAt           string `json:"paired_at,omitempty"`
	Status             string `json:"status"` // unpaired | paired | revoked
	AutoApproveBridges bool   `json:"auto_approve_bridges"`
	// FLEET-113: when true, the server pushes the bridge.confirmation_code
	// RPC to the gateway WS on bridge registration AND requires the OOB
	// code on /approve. When false (default until the gateway-side
	// OpenClaw RFC ships), /approve is permitted without a code.
	OOBDeliveryEnabled bool `json:"oob_delivery_enabled"`
	// FLEET-114: per-gateway attestation enforcement. Combined with the
	// FLEETCOM_REGISTER_ATTESTATION_REQUIRED env (effective = env AND
	// per-gateway flag) so a single misbehaving gateway can be downgraded
	// without flipping the global env.
	AttestationRequired bool `json:"attestation_required"`
	// FLEET-114: gateway's own raw Ed25519 pubkey (b64url-no-padding).
	// Empty until captured (operator paste OR future pair-time exchange).
	// Verification falls through to "skipped" when empty.
	GatewayPubkeyB64 string `json:"gateway_pubkey_b64,omitempty"`
	CreatedAt        string `json:"created_at,omitempty"`
}

// BridgePairing represents one agent-bridge instance paired (or pending)
// with FleetCom. Multiple agents per host each get their own row.
type BridgePairing struct {
	ID         int64  `json:"id"`
	Host       string `json:"host"`
	Agent      string `json:"agent"`
	PubkeyFP   string `json:"pubkey_fp"`
	PubkeyPEM  string `json:"pubkey_pem,omitempty"`
	Status     string `json:"status"` // pending | approved | revoked
	ApprovedAt string `json:"approved_at,omitempty"`
	RequestID  string `json:"request_id,omitempty"`
	LastSeenAt string `json:"last_seen_at,omitempty"`
	// FLEET-114: per-row attestation outcome — unknown | verified | skipped.
	// Renders as a small badge on the bridge row so operators can spot
	// rows that came in under attestation_skipped (post-rollout audit).
	AttestationStatus string `json:"attestation_status,omitempty"`
}

// UpsertGateway is called when a new OpenClaw gateway is detected on a
// host (via bosun heartbeat enrichment) or when the pairing status
// changes.
func (s *Store) UpsertGateway(host, url string) (int64, error) {
	var id int64
	err := s.DB.QueryRow(`
		INSERT INTO openclaw_gateways (host, url) VALUES (?, ?)
		ON CONFLICT(host) DO UPDATE SET url = excluded.url
		RETURNING id
	`, host, url).Scan(&id)
	return id, err
}

// MarkGatewayPaired records a successful pairing with a specific
// FleetCom pubkey + stores the token hash for later rotation audits.
func (s *Store) MarkGatewayPaired(host, pubkeyB64, tokenHash string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.DB.Exec(`
		UPDATE openclaw_gateways
		SET fc_pubkey_b64 = ?, fc_device_token_hash = ?, paired_at = ?, status = 'paired'
		WHERE host = ?
	`, pubkeyB64, tokenHash, now, host)
	return err
}

// AllGateways returns every gateway row. Admin-only consumer.
func (s *Store) AllGateways() ([]OpenClawGateway, error) {
	rows, err := s.DB.Query(`
		SELECT id, host, url, fc_pubkey_b64, paired_at, status,
		       auto_approve_bridges, oob_delivery_enabled,
		       attestation_required, gateway_pubkey_b64, created_at
		FROM openclaw_gateways ORDER BY host
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []OpenClawGateway{}
	for rows.Next() {
		var g OpenClawGateway
		var auto, oob, att int
		if err := rows.Scan(&g.ID, &g.Host, &g.URL, &g.FCPubkeyB64, &g.PairedAt, &g.Status,
			&auto, &oob, &att, &g.GatewayPubkeyB64, &g.CreatedAt); err != nil {
			return nil, err
		}
		g.AutoApproveBridges = auto != 0
		g.OOBDeliveryEnabled = oob != 0
		g.AttestationRequired = att != 0
		out = append(out, g)
	}
	return out, rows.Err()
}

// SetGatewayAttestationRequired flips the per-gateway attestation flag.
// Combined with the env via AND so flipping it OFF on a single gateway
// is enough to skip enforcement just for that one (the env stays ON).
func (s *Store) SetGatewayAttestationRequired(host string, on bool) error {
	val := 0
	if on {
		val = 1
	}
	res, err := s.DB.Exec(`UPDATE openclaw_gateways SET attestation_required = ? WHERE host = ?`, val, host)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("gateway not found: %s", host)
	}
	return nil
}

// SetGatewayPubkey stores the gateway's raw Ed25519 pubkey (b64url-no-padding).
// Caller validates the format upstream — store is policy-free. Empty value
// is allowed (resets to "no key captured", verification falls through to
// skipped).
func (s *Store) SetGatewayPubkey(host, pubkeyB64 string) error {
	res, err := s.DB.Exec(`UPDATE openclaw_gateways SET gateway_pubkey_b64 = ? WHERE host = ?`, pubkeyB64, host)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("gateway not found: %s", host)
	}
	return nil
}

// TOFUPubkeyResult reports the outcome of SetGatewayPubkeyTOFU. Exactly
// one of Pinned/AlreadyMatches/Mismatch is true when err is nil.
type TOFUPubkeyResult struct {
	Pinned         bool   // column was empty, value just stored
	AlreadyMatches bool   // column already held the same value
	Mismatch       bool   // column held a different value (caller decides whether to log/alert)
	Existing       string // current stored value (empty if Pinned)
}

// SetGatewayPubkeyTOFU is the trust-on-first-use path for the auto-pin
// flow (FLEET-123). Behavior:
//
//   - column empty           → write the value, return Pinned=true
//   - column == newPubkey    → no-op, return AlreadyMatches=true
//   - column != newPubkey    → no-op, return Mismatch=true (caller logs)
//
// Why a dedicated method instead of plain SetGatewayPubkey: TOFU must
// never silently overwrite a previously-pinned key. If the gateway's
// identity changes mid-life (rotation, reinstall, MITM), the operator
// has to clear-and-re-pin via the existing PUT /api/gateways/{host}/pubkey
// endpoint (which calls SetGatewayPubkey and is intentionally
// destructive). The auto-pin path stays additive-only.
//
// Mutation is gated by a SQL WHERE on the empty-string default so the
// pin is atomic against concurrent writes.
func (s *Store) SetGatewayPubkeyTOFU(host, newPubkey string) (TOFUPubkeyResult, error) {
	if newPubkey == "" {
		return TOFUPubkeyResult{}, errors.New("SetGatewayPubkeyTOFU: empty pubkey")
	}
	res, err := s.DB.Exec(
		`UPDATE openclaw_gateways
		    SET gateway_pubkey_b64 = ?
		  WHERE host = ?
		    AND gateway_pubkey_b64 = ''`,
		newPubkey, host,
	)
	if err != nil {
		return TOFUPubkeyResult{}, err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		return TOFUPubkeyResult{Pinned: true}, nil
	}
	// Either the column was non-empty or the row doesn't exist. Probe to
	// distinguish.
	var existing string
	err = s.DB.QueryRow(
		`SELECT gateway_pubkey_b64 FROM openclaw_gateways WHERE host = ?`,
		host,
	).Scan(&existing)
	if errors.Is(err, sql.ErrNoRows) {
		return TOFUPubkeyResult{}, fmt.Errorf("gateway not found: %s", host)
	}
	if err != nil {
		return TOFUPubkeyResult{}, err
	}
	if existing == newPubkey {
		return TOFUPubkeyResult{AlreadyMatches: true, Existing: existing}, nil
	}
	return TOFUPubkeyResult{Mismatch: true, Existing: existing}, nil
}

// SetBridgeAttestationStatus persists the per-row outcome. Filters on
// status='pending' so a re-registration with the same fp (which keeps
// the row at status='approved') cannot downgrade an already-verified
// row's badge — an attacker with the bosun token would otherwise be
// able to wipe the audit signal by re-posting without a signature.
// Once the row flips to approved, attestation_status is frozen.
func (s *Store) SetBridgeAttestationStatus(host, agent, status string) error {
	_, err := s.DB.Exec(
		`UPDATE bridge_pairings SET attestation_status = ?
		 WHERE host = ? AND agent = ? AND status = 'pending'`,
		status, host, agent,
	)
	return err
}

// SetOOBDelivery flips the per-gateway oob_delivery_enabled flag. When
// ON, the server pushes confirmation codes via WS on bridge registration
// AND requires the code on /approve.
func (s *Store) SetOOBDelivery(host string, on bool) error {
	val := 0
	if on {
		val = 1
	}
	res, err := s.DB.Exec(`UPDATE openclaw_gateways SET oob_delivery_enabled = ? WHERE host = ?`, val, host)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("gateway not found: %s", host)
	}
	return nil
}

// DeleteGateway drops an openclaw_gateways row (the unpair path).
// Callers are responsible for stopping the manager's WS client and
// removing the on-disk keypair before calling this; otherwise the
// manager will just re-upsert the row on its next reconcile.
func (s *Store) DeleteGateway(host string) error {
	_, err := s.DB.Exec(`DELETE FROM openclaw_gateways WHERE host = ?`, host)
	return err
}

// SetAutoApprove flips the per-gateway auto-approval flag.
func (s *Store) SetAutoApprove(host string, on bool) error {
	val := 0
	if on {
		val = 1
	}
	res, err := s.DB.Exec(`UPDATE openclaw_gateways SET auto_approve_bridges = ? WHERE host = ?`, val, host)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("gateway not found: %s", host)
	}
	return nil
}

// SetGatewayPosture (FLEET-117) atomically applies a named posture —
// (auto-pair / reviewed / hardened) — to the per-gateway flag triple.
// Hardened is gated on the gateway pubkey being non-empty; without it
// FleetCom can't verify the attestation signatures the posture
// promises, so flipping the flags would be a lie.
//
// One UPDATE statement covers all three flags so the gateway is never
// observed in an intermediate state by SSE subscribers.
func (s *Store) SetGatewayPosture(host, posture string) error {
	var aa, oo, at int
	switch posture {
	case PostureAutoPair:
		aa, oo, at = 1, 0, 0
	case PostureReviewed:
		aa, oo, at = 0, 0, 1
	case PostureHardened:
		// Probe pubkey before mutating — operators get a 422 instead of
		// a half-applied state.
		var pubkey string
		err := s.DB.QueryRow(
			`SELECT gateway_pubkey_b64 FROM openclaw_gateways WHERE host = ?`,
			host,
		).Scan(&pubkey)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("gateway not found: %s", host)
		}
		if err != nil {
			return err
		}
		if pubkey == "" {
			return ErrPostureLocked
		}
		aa, oo, at = 0, 1, 1
	default:
		return ErrUnknownPosture
	}

	res, err := s.DB.Exec(`
		UPDATE openclaw_gateways
		   SET auto_approve_bridges = ?,
		       oob_delivery_enabled = ?,
		       attestation_required = ?
		 WHERE host = ?`,
		aa, oo, at, host,
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("gateway not found: %s", host)
	}
	return nil
}

// RegisterBridge stashes a bridge's pubkey fingerprint under its
// (host, agent) identity. Idempotent — re-registering the same host+agent
// replaces the fingerprint and bumps status back to pending if the key
// changed (which means the bridge lost its volume and regenerated).
func (s *Store) RegisterBridge(host, agent, pubkeyFP, pubkeyPEM string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.DB.Exec(`
		INSERT INTO bridge_pairings (host, agent, pubkey_fp, pubkey_pem, status, last_seen_at)
		VALUES (?, ?, ?, ?, 'pending', ?)
		ON CONFLICT(host, agent) DO UPDATE SET
			pubkey_fp = CASE WHEN pubkey_fp = excluded.pubkey_fp THEN pubkey_fp ELSE excluded.pubkey_fp END,
			pubkey_pem = excluded.pubkey_pem,
			status = CASE WHEN pubkey_fp = excluded.pubkey_fp THEN status ELSE 'pending' END,
			last_seen_at = excluded.last_seen_at
	`, host, agent, pubkeyFP, pubkeyPEM, now)
	return err
}

// BridgeByHostAgent looks up a bridge by its (host, agent) primary
// key. Returns (nil, nil) when no row matches.
func (s *Store) BridgeByHostAgent(host, agent string) (*BridgePairing, error) {
	row := s.DB.QueryRow(`
		SELECT id, host, agent, pubkey_fp, pubkey_pem, status, approved_at, request_id, last_seen_at, attestation_status
		FROM bridge_pairings WHERE host = ? AND agent = ?
	`, host, agent)
	var b BridgePairing
	err := row.Scan(&b.ID, &b.Host, &b.Agent, &b.PubkeyFP, &b.PubkeyPEM, &b.Status, &b.ApprovedAt, &b.RequestID, &b.LastSeenAt, &b.AttestationStatus)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &b, err
}

// BridgeByFingerprint looks up a registered bridge by its fingerprint,
// scoped to one host. Returns nil when no match.
func (s *Store) BridgeByFingerprint(host, fp string) (*BridgePairing, error) {
	row := s.DB.QueryRow(`
		SELECT id, host, agent, pubkey_fp, pubkey_pem, status, approved_at, request_id, last_seen_at, attestation_status
		FROM bridge_pairings WHERE host = ? AND pubkey_fp = ?
	`, host, fp)
	var b BridgePairing
	err := row.Scan(&b.ID, &b.Host, &b.Agent, &b.PubkeyFP, &b.PubkeyPEM, &b.Status, &b.ApprovedAt, &b.RequestID, &b.LastSeenAt, &b.AttestationStatus)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &b, err
}

// MarkBridgeApproved records that the gateway accepted the pairing.
func (s *Store) MarkBridgeApproved(host, agent, requestID string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := s.DB.Exec(`
		UPDATE bridge_pairings SET status = 'approved', approved_at = ?, request_id = ?
		WHERE host = ? AND agent = ?
	`, now, requestID, host, agent)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("bridge pairing not found: %s/%s", host, agent)
	}
	return nil
}

// PendingBridges returns every bridge row whose status is 'pending'.
// Drives the FLEET-112 approval surface (host drawer + header counter).
// Cheap full-table scan — pending count is bounded by operator attention,
// not fleet size.
func (s *Store) PendingBridges() ([]BridgePairing, error) {
	rows, err := s.DB.Query(`
		SELECT id, host, agent, pubkey_fp, pubkey_pem, status, approved_at, request_id, last_seen_at, attestation_status
		FROM bridge_pairings WHERE status = 'pending' ORDER BY created_at DESC, host, agent
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []BridgePairing{}
	for rows.Next() {
		var b BridgePairing
		if err := rows.Scan(&b.ID, &b.Host, &b.Agent, &b.PubkeyFP, &b.PubkeyPEM, &b.Status, &b.ApprovedAt, &b.RequestID, &b.LastSeenAt, &b.AttestationStatus); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// MarkBridgeApprovedManual is the manual-approve path (FLEET-112). Same
// shape as MarkBridgeApproved but skips the requestId requirement —
// manual approval has no gateway-side request to correlate against.
func (s *Store) MarkBridgeApprovedManual(host, agent string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := s.DB.Exec(`
		UPDATE bridge_pairings SET status = 'approved', approved_at = ?
		WHERE host = ? AND agent = ? AND status = 'pending'
	`, now, host, agent)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("no pending bridge pairing for %s/%s", host, agent)
	}
	return nil
}

// FLEET-113: per-row OOB confirmation-code state.

// MaxConfirmationAttempts is the rate limit on /approve. Once exceeded
// the row is auto-deleted and the bridge must re-register.
const MaxConfirmationAttempts = 5

// ConfirmationCodeRow is the minimal slice of a pending row needed to
// validate a submitted code. Returned by GetConfirmationCode so the
// API handler can do the constant-time hash compare without a second
// round trip.
type ConfirmationCodeRow struct {
	Hash      string
	ExpiresAt string // RFC3339
	Attempts  int
	PubkeyFP  string
}

// SetConfirmationCode stores a fresh hash + expiry on a pending row,
// resetting the attempts counter. Caller is responsible for computing
// SHA-256(code || pubkey_fp) — the store is policy-free.
func (s *Store) SetConfirmationCode(host, agent, hash, expiresAt string) error {
	res, err := s.DB.Exec(`
		UPDATE bridge_pairings
		SET confirmation_code_hash = ?, confirmation_code_expires_at = ?, confirmation_attempts = 0
		WHERE host = ? AND agent = ?
	`, hash, expiresAt, host, agent)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("bridge pairing not found: %s/%s", host, agent)
	}
	return nil
}

// GetConfirmationCode returns the code-validation slice for a row.
// Empty Hash means "no active code on this row" (legacy or cleared).
func (s *Store) GetConfirmationCode(host, agent string) (*ConfirmationCodeRow, error) {
	row := s.DB.QueryRow(`
		SELECT confirmation_code_hash, confirmation_code_expires_at, confirmation_attempts, pubkey_fp
		FROM bridge_pairings WHERE host = ? AND agent = ?
	`, host, agent)
	var c ConfirmationCodeRow
	err := row.Scan(&c.Hash, &c.ExpiresAt, &c.Attempts, &c.PubkeyFP)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// ClearConfirmationCode wipes the hash + expiry after a successful
// /approve so the same code cannot be replayed.
func (s *Store) ClearConfirmationCode(host, agent string) error {
	_, err := s.DB.Exec(`
		UPDATE bridge_pairings
		SET confirmation_code_hash = '', confirmation_code_expires_at = '', confirmation_attempts = 0
		WHERE host = ? AND agent = ?
	`, host, agent)
	return err
}

// ConsumeConfirmationAttempt atomically reserves one brute-force slot.
// Returns:
//
//	(newAttempts, true, nil)  — slot consumed, current count is newAttempts
//	(0, false, nil)           — cap already reached or row gone (caller treats both as auto-reject)
//	(0, false, err)           — storage error
//
// The atomic UPDATE-with-cap is critical for the rate limit: without it,
// concurrent /approve requests can all read attempts < max, all execute
// their constant-time compare (each one a brute-force probe), and only
// afterward increment. Net effect would be parallelism-bounded brute
// force, not 5-bounded. The WHERE confirmation_attempts < ? clause
// serializes the increments through SQLite's row locks: any request
// that finds the cap reached gets RowsAffected==0 and bails before
// touching the cryptographic compare path.
//
// Callers MUST consume an attempt slot BEFORE doing the constant-time
// compare. A successful match should follow with ClearConfirmationCode
// to reset the counter for any future code mint.
func (s *Store) ConsumeConfirmationAttempt(host, agent string) (newAttempts int, ok bool, err error) {
	res, err := s.DB.Exec(`
		UPDATE bridge_pairings
		   SET confirmation_attempts = confirmation_attempts + 1
		 WHERE host = ? AND agent = ?
		   AND confirmation_attempts < ?
	`, host, agent, MaxConfirmationAttempts)
	if err != nil {
		return 0, false, err
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		// Cap already reached, OR the row disappeared (race with reject).
		// Either way the caller should treat this as auto-reject.
		return 0, false, nil
	}
	// Read back the post-increment value so the handler can format
	// "<n>/<max>" feedback for the operator.
	if err := s.DB.QueryRow(
		`SELECT confirmation_attempts FROM bridge_pairings WHERE host = ? AND agent = ?`,
		host, agent,
	).Scan(&newAttempts); err != nil {
		return 0, true, err
	}
	return newAttempts, true, nil
}

// AllBridgePairings returns every bridge row for the dashboard.
func (s *Store) AllBridgePairings() ([]BridgePairing, error) {
	rows, err := s.DB.Query(`
		SELECT id, host, agent, pubkey_fp, pubkey_pem, status, approved_at, request_id, last_seen_at, attestation_status
		FROM bridge_pairings ORDER BY host, agent
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []BridgePairing{}
	for rows.Next() {
		var b BridgePairing
		if err := rows.Scan(&b.ID, &b.Host, &b.Agent, &b.PubkeyFP, &b.PubkeyPEM, &b.Status, &b.ApprovedAt, &b.RequestID, &b.LastSeenAt, &b.AttestationStatus); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// FLEET-109: data sources for the bridge-deploy suggestion chip rails.

// BridgeAgentsForHost returns the agent names already paired as bridges
// on this host. Drives the "ON THIS HOST" rail.
func (s *Store) BridgeAgentsForHost(host string) ([]string, error) {
	rows, err := s.DB.Query(
		`SELECT agent FROM bridge_pairings WHERE host = ? ORDER BY agent`, host,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// HeartbeatAgentsForHost returns the agent names reported via bosun's
// FLEETCOM_AGENTS heartbeat for this host. Drives the second source for
// the "ON THIS HOST" rail (union with bridges).
func (s *Store) HeartbeatAgentsForHost(host string) ([]string, error) {
	rows, err := s.DB.Query(
		`SELECT a.name FROM agents a
		 JOIN hosts h ON h.id = a.host_id
		 WHERE h.hostname = ?
		 ORDER BY a.name`, host,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// TopBridgeNamesAcrossFleet returns up to limit agent names by frequency
// across all hosts EXCEPT excludeHost. Drives the "SEEN IN YOUR FLEET"
// rail. Trivial cardinality so a single GROUP BY is fine; no need for
// a pre-aggregated table at this scale.
func (s *Store) TopBridgeNamesAcrossFleet(excludeHost string, limit int) ([]string, error) {
	if limit <= 0 {
		limit = 3
	}
	rows, err := s.DB.Query(
		`SELECT agent FROM bridge_pairings
		 WHERE host != ?
		 GROUP BY agent
		 ORDER BY COUNT(*) DESC, agent ASC
		 LIMIT ?`, excludeHost, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// DeleteBridgePairing removes a bridge row (the revoke path). The caller
// is responsible for also invoking device.token.revoke on the gateway.
func (s *Store) DeleteBridgePairing(host, agent string) error {
	res, err := s.DB.Exec(`DELETE FROM bridge_pairings WHERE host = ? AND agent = ?`, host, agent)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("bridge pairing not found: %s/%s", host, agent)
	}
	return nil
}

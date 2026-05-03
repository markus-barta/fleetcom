package db

import (
	"database/sql"
	"fmt"
	"time"
)

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
	OOBDeliveryEnabled bool   `json:"oob_delivery_enabled"`
	CreatedAt          string `json:"created_at,omitempty"`
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
		SELECT id, host, url, fc_pubkey_b64, paired_at, status, auto_approve_bridges, oob_delivery_enabled, created_at
		FROM openclaw_gateways ORDER BY host
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []OpenClawGateway{}
	for rows.Next() {
		var g OpenClawGateway
		var auto, oob int
		if err := rows.Scan(&g.ID, &g.Host, &g.URL, &g.FCPubkeyB64, &g.PairedAt, &g.Status, &auto, &oob, &g.CreatedAt); err != nil {
			return nil, err
		}
		g.AutoApproveBridges = auto != 0
		g.OOBDeliveryEnabled = oob != 0
		out = append(out, g)
	}
	return out, rows.Err()
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
		SELECT id, host, agent, pubkey_fp, pubkey_pem, status, approved_at, request_id, last_seen_at
		FROM bridge_pairings WHERE host = ? AND agent = ?
	`, host, agent)
	var b BridgePairing
	err := row.Scan(&b.ID, &b.Host, &b.Agent, &b.PubkeyFP, &b.PubkeyPEM, &b.Status, &b.ApprovedAt, &b.RequestID, &b.LastSeenAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &b, err
}

// BridgeByFingerprint looks up a registered bridge by its fingerprint,
// scoped to one host. Returns nil when no match.
func (s *Store) BridgeByFingerprint(host, fp string) (*BridgePairing, error) {
	row := s.DB.QueryRow(`
		SELECT id, host, agent, pubkey_fp, pubkey_pem, status, approved_at, request_id, last_seen_at
		FROM bridge_pairings WHERE host = ? AND pubkey_fp = ?
	`, host, fp)
	var b BridgePairing
	err := row.Scan(&b.ID, &b.Host, &b.Agent, &b.PubkeyFP, &b.PubkeyPEM, &b.Status, &b.ApprovedAt, &b.RequestID, &b.LastSeenAt)
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
		SELECT id, host, agent, pubkey_fp, pubkey_pem, status, approved_at, request_id, last_seen_at
		FROM bridge_pairings WHERE status = 'pending' ORDER BY created_at DESC, host, agent
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []BridgePairing{}
	for rows.Next() {
		var b BridgePairing
		if err := rows.Scan(&b.ID, &b.Host, &b.Agent, &b.PubkeyFP, &b.PubkeyPEM, &b.Status, &b.ApprovedAt, &b.RequestID, &b.LastSeenAt); err != nil {
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

// IncrementConfirmationAttempts bumps the per-row attempts counter and
// returns the new value. Callers compare against MaxConfirmationAttempts
// to decide whether to auto-reject.
func (s *Store) IncrementConfirmationAttempts(host, agent string) (int, error) {
	if _, err := s.DB.Exec(`
		UPDATE bridge_pairings SET confirmation_attempts = confirmation_attempts + 1
		WHERE host = ? AND agent = ?
	`, host, agent); err != nil {
		return 0, err
	}
	var n int
	err := s.DB.QueryRow(`SELECT confirmation_attempts FROM bridge_pairings WHERE host = ? AND agent = ?`, host, agent).Scan(&n)
	return n, err
}

// AllBridgePairings returns every bridge row for the dashboard.
func (s *Store) AllBridgePairings() ([]BridgePairing, error) {
	rows, err := s.DB.Query(`
		SELECT id, host, agent, pubkey_fp, pubkey_pem, status, approved_at, request_id, last_seen_at
		FROM bridge_pairings ORDER BY host, agent
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []BridgePairing{}
	for rows.Next() {
		var b BridgePairing
		if err := rows.Scan(&b.ID, &b.Host, &b.Agent, &b.PubkeyFP, &b.PubkeyPEM, &b.Status, &b.ApprovedAt, &b.RequestID, &b.LastSeenAt); err != nil {
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

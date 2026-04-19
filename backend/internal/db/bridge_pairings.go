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
		SELECT id, host, url, fc_pubkey_b64, paired_at, status, auto_approve_bridges, created_at
		FROM openclaw_gateways ORDER BY host
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []OpenClawGateway{}
	for rows.Next() {
		var g OpenClawGateway
		var auto int
		if err := rows.Scan(&g.ID, &g.Host, &g.URL, &g.FCPubkeyB64, &g.PairedAt, &g.Status, &auto, &g.CreatedAt); err != nil {
			return nil, err
		}
		g.AutoApproveBridges = auto != 0
		out = append(out, g)
	}
	return out, rows.Err()
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

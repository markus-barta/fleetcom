package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/markus-barta/fleetcom/internal/auth"
	"github.com/markus-barta/fleetcom/internal/db"
	"github.com/markus-barta/fleetcom/internal/openclaw"
)

// PairGateway implements POST /api/gateways/{host}/pair (FLEET-61):
// generate FleetCom's Ed25519 keypair + operator token for a gateway
// on `{host}`, persist them to the fleetcom container's own data dir
// (/app/data/openclaw-keys/<host>/), and enqueue an openclaw.pair
// command so the host's bosun merges our entry into OpenClaw's
// paired.json + restarts the gateway container.
//
// After this endpoint returns, the flow is entirely async:
//  1. bosun's next heartbeat picks up the command (up to interval sec)
//  2. bosun exec-merges paired.json, restarts openclaw-gateway
//  3. bosun reports back via /api/command-results
//  4. manager's next reconcile (up to 2m) finds the new keypair on
//     disk, upserts openclaw_gateways, starts a WS client, connects
//
// End-to-end: "+ Add Gateway" click → paired gateway within ~2-3min.
//
// Idempotency: if keys already exist, returns 409. Admin must DELETE
// the gateway first (tears down client + removes files) to re-pair.
func PairGateway(store *db.Store, keyRoot string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		host := chi.URLParam(r, "host")
		if host == "" {
			http.Error(w, "host required", http.StatusBadRequest)
			return
		}
		// Basic sanitization — host is used in a filesystem path.
		if strings.ContainsAny(host, "/\\..") || strings.ContainsAny(host, " \t\n") {
			http.Error(w, "invalid host", http.StatusBadRequest)
			return
		}

		// Verify host is registered with FleetCom (has a bosun token).
		// Without a registered host, bosun can't authenticate to pick
		// up the command.
		hosts, _ := store.AllHosts()
		known := false
		for _, h := range hosts {
			if h.Hostname == host {
				known = true
				break
			}
		}
		if !known {
			http.Error(w, "host not found in fleetcom registry", http.StatusNotFound)
			return
		}

		dir := filepath.Join(keyRoot, host)
		privPath := filepath.Join(dir, "private.pem")
		pubPath := filepath.Join(dir, "public.pem")
		tokPath := filepath.Join(dir, "operator-token")

		// Idempotency guard.
		if _, err := os.Stat(privPath); err == nil {
			http.Error(w, "gateway already paired — delete the existing pairing first", http.StatusConflict)
			return
		}

		// Generate + persist identity.
		if err := os.MkdirAll(dir, 0o700); err != nil {
			log.Printf("pair-gateway mkdir %s: %v", dir, err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		id, err := openclaw.GenerateIdentity(privPath, pubPath)
		if err != nil {
			log.Printf("pair-gateway generate identity: %v", err)
			http.Error(w, "keygen failed", http.StatusInternalServerError)
			return
		}

		// Generate + persist a 32-byte hex operator token.
		var tokBytes [32]byte
		if _, err := rand.Read(tokBytes[:]); err != nil {
			log.Printf("pair-gateway random: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		opToken := hex.EncodeToString(tokBytes[:])
		if err := os.WriteFile(tokPath, []byte(opToken+"\n"), 0o600); err != nil {
			log.Printf("pair-gateway write token: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		// Read back the public PEM for the command params — the bosun
		// handler needs it in PEM form to merge into paired.json.
		pubPEM, err := os.ReadFile(pubPath)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		// Optional container name override from the UI picker — lets
		// admins target hosts where the gateway container is named
		// differently (miniserver-bp's openclaw-percaival, etc.).
		containerName := "openclaw-gateway"
		if r.Body != nil && r.ContentLength != 0 {
			var body struct {
				ContainerName string `json:"container_name"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err == nil {
				if c := strings.TrimSpace(body.ContainerName); c != "" {
					containerName = c
				}
			}
		}

		// Enqueue the bosun-side command. Params carry exactly what
		// bosun needs; keys never leave this server otherwise.
		params := map[string]any{
			"public_key_pem": string(pubPEM),
			"operator_token": opToken,
			"container_name": containerName,
		}
		user := auth.GetUser(r)
		var uid *int64
		if user != nil {
			v := user.ID
			uid = &v
		}
		cmdID, err := store.EnqueueCommand(host, "openclaw.pair", params, uid)
		if err != nil {
			log.Printf("pair-gateway enqueue: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		// Also upsert the gateway row so the Gateways tab shows a
		// "pending" entry immediately, instead of waiting for the
		// manager's 2m reconcile tick.
		url := fmt.Sprintf("wss://%s:18789", host)
		if _, err := store.UpsertGateway(host, url); err != nil {
			log.Printf("pair-gateway upsert: %v", err)
		}

		log.Printf("pair-gateway %s: keys generated, command %d enqueued, deviceId=%s", host, cmdID, id.DeviceID[:12])

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":         true,
			"host":       host,
			"command_id": cmdID,
			"device_id":  id.DeviceID,
			"status":     "pending",
		})
	}
}

// UnpairGateway tears down a gateway pairing: stops the WS client,
// removes the keypair + token files, deletes the openclaw_gateways row
// (cascades nothing — we're deliberate about leaving bridge_pairings
// rows intact so admins can inspect history). Does NOT touch the
// gateway-side paired.json; admin needs to remove our entry manually
// if they want to fully decommission.
func UnpairGateway(store *db.Store, keyRoot string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		host := chi.URLParam(r, "host")
		if host == "" {
			http.Error(w, "host required", http.StatusBadRequest)
			return
		}
		dir := filepath.Join(keyRoot, host)
		if err := os.RemoveAll(dir); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Printf("unpair-gateway rm %s: %v", dir, err)
		}
		if err := store.DeleteGateway(host); err != nil {
			log.Printf("unpair-gateway delete row: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// HostsAvailableForPairing returns the hostnames that are registered
// with FleetCom but don't yet have a paired gateway. Used by the
// + Add Gateway UI to populate a picker.
func HostsAvailableForPairing(store *db.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		hosts, err := store.AllHosts()
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		gws, _ := store.AllGateways()
		paired := make(map[string]struct{}, len(gws))
		for _, g := range gws {
			paired[g.Host] = struct{}{}
		}
		out := []string{}
		for _, h := range hosts {
			if _, ok := paired[h.Hostname]; !ok {
				out = append(out, h.Hostname)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	}
}

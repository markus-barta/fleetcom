package api

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/markus-barta/fleetcom/internal/auth"
	"github.com/markus-barta/fleetcom/internal/db"
)

// FLEET-108: operator activity log API.
//
// POST /api/activity   — busy() in the browser fires this after every
//                        async user-initiated action settles
// GET  /api/activity   — drawer reads recent rows
//
// Auth: both endpoints require a session (cookie + TOTP). API tokens
// don't authenticate here — the activity log is operator-context, and
// agent-driven actions go through other write endpoints (heartbeat,
// command-results) that have their own audit story.
//
// Scoping: regular users see only their own rows; admins see everything.

const (
	activityVerbMaxLen   = 32
	activityTargetMaxLen = 256
	activityErrorMaxLen  = 512
)

var activityValidOutcomes = map[string]bool{"ok": true, "err": true, "pend": true}

type recordActivityRequest struct {
	Verb       string `json:"verb"`
	TargetType string `json:"target_type"`
	TargetKey  string `json:"target_key"`
	Outcome    string `json:"outcome"`
	DurationMs int64  `json:"duration_ms"`
	Error      string `json:"error"`
}

// RecordActivity handles POST /api/activity. Stores a single row scoped
// to the calling user. Returns 201 + the new id.
func RecordActivity(store *db.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := auth.GetUser(r)
		if u == nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		var req recordActivityRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		// Normalize + validate. Verbs are caps-only enums by convention.
		req.Verb = strings.ToUpper(strings.TrimSpace(req.Verb))
		req.TargetType = strings.TrimSpace(req.TargetType)
		req.TargetKey = strings.TrimSpace(req.TargetKey)
		req.Outcome = strings.ToLower(strings.TrimSpace(req.Outcome))
		if req.Outcome == "" {
			req.Outcome = "ok"
		}

		if req.Verb == "" || len(req.Verb) > activityVerbMaxLen {
			http.Error(w, "verb is required (max "+strconv.Itoa(activityVerbMaxLen)+" chars)", http.StatusBadRequest)
			return
		}
		if len(req.TargetType) > activityTargetMaxLen || len(req.TargetKey) > activityTargetMaxLen {
			http.Error(w, "target too long", http.StatusBadRequest)
			return
		}
		if !activityValidOutcomes[req.Outcome] {
			http.Error(w, "outcome must be ok|err|pend", http.StatusBadRequest)
			return
		}
		if len(req.Error) > activityErrorMaxLen {
			req.Error = req.Error[:activityErrorMaxLen]
		}
		if req.DurationMs < 0 {
			req.DurationMs = 0
		}

		id, err := store.RecordActivity(db.ActivityEvent{
			UserID:     u.ID,
			Verb:       req.Verb,
			TargetType: req.TargetType,
			TargetKey:  req.TargetKey,
			Outcome:    req.Outcome,
			DurationMs: req.DurationMs,
			Error:      req.Error,
		})
		if err != nil {
			log.Printf("error: record activity user_id=%d: %v", u.ID, err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{"id": id})
	}
}

// ListActivity handles GET /api/activity?since=&limit=&verb=&target=&outcome=
// — admins see everything, regular users see only their own rows.
func ListActivity(store *db.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := auth.GetUser(r)
		if u == nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		q := r.URL.Query()
		opts := db.ListActivityOpts{
			Verb:    strings.ToUpper(strings.TrimSpace(q.Get("verb"))),
			Target:  strings.TrimSpace(q.Get("target")),
			Outcome: strings.ToLower(strings.TrimSpace(q.Get("outcome"))),
		}
		if u.Role != "admin" {
			opts.UserID = u.ID
		}
		if since, err := db.ActivityValidateSince(q.Get("since")); err != nil {
			http.Error(w, "since must be RFC3339", http.StatusBadRequest)
			return
		} else {
			opts.Since = since
		}
		if l := q.Get("limit"); l != "" {
			if n, err := strconv.Atoi(l); err == nil && n > 0 {
				opts.Limit = n
			}
		}

		rows, err := store.ListActivity(opts)
		if err != nil {
			log.Printf("error: list activity user_id=%d: %v", u.ID, err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if rows == nil {
			rows = []db.ActivityEvent{}
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(rows)
	}
}

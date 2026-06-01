package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/markus-barta/fleetcom/internal/alerting"
	"github.com/markus-barta/fleetcom/internal/db"
)

// GetAlertConfig returns DB-backed alert switches plus env-token status.
func GetAlertConfig(engine *alerting.Engine) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(engine.Config())
	}
}

// UpdateAlertConfig updates non-secret alerting settings. The Telegram bot
// token is intentionally env/agenix-only and never writable through HTTP.
func UpdateAlertConfig(store *db.Store, engine *alerting.Engine) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			AlertingEnabled      *bool   `json:"alerting_enabled"`
			TelegramEnabled      *bool   `json:"telegram_enabled"`
			TelegramChatID       *string `json:"telegram_chat_id"`
			HostDownEnabled      *bool   `json:"host_down_enabled"`
			HostDownGraceSeconds *int    `json:"host_down_grace_seconds"`
			BackupEnabled        *bool   `json:"backup_unhealthy_enabled"`
			BackupStaleSeconds   *int    `json:"backup_stale_seconds"`
			BackupExpectedHosts  *string `json:"backup_expected_hosts"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}
		if body.AlertingEnabled != nil {
			if err := store.SetSetting("alerting_enabled", boolString(*body.AlertingEnabled)); err != nil {
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
		}
		if body.TelegramEnabled != nil {
			if err := store.SetSetting("alert_telegram_enabled", boolString(*body.TelegramEnabled)); err != nil {
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
		}
		if body.TelegramChatID != nil {
			v := strings.TrimSpace(*body.TelegramChatID)
			if len(v) > 128 {
				http.Error(w, "telegram_chat_id too long", http.StatusBadRequest)
				return
			}
			if err := store.SetSetting("alert_telegram_chat_id", v); err != nil {
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
		}
		if body.HostDownEnabled != nil {
			if err := store.SetSetting("alert_host_down_enabled", boolString(*body.HostDownEnabled)); err != nil {
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
		}
		if body.HostDownGraceSeconds != nil {
			v := *body.HostDownGraceSeconds
			if v < 60 || v > 86400 {
				http.Error(w, "host_down_grace_seconds must be 60-86400", http.StatusBadRequest)
				return
			}
			if err := store.SetSetting("alert_host_down_grace_seconds", strconv.Itoa(v)); err != nil {
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
		}
		if body.BackupEnabled != nil {
			if err := store.SetSetting("alert_backup_unhealthy_enabled", boolString(*body.BackupEnabled)); err != nil {
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
		}
		if body.BackupStaleSeconds != nil {
			v := *body.BackupStaleSeconds
			if v < 3600 || v > 30*24*60*60 {
				http.Error(w, "backup_stale_seconds must be 3600-2592000", http.StatusBadRequest)
				return
			}
			if err := store.SetSetting("alert_backup_stale_seconds", strconv.Itoa(v)); err != nil {
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
		}
		if body.BackupExpectedHosts != nil {
			v := strings.TrimSpace(*body.BackupExpectedHosts)
			if len(v) > 512 {
				http.Error(w, "backup_expected_hosts too long", http.StatusBadRequest)
				return
			}
			if err := store.SetSetting("alert_backup_expected_hosts", v); err != nil {
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(engine.Config())
	}
}

func SendTestAlert(engine *alerting.Engine) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := engine.SendTest(r.Context()); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`))
	}
}

func boolString(v bool) string {
	if v {
		return "1"
	}
	return "0"
}

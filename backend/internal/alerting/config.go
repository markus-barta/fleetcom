package alerting

import (
	"os"
	"strconv"
	"strings"

	"github.com/markus-barta/fleetcom/internal/db"
)

const (
	EnvTelegramBotToken    = "FLEETCOM_TELEGRAM_BOT_TOKEN"
	EnvTelegramChatID      = "FLEETCOM_TELEGRAM_CHAT_ID"
	EnvBackupExpectedHosts = "FLEETCOM_BACKUP_EXPECTED_HOSTS"

	settingAlertingEnabled     = "alerting_enabled"
	settingTelegramEnabled     = "alert_telegram_enabled"
	settingTelegramChatID      = "alert_telegram_chat_id"
	settingHostDownEnabled     = "alert_host_down_enabled"
	settingHostDownGrace       = "alert_host_down_grace_seconds"
	settingBackupEnabled       = "alert_backup_unhealthy_enabled"
	settingBackupStale         = "alert_backup_stale_seconds"
	settingBackupExpectedHosts = "alert_backup_expected_hosts"
	defaultHostDownGraceSecond = 300
	defaultBackupStaleSecond   = 48 * 60 * 60
)

// Config is the admin-editable alerting configuration plus env-derived
// secret availability. The Telegram bot token intentionally never leaves
// the process; API callers only see TokenConfigured.
type Config struct {
	AlertingEnabled      bool   `json:"alerting_enabled"`
	TelegramEnabled      bool   `json:"telegram_enabled"`
	TelegramTokenSet     bool   `json:"telegram_token_configured"`
	TelegramChatID       string `json:"telegram_chat_id"`
	HostDownEnabled      bool   `json:"host_down_enabled"`
	HostDownGraceSeconds int    `json:"host_down_grace_seconds"`
	BackupEnabled        bool   `json:"backup_unhealthy_enabled"`
	BackupStaleSeconds   int    `json:"backup_stale_seconds"`
	BackupExpectedHosts  string `json:"backup_expected_hosts"`
}

func LoadConfig(store *db.Store) Config {
	chatID, _ := store.GetSetting(settingTelegramChatID, "")
	if strings.TrimSpace(chatID) == "" {
		chatID = strings.TrimSpace(os.Getenv(EnvTelegramChatID))
	}
	expectedHosts, _ := store.GetSetting(settingBackupExpectedHosts, "")
	if strings.TrimSpace(expectedHosts) == "" {
		expectedHosts = strings.TrimSpace(os.Getenv(EnvBackupExpectedHosts))
	}
	hostDownGrace := intSetting(store, settingHostDownGrace, defaultHostDownGraceSecond)
	if hostDownGrace < 60 || hostDownGrace > 86400 {
		hostDownGrace = defaultHostDownGraceSecond
	}
	backupStale := intSetting(store, settingBackupStale, defaultBackupStaleSecond)
	if backupStale < 3600 || backupStale > 30*24*60*60 {
		backupStale = defaultBackupStaleSecond
	}
	return Config{
		AlertingEnabled:      boolSetting(store, settingAlertingEnabled, false),
		TelegramEnabled:      boolSetting(store, settingTelegramEnabled, false),
		TelegramTokenSet:     strings.TrimSpace(os.Getenv(EnvTelegramBotToken)) != "",
		TelegramChatID:       strings.TrimSpace(chatID),
		HostDownEnabled:      boolSetting(store, settingHostDownEnabled, false),
		HostDownGraceSeconds: hostDownGrace,
		BackupEnabled:        boolSetting(store, settingBackupEnabled, false),
		BackupStaleSeconds:   backupStale,
		BackupExpectedHosts:  strings.TrimSpace(expectedHosts),
	}
}

func boolSetting(store *db.Store, key string, fallback bool) bool {
	fb := "0"
	if fallback {
		fb = "1"
	}
	v, _ := store.GetSetting(key, fb)
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func intSetting(store *db.Store, key string, fallback int) int {
	v, _ := store.GetSetting(key, strconv.Itoa(fallback))
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return fallback
	}
	return n
}

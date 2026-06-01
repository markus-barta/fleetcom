package alerting

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/markus-barta/fleetcom/internal/db"
)

type fakeTelegramSender struct {
	messages []string
	err      error
}

func (f *fakeTelegramSender) SendTelegram(ctx context.Context, chatID, text string) error {
	if f.err != nil {
		return f.err
	}
	f.messages = append(f.messages, text)
	return nil
}

func newAlertTestStore(t *testing.T) *db.Store {
	t.Helper()
	store, err := db.Open(filepath.Join(t.TempDir(), "alerts.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func enableHostDownAlerts(t *testing.T, store *db.Store) {
	t.Helper()
	for k, v := range map[string]string{
		settingAlertingEnabled: "1",
		settingTelegramEnabled: "1",
		settingTelegramChatID:  "-100123",
		settingHostDownEnabled: "1",
		settingHostDownGrace:   "300",
	} {
		if err := store.SetSetting(k, v); err != nil {
			t.Fatalf("SetSetting(%s): %v", k, err)
		}
	}
}

func enableBackupAlerts(t *testing.T, store *db.Store) {
	t.Helper()
	for k, v := range map[string]string{
		settingAlertingEnabled:     "1",
		settingTelegramEnabled:     "1",
		settingTelegramChatID:      "-100123",
		settingBackupEnabled:       "1",
		settingBackupStale:         "172800",
		settingBackupExpectedHosts: "hsb1,hsb8",
	} {
		if err := store.SetSetting(k, v); err != nil {
			t.Fatalf("SetSetting(%s): %v", k, err)
		}
	}
}

func TestEngineHostDownFiresOnceAndRecovers(t *testing.T) {
	store := newAlertTestStore(t)
	enableHostDownAlerts(t, store)
	if _, err := store.UpsertHeartbeat("hsb1", "NixOS", "6.6", 1, "test", "docker", "boot", nil, nil, nil); err != nil {
		t.Fatalf("UpsertHeartbeat: %v", err)
	}
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	old := now.Add(-10 * time.Minute).Format(time.RFC3339)
	if _, err := store.DB.Exec(`UPDATE hosts SET last_seen = ? WHERE hostname = 'hsb1'`, old); err != nil {
		t.Fatalf("age host: %v", err)
	}

	sender := &fakeTelegramSender{}
	engine := NewEngineWithSender(store, sender)
	engine.now = func() time.Time { return now }

	if err := engine.EvaluateOnce(context.Background()); err != nil {
		t.Fatalf("EvaluateOnce #1: %v", err)
	}
	if len(sender.messages) != 1 || !strings.Contains(sender.messages[0], "host heartbeat missing") {
		t.Fatalf("messages after fire = %#v", sender.messages)
	}
	if err := engine.EvaluateOnce(context.Background()); err != nil {
		t.Fatalf("EvaluateOnce #2: %v", err)
	}
	if len(sender.messages) != 1 {
		t.Fatalf("duplicate alert sent: %#v", sender.messages)
	}

	fresh := now.Add(-time.Minute).Format(time.RFC3339)
	if _, err := store.DB.Exec(`UPDATE hosts SET last_seen = ? WHERE hostname = 'hsb1'`, fresh); err != nil {
		t.Fatalf("freshen host: %v", err)
	}
	if err := engine.EvaluateOnce(context.Background()); err != nil {
		t.Fatalf("EvaluateOnce #3: %v", err)
	}
	if len(sender.messages) != 2 || !strings.Contains(sender.messages[1], "heartbeat restored") {
		t.Fatalf("messages after recovery = %#v", sender.messages)
	}
	st, err := store.AlertState(hostDownRuleKey, "hsb1")
	if err != nil {
		t.Fatalf("AlertState: %v", err)
	}
	if st == nil || st.Status != "ok" {
		t.Fatalf("state = %#v", st)
	}
}

func TestEngineBackupUnhealthyAndMissing(t *testing.T) {
	store := newAlertTestStore(t)
	enableBackupAlerts(t, store)
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	old := now.Add(-72 * time.Hour).Format(time.RFC3339)
	if _, err := store.UpsertHeartbeat("hsb1", "NixOS", "6.6", 1, "test", "docker", "boot", nil, nil, nil, []db.Backup{{
		Name: "restic-cron-hetzner", Kind: "restic", Status: "ok", LastSuccessAt: old, LastCheckedAt: now.Format(time.RFC3339),
	}}); err != nil {
		t.Fatalf("UpsertHeartbeat hsb1: %v", err)
	}
	if _, err := store.UpsertHeartbeat("hsb8", "NixOS", "6.6", 1, "test", "docker", "boot", nil, nil, nil, []db.Backup{}); err != nil {
		t.Fatalf("UpsertHeartbeat hsb8: %v", err)
	}

	sender := &fakeTelegramSender{}
	engine := NewEngineWithSender(store, sender)
	engine.now = func() time.Time { return now }

	if err := engine.EvaluateOnce(context.Background()); err != nil {
		t.Fatalf("EvaluateOnce #1: %v", err)
	}
	if len(sender.messages) != 2 {
		t.Fatalf("messages after fire = %#v", sender.messages)
	}
	if !strings.Contains(strings.Join(sender.messages, "\n"), "backup unhealthy") ||
		!strings.Contains(strings.Join(sender.messages, "\n"), "backup missing") {
		t.Fatalf("missing backup alerts: %#v", sender.messages)
	}
	if err := engine.EvaluateOnce(context.Background()); err != nil {
		t.Fatalf("EvaluateOnce #2: %v", err)
	}
	if len(sender.messages) != 2 {
		t.Fatalf("duplicate backup alert sent: %#v", sender.messages)
	}

	fresh := now.Add(-2 * time.Hour).Format(time.RFC3339)
	if _, err := store.UpsertHeartbeat("hsb1", "NixOS", "6.6", 1, "test", "docker", "boot", nil, nil, nil, []db.Backup{{
		Name: "restic-cron-hetzner", Kind: "restic", Status: "ok", LastSuccessAt: fresh, LastCheckedAt: now.Format(time.RFC3339),
	}}); err != nil {
		t.Fatalf("fresh backup hsb1: %v", err)
	}
	if _, err := store.UpsertHeartbeat("hsb8", "NixOS", "6.6", 1, "test", "docker", "boot", nil, nil, nil, []db.Backup{{
		Name: "restic-cron-hetzner", Kind: "restic", Status: "ok", LastSuccessAt: fresh, LastCheckedAt: now.Format(time.RFC3339),
	}}); err != nil {
		t.Fatalf("fresh backup hsb8: %v", err)
	}
	if err := engine.EvaluateOnce(context.Background()); err != nil {
		t.Fatalf("EvaluateOnce #3: %v", err)
	}
	joined := strings.Join(sender.messages, "\n")
	if len(sender.messages) != 4 || !strings.Contains(joined, "backup healthy") || !strings.Contains(joined, "metadata restored") {
		t.Fatalf("messages after recovery = %#v", sender.messages)
	}
}

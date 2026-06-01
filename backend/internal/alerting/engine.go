package alerting

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/markus-barta/fleetcom/internal/db"
)

const hostDownRuleKey = "host_down"
const backupUnhealthyRuleKey = "backup_unhealthy"
const backupMissingRuleKey = "backup_missing"

type Engine struct {
	store  *db.Store
	sender TelegramSender
	now    func() time.Time
}

func NewEngine(store *db.Store) *Engine {
	return NewEngineWithSender(store, NewTelegramClientFromEnv())
}

func NewEngineWithSender(store *db.Store, sender TelegramSender) *Engine {
	return &Engine{
		store:  store,
		sender: sender,
		now:    func() time.Time { return time.Now().UTC() },
	}
}

func (e *Engine) Start(ctx context.Context) {
	go func() {
		if err := e.EvaluateOnce(ctx); err != nil {
			log.Printf("alerting: initial evaluation failed: %v", err)
		}
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := e.EvaluateOnce(ctx); err != nil {
					log.Printf("alerting: evaluation failed: %v", err)
				}
			}
		}
	}()
}

func (e *Engine) Config() Config {
	return LoadConfig(e.store)
}

func (e *Engine) EvaluateOnce(ctx context.Context) error {
	cfg := LoadConfig(e.store)
	if !cfg.AlertingEnabled {
		return nil
	}
	if cfg.HostDownEnabled {
		if err := e.evaluateHostDown(ctx, cfg); err != nil {
			return err
		}
	}
	if cfg.BackupEnabled {
		if err := e.evaluateBackups(ctx, cfg); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) SendTest(ctx context.Context) error {
	cfg := LoadConfig(e.store)
	return e.sendTelegram(ctx, cfg, "FleetCom test alert\n\nIf you received this, the Telegram destination is wired correctly.")
}

func (e *Engine) evaluateHostDown(ctx context.Context, cfg Config) error {
	hosts, err := e.store.HostsBareList()
	if err != nil {
		return err
	}
	now := e.now().UTC()
	grace := time.Duration(cfg.HostDownGraceSeconds) * time.Second
	for _, h := range hosts {
		if strings.TrimSpace(h.LastSeen) == "" {
			continue
		}
		last, err := time.Parse(time.RFC3339, h.LastSeen)
		if err != nil {
			continue
		}
		age := now.Sub(last)
		summary := fmt.Sprintf("last heartbeat %s ago (threshold %s)", roundDuration(age), roundDuration(grace))
		if age > grace {
			title := "Host heartbeat missing: " + h.Hostname
			msg := fmt.Sprintf("FleetCom alert: host heartbeat missing\n\nHost: %s\nLast heartbeat: %s\nAge: %s\nThreshold: %s",
				h.Hostname, last.Format(time.RFC3339), roundDuration(age), roundDuration(grace))
			if err := e.ensureFiring(ctx, cfg, hostDownRuleKey, "host", h.Hostname, title, summary, msg, now); err != nil {
				log.Printf("alerting: host_down %s notification failed: %v", h.Hostname, err)
			}
			continue
		}
		msg := fmt.Sprintf("FleetCom recovery: host heartbeat restored\n\nHost: %s\nLast heartbeat: %s\nAge: %s\nThreshold: %s",
			h.Hostname, last.Format(time.RFC3339), roundDuration(age), roundDuration(grace))
		if err := e.ensureOK(ctx, cfg, hostDownRuleKey, "host", h.Hostname, "Host heartbeat restored: "+h.Hostname, summary, msg, now); err != nil {
			log.Printf("alerting: host_down %s recovery notification failed: %v", h.Hostname, err)
		}
	}
	return nil
}

func (e *Engine) evaluateBackups(ctx context.Context, cfg Config) error {
	backups, err := e.store.AllBackups()
	if err != nil {
		return err
	}
	now := e.now().UTC()
	threshold := time.Duration(cfg.BackupStaleSeconds) * time.Second
	byHost := map[string]int{}
	for _, b := range backups {
		if b.Hostname != "" {
			byHost[b.Hostname]++
		}
		key := b.Hostname + "/" + b.Name
		unhealthy, reason := backupUnhealthyReason(b, threshold, now)
		if unhealthy {
			title := "Backup unhealthy: " + key
			summary := reason
			msg := fmt.Sprintf("FleetCom alert: backup unhealthy\n\nHost: %s\nBackup: %s\nReason: %s\nLast success: %s\nLast check: %s",
				b.Hostname, b.Name, reason, fallbackDash(b.LastSuccessAt), fallbackDash(b.LastCheckedAt))
			if err := e.ensureFiring(ctx, cfg, backupUnhealthyRuleKey, "backup", key, title, summary, msg, now); err != nil {
				log.Printf("alerting: backup_unhealthy %s notification failed: %v", key, err)
			}
			continue
		}
		msg := fmt.Sprintf("FleetCom recovery: backup healthy\n\nHost: %s\nBackup: %s\nLast success: %s",
			b.Hostname, b.Name, fallbackDash(b.LastSuccessAt))
		if err := e.ensureOK(ctx, cfg, backupUnhealthyRuleKey, "backup", key, "Backup healthy: "+key, "latest snapshot fresh", msg, now); err != nil {
			log.Printf("alerting: backup_unhealthy %s recovery notification failed: %v", key, err)
		}
	}

	knownHosts := map[string]bool{}
	hosts, err := e.store.HostsBareList()
	if err != nil {
		return err
	}
	for _, h := range hosts {
		knownHosts[h.Hostname] = true
	}
	for _, host := range parseCSV(cfg.BackupExpectedHosts) {
		if !knownHosts[host] {
			continue
		}
		if byHost[host] == 0 {
			msg := fmt.Sprintf("FleetCom alert: backup missing\n\nHost: %s\nExpected: restic backup metadata in bosun heartbeat", host)
			if err := e.ensureFiring(ctx, cfg, backupMissingRuleKey, "host", host, "Backup missing: "+host, "no backup metadata reported", msg, now); err != nil {
				log.Printf("alerting: backup_missing %s notification failed: %v", host, err)
			}
			continue
		}
		msg := fmt.Sprintf("FleetCom recovery: backup metadata restored\n\nHost: %s", host)
		if err := e.ensureOK(ctx, cfg, backupMissingRuleKey, "host", host, "Backup metadata restored: "+host, "backup metadata present", msg, now); err != nil {
			log.Printf("alerting: backup_missing %s recovery notification failed: %v", host, err)
		}
	}
	return nil
}

func (e *Engine) ensureFiring(ctx context.Context, cfg Config, ruleKey, entityType, entityKey, title, summary, msg string, now time.Time) error {
	st, err := e.store.AlertState(ruleKey, entityKey)
	if err != nil {
		return err
	}
	activeSince := now.Format(time.RFC3339)
	lastSentAt := ""
	if st != nil {
		if st.Status == "firing" && st.LastSentAt != "" {
			return e.store.UpsertAlertState(db.AlertStateUpdate{
				RuleKey: ruleKey, EntityType: entityType, EntityKey: entityKey,
				Status: "firing", Title: title, Summary: summary,
				ActiveSince: st.ActiveSince, ResolvedAt: "", LastSentAt: st.LastSentAt,
				LastError: "", UpdatedAt: activeSince,
			})
		}
		if st.ActiveSince != "" {
			activeSince = st.ActiveSince
		}
		lastSentAt = st.LastSentAt
	}
	if err := e.sendTelegram(ctx, cfg, msg); err != nil {
		_ = e.store.UpsertAlertState(db.AlertStateUpdate{
			RuleKey: ruleKey, EntityType: entityType, EntityKey: entityKey,
			Status: "pending", Title: title, Summary: summary,
			ActiveSince: activeSince, ResolvedAt: "", LastSentAt: lastSentAt,
			LastError: sanitizeAlertError(err), UpdatedAt: now.Format(time.RFC3339),
		})
		return err
	}
	return e.store.UpsertAlertState(db.AlertStateUpdate{
		RuleKey: ruleKey, EntityType: entityType, EntityKey: entityKey,
		Status: "firing", Title: title, Summary: summary,
		ActiveSince: activeSince, ResolvedAt: "", LastSentAt: now.Format(time.RFC3339),
		LastError: "", UpdatedAt: now.Format(time.RFC3339),
	})
}

func (e *Engine) ensureOK(ctx context.Context, cfg Config, ruleKey, entityType, entityKey, title, summary, msg string, now time.Time) error {
	st, err := e.store.AlertState(ruleKey, entityKey)
	if err != nil {
		return err
	}
	if st == nil || st.Status == "ok" {
		return nil
	}
	// If the original firing notification never went out, recovering to OK
	// should clear the pending state without sending a confusing recovery.
	if st.Status == "pending" && st.LastSentAt == "" {
		return e.store.UpsertAlertState(db.AlertStateUpdate{
			RuleKey: ruleKey, EntityType: entityType, EntityKey: entityKey,
			Status: "ok", Title: title, Summary: summary,
			ActiveSince: st.ActiveSince, ResolvedAt: now.Format(time.RFC3339),
			LastSentAt: st.LastSentAt, LastError: "", UpdatedAt: now.Format(time.RFC3339),
		})
	}
	if err := e.sendTelegram(ctx, cfg, msg); err != nil {
		_ = e.store.UpsertAlertState(db.AlertStateUpdate{
			RuleKey: ruleKey, EntityType: entityType, EntityKey: entityKey,
			Status: st.Status, Title: st.Title, Summary: st.Summary,
			ActiveSince: st.ActiveSince, ResolvedAt: st.ResolvedAt,
			LastSentAt: st.LastSentAt, LastError: sanitizeAlertError(err),
			UpdatedAt: now.Format(time.RFC3339),
		})
		return err
	}
	return e.store.UpsertAlertState(db.AlertStateUpdate{
		RuleKey: ruleKey, EntityType: entityType, EntityKey: entityKey,
		Status: "ok", Title: title, Summary: summary,
		ActiveSince: st.ActiveSince, ResolvedAt: now.Format(time.RFC3339),
		LastSentAt: now.Format(time.RFC3339), LastError: "",
		UpdatedAt: now.Format(time.RFC3339),
	})
}

func (e *Engine) sendTelegram(ctx context.Context, cfg Config, msg string) error {
	if !cfg.TelegramEnabled {
		return fmt.Errorf("telegram notifier is disabled")
	}
	if e.sender == nil {
		return fmt.Errorf("telegram sender is not configured")
	}
	return e.sender.SendTelegram(ctx, cfg.TelegramChatID, msg)
}

func roundDuration(d time.Duration) string {
	if d < 0 {
		d = -d
	}
	if d >= time.Hour {
		return d.Round(time.Minute).String()
	}
	return d.Round(time.Second).String()
}

func sanitizeAlertError(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if len(msg) > 512 {
		msg = msg[:512]
	}
	return msg
}

func backupUnhealthyReason(b db.Backup, threshold time.Duration, now time.Time) (bool, string) {
	switch strings.ToLower(strings.TrimSpace(b.Status)) {
	case "error", "failed", "missing":
		if strings.TrimSpace(b.Error) != "" {
			return true, truncate(b.Error, 180)
		}
		return true, "backup probe status is " + b.Status
	}
	if strings.TrimSpace(b.LastSuccessAt) == "" {
		return true, "no successful snapshot reported"
	}
	last, err := time.Parse(time.RFC3339, b.LastSuccessAt)
	if err != nil {
		return true, "invalid last_success_at timestamp"
	}
	age := now.Sub(last)
	if age > threshold {
		return true, fmt.Sprintf("last successful snapshot %s ago (threshold %s)", roundDuration(age), roundDuration(threshold))
	}
	return false, ""
}

func parseCSV(v string) []string {
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	seen := map[string]bool{}
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

func fallbackDash(v string) string {
	if strings.TrimSpace(v) == "" {
		return "-"
	}
	return v
}

func truncate(v string, n int) string {
	if len(v) <= n {
		return v
	}
	return v[:n]
}

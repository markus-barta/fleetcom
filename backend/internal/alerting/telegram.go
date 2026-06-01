package alerting

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

type TelegramSender interface {
	SendTelegram(ctx context.Context, chatID, text string) error
}

type TelegramClient struct {
	Token      string
	APIBase    string
	HTTPClient *http.Client
}

func NewTelegramClientFromEnv() *TelegramClient {
	return &TelegramClient{
		Token:      strings.TrimSpace(os.Getenv(EnvTelegramBotToken)),
		APIBase:    "https://api.telegram.org",
		HTTPClient: &http.Client{Timeout: 10 * time.Second},
	}
}

func (c *TelegramClient) SendTelegram(ctx context.Context, chatID, text string) error {
	if strings.TrimSpace(c.Token) == "" {
		return errors.New("telegram bot token is not configured")
	}
	if strings.TrimSpace(chatID) == "" {
		return errors.New("telegram chat id is not configured")
	}
	if strings.TrimSpace(text) == "" {
		return errors.New("telegram message is empty")
	}
	base := strings.TrimRight(c.APIBase, "/")
	if base == "" {
		base = "https://api.telegram.org"
	}
	client := c.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}

	payload := map[string]any{
		"chat_id":                  chatID,
		"text":                     text,
		"disable_web_page_preview": true,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/bot"+c.Token+"/sendMessage", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("telegram send failed: %s", redactTelegramToken(err.Error(), c.Token))
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("telegram send failed: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	var out struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(respBody, &out); err == nil && !out.OK {
		if out.Description == "" {
			out.Description = "telegram returned ok=false"
		}
		return errors.New(out.Description)
	}
	return nil
}

func redactTelegramToken(msg, token string) string {
	token = strings.TrimSpace(token)
	if token == "" {
		return msg
	}
	return strings.ReplaceAll(msg, token, "[redacted]")
}

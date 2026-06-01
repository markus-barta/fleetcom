package alerting

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTelegramClientSendTelegram(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	c := &TelegramClient{
		Token:      "test-token",
		APIBase:    srv.URL,
		HTTPClient: srv.Client(),
	}
	if err := c.SendTelegram(context.Background(), "-100123", "hello"); err != nil {
		t.Fatalf("SendTelegram: %v", err)
	}
	if gotPath != "/bottest-token/sendMessage" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotBody["chat_id"] != "-100123" || gotBody["text"] != "hello" {
		t.Fatalf("body = %#v", gotBody)
	}
}

func TestTelegramClientDoesNotLeakTokenInMissingConfigError(t *testing.T) {
	c := &TelegramClient{Token: ""}
	err := c.SendTelegram(context.Background(), "-100123", "hello")
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "test-token") {
		t.Fatalf("error leaked token: %v", err)
	}
}

func TestTelegramClientRedactsTokenInTransportError(t *testing.T) {
	c := &TelegramClient{
		Token:   "test-token",
		APIBase: "https://api.telegram.example",
		HTTPClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New(`Post "https://api.telegram.example/bottest-token/sendMessage": boom`)
		})},
	}
	err := c.SendTelegram(context.Background(), "-100123", "hello")
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "test-token") {
		t.Fatalf("error leaked token: %v", err)
	}
	if !strings.Contains(err.Error(), "[redacted]") {
		t.Fatalf("error did not include redaction marker: %v", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

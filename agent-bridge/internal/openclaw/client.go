package openclaw

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

// ClientOptions configures a single per-gateway WebSocket client.
type ClientOptions struct {
	URL           string    // wss://host:18789
	Identity      *Identity // FleetCom's keypair for this gateway
	OperatorToken string    // returned by an earlier pairing; empty on first pair
	Role          string    // usually "operator"
	Scopes        []string  // e.g. ["operator.read", "operator.pairing"]
	ClientID      string    // "gateway-client" unless testing another surface
	ClientMode    string    // "backend" for server-to-server
	ClientVersion string    // shown in gateway logs + UI
	Platform      string    // "linux" (lowercased server-side)
	DeviceFamily  string    // free-form tag; "fleetcom-server" etc.

	OnEvent      func(event string, payload json.RawMessage)
	OnConnected  func(hello json.RawMessage)
	OnDisconnect func(err error)
}

// Client is one WebSocket connection to one OpenClaw gateway. It
// handles the challenge/connect handshake, tracks in-flight RPCs,
// dispatches events, and auto-reconnects with a fixed 5s backoff.
type Client struct {
	opts ClientOptions

	mu      sync.Mutex
	ws      *websocket.Conn
	pending map[int64]chan rpcResult
	nonce   string
	nextID  int64
}

type rpcResult struct {
	payload json.RawMessage
	err     error
}

// Frame is the union envelope on the wire. OpenClaw sends `type` as one
// of "req", "res", "event" and only populates the fields relevant to
// that frame kind.
type frame struct {
	Type    string          `json:"type"`
	ID      int64           `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
	OK      *bool           `json:"ok,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
	Event   string          `json:"event,omitempty"`
	Seq     int             `json:"seq,omitempty"`
}

type rpcError struct {
	Code    string      `json:"code"`
	Message string      `json:"message"`
	Details interface{} `json:"details,omitempty"`
}

// NewClient returns a not-yet-running client. Call Run to start.
func NewClient(opts ClientOptions) *Client {
	return &Client{opts: opts}
}

// Run blocks until ctx is cancelled, reconnecting on transient errors.
// Returned error is always ctx.Err() in normal shutdown.
func (c *Client) Run(ctx context.Context) error {
	for {
		err := c.runOnce(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			log.Printf("openclaw %s: %v — reconnect in 5s", c.opts.URL, err)
			if c.opts.OnDisconnect != nil {
				c.opts.OnDisconnect(err)
			}
		}
		select {
		case <-time.After(5 * time.Second):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (c *Client) runOnce(ctx context.Context) error {
	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(dialCtx, c.opts.URL, &websocket.DialOptions{
		Subprotocols: []string{"openclaw-gateway.v3"},
	})
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.CloseNow()

	c.mu.Lock()
	c.ws = conn
	c.pending = make(map[int64]chan rpcResult)
	c.nonce = ""
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		c.ws = nil
		for _, ch := range c.pending {
			ch <- rpcResult{err: errors.New("connection closed")}
		}
		c.pending = nil
		c.mu.Unlock()
	}()

	// Arm a challenge timeout — if we never see connect.challenge we
	// want to kick this connection rather than hang forever.
	challengeDeadline := time.NewTimer(10 * time.Second)
	defer challengeDeadline.Stop()
	connectDone := make(chan struct{})

	go func() {
		select {
		case <-connectDone:
		case <-challengeDeadline.C:
			// If we haven't sent connect yet, the gateway never sent
			// its challenge — close to trigger reconnect.
			c.mu.Lock()
			stale := c.nonce == ""
			c.mu.Unlock()
			if stale {
				_ = conn.Close(websocket.StatusPolicyViolation, "connect.challenge timeout")
			}
		case <-ctx.Done():
		}
	}()

	// Read loop runs inline; handshake kicks off when we see the
	// challenge event.
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			close(connectDone)
			return fmt.Errorf("read: %w", err)
		}
		var env frame
		if err := json.Unmarshal(data, &env); err != nil {
			continue
		}
		switch env.Type {
		case "event":
			if env.Event == "connect.challenge" {
				var p struct {
					Nonce string `json:"nonce"`
				}
				_ = json.Unmarshal(env.Payload, &p)
				if p.Nonce == "" {
					return errors.New("empty nonce in connect.challenge")
				}
				c.mu.Lock()
				c.nonce = p.Nonce
				c.mu.Unlock()
				close(connectDone)
				if err := c.sendConnect(ctx); err != nil {
					return fmt.Errorf("connect: %w", err)
				}
				continue
			}
			if c.opts.OnEvent != nil {
				c.opts.OnEvent(env.Event, env.Payload)
			}
		case "res":
			c.mu.Lock()
			ch, ok := c.pending[env.ID]
			if ok {
				delete(c.pending, env.ID)
			}
			c.mu.Unlock()
			if !ok {
				continue
			}
			if env.OK != nil && *env.OK {
				ch <- rpcResult{payload: env.Payload}
			} else {
				msg := "unknown error"
				if env.Error != nil && env.Error.Message != "" {
					msg = fmt.Sprintf("%s: %s", env.Error.Code, env.Error.Message)
				}
				ch <- rpcResult{err: errors.New(msg)}
			}
		}
	}
}

func (c *Client) sendConnect(ctx context.Context) error {
	c.mu.Lock()
	nonce := c.nonce
	c.mu.Unlock()

	signedAtMs := time.Now().UnixMilli()
	payload := BuildPayloadV3(
		c.opts.Identity.DeviceID,
		c.opts.ClientID,
		c.opts.ClientMode,
		c.opts.Role,
		c.opts.Scopes,
		signedAtMs,
		c.opts.OperatorToken,
		nonce,
		c.opts.Platform,
		c.opts.DeviceFamily,
	)
	sig := c.opts.Identity.Sign(payload)

	params := map[string]interface{}{
		"minProtocol": 3,
		"maxProtocol": 3,
		"client": map[string]interface{}{
			"id":           c.opts.ClientID,
			"version":      c.opts.ClientVersion,
			"platform":     c.opts.Platform,
			"deviceFamily": c.opts.DeviceFamily,
			"mode":         c.opts.ClientMode,
		},
		"caps":   []string{},
		"role":   c.opts.Role,
		"scopes": c.opts.Scopes,
		"device": map[string]interface{}{
			"id":        c.opts.Identity.DeviceID,
			"publicKey": c.opts.Identity.PubKeyRawB64U,
			"signature": sig,
			"signedAt":  signedAtMs,
			"nonce":     nonce,
		},
	}
	if c.opts.OperatorToken != "" {
		params["auth"] = map[string]interface{}{"token": c.opts.OperatorToken}
	}

	hello, err := c.Call(ctx, "connect", params, 15*time.Second)
	if err != nil {
		return err
	}
	log.Printf("openclaw %s: connected (deviceId=%s)", c.opts.URL, c.opts.Identity.DeviceID[:12])
	if c.opts.OnConnected != nil {
		c.opts.OnConnected(hello)
	}
	return nil
}

// Call sends an RPC and waits for the matching response. Safe for
// concurrent callers.
func (c *Client) Call(ctx context.Context, method string, params interface{}, timeout time.Duration) (json.RawMessage, error) {
	id := atomic.AddInt64(&c.nextID, 1)
	paramsJSON, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("marshal params: %w", err)
	}
	data, err := json.Marshal(frame{
		Type:   "req",
		ID:     id,
		Method: method,
		Params: paramsJSON,
	})
	if err != nil {
		return nil, err
	}

	ch := make(chan rpcResult, 1)
	c.mu.Lock()
	if c.ws == nil || c.pending == nil {
		c.mu.Unlock()
		return nil, errors.New("not connected")
	}
	c.pending[id] = ch
	ws := c.ws
	c.mu.Unlock()

	writeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := ws.Write(writeCtx, websocket.MessageText, data); err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, fmt.Errorf("write: %w", err)
	}

	select {
	case r := <-ch:
		return r.payload, r.err
	case <-time.After(timeout):
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, fmt.Errorf("rpc timeout: %s", method)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Connected reports whether the client currently has a live WS.
func (c *Client) Connected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ws != nil && c.pending != nil
}

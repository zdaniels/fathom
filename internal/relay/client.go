// Package relay is Fathom's outbound client for the fantazmai/relay
// Cloudflare Worker. When ~/.fantazm/relay.token exists, the gateway
// spawns a Client that holds an outbound WebSocket to relay.fantazm.ai,
// forwards inbound request frames to the local gateway, and ships
// responses back. Mobile clients pointed at the relay URL see the
// agent transparently — no LAN, no Tailscale, no per-user CF Tunnel.
//
// Wire protocol mirrors github.com/fantazmai/relay/src/types.ts.
package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/zdaniels/fathom/internal/brandenv"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"nhooyr.io/websocket"
)

// Config is what we need to dial the relay. Loaded from
// ~/.fantazm/relay.token (written by `fathom relay enable`).
type Config struct {
	RelayID  string `json:"relay_id"`
	Secret   string `json:"secret"`
	AgentURL string `json:"agent_url"` // wss://relay.fantazm/agent/{rid}
}

// DefaultConfigPath returns ~/.fantazm/relay.token (override via
// $FANTAZM_RELAY_TOKEN_FILE).
func DefaultConfigPath() string {
	if env := brandenv.Get("FATHOM_RELAY_TOKEN_FILE"); env != "" {
		return env
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".fantazm", "relay.token")
}

// LoadConfig reads + parses the relay token file. Returns os.ErrNotExist
// when the file is absent; callers should treat that as "relay
// disabled" rather than fatal.
func LoadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return Config{}, fmt.Errorf("relay token: parse: %w", err)
	}
	if c.RelayID == "" || c.Secret == "" || c.AgentURL == "" {
		return Config{}, errors.New("relay token: missing required fields")
	}
	return c, nil
}

// SaveConfig writes a Config back to disk at 0600. Used by `fathom
// relay enable` after enrolling with the relay.
func SaveConfig(path string, c Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(c, "", "  ")
	return os.WriteFile(path, data, 0o600)
}

// Client owns the outbound WebSocket to the relay and dispatches
// inbound request/stream frames to the local gateway handler.
type Client struct {
	cfg     Config
	handler http.Handler // in-process gateway handler — no TCP loopback

	mu      sync.Mutex
	conn    *websocket.Conn
	streams map[string]context.CancelFunc // stream_id → cancel for the local SSE consumer
	stopCh  chan struct{}
	stopped bool
}

// New constructs a Client. `gateway` is the Fathom gateway's HTTP
// handler — request frames are dispatched into it via ServeHTTP so
// there's no network hop and existing auth/policy code applies
// unchanged.
func New(cfg Config, gateway http.Handler) *Client {
	return &Client{
		cfg:     cfg,
		handler: gateway,
		streams: map[string]context.CancelFunc{},
		stopCh:  make(chan struct{}),
	}
}

// Run dials the relay and blocks until ctx is cancelled or Stop is
// called. Reconnects with exponential backoff (capped at 30s) on
// disconnects. Safe to spawn as `go client.Run(ctx)`.
func (c *Client) Run(ctx context.Context) {
	backoff := 1 * time.Second
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.stopCh:
			return
		default:
		}

		if err := c.runOnce(ctx); err != nil {
			slog.Warn("relay: connection lost", "err", err, "retry_in", backoff)
		}
		select {
		case <-ctx.Done():
			return
		case <-c.stopCh:
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

// Stop tears down the connection + any in-flight streams. Idempotent.
func (c *Client) Stop() {
	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		return
	}
	c.stopped = true
	close(c.stopCh)
	conn := c.conn
	for _, cancel := range c.streams {
		cancel()
	}
	c.streams = nil
	c.mu.Unlock()
	if conn != nil {
		_ = conn.Close(websocket.StatusNormalClosure, "shutdown")
	}
}

func (c *Client) runOnce(ctx context.Context) error {
	dialCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(dialCtx, c.cfg.AgentURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + c.cfg.Secret}},
	})
	if err != nil {
		return fmt.Errorf("dial %s: %w", c.cfg.AgentURL, err)
	}
	c.mu.Lock()
	c.conn = conn
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.conn = nil
		c.mu.Unlock()
		_ = conn.Close(websocket.StatusInternalError, "loop exit")
	}()

	conn.SetReadLimit(8 * 1024 * 1024) // 8MB cap on inbound frames

	// First message: hello. Identifies the relay_id + presents the secret.
	hello := map[string]any{
		"type":     "hello",
		"relay_id": c.cfg.RelayID,
		"secret":   c.cfg.Secret,
	}
	if err := writeJSON(ctx, conn, hello); err != nil {
		return fmt.Errorf("send hello: %w", err)
	}
	slog.Info("relay: connected", "relay_id", c.cfg.RelayID)

	// Heartbeat — every 25s. Keeps the WS alive through aggressive
	// middleboxes + lets us notice a stalled connection faster than
	// the OS TCP keepalive.
	hbCtx, hbCancel := context.WithCancel(ctx)
	defer hbCancel()
	go c.heartbeat(hbCtx, conn)

	// Read loop.
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return fmt.Errorf("read: %w", err)
		}
		c.dispatch(ctx, conn, data)
	}
}

func (c *Client) heartbeat(ctx context.Context, conn *websocket.Conn) {
	t := time.NewTicker(25 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_ = writeJSON(ctx, conn, map[string]any{
				"type": "ping", "at": time.Now().UnixMilli(),
			})
		}
	}
}

// dispatch parses an inbound frame and routes it to the right handler.
// Each handler is responsible for either writing a response frame
// back to the WS or starting/closing a stream.
func (c *Client) dispatch(ctx context.Context, conn *websocket.Conn, data []byte) {
	var hdr struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &hdr); err != nil {
		slog.Warn("relay: malformed frame", "err", err)
		return
	}
	switch hdr.Type {
	case "request":
		var req requestFrame
		if err := json.Unmarshal(data, &req); err != nil {
			slog.Warn("relay: bad request frame", "err", err)
			return
		}
		// Each request runs in its own goroutine — the relay can
		// pipeline multiple in-flight requests across the same WS.
		go c.handleRequest(ctx, conn, req)
	case "stream_open":
		var so streamOpenFrame
		if err := json.Unmarshal(data, &so); err != nil {
			slog.Warn("relay: bad stream_open", "err", err)
			return
		}
		go c.handleStreamOpen(ctx, conn, so)
	case "stream_close":
		var sc struct {
			StreamID string `json:"stream_id"`
		}
		if err := json.Unmarshal(data, &sc); err != nil {
			return
		}
		c.closeStream(sc.StreamID)
	case "ping":
		_ = writeJSON(ctx, conn, map[string]any{
			"type": "pong", "at": time.Now().UnixMilli(),
		})
	case "pong":
		// noop
	default:
		slog.Debug("relay: unknown frame type", "type", hdr.Type)
	}
}

// handleRequest dispatches a one-shot HTTP request into the gateway
// handler and ships the response back as a response frame.
func (c *Client) handleRequest(ctx context.Context, conn *websocket.Conn, req requestFrame) {
	r := buildLocalRequest(req)
	rec := httptest.NewRecorder()
	c.handler.ServeHTTP(rec, r)

	resp := responseFrame{
		Type:    "response",
		ID:      req.ID,
		Status:  rec.Code,
		Headers: flattenHeaders(rec.Result().Header),
		Body:    rec.Body.String(),
	}
	if err := writeJSON(ctx, conn, resp); err != nil {
		slog.Warn("relay: send response failed", "id", req.ID, "err", err)
	}
}

// handleStreamOpen subscribes to a local SSE endpoint and forwards
// each event as a stream_event frame. Uses a custom ResponseWriter
// (sseForwarder) that pushes flushed chunks across the WS as they
// arrive instead of buffering until the handler returns.
func (c *Client) handleStreamOpen(ctx context.Context, conn *websocket.Conn, so streamOpenFrame) {
	streamCtx, cancel := context.WithCancel(ctx)
	c.mu.Lock()
	c.streams[so.StreamID] = cancel
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.streams, so.StreamID)
		c.mu.Unlock()
		_ = writeJSON(ctx, conn, map[string]any{
			"type":      "stream_close",
			"stream_id": so.StreamID,
			"reason":    "agent_eof",
		})
	}()

	u, err := url.Parse(so.Path)
	if err != nil {
		slog.Warn("relay: bad stream path", "path", so.Path)
		return
	}
	req := &http.Request{
		Method: http.MethodGet,
		URL:    u,
		Header: make(http.Header),
	}
	for k, v := range so.Headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.RequestURI = so.Path
	r := req.WithContext(streamCtx)

	fw := &sseForwarder{
		ctx:      streamCtx,
		conn:     conn,
		streamID: so.StreamID,
		headers:  make(http.Header),
	}
	c.handler.ServeHTTP(fw, r)
}

// closeStream cancels the local SSE subscription for stream_id.
func (c *Client) closeStream(streamID string) {
	c.mu.Lock()
	cancel, ok := c.streams[streamID]
	delete(c.streams, streamID)
	c.mu.Unlock()
	if ok {
		cancel()
	}
}

// === Wire types — match relay/src/types.ts ============================

type requestFrame struct {
	Type    string            `json:"type"`
	ID      string            `json:"id"`
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body,omitempty"`
}

type responseFrame struct {
	Type    string            `json:"type"`
	ID      string            `json:"id"`
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body,omitempty"`
}

type streamOpenFrame struct {
	Type     string            `json:"type"`
	StreamID string            `json:"stream_id"`
	Path     string            `json:"path"`
	Headers  map[string]string `json:"headers"`
}

// === Helpers ==========================================================

func writeJSON(ctx context.Context, conn *websocket.Conn, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return conn.Write(ctx, websocket.MessageText, data)
}

func buildLocalRequest(rf requestFrame) *http.Request {
	body := io.NopCloser(strings.NewReader(rf.Body))
	r, _ := http.NewRequest(rf.Method, rf.Path, body)
	for k, v := range rf.Headers {
		r.Header.Set(k, v)
	}
	return r
}

func flattenHeaders(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	for k, v := range h {
		if len(v) > 0 {
			out[k] = v[0]
		}
	}
	return out
}

// === sseForwarder — http.ResponseWriter that ships chunks to the WS ===

// sseForwarder satisfies http.ResponseWriter + http.Flusher. Each
// Write call from the gateway's SSE handler is parsed for event/data
// lines and turned into stream_event frames sent back across the WS.
type sseForwarder struct {
	ctx      context.Context
	conn     *websocket.Conn
	streamID string
	headers  http.Header
	wroteHdr bool
	buf      bytes.Buffer
	mu       sync.Mutex
}

func (f *sseForwarder) Header() http.Header { return f.headers }

func (f *sseForwarder) WriteHeader(code int) {
	f.wroteHdr = true
	// Status code is implicit in stream_event semantics; ignored here.
}

func (f *sseForwarder) Write(p []byte) (int, error) {
	if !f.wroteHdr {
		f.WriteHeader(http.StatusOK)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.buf.Write(p)
	f.tryDispatch()
	return len(p), nil
}

// tryDispatch peels off complete SSE events ("event:...\ndata:...\n\n")
// from the buffer and ships each as a stream_event frame. Caller holds
// the lock.
func (f *sseForwarder) tryDispatch() {
	for {
		data := f.buf.Bytes()
		end := bytes.Index(data, []byte("\n\n"))
		if end < 0 {
			return
		}
		eventBlock := string(data[:end])
		f.buf.Next(end + 2)

		var event, payload string
		for _, line := range strings.Split(eventBlock, "\n") {
			switch {
			case strings.HasPrefix(line, ":"):
				// Comment / heartbeat. Skip.
			case strings.HasPrefix(line, "event:"):
				event = strings.TrimSpace(line[6:])
			case strings.HasPrefix(line, "data:"):
				if payload != "" {
					payload += "\n"
				}
				payload += strings.TrimSpace(line[5:])
			}
		}
		if event == "" && payload == "" {
			continue
		}
		_ = writeJSON(f.ctx, f.conn, map[string]any{
			"type":      "stream_event",
			"stream_id": f.streamID,
			"event":     event,
			"data":      payload,
		})
	}
}

// Flush satisfies http.Flusher — the gateway's SSE handler calls
// this after every event. We've already dispatched on \n\n boundaries
// in Write; Flush is a no-op here but must exist for type assertion.
func (f *sseForwarder) Flush() {}

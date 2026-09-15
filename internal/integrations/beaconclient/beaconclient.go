// Package beaconclient is Fathom's tiny OTLP/HTTP/JSON emitter for
// shipping traces to a Beacon server (or any OTLP-compatible receiver).
//
// We deliberately don't pull in the upstream go.opentelemetry.io/otel
// SDK. That SDK is large, opinionated about context propagation, and
// would tangle with Fathom's existing in-process span-like audit log.
// What Fathom needs is just: "for every agent loop iteration + tool
// call, mint a span, set some attrs, end it, ship it." 200 LOC of
// hand-rolled OTLP/JSON is the right size for that.
//
// Wire format: OTLP/HTTP/JSON, the spec-conformant JSON shape of OTLP.
// Any compliant receiver — Beacon, the OTEL Collector, Honeycomb,
// Datadog (via the collector) — accepts it without modification.
package beaconclient

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Config bundles what we need to talk to a Beacon-compatible OTLP/HTTP
// receiver. ServiceName populates the service.name Resource attribute;
// ScopeName populates the instrumentation scope.
type Config struct {
	URL         string // e.g. http://127.0.0.1:4318
	Token       string // Bearer token (optional; many OTLP receivers don't auth ingest)
	ServiceName string // e.g. "fathom"
	ScopeName   string // e.g. "github.com/zdaniels/fathom"
}

// Validate sanity-checks the config. Empty URL → no-op client (used when
// telemetry is unconfigured; callers shouldn't have to nil-check).
func (c *Config) Validate() error {
	if c.URL == "" {
		return nil // disabled is a valid state
	}
	if !strings.HasPrefix(c.URL, "http://") && !strings.HasPrefix(c.URL, "https://") {
		return fmt.Errorf("beacon: URL %q must include scheme", c.URL)
	}
	if c.ServiceName == "" {
		c.ServiceName = "fathom"
	}
	if c.ScopeName == "" {
		c.ScopeName = "github.com/zdaniels/fathom"
	}
	return nil
}

// LoadTokenFromFile is the same shape as the Charon/Chasm helpers —
// tilde-expand, read, trim. Empty path returns ("", nil).
func LoadTokenFromFile(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	expanded := path
	if strings.HasPrefix(expanded, "~/") {
		if home, _ := os.UserHomeDir(); home != "" {
			expanded = home + expanded[1:]
		}
	}
	raw, err := os.ReadFile(expanded)
	if err != nil {
		return "", fmt.Errorf("read beacon token file %s: %w", expanded, err)
	}
	return strings.TrimSpace(string(raw)), nil
}

// Client batches spans + flushes to Beacon periodically. Construct once,
// reuse for the lifetime of the process. The zero-value Client (URL=="")
// is a working no-op so callers don't need to nil-check on every span.
type Client struct {
	cfg   Config
	http  *http.Client
	flush time.Duration

	mu     sync.Mutex
	buffer []span

	stopCh chan struct{}
	wg     sync.WaitGroup
}

// span is the in-progress representation. SpanHandle.End() finalises +
// queues.
type span struct {
	traceID    string
	spanID     string
	parentID   string
	name       string
	startUnix  int64
	endUnix    int64
	attrs      map[string]string
	statusCode int
	statusMsg  string
}

// SpanHandle is the public handle returned from StartSpan / StartChild.
type SpanHandle struct {
	c *Client
	s *span
}

// New constructs a Client. When cfg.URL == "", returns a working no-op
// (calls do nothing; nil-safe). Otherwise starts a background goroutine
// that flushes every flushInterval.
func New(cfg Config, flushInterval time.Duration) *Client {
	if flushInterval == 0 {
		flushInterval = 5 * time.Second
	}
	c := &Client{
		cfg:    cfg,
		http:   &http.Client{Timeout: 10 * time.Second},
		flush:  flushInterval,
		stopCh: make(chan struct{}),
	}
	if cfg.URL == "" {
		return c // no-op mode
	}
	c.wg.Add(1)
	go c.flushLoop()
	return c
}

// StartSpan begins a new root span (no parent). Use for the entry point
// of a request (e.g. one user message → one root span).
func (c *Client) StartSpan(ctx context.Context, name string) *SpanHandle {
	if c == nil || c.cfg.URL == "" {
		return &SpanHandle{c: c, s: &span{name: name}}
	}
	_ = ctx
	return &SpanHandle{
		c: c,
		s: &span{
			traceID:   randHex(16),
			spanID:    randHex(8),
			name:      name,
			startUnix: time.Now().UnixNano(),
			attrs:     map[string]string{},
		},
	}
}

// StartChild begins a span whose parent is the provided handle. Trace
// ID is inherited so the tree stays connected.
func (c *Client) StartChild(parent *SpanHandle, name string) *SpanHandle {
	if c == nil || c.cfg.URL == "" || parent == nil {
		return &SpanHandle{c: c, s: &span{name: name}}
	}
	return &SpanHandle{
		c: c,
		s: &span{
			traceID:   parent.s.traceID,
			parentID:  parent.s.spanID,
			spanID:    randHex(8),
			name:      name,
			startUnix: time.Now().UnixNano(),
			attrs:     map[string]string{},
		},
	}
}

// SetAttr stamps a key/value on the span. No-op for noop spans.
func (h *SpanHandle) SetAttr(key string, val any) *SpanHandle {
	if h == nil || h.s == nil || h.s.attrs == nil {
		return h
	}
	h.s.attrs[key] = fmt.Sprintf("%v", val)
	return h
}

// SetError marks the span errored.
func (h *SpanHandle) SetError(msg string) *SpanHandle {
	if h == nil || h.s == nil {
		return h
	}
	h.s.statusCode = 2
	h.s.statusMsg = msg
	return h
}

// End finalises the span and queues for flush. No-op for noop spans.
func (h *SpanHandle) End() {
	if h == nil || h.c == nil || h.c.cfg.URL == "" || h.s == nil {
		return
	}
	h.s.endUnix = time.Now().UnixNano()
	h.c.mu.Lock()
	h.c.buffer = append(h.c.buffer, *h.s)
	h.c.mu.Unlock()
}

// TraceID returns the trace ID. Useful for surfacing in user-facing
// errors ("trace: abc123...") so the operator can `beacon trace abc123`.
func (h *SpanHandle) TraceID() string {
	if h == nil || h.s == nil {
		return ""
	}
	return h.s.traceID
}

// SpanID returns this span's ID.
func (h *SpanHandle) SpanID() string {
	if h == nil || h.s == nil {
		return ""
	}
	return h.s.spanID
}

// Stop shuts down the background flush loop after one final flush.
// Safe to call multiple times.
func (c *Client) Stop(ctx context.Context) error {
	if c == nil || c.cfg.URL == "" {
		return nil
	}
	select {
	case <-c.stopCh:
		// already stopped
	default:
		close(c.stopCh)
	}
	c.wg.Wait()
	return c.flushNow(ctx)
}

// flushLoop runs in the background, flushing periodically. Errors are
// logged at WARN so a misconfigured Beacon doesn't blow up the agent
// loop.
func (c *Client) flushLoop() {
	defer c.wg.Done()
	t := time.NewTicker(c.flush)
	defer t.Stop()
	for {
		select {
		case <-c.stopCh:
			return
		case <-t.C:
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			if err := c.flushNow(ctx); err != nil {
				slog.Warn("beacon flush failed", "err", err)
			}
			cancel()
		}
	}
}

// flushNow ships the current buffer to Beacon in one OTLP request.
func (c *Client) flushNow(ctx context.Context) error {
	c.mu.Lock()
	buf := c.buffer
	c.buffer = nil
	c.mu.Unlock()
	if len(buf) == 0 {
		return nil
	}

	spans := make([]map[string]any, len(buf))
	for i, s := range buf {
		attrs := make([]map[string]any, 0, len(s.attrs))
		for k, v := range s.attrs {
			attrs = append(attrs, map[string]any{
				"key":   k,
				"value": map[string]any{"stringValue": v},
			})
		}
		sp := map[string]any{
			"traceId":           s.traceID,
			"spanId":            s.spanID,
			"name":              s.name,
			"startTimeUnixNano": strconv.FormatInt(s.startUnix, 10),
			"endTimeUnixNano":   strconv.FormatInt(s.endUnix, 10),
			"attributes":        attrs,
		}
		if s.parentID != "" {
			sp["parentSpanId"] = s.parentID
		}
		if s.statusCode != 0 {
			sp["status"] = map[string]any{
				"code":    s.statusCode,
				"message": s.statusMsg,
			}
		}
		spans[i] = sp
	}

	body := map[string]any{
		"resourceSpans": []map[string]any{{
			"resource": map[string]any{
				"attributes": []map[string]any{{
					"key":   "service.name",
					"value": map[string]any{"stringValue": c.cfg.ServiceName},
				}},
			},
			"scopeSpans": []map[string]any{{
				"scope": map[string]any{"name": c.cfg.ScopeName},
				"spans": spans,
			}},
		}},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.URL+"/v1/traces", bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.cfg.Token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("beacon returned %s", resp.Status)
	}
	return nil
}

// randHex returns 2*n hex chars. OTLP requires hex-encoded IDs
// (16 chars for span, 32 for trace).
func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

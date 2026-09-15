// Package gateway is the HTTP + WebSocket front door. It owns the routes
// /api/v1/{health,message,session} plus the WebSocket endpoint, delegates
// to a MessageHandler the agent factory wires up, and exposes an extension
// hook for the AdminAPI when running in team/enterprise mode.
package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/zdaniels/fathom/internal/brandenv"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/zdaniels/fathom/internal/auth"
	"github.com/zdaniels/fathom/internal/gateway/web"
	"github.com/zdaniels/fathom/internal/threads"
	"github.com/zdaniels/fathom/pkg/types"
)

// MessageHandler is the function the gateway calls for each inbound message.
// The agent factory builds one that runs through the agent loop.
type MessageHandler func(ctx context.Context, msg types.ChannelMessage, session types.Session) (string, error)

// MessageHandlerN is the per-model variant — the gateway passes the
// caller-chosen model name through to the agent loop, bypassing the
// router's classifier. Used when a thread has a `/model <name>`
// override pinned to it. nil = fall back to the regular MessageHandler.
type MessageHandlerN func(ctx context.Context, msg types.ChannelMessage, session types.Session, model string) (string, error)

// MessageHandlerNU is HandlerN + Usage. Returns aggregated token
// counts + the resolved model name alongside the reply. When the
// gateway has one wired, it's preferred over MessageHandlerN so that
// every persisted agent message can carry usage metadata. nil = fall
// back to MessageHandlerN (no usage captured).
type MessageHandlerNU func(ctx context.Context, msg types.ChannelMessage, session types.Session, model string) (reply string, usage *UsageSnapshot, resolvedModel string, err error)

// UsageSnapshot is a copy of the LLM token usage from one agent turn.
// Mirrors agentfactory.Usage but defined here so the gateway package
// doesn't import agentfactory (circular risk down the line).
type UsageSnapshot struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

// ExtensionHandler lets external modules (e.g. the enterprise AdminAPI)
// register routes the gateway didn't itself recognise. Return true if the
// request was handled (response written), false to fall through to 404.
type ExtensionHandler func(w http.ResponseWriter, r *http.Request) bool

// Gateway is the HTTP/WebSocket server. Single instance per process.
type Gateway struct {
	cfg      types.Config
	Auth     *auth.Manager
	Sessions *auth.SessionStore
	Pairing  *auth.PairingStore
	Events   *EventBus

	// Threads + ThreadHub power cross-device session continuity. nil-safe:
	// when threads aren't wired (early-boot, tests), the related endpoints
	// degrade gracefully (503 / empty list).
	Threads   *threads.Store
	ThreadHub *ThreadHub

	// DeviceStore is the persistent backing for auth.Manager. nil when
	// running ephemeral (legacy New() / tests). Held here so Stop() can
	// close it after the HTTP server drains.
	DeviceStore *auth.DeviceStore

	server        *http.Server
	mux           *http.ServeMux // exposed via Handler() for in-process dispatch (relay client)
	mu            sync.RWMutex
	msgHandler    MessageHandler
	msgHandlerN   MessageHandlerN
	msgHandlerNU  MessageHandlerNU
	router        ModelRegistry // optional: lets /models slash command list available models
	audit         AuditRecorder // optional: pairing-flow audit hook
	extensionHook ExtensionHandler

	// Settings surface (/api/v1/settings). policyCtl lets the settings
	// handler read + hot-swap the live policy; configPath/policyPath are
	// where edits are persisted; settingsEditAuth decides whether a given
	// user may modify settings (personal: always; team/enterprise: admins
	// only — everyone else gets a read-only view). All optional: unset
	// degrades to a read-only settings view backed by the boot config.
	policyCtl        PolicyController
	configPath       string
	policyPath       string
	settingsEditAuth func(userID string) bool
	cleanupTicker    *time.Ticker
	cleanupStop      chan struct{}

	// pairClaimLimiter throttles unauthenticated /api/v1/pair/claim
	// requests per source IP. 6-digit codes + 60s TTL × 5/min keeps brute
	// force statistically negligible.
	pairClaimLimiter *ipRateLimiter
}

// New constructs the gateway. Auth and SessionStore are exposed so the
// agent factory + mountEnterprise can share them. The auth Manager is
// in-memory only — callers wanting restart-survival for paired devices
// should use NewWithDeviceStore.
func New(cfg types.Config) *Gateway {
	authMgr := auth.New()
	return newGatewayFromAuth(cfg, authMgr, nil)
}

// NewWithDeviceStore is like New but persists the token map to a
// DeviceStore so paired devices survive `fathom start` restarts. The
// caller owns the store and is responsible for Close()-ing it after
// Gateway.Stop() returns.
func NewWithDeviceStore(cfg types.Config, store *auth.DeviceStore) (*Gateway, error) {
	authMgr, err := auth.NewWithStore(store)
	if err != nil {
		return nil, err
	}
	return newGatewayFromAuth(cfg, authMgr, store), nil
}

func newGatewayFromAuth(cfg types.Config, authMgr *auth.Manager, store *auth.DeviceStore) *Gateway {
	return &Gateway{
		cfg:              cfg,
		Auth:             authMgr,
		DeviceStore:      store,
		Sessions:         auth.NewSessionStore(cfg.Auth.SessionTimeout),
		Pairing:          auth.NewPairingStore(authMgr, 0),
		Events:           NewEventBus(1000),
		ThreadHub:        NewThreadHub(),
		pairClaimLimiter: newIPRateLimiter(5, time.Minute),
	}
}

// Handler returns the gateway's http.Handler so in-process dispatchers
// (relay client, tests) can invoke the gateway without going through the
// TCP listener. Same routes, same auth, same handlers — just bypasses
// the network. Returns nil if called before Start.
func (g *Gateway) Handler() http.Handler {
	return g.mux
}

// SetThreadStore wires the persistent thread store. Optional —
// callers that don't pass a store get a Gateway whose /api/v1/threads/*
// endpoints return 503. The agent factory wires this on startup.
func (g *Gateway) SetThreadStore(s *threads.Store) {
	g.mu.Lock()
	g.Threads = s
	g.mu.Unlock()
}

// SetMessageHandler wires the agent's inbound handler.
func (g *Gateway) SetMessageHandler(h MessageHandler) {
	g.mu.Lock()
	g.msgHandler = h
	g.mu.Unlock()
}

// SetMessageHandlerN wires the model-aware handler used when a thread
// has a /model override. Optional — when nil the gateway always calls
// the regular MessageHandler regardless of per-thread model.
func (g *Gateway) SetMessageHandlerN(h MessageHandlerN) {
	g.mu.Lock()
	g.msgHandlerN = h
	g.mu.Unlock()
}

// SetMessageHandlerNU wires the usage-aware handler. Preferred over
// MessageHandlerN when set — every agent reply gets usage data
// persisted into its message.metadata. Optional.
func (g *Gateway) SetMessageHandlerNU(h MessageHandlerNU) {
	g.mu.Lock()
	g.msgHandlerNU = h
	g.mu.Unlock()
}

// ModelRegistry is the minimal subset of llm.Router the gateway needs
// to answer `/models` listing requests. Kept as an interface so the
// gateway doesn't need to import the llm package directly — callers
// adapt their concrete router to it.
type ModelRegistry interface {
	Names() []string
	DefaultName() string
}

// SetModelRegistry wires the router so `/models` can list what's
// available. Optional — without it, `/models` returns a generic
// "model switching unavailable" message.
func (g *Gateway) SetModelRegistry(r ModelRegistry) {
	g.mu.Lock()
	g.router = r
	g.mu.Unlock()
}

// PolicyController is the slice of the security policy engine the settings
// handler needs: read the live config to render it, and hot-swap it after
// an admin saves. Defined as an interface so the gateway doesn't import
// internal/security (same pattern as AuditRecorder). *security.PolicyEngine
// satisfies it.
type PolicyController interface {
	Snapshot() types.PolicyConfig
	Reload(types.PolicyConfig)
}

// SetPolicyController wires the live policy engine so the settings page can
// read and hot-reload policy. Optional — without it, the settings page
// shows policy from the on-disk file and reports restart-required on save.
func (g *Gateway) SetPolicyController(p PolicyController) {
	g.mu.Lock()
	g.policyCtl = p
	g.mu.Unlock()
}

// SetSettingsPaths records where the settings handler persists edits — the
// resolved config.yaml and policy.yaml paths. Empty paths make the settings
// page read-only (nothing to write back to).
func (g *Gateway) SetSettingsPaths(configPath, policyPath string) {
	g.mu.Lock()
	g.configPath = configPath
	g.policyPath = policyPath
	g.mu.Unlock()
}

// SetSettingsEditAuth installs the predicate that decides whether a user may
// modify settings. Personal mode wires "always true"; team/enterprise wires
// an RBAC check so only admins can edit and everyone else is read-only.
func (g *Gateway) SetSettingsEditAuth(fn func(userID string) bool) {
	g.mu.Lock()
	g.settingsEditAuth = fn
	g.mu.Unlock()
}

// canEditSettings resolves the effective edit permission for a user. When no
// predicate is wired, fall back to mode: personal mode is single-user and
// self-administered (editable); team/enterprise without an RBAC wiring is
// locked down (read-only) as the safe default.
func (g *Gateway) canEditSettings(userID string) bool {
	g.mu.RLock()
	fn := g.settingsEditAuth
	mode := g.cfg.Mode
	g.mu.RUnlock()
	if fn != nil {
		return fn(userID)
	}
	return mode == types.ModePersonal
}

// AuditRecorder is the minimal pairing-event audit surface. Defined
// as an interface here so the gateway package doesn't import
// internal/security — callers adapt their AuditLogger to it. When
// unset, pairing.go's recordAudit is a no-op (legacy embedders +
// tests don't have to wire one).
type AuditRecorder interface {
	Log(sessionID, userID, action string, detail map[string]interface{}) error
}

// SetAuditRecorder wires the audit recorder. Optional.
func (g *Gateway) SetAuditRecorder(r AuditRecorder) {
	g.mu.Lock()
	g.audit = r
	g.mu.Unlock()
}

// SetExtensionHandler registers the AdminAPI / future plugins. Only one
// extension at a time; the latest replaces.
func (g *Gateway) SetExtensionHandler(h ExtensionHandler) {
	g.mu.Lock()
	g.extensionHook = h
	g.mu.Unlock()
}

// Start binds and serves. Returns once the listener is up; serving runs in
// a background goroutine.
func (g *Gateway) Start() error {
	if !g.Auth.HasTokens() {
		token, err := g.Auth.SetupInitialToken()
		if err != nil {
			return err
		}
		printInitialTokenBanner(token)
		// Also stash the raw token at ~/.fantazm/api-token (0600) so local
		// clients on the same machine — the Mac menu-bar app, the web UI
		// when accessed from the same user — can auto-populate without
		// the user copy-pasting from the terminal banner. Same trust
		// model as the master.key file: protected by filesystem perms,
		// owner-only. Best-effort: errors are logged and ignored, since
		// the banner is still the source of truth.
		if err := writeLocalTokenFile(token); err != nil {
			slog.Warn("couldn't stash initial token for local clients", "err", err)
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/health", g.handleHealth)
	mux.HandleFunc("/api/v1/message", g.handleMessage)
	mux.HandleFunc("/api/v1/stream", g.handleStream)
	mux.HandleFunc("/api/v1/session", g.handleGetSession)
	mux.HandleFunc("/api/v1/events", g.handleEvents)
	// Device pairing flow — see handlers in pairing.go.
	mux.HandleFunc("/api/v1/pair/start", g.handlePairStart)
	mux.HandleFunc("/api/v1/pair/watch", g.handlePairWatch)
	mux.HandleFunc("/api/v1/pair/claim", g.handlePairClaim)
	// Device management for paired-device tokens.
	mux.HandleFunc("/api/v1/devices", g.handleDevices)
	mux.HandleFunc("/api/v1/devices/", g.handleDeviceItem) // trailing slash → /{id}
	// Persistent threads + per-thread real-time SSE for cross-device
	// session continuity. See pkg internal/threads + gateway/threads.go.
	mux.HandleFunc("/api/v1/threads", g.handleThreadsRoot)
	mux.HandleFunc("/api/v1/threads/", g.handleThreadItem)
	// Feature/policy/model settings surface for the web UI.
	mux.HandleFunc("/api/v1/settings", g.handleSettings)
	mux.HandleFunc("/", g.handleFallback)

	addr := fmt.Sprintf("%s:%d", g.cfg.Host, g.cfg.Port)
	g.server = &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 120 * time.Second,
	}
	g.mux = mux
	g.cleanupTicker = time.NewTicker(60 * time.Second)
	g.cleanupStop = make(chan struct{})
	go func() {
		for {
			select {
			case <-g.cleanupTicker.C:
				g.Sessions.Cleanup()
			case <-g.cleanupStop:
				return
			}
		}
	}()
	go func() {
		if err := g.server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("gateway listener error", "err", err)
		}
	}()
	slog.Info("Fathom gateway listening", "host", g.cfg.Host, "port", g.cfg.Port)
	return nil
}

// Stop shuts the gateway down, including the session-cleanup ticker
// and the pairing-code sweeper.
func (g *Gateway) Stop(ctx context.Context) error {
	if g.cleanupTicker != nil {
		g.cleanupTicker.Stop()
		close(g.cleanupStop)
	}
	if g.Pairing != nil {
		g.Pairing.Close()
	}
	if g.Threads != nil {
		_ = g.Threads.Close()
	}
	if g.DeviceStore != nil {
		_ = g.DeviceStore.Close()
	}
	if g.server != nil {
		if err := g.server.Shutdown(ctx); err != nil {
			return err
		}
	}
	slog.Info("gateway stopped")
	return nil
}

func (g *Gateway) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		jsonError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":    "ok",
		"version":   "0.1.0",
		"uptime":    int(time.Since(startTime).Seconds()),
		"sessions":  g.Sessions.Count(),
		"timestamp": time.Now().UTC(),
		"mode":      string(g.cfg.Mode),
		"profile":   string(g.cfg.Profile),
		// `features` tells clients what subsystems are loaded so the
		// mobile UI can fall back gracefully (e.g. no /threads in
		// minimal mode → use /message instead). Cheap one-time
		// computation — re-snapshotted on every call so a future
		// admin-flip surface works without restart.
		"features": map[string]bool{
			"threads": g.Threads != nil,
		},
	})
}

func (g *Gateway) handleMessage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	authResult, ok := g.authenticate(w, r)
	if !ok {
		return
	}
	sess := g.Sessions.Create(authResult.UserID, remoteAddr(r))

	body, err := io.ReadAll(io.LimitReader(r.Body, 1024*1024))
	if err != nil {
		jsonError(w, http.StatusBadRequest, "body unreadable")
		return
	}
	var in struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(body, &in); err != nil || in.Text == "" {
		jsonError(w, http.StatusBadRequest, "missing 'text' field")
		return
	}

	msg := types.ChannelMessage{
		ChannelType: "rest",
		ChannelID:   "api",
		SenderID:    authResult.UserID,
		Text:        in.Text,
		Timestamp:   time.Now().UTC(),
	}
	g.mu.RLock()
	handler := g.msgHandler
	g.mu.RUnlock()
	if handler == nil {
		jsonError(w, http.StatusServiceUnavailable, "no message handler configured")
		return
	}
	reply, err := handler(r.Context(), msg, sess)
	g.Sessions.Destroy(sess.ID)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"response":  reply,
		"sessionId": sess.ID,
	})
}

func (g *Gateway) handleGetSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		jsonError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	authResult, ok := g.authenticate(w, r)
	if !ok {
		return
	}
	sessions := g.Sessions.ByUser(authResult.UserID)
	if sessions == nil {
		sessions = []types.Session{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"sessions": sessions,
	})
}

// handleFallback consults the extension hook (AdminAPI in team/enterprise
// mode), then serves the embedded PWA assets, then 404s.
//
// Asset serving rules:
//   - GET / (also /chat, /chat.html for back-compat with old bookmarks)
//     serves index.html — no-store so updates land immediately
//   - GET /chat.css, /chat.js, /sw.js — served as-is (versioned by the SW
//     itself via CACHE_VERSION). Browsers may still revalidate, that's fine.
//   - GET /manifest.webmanifest, /icons/* — served with a short cache.
//     Icons are content-hash-safe in practice (we don't rename them) but
//     the PWA spec wants the manifest fetched live to pick up changes.
//   - Anything else under our static surface → 404, doesn't fall through to
//     a wildcard so we don't accidentally serve files we didn't intend to.
func (g *Gateway) handleFallback(w http.ResponseWriter, r *http.Request) {
	g.mu.RLock()
	hook := g.extensionHook
	g.mu.RUnlock()
	if hook != nil && hook(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		jsonError(w, http.StatusNotFound, "Not found")
		return
	}
	path := r.URL.Path
	// Root + legacy aliases all serve the index.
	if path == "/" || path == "/chat" || path == "/chat.html" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(web.IndexHTML())
		return
	}
	// Settings page, served at a clean /settings as well as /settings.html.
	// Reachability mirrors the API: personal mode is loopback-only (never the
	// relay/LAN); team/enterprise allows remote so server-deployed admins can
	// reach it (RBAC is the boundary). Unreachable callers get a plain 404 so
	// the surface stays invisible.
	if path == "/settings" || path == "/settings.html" {
		if !g.settingsReachable(r) {
			jsonError(w, http.StatusNotFound, "Not found")
			return
		}
		if data, ok := web.AssetBytes("settings.html"); ok {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(data)
			return
		}
	}
	if data, ok := web.AssetBytes(path); ok {
		w.Header().Set("Content-Type", web.ContentType(path))
		// SW + manifest must always revalidate so we never serve stale.
		// Icons + CSS/JS can be cached briefly — the SW already handles
		// shell freshness via CACHE_VERSION.
		if strings.HasSuffix(path, "/sw.js") || strings.HasSuffix(path, "/manifest.webmanifest") {
			w.Header().Set("Cache-Control", "no-store")
		} else {
			w.Header().Set("Cache-Control", "public, max-age=60")
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
		return
	}
	jsonError(w, http.StatusNotFound, "Not found")
}

func (g *Gateway) authenticate(w http.ResponseWriter, r *http.Request) (auth.Result, bool) {
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, "Bearer ") {
		jsonError(w, http.StatusUnauthorized, "Missing or invalid Authorization header")
		return auth.Result{}, false
	}
	token := strings.TrimPrefix(header, "Bearer ")
	res, err := g.Auth.Authenticate(token)
	if err != nil {
		jsonError(w, http.StatusUnauthorized, err.Error())
		return auth.Result{}, false
	}
	return res, true
}

func remoteAddr(r *http.Request) string {
	if ra := r.RemoteAddr; ra != "" {
		return ra
	}
	return "unknown"
}

// isLocalRequest reports whether the request came from this same machine —
// i.e. a genuine loopback TCP connection to the gateway's listener. This is
// the security boundary for the settings/admin surface: those endpoints can
// flip shell on and rewrite policy, so we never expose them over the relay
// or the LAN, only to a browser running on the host itself.
//
// The check trusts ONLY r.RemoteAddr, which net/http fills from the actual
// socket peer — it cannot be forged by a header (X-Forwarded-For etc.).
// Relay-forwarded requests are dispatched in-process via http.NewRequest,
// which leaves RemoteAddr empty, so they read as non-local and are denied.
func isLocalRequest(r *http.Request) bool {
	host := r.RemoteAddr
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// settingsReachable reports whether r is allowed to reach the settings
// surface at all, given the deployment mode. The loopback restriction only
// makes sense in personal mode, where the gateway runs on the user's own
// machine and remote access means the relay/LAN (which must NOT reach
// settings). Team/enterprise gateways run on a server or in a container and
// admins connect over the network, so there we allow remote requests and
// rely on RBAC (SetSettingsEditAuth) as the boundary instead — a loopback
// gate would lock every admin out.
func (g *Gateway) settingsReachable(r *http.Request) bool {
	if g.cfg.Mode == types.ModePersonal {
		return isLocalRequest(r)
	}
	return true
}

var startTime = time.Now()

func jsonError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func writeJSON(w http.ResponseWriter, status int, body interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func printInitialTokenBanner(token string) {
	bar := strings.Repeat("=", 60)
	slog.Info(bar)
	slog.Info("No API tokens found. Initial admin token created:")
	slog.Info("  " + token)
	slog.Info("Store this securely. It will not be shown again.")
	slog.Info("Local clients (Mac app, web UI) auto-load from ~/.fantazm/api-token")
	slog.Info(bar)
}

// writeLocalTokenFile persists the bootstrap admin token at
// ~/.fantazm/api-token with 0600 permissions. Local same-user clients
// — the Mac menu-bar app, the embedded web UI — read from here if
// they don't have a token configured, so the user never has to copy
// the banner value by hand.
//
// Honors $FANTAZM_TOKEN_FILE for override (test fixtures, multi-user
// setups). Writes to a tempfile + rename so a partial write can't
// produce a corrupt secret.
func writeLocalTokenFile(token string) error {
	target := brandenv.Get("FATHOM_TOKEN_FILE")
	if target == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		target = filepath.Join(home, ".fantazm", "api-token")
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return err
	}
	tmp := target + ".tmp"
	if err := os.WriteFile(tmp, []byte(token), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, target)
}

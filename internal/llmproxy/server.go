package llmproxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"path"
	"strings"

	"github.com/zdaniels/fathom/internal/security"
	"github.com/zdaniels/fathom/pkg/types"
)

// Server is the HTTP handler exposed by the gateway. Routes:
//
//	POST /v1/chat/completions  → auth, pick provider, proxy upstream
//	GET  /v1/models            → list models the caller's tenant can use
//	GET  /admin/health         → liveness probe
//	GET  /admin/usage          → per-tenant counters (auth gated to admin tenant)
//
// The server does NOT translate provider response shapes. v0.1 assumes
// every configured upstream speaks OpenAI's /chat/completions wire
// format; the request body is forwarded essentially as-is (with the
// "<provider>/" prefix stripped from the model field, and auth headers
// rewritten per provider's AuthStyle).
type Server struct {
	router  *Router
	tenants *TenantStore
	audit   security.Recorder
	client  *http.Client
}

// NewServer wires the request path. The audit recorder is optional —
// pass nil to skip audit log writes (mainly for tests).
func NewServer(router *Router, tenants *TenantStore, audit security.Recorder) *Server {
	return &Server{
		router:  router,
		tenants: tenants,
		audit:   audit,
		client:  &http.Client{},
	}
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/chat/completions":
		s.handleChatCompletions(w, r)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/models":
		s.handleModels(w, r)
	case r.Method == http.MethodGet && r.URL.Path == "/admin/health":
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	case r.Method == http.MethodGet && r.URL.Path == "/admin/usage":
		s.handleAdminUsage(w, r)
	default:
		writeJSON(w, http.StatusNotFound, errorBody("not found"))
	}
}

func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	tenant, ok := s.authenticate(w, r)
	if !ok {
		return
	}

	// Pull the body so we can parse `model`, then re-marshal with the
	// "<provider>/" prefix stripped. Body is bounded to 8 MiB — anything
	// larger is almost certainly a misuse or attack.
	raw, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody("read body: "+err.Error()))
		return
	}
	var envelope map[string]interface{}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody("invalid JSON: "+err.Error()))
		return
	}
	modelField, _ := envelope["model"].(string)
	if modelField == "" {
		writeJSON(w, http.StatusBadRequest, errorBody("missing model field"))
		return
	}

	provider, downstreamModel, err := s.router.Pick(modelField)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody(err.Error()))
		return
	}

	if !s.tenants.Allows(tenant, modelField) {
		s.logAudit(tenant.ID, types.AuditAction("llm.denied"), map[string]interface{}{
			"model":  modelField,
			"reason": "not in allowedModels",
		}, types.PolicyDeny)
		writeJSON(w, http.StatusForbidden, errorBody("model not allowed for this tenant"))
		return
	}
	if err := s.tenants.ReserveQuota(tenant); err != nil {
		s.logAudit(tenant.ID, types.AuditAction("llm.denied"), map[string]interface{}{
			"model":  modelField,
			"reason": err.Error(),
		}, types.PolicyDeny)
		writeJSON(w, http.StatusTooManyRequests, errorBody(err.Error()))
		return
	}

	envelope["model"] = downstreamModel
	rewritten, _ := json.Marshal(envelope)

	upstreamURL := provider.BaseURL + "/chat/completions"
	upReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, upstreamURL, bytes.NewReader(rewritten))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorBody("build upstream req: "+err.Error()))
		return
	}
	upReq.Header.Set("Content-Type", "application/json")
	for k, v := range provider.Headers {
		upReq.Header.Set(k, v)
	}
	applyAuth(upReq, provider)

	// Streaming pass-through: when the body has stream:true we pipe the
	// upstream response bytes back without buffering, so SSE deltas
	// arrive at the client as they leave the upstream.
	streaming, _ := envelope["stream"].(bool)
	upResp, err := s.client.Do(upReq)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, errorBody("upstream: "+err.Error()))
		return
	}
	defer upResp.Body.Close()

	// Forward status + headers that matter for the protocol. Skip
	// hop-by-hop and connection-management headers.
	for k, vs := range upResp.Header {
		if isHopByHop(k) {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(upResp.StatusCode)

	if streaming {
		// Flush as bytes arrive; this is how SSE actually works.
		flusher, _ := w.(http.Flusher)
		buf := make([]byte, 4096)
		for {
			n, rerr := upResp.Body.Read(buf)
			if n > 0 {
				_, _ = w.Write(buf[:n])
				if flusher != nil {
					flusher.Flush()
				}
			}
			if rerr != nil {
				break
			}
		}
		// Streaming responses include usage in the final event
		// (OpenAI's `stream_options.include_usage: true`); we don't
		// parse mid-flight in v0.1. Tenants on streaming workloads
		// should set TokensPerDay: 0 for now.
		s.logAudit(tenant.ID, types.AuditAction("llm.call"), map[string]interface{}{
			"provider": provider.Name,
			"model":    downstreamModel,
			"stream":   true,
			"status":   upResp.StatusCode,
		}, types.PolicyAllow)
		return
	}

	// Non-streaming: buffer the whole response, parse usage, write back.
	body, _ := io.ReadAll(io.LimitReader(upResp.Body, 16<<20))
	_, _ = w.Write(body)

	if upResp.StatusCode == http.StatusOK {
		var parsed struct {
			Usage struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
		}
		_ = json.Unmarshal(body, &parsed)
		s.tenants.RecordTokens(tenant, parsed.Usage.PromptTokens, parsed.Usage.CompletionTokens)
		s.logAudit(tenant.ID, types.AuditAction("llm.call"), map[string]interface{}{
			"provider":         provider.Name,
			"model":            downstreamModel,
			"promptTokens":     parsed.Usage.PromptTokens,
			"completionTokens": parsed.Usage.CompletionTokens,
		}, types.PolicyAllow)
	} else {
		s.logAudit(tenant.ID, types.AuditAction("llm.error"), map[string]interface{}{
			"provider": provider.Name,
			"model":    downstreamModel,
			"status":   upResp.StatusCode,
		}, types.PolicyAllow)
	}
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	tenant, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	// /v1/models returns the tenant's allowedModels glob list — we don't
	// have a per-provider model catalog at this layer. Clients use this
	// to render the model picker.
	data := make([]map[string]string, 0, len(tenant.AllowedModels))
	for _, m := range tenant.AllowedModels {
		data = append(data, map[string]string{"id": m, "object": "model"})
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"object": "list",
		"data":   data,
	})
}

func (s *Server) handleAdminUsage(w http.ResponseWriter, r *http.Request) {
	tenant, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	// Admin gate: only tenants whose ID begins with "admin" see global
	// usage. Everyone else gets their own tenant's row only. v0.2 will
	// replace this with proper RBAC via enterprise/core.
	snap := s.tenants.Snapshot()
	if !strings.HasPrefix(tenant.ID, "admin") {
		if u, ok := snap[tenant.ID]; ok {
			writeJSON(w, http.StatusOK, map[string]interface{}{tenant.ID: u})
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{})
		return
	}
	writeJSON(w, http.StatusOK, snap)
}

// authenticate extracts the bearer token, looks it up, writes the 401
// response if invalid. Returns (tenant, true) on success; (nil, false)
// on failure with the response already written.
func (s *Server) authenticate(w http.ResponseWriter, r *http.Request) (*Tenant, bool) {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		writeJSON(w, http.StatusUnauthorized, errorBody("missing bearer token"))
		return nil, false
	}
	tok := strings.TrimPrefix(h, "Bearer ")
	t, err := s.tenants.Authenticate(tok)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, errorBody("invalid bearer token"))
		return nil, false
	}
	return t, true
}

// applyAuth sets the upstream auth header per the provider's style.
// Empty key + "none" style skips auth entirely (LM Studio, Ollama).
func applyAuth(req *http.Request, p *Provider) {
	if p.APIKey == "" {
		return
	}
	switch {
	case p.AuthStyle == "none":
		return
	case p.AuthStyle == "x-api-key":
		req.Header.Set("x-api-key", p.APIKey)
	case strings.HasPrefix(p.AuthStyle, "header:"):
		name := strings.TrimPrefix(p.AuthStyle, "header:")
		req.Header.Set(name, p.APIKey)
	default: // "bearer"
		req.Header.Set("Authorization", "Bearer "+p.APIKey)
	}
}

func writeJSON(w http.ResponseWriter, status int, body interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func errorBody(msg string) map[string]interface{} {
	return map[string]interface{}{"error": map[string]string{"message": msg}}
}

// isHopByHop filters headers the proxy must NOT forward verbatim. See
// RFC 7230 §6.1 — these are connection-management headers tied to the
// hop the client/upstream is on, not end-to-end semantics.
func isHopByHop(name string) bool {
	switch strings.ToLower(name) {
	case "connection", "keep-alive", "proxy-authenticate", "proxy-authorization",
		"te", "trailer", "transfer-encoding", "upgrade":
		return true
	}
	return false
}

// logAudit is the one-line bridge to the optional security.Recorder.
// It accepts a SessionID-shaped first arg, but in this gateway there
// are no sessions — pass tenantID instead so audit rows are at least
// queryable by who-made-the-call.
func (s *Server) logAudit(tenantID string, action types.AuditAction, detail map[string]interface{}, decision types.PolicyDecision) {
	if s.audit == nil {
		return
	}
	s.audit.Log("", tenantID, action, detail, decision)
}

// pathMatches is a thin wrapper around path.Match for testability; not
// currently used outside of tests but kept exported-equivalent so the
// test file doesn't have to reach into tenants.go.
func pathMatches(glob, s string) bool {
	ok, _ := path.Match(glob, s)
	return ok
}

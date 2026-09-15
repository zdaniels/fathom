// Gateway HTTP handlers for the device-pairing flow.
//
// Endpoints:
//
//	POST /api/v1/pair/start            (authenticated: admin or device)
//	GET  /api/v1/pair/watch?code=...   (authenticated)
//	POST /api/v1/pair/claim            (UNAUTHENTICATED, IP-rate-limited)
//	GET  /api/v1/devices               (authenticated)
//	DELETE /api/v1/devices/{id}        (authenticated)
//
// /pair/claim is the only public endpoint here — by design, since the
// claiming device doesn't have a token yet. Rate-limited to 5/min/IP to
// foreclose brute force on the 6-digit code (already a ~10^-5 chance
// per attempt over 60 seconds).
package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/zdaniels/fathom/internal/auth"
)

// handlePairStart mints a pairing code on behalf of the authenticated
// admin (or the CLI calling with the bootstrap token). Response carries
// the raw code so the caller can render it as text + QR.
func (g *Gateway) handlePairStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	res, ok := g.authenticate(w, r)
	if !ok {
		return
	}
	pc, err := g.Pairing.Generate(res.UserID)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	g.recordAudit(res, "pair_start", map[string]interface{}{
		"code": pc.Code,
	})
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"code":       pc.Code,
		"expires_at": pc.ExpiresAt,
	})
}

// handlePairWatch is the long-poll endpoint the CLI uses to be notified
// when a code is claimed. Blocks up to the code's TTL, then returns
// either { claimed: true, device: {...} } or 408.
func (g *Gateway) handlePairWatch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		jsonError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	if _, ok := g.authenticate(w, r); !ok {
		return
	}
	code := strings.TrimSpace(r.URL.Query().Get("code"))
	if code == "" {
		jsonError(w, http.StatusBadRequest, "code required")
		return
	}
	// Cap the wait so misconfigured clients can't hold a connection
	// forever. The code's own TTL is shorter (60s default); use whichever
	// fires first.
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	dev, err := g.Pairing.Wait(ctx, code)
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrPairingExpired):
			jsonError(w, http.StatusGone, "pairing code expired")
		case errors.Is(err, auth.ErrPairingNoSuchCode):
			jsonError(w, http.StatusNotFound, "no such pairing code")
		case errors.Is(err, context.DeadlineExceeded):
			jsonError(w, http.StatusRequestTimeout, "no claim yet — poll again")
		case errors.Is(err, context.Canceled):
			// Client gave up. Best effort response; the connection is
			// probably already half-closed, so the write may fail.
			jsonError(w, http.StatusRequestTimeout, "client canceled")
		default:
			jsonError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"claimed": true,
		"device":  dev,
	})
}

// handlePairClaim is called by the pairing device (phone, second laptop,
// etc.). It does NOT require auth — the 6-digit code IS the auth.
// Rate-limited per source IP to foreclose brute force.
func (g *Gateway) handlePairClaim(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	if g.pairClaimLimiter != nil && !g.pairClaimLimiter.allow(clientIP(r)) {
		jsonError(w, http.StatusTooManyRequests, "too many pairing attempts; try again in a minute")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 2048))
	if err != nil {
		jsonError(w, http.StatusBadRequest, "body unreadable")
		return
	}
	var in struct {
		Code       string `json:"code"`
		DeviceName string `json:"device_name"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		jsonError(w, http.StatusBadRequest, "bad json")
		return
	}
	dev, token, err := g.Pairing.Claim(in.Code, in.DeviceName)
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrPairingExpired):
			jsonError(w, http.StatusGone, "pairing code expired")
		case errors.Is(err, auth.ErrPairingClaimed):
			jsonError(w, http.StatusConflict, "code already claimed")
		case errors.Is(err, auth.ErrPairingNoSuchCode):
			jsonError(w, http.StatusNotFound, "invalid pairing code")
		default:
			jsonError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"token":       token,
		"device_id":   dev.ID,
		"device_name": dev.Name,
	})
}

// handleDevices: GET = list paired devices for the authenticated user;
// DELETE on /api/v1/devices/{id} = revoke (see handleDeviceItem).
func (g *Gateway) handleDevices(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		jsonError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	res, ok := g.authenticate(w, r)
	if !ok {
		return
	}
	devices := g.Auth.ListDevices(res.UserID)
	if devices == nil {
		devices = []auth.Device{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"devices": devices,
	})
}

// handleDeviceItem handles DELETE /api/v1/devices/{id} for revocation
// and (currently) 405 for everything else. Path is parsed by stripping
// the prefix — http.ServeMux trailing-slash routing means everything
// under /api/v1/devices/ lands here.
func (g *Gateway) handleDeviceItem(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		jsonError(w, http.StatusMethodNotAllowed, "DELETE only")
		return
	}
	res, ok := g.authenticate(w, r)
	if !ok {
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/v1/devices/")
	if id == "" || strings.Contains(id, "/") {
		jsonError(w, http.StatusBadRequest, "missing or invalid device id")
		return
	}
	if !g.Auth.RevokeDevice(id) {
		jsonError(w, http.StatusNotFound, "no such device")
		return
	}
	g.recordAudit(res, "device_revoke", map[string]interface{}{
		"device_id": id,
	})
	writeJSON(w, http.StatusOK, map[string]interface{}{"revoked": id})
}

// recordAudit emits a pairing-flow event into the gateway's audit
// recorder when one is wired (via SetAuditRecorder). When unset,
// this is a no-op — keeps tests + legacy embedders from needing the
// dependency. Both branches must stay quiet on disk: pairing events
// can be high-volume during onboarding and the audit log is in-memory
// + hash-chained; spurious errors here shouldn't break the pairing
// HTTP path itself.
func (g *Gateway) recordAudit(res auth.Result, action string, detail map[string]interface{}) {
	g.mu.RLock()
	recorder := g.audit
	g.mu.RUnlock()
	if recorder == nil {
		return
	}
	if detail == nil {
		detail = map[string]interface{}{}
	}
	// Stamp the device id so each pairing event is attributable even
	// when the recorder only sees the abstract AuditRecorder surface.
	if res.DeviceID != "" {
		detail["actor_device_id"] = res.DeviceID
	}
	_ = recorder.Log("", res.UserID, "pairing."+action, detail)
}

// === IP rate limiter — small fixed-window-per-IP counter ================

// ipRateLimiter caps how many requests a single source IP can make in
// a window. Fixed-window (not sliding) for simplicity; the limit only
// matters for foreclosing brute force, so the exact distribution is
// less important than the rate.
type ipRateLimiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	state  map[string]*ipBucket
}

type ipBucket struct {
	count     int
	windowEnd time.Time
}

func newIPRateLimiter(max int, window time.Duration) *ipRateLimiter {
	return &ipRateLimiter{max: max, window: window, state: map[string]*ipBucket{}}
}

// allow returns true if the IP can proceed, false if it's over budget.
// Also opportunistically prunes expired entries to keep the map small.
func (l *ipRateLimiter) allow(ip string) bool {
	if ip == "" {
		return true // can't classify → don't punish
	}
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.state[ip]
	if !ok || now.After(b.windowEnd) {
		l.state[ip] = &ipBucket{count: 1, windowEnd: now.Add(l.window)}
		// Cheap prune: every 64th miss, walk and drop expired entries.
		if len(l.state)%64 == 0 {
			for k, v := range l.state {
				if now.After(v.windowEnd) {
					delete(l.state, k)
				}
			}
		}
		return true
	}
	if b.count >= l.max {
		return false
	}
	b.count++
	return true
}

// clientIP extracts the source IP. Honors X-Forwarded-For when set (CF
// Tunnel sets this), falls back to RemoteAddr.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i > 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	// RemoteAddr is host:port; trim the port.
	if ra := r.RemoteAddr; ra != "" {
		if i := strings.LastIndexByte(ra, ':'); i > 0 {
			return ra[:i]
		}
		return ra
	}
	return ""
}

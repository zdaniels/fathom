package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zdaniels/fathom/internal/auth"
)

func TestPairStartReturnsCode(t *testing.T) {
	g, tok := newTestGateway(t)
	defer g.Pairing.Close()

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/pair/start", nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	g.handlePairStart(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var body struct {
		Code      string    `json:"code"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Code) != 6 {
		t.Errorf("code length = %d, want 6 (got %q)", len(body.Code), body.Code)
	}
	if body.ExpiresAt.Before(time.Now()) {
		t.Errorf("expires_at is in the past: %v", body.ExpiresAt)
	}
}

func TestPairClaimMintsDeviceToken(t *testing.T) {
	g, tok := newTestGateway(t)
	defer g.Pairing.Close()

	// Step 1: admin starts a pairing.
	pc, err := g.Pairing.Generate("admin")
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	// Step 2: phone claims with the code.
	body := `{"code":"` + pc.Code + `","device_name":"iPhone"}`
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/pair/claim",
		strings.NewReader(body))
	g.handlePairClaim(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("claim status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	var resp struct {
		Token      string `json:"token"`
		DeviceID   string `json:"device_id"`
		DeviceName string `json:"device_name"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode claim: %v", err)
	}
	if resp.Token == "" || resp.DeviceID == "" {
		t.Fatalf("missing token or device_id in claim response: %+v", resp)
	}
	if resp.DeviceName != "iPhone" {
		t.Errorf("device_name = %q, want 'iPhone'", resp.DeviceName)
	}

	// Step 3: the new device token works for an authenticated call.
	w2 := httptest.NewRecorder()
	r2 := httptest.NewRequest(http.MethodGet, "/api/v1/devices", nil)
	r2.Header.Set("Authorization", "Bearer "+resp.Token)
	g.handleDevices(w2, r2)
	if w2.Code != http.StatusOK {
		t.Errorf("device-token list call = %d, want 200", w2.Code)
	}
	// Suppress unused warning on the admin token.
	_ = tok
}

func TestPairClaimRejectsAlreadyClaimedCode(t *testing.T) {
	g, _ := newTestGateway(t)
	defer g.Pairing.Close()

	pc, _ := g.Pairing.Generate("admin")
	// First claim succeeds…
	if _, _, err := g.Pairing.Claim(pc.Code, "phone1"); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	// …second claim fails with 409.
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/pair/claim",
		strings.NewReader(`{"code":"`+pc.Code+`","device_name":"phone2"}`))
	g.handlePairClaim(w, r)
	if w.Code != http.StatusConflict {
		t.Errorf("re-claim status = %d, want 409 (body: %s)", w.Code, w.Body.String())
	}
}

func TestPairClaimRejectsBadCode(t *testing.T) {
	g, _ := newTestGateway(t)
	defer g.Pairing.Close()

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/pair/claim",
		strings.NewReader(`{"code":"999999","device_name":"x"}`))
	g.handlePairClaim(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("unknown-code status = %d, want 404", w.Code)
	}
}

func TestPairClaimIsRateLimited(t *testing.T) {
	g, _ := newTestGateway(t)
	defer g.Pairing.Close()

	// Default limiter: 5/min/IP. Fire 6 from the same RemoteAddr.
	for i := 0; i < 5; i++ {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/api/v1/pair/claim",
			strings.NewReader(`{"code":"000000"}`))
		r.RemoteAddr = "10.0.0.1:1234"
		g.handlePairClaim(w, r)
		if w.Code == http.StatusTooManyRequests {
			t.Fatalf("hit rate limit on attempt %d, want first 5 allowed", i+1)
		}
	}
	// 6th should 429.
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/pair/claim",
		strings.NewReader(`{"code":"000000"}`))
	r.RemoteAddr = "10.0.0.1:1234"
	g.handlePairClaim(w, r)
	if w.Code != http.StatusTooManyRequests {
		t.Errorf("6th attempt status = %d, want 429", w.Code)
	}
}

func TestPairWatchWakesOnClaim(t *testing.T) {
	g, tok := newTestGateway(t)
	defer g.Pairing.Close()

	pc, _ := g.Pairing.Generate("admin")

	// Start a watcher in a goroutine; it should return once the code
	// is claimed via the (unauthenticated) claim endpoint.
	type result struct {
		code int
		dev  auth.Device
	}
	done := make(chan result, 1)
	go func() {
		w := httptest.NewRecorder()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		r := httptest.NewRequest(http.MethodGet,
			"/api/v1/pair/watch?code="+pc.Code, nil).WithContext(ctx)
		r.Header.Set("Authorization", "Bearer "+tok)
		g.handlePairWatch(w, r)
		var body struct {
			Claimed bool        `json:"claimed"`
			Device  auth.Device `json:"device"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &body)
		done <- result{code: w.Code, dev: body.Device}
	}()

	// Give the watcher a tick to start blocking.
	time.Sleep(40 * time.Millisecond)
	if _, _, err := g.Pairing.Claim(pc.Code, "test-device"); err != nil {
		t.Fatalf("claim: %v", err)
	}

	select {
	case r := <-done:
		if r.code != http.StatusOK {
			t.Errorf("watch status = %d, want 200", r.code)
		}
		if r.dev.Name != "test-device" {
			t.Errorf("device name = %q, want 'test-device'", r.dev.Name)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("watch never returned after claim")
	}
}

func TestDeviceRevokeKillsToken(t *testing.T) {
	g, tok := newTestGateway(t)
	defer g.Pairing.Close()

	pc, _ := g.Pairing.Generate("admin")
	_, deviceTok, err := g.Pairing.Claim(pc.Code, "to-be-revoked")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	devices := g.Auth.ListDevices("admin")
	if len(devices) != 1 {
		t.Fatalf("ListDevices returned %d, want 1", len(devices))
	}
	deviceID := devices[0].ID

	// Revoke via the HTTP endpoint (using admin token, simulating the
	// `fathom devices revoke` CLI path).
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodDelete,
		"/api/v1/devices/"+deviceID, nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	g.handleDeviceItem(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("revoke status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}

	// The device token should no longer authenticate.
	w2 := httptest.NewRecorder()
	r2 := httptest.NewRequest(http.MethodGet, "/api/v1/devices", nil)
	r2.Header.Set("Authorization", "Bearer "+deviceTok)
	g.handleDevices(w2, r2)
	if w2.Code != http.StatusUnauthorized {
		t.Errorf("revoked-token auth = %d, want 401", w2.Code)
	}
}

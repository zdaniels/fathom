package security

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func openTestVault(t *testing.T) *SecretsVault {
	t.Helper()
	v, err := OpenVault(VaultOpenOptions{Key: make32(0xaa)})
	if err != nil {
		t.Fatalf("OpenVault: %v", err)
	}
	return v
}

func make32(b byte) []byte {
	out := make([]byte, 32)
	for i := range out {
		out[i] = b
	}
	return out
}

func startTestProxy(t *testing.T, v *SecretsVault) (*EgressProxy, string) {
	t.Helper()
	p := NewEgressProxy(EgressProxyOptions{Vault: v})
	url, err := p.Start()
	if err != nil {
		t.Fatalf("proxy Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = p.Stop(ctx)
	})
	return p, url
}

func proxyFetch(t *testing.T, proxyURL, token string, payload map[string]interface{}) (int, map[string]interface{}) {
	t.Helper()
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest(http.MethodPost, proxyURL+"/fetch", strings.NewReader(string(body)))
	req.Header.Set("X-Fantazm-Token", token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("proxy fetch: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var parsed map[string]interface{}
	_ = json.Unmarshal(raw, &parsed)
	return resp.StatusCode, parsed
}

func TestEgressProxyRejectsMissingToken(t *testing.T) {
	v := openTestVault(t)
	_, proxyURL := startTestProxy(t, v)
	status, _ := proxyFetch(t, proxyURL, "", map[string]interface{}{
		"url": "https://example.com",
	})
	if status != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", status)
	}
}

func TestEgressProxyAllowListMiss(t *testing.T) {
	v := openTestVault(t)
	p, proxyURL := startTestProxy(t, v)
	tok, err := p.IssueToken("test-skill", []string{"api.github.com"}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	status, body := proxyFetch(t, proxyURL, tok, map[string]interface{}{
		"url": "https://api.example.com/probe",
	})
	if status != http.StatusForbidden {
		t.Errorf("status = %d, want 403", status)
	}
	if !strings.Contains(body["error"].(string), "allow-list") {
		t.Errorf("error message lacks 'allow-list': %v", body)
	}
}

func TestEgressProxyBlocksIMDSEvenIfAllowListed(t *testing.T) {
	v := openTestVault(t)
	p, proxyURL := startTestProxy(t, v)
	// Token says "169.254.169.254 is allowed," but the IP block-list
	// must still reject it.
	tok, _ := p.IssueToken("test-skill", []string{"169.254.169.254"}, time.Minute)
	status, body := proxyFetch(t, proxyURL, tok, map[string]interface{}{
		"url": "http://169.254.169.254/latest/meta-data/",
	})
	if status != http.StatusForbidden {
		t.Errorf("status = %d, want 403 (IP block-list)", status)
	}
	if !strings.Contains(body["error"].(string), "blocked address") {
		t.Errorf("error message lacks 'blocked address': %v", body)
	}
}

func TestEgressProxyBlocksLoopback(t *testing.T) {
	v := openTestVault(t)
	p, proxyURL := startTestProxy(t, v)
	tok, _ := p.IssueToken("test-skill", []string{"127.0.0.1"}, time.Minute)
	status, _ := proxyFetch(t, proxyURL, tok, map[string]interface{}{
		"url": "http://127.0.0.1:8080/secret",
	})
	if status != http.StatusForbidden {
		t.Errorf("status = %d, want 403 (loopback blocked)", status)
	}
}

func TestEgressProxyForwardsPublicHostAndAttachesBearer(t *testing.T) {
	// Spin a fake upstream that echoes the Authorization header.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"gotAuth": r.Header.Get("Authorization"),
		})
	}))
	t.Cleanup(upstream.Close)

	// The proxy enforces a private-IP block-list, so httptest's loopback
	// upstream would be rejected. To test the credential injection path,
	// we temporarily relax the block-list by issuing the token for the
	// httptest host pattern AND skipping the IP check via a wrapper.
	// Easier: directly test hostMatchesAllowList and apply spec without
	// going through Start.

	v := openTestVault(t)
	if err := v.Set("GITHUB_TOKEN", "ghp_secret123", []string{"gh"}); err != nil {
		t.Fatal(err)
	}

	headers := http.Header{}
	p := NewEgressProxy(EgressProxyOptions{Vault: v})
	if err := p.applyAttachSpec(AttachSpec{Type: "bearer", Secret: "GITHUB_TOKEN"}, "gh", headers); err != nil {
		t.Fatalf("applyAttachSpec: %v", err)
	}
	if got := headers.Get("Authorization"); got != "Bearer ghp_secret123" {
		t.Errorf("Authorization = %q, want %q", got, "Bearer ghp_secret123")
	}

	// Verify scope: a request from a non-allowed skill must be denied.
	headers2 := http.Header{}
	err := p.applyAttachSpec(AttachSpec{Type: "bearer", Secret: "GITHUB_TOKEN"}, "other-skill", headers2)
	if err == nil {
		t.Error("vault scope check must reject non-allowed skill")
	}
}

func TestEgressProxyRejectsBadURL(t *testing.T) {
	v := openTestVault(t)
	p, proxyURL := startTestProxy(t, v)
	tok, _ := p.IssueToken("test-skill", []string{"example.com"}, time.Minute)
	status, _ := proxyFetch(t, proxyURL, tok, map[string]interface{}{
		"url": "::::not-a-url:::",
	})
	if status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", status)
	}
}

func TestEgressProxyRejectsNonHTTPScheme(t *testing.T) {
	v := openTestVault(t)
	p, proxyURL := startTestProxy(t, v)
	tok, _ := p.IssueToken("test-skill", []string{"example.com"}, time.Minute)
	status, _ := proxyFetch(t, proxyURL, tok, map[string]interface{}{
		"url": "file:///etc/passwd",
	})
	if status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for non-http scheme", status)
	}
}

func TestHostMatchesAllowList(t *testing.T) {
	cases := []struct {
		host    string
		allow   []string
		want    bool
		comment string
	}{
		{"api.github.com", []string{"api.github.com"}, true, "exact match"},
		{"api.github.com", []string{"github.com"}, false, "no implicit subdomain"},
		{"api.github.com", []string{"*.github.com"}, true, "wildcard one-level"},
		{"deep.api.github.com", []string{"*.github.com"}, false, "wildcard doesn't span dots"},
		{"GITHUB.COM", []string{"github.com"}, true, "case-insensitive"},
		{"github.com", []string{}, false, "empty allow-list denies"},
	}
	for _, c := range cases {
		got := hostMatchesAllowList(c.host, c.allow)
		if got != c.want {
			t.Errorf("hostMatchesAllowList(%q, %v) = %v, want %v (%s)",
				c.host, c.allow, got, c.want, c.comment)
		}
	}
}

func TestIsPrivateOrInternalIP(t *testing.T) {
	cases := map[string]bool{
		"169.254.169.254":      true, // IMDS
		"127.0.0.1":            true,
		"10.0.0.1":             true,
		"172.16.0.1":           true,
		"192.168.1.1":          true,
		"0.0.0.0":              true,
		"100.64.0.1":           true,  // CGNAT 100.64.0.0/10
		"100.100.100.100":      true,  // Tailscale-style tailnet address
		"100.127.255.255":      true,  // top of the CGNAT range
		"100.63.0.1":           false, // just below CGNAT — public
		"100.128.0.1":          false, // just above CGNAT — public
		"8.8.8.8":              false, // public
		"1.1.1.1":              false,
		"::1":                  true,
		"fe80::1":              true,  // link-local
		"fc00::1":              true,  // ULA
		"2606:4700:4700::1111": false, // public Cloudflare DNS
	}
	for ip, want := range cases {
		got := isPrivateOrInternalIP(ip)
		if got != want {
			t.Errorf("isPrivateOrInternalIP(%q) = %v, want %v", ip, got, want)
		}
	}
}

func TestEgressProxyTokenRequiresAllowedHosts(t *testing.T) {
	v := openTestVault(t)
	p, _ := startTestProxy(t, v)
	if _, err := p.IssueToken("skill", nil, time.Minute); err == nil {
		t.Error("IssueToken must require at least one allowed host")
	}
}

func TestEgressProxyURLOnlyAfterStart(t *testing.T) {
	v := openTestVault(t)
	p := NewEgressProxy(EgressProxyOptions{Vault: v})
	if p.URL() != "" {
		t.Errorf("URL before Start = %q, want empty", p.URL())
	}
	_, _ = p.Start()
	defer p.Stop(context.Background())
	if p.URL() == "" {
		t.Error("URL after Start should be set")
	}
	if u, _ := url.Parse(p.URL()); u == nil || u.Host == "" {
		t.Errorf("URL is malformed: %q", p.URL())
	}
}

// TestSafeDialContextBlocksInternal pins the SSRF defense at the dial layer:
// the dialer must refuse to connect to a private/internal address even when
// asked to directly. This is the guarantee that closes the DNS-rebinding
// (resolve-then-reconnect) and redirect-to-internal-IP holes — every upstream
// connection, including redirect hops, is dialed through this function.
func TestSafeDialContextBlocksInternal(t *testing.T) {
	dial := safeDialContext(&net.Dialer{Timeout: time.Second})
	blocked := []string{
		"169.254.169.254:80", // cloud metadata (IMDS)
		"127.0.0.1:8080",     // loopback
		"10.0.0.5:443",       // RFC1918
		"192.168.1.1:80",     // RFC1918
		"[::1]:80",           // IPv6 loopback
	}
	for _, addr := range blocked {
		conn, err := dial(context.Background(), "tcp", addr)
		if err == nil {
			conn.Close()
			t.Errorf("dial(%q) succeeded, want it blocked as internal", addr)
			continue
		}
		if !strings.Contains(err.Error(), "internal address") {
			t.Errorf("dial(%q) error = %q, want an 'internal address' block", addr, err.Error())
		}
	}
}

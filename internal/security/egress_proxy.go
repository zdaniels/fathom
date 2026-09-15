package security

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// EgressProxy is the localhost HTTP server that mediates outbound HTTPS calls
// from sandboxed skill subprocesses. The subprocess can't reach the internet
// directly — it can only POST to /fetch on this proxy with a valid token,
// and the proxy:
//
//  1. Validates the per-invocation token (bound to a skill name + allow-list).
//  2. Parses the requested URL.
//  3. DNS-resolves the hostname and rejects any address in the private /
//     loopback / link-local block-list (defeats DNS rebinding).
//  4. Checks the resolved hostname against the per-token allow-list.
//  5. Optionally attaches credentials via vault lookup (bearer / header /
//     google-oauth refresh) — the skill never sees the raw secret.
//  6. Forwards the request and returns status + headers + body to the skill.
//
// The skill's "I want auth attached" model is expressed by the attach[] array
// in the JSON body — the skill describes *what kind* of credential it needs,
// not *what* the credential is. The proxy is the one that knows.
type EgressProxy struct {
	vault  *SecretsVault
	server *http.Server
	host   string
	port   int
	url    string
	mu     sync.Mutex
	tokens map[string]tokenBinding
	caches map[string]cachedAccessToken // for google-oauth refresh exchange
}

type tokenBinding struct {
	skillName    string
	allowedHosts []string
	expiresAt    time.Time
}

type cachedAccessToken struct {
	value     string
	expiresAt time.Time
}

// EgressProxyOptions configures a new proxy. Host defaults to 127.0.0.1
// (NEVER expose to the network); port 0 picks a free port.
type EgressProxyOptions struct {
	Vault *SecretsVault
	Host  string
	Port  int
}

// AttachSpec describes a single credential-attach the proxy should perform.
// The skill ships these in the /fetch request body; the proxy applies them
// to the upstream request in order.
type AttachSpec struct {
	Type string `json:"type"` // "bearer" | "header" | "google-oauth"

	// "bearer" / "header" / "google-oauth" all use Secret. For "header" the
	// secret value can be wrapped in a template ("Bot ${secret}", etc.).
	Secret string `json:"secret,omitempty"`

	// "header" only.
	Name     string `json:"name,omitempty"`
	Template string `json:"template,omitempty"`

	// "google-oauth" only.
	RefreshTokenSecret string `json:"refreshTokenSecret,omitempty"`
	ClientIDSecret     string `json:"clientIdSecret,omitempty"`
	ClientSecretSecret string `json:"clientSecretSecret,omitempty"`
}

type fetchRequest struct {
	URL     string            `json:"url"`
	Method  string            `json:"method"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body"`
	Attach  []AttachSpec      `json:"attach"`
}

type fetchResponse struct {
	Status     int               `json:"status"`
	StatusText string            `json:"statusText"`
	Headers    map[string]string `json:"headers"`
	Body       string            `json:"body"`
}

// NewEgressProxy constructs a proxy bound to opts.Host:opts.Port. Start must
// be called before any token issuance.
func NewEgressProxy(opts EgressProxyOptions) *EgressProxy {
	host := opts.Host
	if host == "" {
		host = "127.0.0.1"
	}
	return &EgressProxy{
		vault:  opts.Vault,
		host:   host,
		port:   opts.Port,
		tokens: make(map[string]tokenBinding),
		caches: make(map[string]cachedAccessToken),
	}
}

// Start binds the listener and starts serving /fetch in a background
// goroutine. Returns the URL the proxy is reachable at.
func (p *EgressProxy) Start() (string, error) {
	if p.server != nil {
		return "", errors.New("egress proxy already started")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/fetch", p.handleFetch)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"Not found. Only POST /fetch is exposed."}`, http.StatusNotFound)
	})

	ln, err := net.Listen("tcp", fmt.Sprintf("%s:%d", p.host, p.port))
	if err != nil {
		return "", err
	}
	addr := ln.Addr().(*net.TCPAddr)
	p.port = addr.Port
	p.url = fmt.Sprintf("http://%s:%d", p.host, p.port)

	// Hold a local pointer to the server for the goroutine — Stop() nils
	// p.server, and without the local capture the goroutine races with a
	// fast-followed Stop and dereferences nil.
	srv := &http.Server{
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Second,
	}
	p.server = srv
	go func() { _ = srv.Serve(ln) }()
	logger.Info("egress proxy listening", "url", p.url)
	return p.url, nil
}

// Stop closes the proxy listener. Safe to call from shutdown handlers.
func (p *EgressProxy) Stop(ctx context.Context) error {
	if p.server == nil {
		return nil
	}
	err := p.server.Shutdown(ctx)
	p.server = nil
	return err
}

// URL returns the http://host:port the proxy is reachable at. Empty before Start.
func (p *EgressProxy) URL() string { return p.url }

// IssueToken creates a per-invocation token bound to a skill + an allow-list
// of upstream hostname patterns. The subprocess receives this token via env;
// the proxy verifies it on every /fetch and uses skillName to scope vault
// lookups. allowedHosts MUST be non-empty — empty would mean "anywhere",
// which defeats the SSRF defense.
func (p *EgressProxy) IssueToken(skillName string, allowedHosts []string, ttl time.Duration) (string, error) {
	if skillName == "" {
		return "", errors.New("skillName required")
	}
	if len(allowedHosts) == 0 {
		return "", errors.New("at least one allowed host pattern required")
	}
	if ttl == 0 {
		ttl = 5 * time.Minute
	}
	tok, err := GenerateToken()
	if err != nil {
		return "", err
	}
	p.mu.Lock()
	p.tokens[tok] = tokenBinding{
		skillName:    skillName,
		allowedHosts: allowedHosts,
		expiresAt:    time.Now().Add(ttl),
	}
	p.mu.Unlock()
	return tok, nil
}

// RevokeToken invalidates a token immediately. Caller should do this after
// the skill subprocess exits to close the window.
func (p *EgressProxy) RevokeToken(token string) {
	p.mu.Lock()
	delete(p.tokens, token)
	p.mu.Unlock()
}

// handleFetch is the only mounted route. Pulls token + body, validates,
// applies attach specs, forwards upstream, returns wrapped response.
func (p *EgressProxy) handleFetch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, http.StatusMethodNotAllowed, "Only POST /fetch is exposed.")
		return
	}
	token := r.Header.Get("X-Fantazm-Token")
	p.mu.Lock()
	binding, ok := p.tokens[token]
	if ok && time.Now().After(binding.expiresAt) {
		delete(p.tokens, token)
		ok = false
	}
	p.mu.Unlock()
	if !ok {
		jsonError(w, http.StatusUnauthorized, "Invalid or expired proxy token.")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 8*1024*1024))
	if err != nil {
		jsonError(w, http.StatusBadRequest, "Request body unreadable: "+err.Error())
		return
	}
	var req fetchRequest
	if err := json.Unmarshal(body, &req); err != nil {
		jsonError(w, http.StatusBadRequest, "Invalid JSON: "+err.Error())
		return
	}

	target, err := url.Parse(req.URL)
	if err != nil {
		jsonError(w, http.StatusBadRequest, "Invalid URL: "+req.URL)
		return
	}
	if target.Scheme != "https" && target.Scheme != "http" {
		jsonError(w, http.StatusBadRequest, "Only http(s) URLs are allowed; got scheme "+target.Scheme)
		return
	}
	if !hostMatchesAllowList(target.Hostname(), binding.allowedHosts) {
		logger.Warn("host blocked by allow-list",
			"host", target.Hostname(),
			"skill", binding.skillName,
			"allowed", binding.allowedHosts,
		)
		jsonError(w, http.StatusForbidden,
			fmt.Sprintf("Host %q is not in this skill's allow-list.", target.Hostname()))
		return
	}
	// DNS-resolve to defeat DNS rebinding: a hostname that resolves to 169.254.169.254
	// (IMDS) still gets blocked here even if the hostname itself looked benign.
	addrs, err := net.LookupHost(target.Hostname())
	if err != nil {
		jsonError(w, http.StatusBadGateway, "DNS lookup failed for "+target.Hostname()+": "+err.Error())
		return
	}
	for _, a := range addrs {
		if isPrivateOrInternalIP(a) {
			logger.Warn("host resolves to blocked IP",
				"host", target.Hostname(), "address", a, "skill", binding.skillName)
			jsonError(w, http.StatusForbidden,
				fmt.Sprintf("Host %q resolves to a blocked address (%s).", target.Hostname(), a))
			return
		}
	}

	// Build outbound headers. Skill-provided headers go first; attach specs
	// can overwrite them. This matches the TS semantics.
	outHeaders := http.Header{}
	for k, v := range req.Headers {
		outHeaders.Set(k, v)
	}
	for _, spec := range req.Attach {
		if err := p.applyAttachSpec(spec, binding.skillName, outHeaders); err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}

	method := req.Method
	if method == "" {
		method = http.MethodGet
	}
	httpReq, err := http.NewRequestWithContext(r.Context(), method, req.URL, strings.NewReader(req.Body))
	if err != nil {
		jsonError(w, http.StatusBadRequest, "Failed to build upstream request: "+err.Error())
		return
	}
	httpReq.Header = outHeaders

	// The pre-flight allow-list + LookupHost check above is necessary but NOT
	// sufficient: a default http.Client re-resolves DNS at dial time (so a
	// rebinding attacker can flip the answer between the check and the
	// connect) and silently follows redirects (so an allowed host can 302 us
	// to 169.254.169.254). We close both holes here:
	//   - safeDialContext re-validates the IP it is ABOUT TO connect to and
	//     pins the dial to that vetted address — the resolve used for the
	//     check IS the resolve used for the connection, which is what
	//     actually defeats DNS rebinding.
	//   - CheckRedirect re-applies the scheme + allow-list gate on every hop,
	//     and each hop still dials through safeDialContext, so a redirect to
	//     an internal IP is refused at connect time too.
	transport := &http.Transport{
		DialContext:           safeDialContext(&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}),
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		IdleConnTimeout:       30 * time.Second,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Timeout:   60 * time.Second,
		Transport: transport,
		CheckRedirect: func(redirect *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("stopped after 10 redirects")
			}
			if redirect.URL.Scheme != "https" && redirect.URL.Scheme != "http" {
				return fmt.Errorf("redirect to disallowed scheme %q blocked", redirect.URL.Scheme)
			}
			if !hostMatchesAllowList(redirect.URL.Hostname(), binding.allowedHosts) {
				return fmt.Errorf("redirect to host %q outside this skill's allow-list blocked", redirect.URL.Hostname())
			}
			return nil
		},
	}
	upstream, err := client.Do(httpReq)
	if err != nil {
		jsonError(w, http.StatusBadGateway, "Upstream fetch failed: "+err.Error())
		return
	}
	defer upstream.Body.Close()

	// Cap upstream body to ~8MB to bound memory + avoid skill-driven OOM.
	respBody, err := io.ReadAll(io.LimitReader(upstream.Body, 8*1024*1024))
	if err != nil {
		jsonError(w, http.StatusBadGateway, "Upstream read failed: "+err.Error())
		return
	}

	resp := fetchResponse{
		Status:     upstream.StatusCode,
		StatusText: upstream.Status,
		Headers:    flattenHeaders(upstream.Header),
		Body:       string(respBody),
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

// applyAttachSpec mutates outHeaders to add the requested credential.
// All vault.Get calls pass skillName, so the per-skill scope check fires
// at the vault layer.
func (p *EgressProxy) applyAttachSpec(spec AttachSpec, skillName string, h http.Header) error {
	switch spec.Type {
	case "bearer":
		v, err := p.vault.Get(spec.Secret, skillName)
		if err != nil {
			return err
		}
		h.Set("Authorization", "Bearer "+v)
		return nil

	case "header":
		v, err := p.vault.Get(spec.Secret, skillName)
		if err != nil {
			return err
		}
		value := v
		if spec.Template != "" {
			value = strings.ReplaceAll(spec.Template, "${secret}", v)
		}
		h.Set(spec.Name, value)
		return nil

	case "google-oauth":
		token, err := p.googleAccessToken(spec, skillName)
		if err != nil {
			return err
		}
		h.Set("Authorization", "Bearer "+token)
		return nil
	}
	return fmt.Errorf("unknown attach type: %q", spec.Type)
}

// googleAccessToken handles the OAuth refresh dance. Cached per (skill,
// refresh-token-name) with the access token's expiry honoured.
func (p *EgressProxy) googleAccessToken(spec AttachSpec, skillName string) (string, error) {
	cacheKey := skillName + "::" + spec.RefreshTokenSecret
	p.mu.Lock()
	cached, ok := p.caches[cacheKey]
	p.mu.Unlock()
	if ok && time.Now().Before(cached.expiresAt) {
		return cached.value, nil
	}
	refresh, err := p.vault.Get(spec.RefreshTokenSecret, skillName)
	if err != nil {
		return "", err
	}
	clientID, err := p.vault.Get(orDefault(spec.ClientIDSecret, "GOOGLE_CLIENT_ID"), skillName)
	if err != nil {
		return "", err
	}
	clientSecret, err := p.vault.Get(orDefault(spec.ClientSecretSecret, "GOOGLE_CLIENT_SECRET"), skillName)
	if err != nil {
		return "", err
	}

	form := url.Values{}
	form.Set("client_id", clientID)
	form.Set("client_secret", clientSecret)
	form.Set("refresh_token", refresh)
	form.Set("grant_type", "refresh_token")

	httpClient := &http.Client{Timeout: 30 * time.Second}
	resp, err := httpClient.Post(
		"https://oauth2.googleapis.com/token",
		"application/x-www-form-urlencoded",
		strings.NewReader(form.Encode()),
	)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("google oauth refresh failed (%d): %s", resp.StatusCode, string(body))
	}
	var tok struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tok); err != nil {
		return "", err
	}
	if tok.AccessToken == "" {
		return "", errors.New("google oauth response missing access_token")
	}
	// Conservative cache TTL: token's stated expiry minus 30s, or 5min if
	// expires_in is missing/zero — defeating NaN-style cache poisoning.
	expiresIn := tok.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 300
	}
	p.mu.Lock()
	p.caches[cacheKey] = cachedAccessToken{
		value:     tok.AccessToken,
		expiresAt: time.Now().Add(time.Duration(expiresIn-30) * time.Second),
	}
	p.mu.Unlock()
	return tok.AccessToken, nil
}

func orDefault(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// hostMatchesAllowList supports exact-match and a single leading "*." wildcard
// matching one subdomain level.
func hostMatchesAllowList(hostname string, allow []string) bool {
	h := strings.ToLower(hostname)
	for _, raw := range allow {
		p := strings.ToLower(strings.TrimSpace(raw))
		if p == "" {
			continue
		}
		if p == h {
			return true
		}
		if strings.HasPrefix(p, "*.") {
			suffix := p[1:] // ".example.com"
			if strings.HasSuffix(h, suffix) && !strings.Contains(h[:len(h)-len(suffix)], ".") {
				return true
			}
		}
	}
	return false
}

// safeDialContext returns a DialContext that resolves the target host itself,
// refuses to connect to any private/internal address, and then dials the
// exact vetted IP. Pinning the connection to the address we validated is what
// makes the SSRF defense sound: the IP that passes isPrivateOrInternalIP is
// the IP the socket actually connects to, so DNS rebinding (a second resolve
// returning a different answer) cannot slip an internal address past us, and
// redirects — which dial through this same function — are covered too.
//
// TLS still uses the hostname from the request URL for SNI + certificate
// verification (http.Transport derives ServerName from the request, not from
// the dialed address), so pinning to an IP does not weaken HTTPS.
func safeDialContext(base *net.Dialer) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		// Refuse outright if ANY resolved address is internal — mirrors the
		// handler's pre-flight semantics and avoids racing a multi-record
		// rebind that mixes one public and one private answer.
		for _, ipAddr := range ips {
			if isPrivateOrInternalIP(ipAddr.IP.String()) {
				return nil, fmt.Errorf("blocked connection to internal address %s (host %q)", ipAddr.IP, host)
			}
		}
		var lastErr error
		for _, ipAddr := range ips {
			conn, derr := base.DialContext(ctx, network, net.JoinHostPort(ipAddr.IP.String(), port))
			if derr == nil {
				return conn, nil
			}
			lastErr = derr
		}
		if lastErr == nil {
			lastErr = fmt.Errorf("no usable address for host %q", host)
		}
		return nil, lastErr
	}
}

// isPrivateOrInternalIP returns true for any address that should never be
// reachable through the proxy: loopback, link-local (incl. AWS/GCP metadata
// 169.254.169.254), RFC1918, IPv6 ULA, multicast.
//
// Hostnames resolving to any such address are rejected — this is the core
// SSRF defense, and because we check the resolved IP (not the input string),
// DNS rebinding doesn't help an attacker.
func isPrivateOrInternalIP(address string) bool {
	ip := net.ParseIP(address)
	if ip == nil {
		// Non-IP got through to here — refuse out of caution.
		return true
	}
	if ip4 := ip.To4(); ip4 != nil {
		return isPrivateOrInternalIPv4(ip4)
	}
	return isPrivateOrInternalIPv6(ip)
}

func isPrivateOrInternalIPv4(ip net.IP) bool {
	a, b := ip[0], ip[1]
	switch {
	case a == 0:
		return true // 0.0.0.0/8
	case a == 10:
		return true // 10.0.0.0/8
	case a == 127:
		return true // loopback
	case a == 100 && b >= 64 && b <= 127:
		return true // 100.64.0.0/10 CGNAT — also Tailscale's tailnet range
	case a == 169 && b == 254:
		return true // link-local incl. IMDS
	case a == 172 && b >= 16 && b <= 31:
		return true // 172.16.0.0/12
	case a == 192 && b == 168:
		return true // 192.168.0.0/16
	case a == 192 && b == 0:
		return true
	case a == 198 && (b == 18 || b == 19):
		return true // benchmarking
	case a >= 224:
		return true // multicast + reserved
	}
	return false
}

func isPrivateOrInternalIPv6(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return true
	}
	// ULA fc00::/7
	if len(ip) >= 1 && (ip[0]&0xfe) == 0xfc {
		return true
	}
	// IPv4-mapped (::ffff:a.b.c.d) — recurse on the embedded v4.
	if ip4 := ip.To4(); ip4 != nil {
		return isPrivateOrInternalIPv4(ip4)
	}
	return false
}

func jsonError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func flattenHeaders(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	for k, vs := range h {
		if strings.EqualFold(k, "Content-Length") {
			continue
		}
		out[k] = strings.Join(vs, ", ")
	}
	return out
}

package enterprise

import (
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// This file holds the defense-in-depth layers that sit *around* the RBAC
// checks in admin.go — IP allow-listing, per-IP rate limiting, and the
// step-up (sudo-style) re-auth for destructive mutations. RBAC decides who
// may call admin; these hold the line even if a role is mis-granted or a
// token leaks.

// clientIP extracts the source IP (no port) from an http.Request's
// RemoteAddr. Returns "" when it can't be parsed — callers treat that as
// "unclassifiable" and fail safe per their own policy.
func clientIP(r *http.Request) string {
	host := r.RemoteAddr
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if net.ParseIP(host) == nil {
		return ""
	}
	return host
}

// parseCIDRs turns a list of CIDR or bare-IP strings into matchers. A bare
// IP (no "/") is treated as a /32 or /128. Unparseable entries are skipped.
func parseCIDRs(entries []string) ([]*net.IPNet, error) {
	var out []*net.IPNet
	for _, e := range entries {
		e = strings.TrimSpace(e)
		if e == "" {
			return nil, fmt.Errorf("empty admin CIDR entry")
		}
		if !hasSlash(e) {
			if ip := net.ParseIP(e); ip != nil {
				bits := 32
				if ip.To4() == nil {
					bits = 128
				}
				out = append(out, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
				continue
			}
		}
		_, n, err := net.ParseCIDR(e)
		if err != nil {
			return nil, fmt.Errorf("invalid admin CIDR %q: %w", e, err)
		}
		out = append(out, n)
	}
	return out, nil
}

func hasSlash(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == '/' {
			return true
		}
	}
	return false
}

// ipAllowed reports whether ip falls inside any of the allowed nets. When
// the allow-list is empty the caller decides the default (we treat empty as
// "no restriction" at the call site, so this is only consulted when set).
func ipAllowed(nets []*net.IPNet, ip string) bool {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return false // can't classify a restricted surface → deny
	}
	for _, n := range nets {
		if n.Contains(parsed) {
			return true
		}
	}
	return false
}

// adminRateLimiter is a fixed-window per-IP limiter, same shape as the
// gateway's pairing limiter. Foreclosing brute force / abuse is the goal, so
// the exact window distribution matters less than the ceiling.
type adminRateLimiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	state  map[string]*rlBucket
}

type rlBucket struct {
	count     int
	windowEnd time.Time
}

func newAdminRateLimiter(maxPerMin int) *adminRateLimiter {
	return &adminRateLimiter{max: maxPerMin, window: time.Minute, state: map[string]*rlBucket{}}
}

func (l *adminRateLimiter) allow(ip string) bool {
	if l == nil || l.max <= 0 {
		return true // disabled
	}
	if ip == "" {
		return true // unclassifiable → don't punish
	}
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.state[ip]
	if !ok || now.After(b.windowEnd) {
		l.state[ip] = &rlBucket{count: 1, windowEnd: now.Add(l.window)}
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

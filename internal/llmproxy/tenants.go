package llmproxy

import (
	"fmt"
	"path"
	"sync"
	"time"
)

// Tenant is a resolved TenantConfig: bearer tokens swapped for their
// literal values, ready for fast O(1) lookup.
type Tenant struct {
	ID            string
	AllowedModels []string // globs, e.g. "anthropic/claude-*"
	Quotas        Quotas
}

// Usage is the rolling counters per tenant. Held separate from Tenant
// so config reloads (a future addition) don't reset live counters.
type Usage struct {
	mu                sync.Mutex
	tokensToday       int64
	dayResetAt        time.Time
	requestsInLastMin []time.Time
}

// TenantStore answers two questions per request: (1) given a bearer
// token, who is this; (2) is this tenant allowed to call this model
// right now (model glob + quota).
type TenantStore struct {
	mu      sync.RWMutex
	byToken map[string]*Tenant // bearer → tenant
	byID    map[string]*Tenant
	usage   map[string]*Usage
	now     func() time.Time // injectable for tests
}

// NewTenantStore resolves every tenant's bearer tokens and builds the
// lookup index. Bearer tokens must be unique across the whole config
// (collisions would be a security bug — two tenants sharing a key).
func NewTenantStore(cfgs []TenantConfig, resolve SecretResolver) (*TenantStore, error) {
	s := &TenantStore{
		byToken: map[string]*Tenant{},
		byID:    map[string]*Tenant{},
		usage:   map[string]*Usage{},
		now:     time.Now,
	}
	for _, c := range cfgs {
		t := &Tenant{ID: c.ID, AllowedModels: c.AllowedModels, Quotas: c.Quotas}
		s.byID[c.ID] = t
		s.usage[c.ID] = &Usage{}
		for _, ref := range c.BearerTokens {
			v, err := resolve(ref)
			if err != nil {
				return nil, fmt.Errorf("tenant %q: %w", c.ID, err)
			}
			if v == "" {
				return nil, fmt.Errorf("tenant %q: bearer token resolved to empty", c.ID)
			}
			if _, dup := s.byToken[v]; dup {
				return nil, fmt.Errorf("tenant %q: bearer token collides with another tenant — tokens MUST be unique", c.ID)
			}
			s.byToken[v] = t
		}
	}
	return s, nil
}

// Authenticate returns the tenant for a bearer token, or an error if
// unknown. Constant-time comparison isn't necessary here: the map
// lookup itself is O(1) and timing-revealing only at the bucket level,
// and bearer tokens are high-entropy.
func (s *TenantStore) Authenticate(bearer string) (*Tenant, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.byToken[bearer]
	if !ok {
		return nil, fmt.Errorf("unknown bearer token")
	}
	return t, nil
}

// Allows checks the model glob list. The model is matched as
// "<provider>/<model>", which is the same string the client sent.
// Globs use path.Match semantics (so "*" does NOT cross the "/" — write
// "anthropic/*" not just "*"). The literal "*" entry is a documented
// shorthand for "any model", useful for an admin-tier tenant.
func (s *TenantStore) Allows(t *Tenant, model string) bool {
	if len(t.AllowedModels) == 0 {
		return false // empty allowlist denies — explicit is safer than implicit
	}
	for _, glob := range t.AllowedModels {
		if glob == "*" {
			return true
		}
		if matched, _ := path.Match(glob, model); matched {
			return true
		}
	}
	return false
}

// ReserveQuota checks the rate-limit + daily-token cap. Returns an error
// if the request would breach either. Does NOT yet record token usage —
// see RecordTokens for that. Two-phase so the gateway can reject
// pre-flight without holding the upstream call open.
func (s *TenantStore) ReserveQuota(t *Tenant) error {
	u := s.usage[t.ID]
	if u == nil {
		return nil
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	now := s.now()

	// Daily reset: rolls at UTC midnight. Cheap; we read+compare on
	// every request which is fine at gateway scale.
	if !sameDay(now, u.dayResetAt) {
		u.tokensToday = 0
		u.dayResetAt = startOfDay(now)
	}

	if t.Quotas.TokensPerDay > 0 && u.tokensToday >= t.Quotas.TokensPerDay {
		return fmt.Errorf("daily token quota exceeded for tenant %q", t.ID)
	}

	if t.Quotas.RequestsPerMinute > 0 {
		cutoff := now.Add(-1 * time.Minute)
		kept := u.requestsInLastMin[:0]
		for _, ts := range u.requestsInLastMin {
			if ts.After(cutoff) {
				kept = append(kept, ts)
			}
		}
		u.requestsInLastMin = kept
		if len(u.requestsInLastMin) >= t.Quotas.RequestsPerMinute {
			return fmt.Errorf("requests-per-minute quota exceeded for tenant %q", t.ID)
		}
		u.requestsInLastMin = append(u.requestsInLastMin, now)
	}
	return nil
}

// RecordTokens advances the daily counter after a successful call. The
// counts come from the upstream's usage field (parsed by the server
// after the response). Providers that don't return usage will under-
// count; tenants set TokensPerDay to 0 (unlimited) to opt out of token
// quotas in those cases.
func (s *TenantStore) RecordTokens(t *Tenant, prompt, completion int) {
	u := s.usage[t.ID]
	if u == nil {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	u.tokensToday += int64(prompt + completion)
}

// Snapshot returns a copy of usage counters for admin reporting.
func (s *TenantStore) Snapshot() map[string]UsageSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := map[string]UsageSnapshot{}
	for id, u := range s.usage {
		u.mu.Lock()
		out[id] = UsageSnapshot{
			TokensToday:     u.tokensToday,
			RequestsLastMin: len(u.requestsInLastMin),
			DayResetAt:      u.dayResetAt,
		}
		u.mu.Unlock()
	}
	return out
}

// UsageSnapshot is the read-only view exposed to /admin/usage.
type UsageSnapshot struct {
	TokensToday     int64     `json:"tokensToday"`
	RequestsLastMin int       `json:"requestsLastMin"`
	DayResetAt      time.Time `json:"dayResetAt"`
}

func sameDay(a, b time.Time) bool {
	ay, am, ad := a.UTC().Date()
	by, bm, bd := b.UTC().Date()
	return ay == by && am == bm && ad == bd
}

func startOfDay(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

package core

import (
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/zdaniels/fathom/internal/security"
)

// Tenant is one isolated workspace. Each has its own policy + per-tenant
// LLM config, plus user quotas.
type Tenant struct {
	ID        string       `json:"id"`
	Name      string       `json:"name"`
	Slug      string       `json:"slug"`
	CreatedAt time.Time    `json:"createdAt"`
	Config    TenantConfig `json:"config"`
	Active    bool         `json:"active"`
}

type TenantConfig struct {
	MaxUsers         int                    `json:"maxUsers"`
	MaxSessions      int                    `json:"maxSessions"`
	AllowedChannels  []string               `json:"allowedChannels"`
	CustomPolicyFile string                 `json:"customPolicyFile,omitempty"`
	LLM              map[string]interface{} `json:"llmConfig,omitempty"`
}

var defaultTenantConfig = TenantConfig{
	MaxUsers:        50,
	MaxSessions:     200,
	AllowedChannels: []string{"webchat", "cli"},
}

// TenantManager owns the in-memory tenant set.
type TenantManager struct {
	db      *sql.DB
	mu      sync.RWMutex
	tenants map[string]Tenant
}

// NewTenantManager returns an empty manager.
func NewTenantManager() *TenantManager {
	return &TenantManager{tenants: make(map[string]Tenant)}
}

// Create registers a new tenant. Returns an error if the slug collides.
func (t *TenantManager) Create(name, slug string, cfg *TenantConfig) (Tenant, error) {
	if strings.TrimSpace(name) == "" || !regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`).MatchString(slug) {
		return Tenant{}, errors.New("tenant requires a name and lowercase alphanumeric slug")
	}
	if cfg != nil && (cfg.MaxUsers < 0 || cfg.MaxSessions < 0) {
		return Tenant{}, errors.New("tenant quotas cannot be negative")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, existing := range t.tenants {
		if existing.Slug == slug {
			return Tenant{}, errors.New("tenant slug already exists")
		}
	}
	tt := Tenant{
		ID:        security.GenerateID(),
		Name:      name,
		Slug:      slug,
		CreatedAt: time.Now().UTC(),
		Config:    defaultTenantConfig,
		Active:    true,
	}
	if cfg != nil {
		tt.Config = *cfg
		if tt.Config.MaxUsers == 0 {
			tt.Config.MaxUsers = defaultTenantConfig.MaxUsers
		}
		if tt.Config.MaxSessions == 0 {
			tt.Config.MaxSessions = defaultTenantConfig.MaxSessions
		}
		if len(tt.Config.AllowedChannels) == 0 {
			tt.Config.AllowedChannels = defaultTenantConfig.AllowedChannels
		}
	}
	if t.db != nil {
		b, err := json.Marshal(tt)
		if err != nil {
			return Tenant{}, err
		}
		_, err = t.db.Exec("INSERT INTO tenants VALUES(?,?,?)", tt.ID, tt.Slug, b)
		if err != nil {
			return Tenant{}, err
		}
	} else {
		t.tenants[tt.ID] = tt
	}
	return tt, nil
}

// Get returns a tenant by ID.
func (t *TenantManager) Get(id string) (Tenant, bool) {
	if t.db != nil {
		var b []byte
		var tt Tenant
		err := t.db.QueryRow("SELECT record FROM tenants WHERE id=?", id).Scan(&b)
		if err != nil {
			return tt, false
		}
		err = json.Unmarshal(b, &tt)
		return tt, err == nil
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	tt, ok := t.tenants[id]
	return tt, ok
}

// GetBySlug returns a tenant by slug (nil if not found).
func (t *TenantManager) GetBySlug(slug string) *Tenant {
	if t.db != nil {
		var b []byte
		var tt Tenant
		err := t.db.QueryRow("SELECT record FROM tenants WHERE slug=?", slug).Scan(&b)
		if err != nil || json.Unmarshal(b, &tt) != nil {
			return nil
		}
		return &tt
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	for _, tt := range t.tenants {
		if tt.Slug == slug {
			tt := tt
			return &tt
		}
	}
	return nil
}

// Delete removes a tenant. Returns true if it existed.
func (t *TenantManager) Delete(id string) bool {
	if t.db != nil {
		tx, err := t.db.Begin()
		if err != nil {
			return false
		}
		defer tx.Rollback()
		if _, err = tx.Exec("DELETE FROM roles WHERE tenant_id=?", id); err != nil {
			return false
		}
		res, err := tx.Exec("DELETE FROM tenants WHERE id=?", id)
		if err != nil {
			return false
		}
		n, _ := res.RowsAffected()
		return tx.Commit() == nil && n > 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.tenants[id]; !ok {
		return false
	}
	delete(t.tenants, id)
	return true
}

// List returns all tenants.
func (t *TenantManager) List() []Tenant {
	if t.db != nil {
		out, _ := t.storedTenants()
		return out
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]Tenant, 0, len(t.tenants))
	for _, tt := range t.tenants {
		out = append(out, tt)
	}
	return out
}

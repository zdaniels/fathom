package core

import "testing"

func TestTenantCreateAppliesDefaults(t *testing.T) {
	m := NewTenantManager()
	tt, err := m.Create("Acme", "acme", nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if tt.Config.MaxUsers != defaultTenantConfig.MaxUsers {
		t.Errorf("MaxUsers = %d, want default %d", tt.Config.MaxUsers, defaultTenantConfig.MaxUsers)
	}
	if len(tt.Config.AllowedChannels) == 0 {
		t.Error("AllowedChannels not defaulted")
	}
	if !tt.Active {
		t.Error("Active should be true after create")
	}
}

func TestTenantCreateRejectsDuplicateSlug(t *testing.T) {
	m := NewTenantManager()
	_, err := m.Create("Acme", "acme", nil)
	if err != nil {
		t.Fatalf("Create #1: %v", err)
	}
	_, err = m.Create("Acme2", "acme", nil)
	if err == nil {
		t.Error("duplicate slug should error")
	}
}

func TestTenantCustomConfigFillsMissingDefaults(t *testing.T) {
	m := NewTenantManager()
	cfg := &TenantConfig{MaxUsers: 999} // only one field set
	tt, _ := m.Create("Big", "big", cfg)
	if tt.Config.MaxUsers != 999 {
		t.Error("custom MaxUsers lost")
	}
	if tt.Config.MaxSessions == 0 {
		t.Error("MaxSessions should default when zero")
	}
	if len(tt.Config.AllowedChannels) == 0 {
		t.Error("AllowedChannels should default when empty")
	}
}

func TestTenantGetByID(t *testing.T) {
	m := NewTenantManager()
	tt, _ := m.Create("X", "x", nil)
	got, ok := m.Get(tt.ID)
	if !ok || got.Slug != "x" {
		t.Errorf("Get: %+v ok=%v", got, ok)
	}
	if _, ok := m.Get("nope"); ok {
		t.Error("Get(unknown) should fail")
	}
}

func TestTenantGetBySlug(t *testing.T) {
	m := NewTenantManager()
	_, _ = m.Create("X", "abc", nil)
	if m.GetBySlug("abc") == nil {
		t.Error("GetBySlug missed an existing tenant")
	}
	if m.GetBySlug("nope") != nil {
		t.Error("GetBySlug should be nil for unknown slug")
	}
}

func TestTenantDelete(t *testing.T) {
	m := NewTenantManager()
	tt, _ := m.Create("X", "x", nil)
	if !m.Delete(tt.ID) {
		t.Error("Delete returned false on existing tenant")
	}
	if m.Delete(tt.ID) {
		t.Error("Delete should be idempotent — no-op returns false")
	}
}

func TestTenantList(t *testing.T) {
	m := NewTenantManager()
	_, _ = m.Create("A", "a", nil)
	_, _ = m.Create("B", "b", nil)
	if len(m.List()) != 2 {
		t.Errorf("List len = %d, want 2", len(m.List()))
	}
}

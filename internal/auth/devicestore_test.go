package auth

import (
	"path/filepath"
	"testing"
)

// TestPersistTokenAcrossRestart is the canonical "paired devices
// survive `fathom service restart`" test. Models the actual flow:
//  1. open store, create Manager-with-store, mint a device token
//  2. close the Manager + store (simulates fathom stopping)
//  3. reopen → the same token authenticates → device shows in list
func TestPersistTokenAcrossRestart(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "devices.db")

	// Boot #1.
	store1, err := OpenDeviceStore(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	mgr1, err := NewWithStore(store1)
	if err != nil {
		t.Fatalf("manager: %v", err)
	}
	if mgr1.HasTokens() {
		t.Fatalf("fresh DB shouldn't have tokens")
	}
	token, err := mgr1.CreateDeviceToken("admin", "dev123", "iPhone")
	if err != nil {
		t.Fatalf("CreateDeviceToken: %v", err)
	}
	apiTok, err := mgr1.CreateAPIToken("admin", "initial-setup")
	if err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}
	// Authenticate once so last_seen has a value.
	if _, err := mgr1.Authenticate(token); err != nil {
		t.Fatalf("first Authenticate: %v", err)
	}
	if err := store1.Close(); err != nil {
		t.Fatalf("close store1: %v", err)
	}

	// Boot #2 — fresh in-memory map, but hydrate from disk.
	store2, err := OpenDeviceStore(dbPath)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer store2.Close()
	mgr2, err := NewWithStore(store2)
	if err != nil {
		t.Fatalf("manager2: %v", err)
	}
	if !mgr2.HasTokens() {
		t.Fatalf("hydrated manager should have tokens; got empty map")
	}

	// Same raw token still authenticates against the new in-memory map.
	res, err := mgr2.Authenticate(token)
	if err != nil {
		t.Fatalf("Authenticate after restart: %v", err)
	}
	if res.UserID != "admin" || res.DeviceID != "dev123" || res.Method != "device" {
		t.Errorf("Authenticate after restart returned %+v, want admin/dev123/device", res)
	}

	// The API token survives too.
	apiRes, err := mgr2.Authenticate(apiTok)
	if err != nil {
		t.Fatalf("API token after restart: %v", err)
	}
	if apiRes.UserID != "admin" || apiRes.Method != "token" {
		t.Errorf("API token returned %+v, want admin/token", apiRes)
	}

	// Device shows up in ListDevices.
	devices := mgr2.ListDevices("admin")
	if len(devices) != 1 || devices[0].ID != "dev123" || devices[0].Name != "iPhone" {
		t.Errorf("ListDevices after restart = %+v, want 1 device {dev123, iPhone}", devices)
	}
}

func TestRevokeDevicePersists(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "devices.db")

	store, err := OpenDeviceStore(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	mgr, err := NewWithStore(store)
	if err != nil {
		t.Fatalf("manager: %v", err)
	}
	token, _ := mgr.CreateDeviceToken("admin", "dev-revoke-me", "Test")

	// Revoke it and close.
	if !mgr.RevokeDevice("dev-revoke-me") {
		t.Fatal("RevokeDevice returned false")
	}
	store.Close()

	// Reopen — the token MUST NOT authenticate.
	store2, err := OpenDeviceStore(dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer store2.Close()
	mgr2, err := NewWithStore(store2)
	if err != nil {
		t.Fatalf("manager2: %v", err)
	}
	if _, err := mgr2.Authenticate(token); err == nil {
		t.Error("revoked token authenticated after restart — revoke didn't persist")
	}
}

func TestHydrateBootstrapTokenSkipsReissue(t *testing.T) {
	// Boot #1: simulate first-run — Manager+store, SetupInitialToken
	// mints a bootstrap. Boot #2: same store, HasTokens should be
	// true so SetupInitialToken isn't called again (the gateway boot
	// path keys off this).
	dbPath := filepath.Join(t.TempDir(), "devices.db")

	store1, _ := OpenDeviceStore(dbPath)
	mgr1, _ := NewWithStore(store1)
	if mgr1.HasTokens() {
		t.Fatal("expected fresh manager empty")
	}
	_, _ = mgr1.SetupInitialToken()
	if !mgr1.HasTokens() {
		t.Fatal("HasTokens false after SetupInitialToken")
	}
	store1.Close()

	store2, _ := OpenDeviceStore(dbPath)
	defer store2.Close()
	mgr2, _ := NewWithStore(store2)
	if !mgr2.HasTokens() {
		t.Error("hydrated manager should report HasTokens=true so boot skips re-issuing")
	}
}

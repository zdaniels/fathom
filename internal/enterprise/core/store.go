package core

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

// OpenState opens transactional role and tenant state shared by the managers.
// Callers must close the returned database when the gateway stops.
func OpenState(path string) (*RBACManager, *TenantManager, *sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, nil, nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, nil, nil, err
	}
	f.Close()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, nil, nil, err
	}
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`PRAGMA busy_timeout=5000; PRAGMA journal_mode=WAL;
 CREATE TABLE IF NOT EXISTS tenants (id TEXT PRIMARY KEY, slug TEXT NOT NULL UNIQUE, record BLOB NOT NULL);
 CREATE TABLE IF NOT EXISTS roles (user_id TEXT NOT NULL, tenant_id TEXT NOT NULL, record BLOB NOT NULL, PRIMARY KEY(user_id,tenant_id));`)
	if err != nil {
		db.Close()
		return nil, nil, nil, err
	}
	r, t := NewRBACManager(), NewTenantManager()
	r.db = db
	t.db = db
	return r, t, db, nil
}

func validAssignment(user string, role Role, tenant string) error {
	if strings.TrimSpace(user) == "" || len(user) > 512 || len(tenant) > 128 {
		return fmt.Errorf("invalid user or tenant ID")
	}
	if _, ok := rolePermissions[role]; !ok {
		return fmt.Errorf("unknown role %q", role)
	}
	return nil
}
func (r *RBACManager) saveAssignment(a Assignment) error {
	if a.TenantID != "" {
		var n int
		if err := r.db.QueryRow("SELECT count(*) FROM tenants WHERE id=?", a.TenantID).Scan(&n); err != nil {
			return err
		}
		if n != 1 {
			return fmt.Errorf("unknown tenant %q", a.TenantID)
		}
	}
	b, err := json.Marshal(a)
	if err != nil {
		return err
	}
	_, err = r.db.Exec("INSERT INTO roles VALUES(?,?,?) ON CONFLICT(user_id,tenant_id) DO UPDATE SET record=excluded.record", a.UserID, a.TenantID, b)
	return err
}
func (r *RBACManager) storedAssignments() ([]Assignment, error) {
	rows, err := r.db.Query("SELECT record FROM roles ORDER BY user_id,tenant_id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Assignment{}
	for rows.Next() {
		var b []byte
		var a Assignment
		if err = rows.Scan(&b); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(b, &a); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
func (t *TenantManager) storedTenants() ([]Tenant, error) {
	rows, err := t.db.Query("SELECT record FROM tenants ORDER BY slug")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Tenant{}
	for rows.Next() {
		var b []byte
		var a Tenant
		if err = rows.Scan(&b); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(b, &a); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

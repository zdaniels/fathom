package collab

import (
	"database/sql"
	"errors"
	"sort"
)

// Connection records override startup YAML, including disabled records. Only
// secret references go into SQLite; credential values live in the vault.
type managedConnection struct {
	Config   Connection
	Revision int
	Enabled  bool
	Origin   string
}

func (s *Store) connectionRecords(workspace string) ([]managedConnection, error) {
	rows, err := s.db.Query(`SELECT provider,scope,site,email,token_secret,revision,enabled FROM task_connections WHERE workspace_id=?`, workspace)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []managedConnection{}
	for rows.Next() {
		c := managedConnection{Origin: "board"}
		c.Config.WorkspaceID = workspace
		if err = rows.Scan(&c.Config.Provider, &c.Config.Scope, &c.Config.Site, &c.Config.Email, &c.Config.TokenSecret, &c.Revision, &c.Enabled); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
func (s *Service) connectionRecords(workspace string) ([]managedConnection, error) {
	records := map[string]managedConnection{}
	for _, c := range s.Connections {
		if c.WorkspaceID == workspace {
			records[c.Provider] = managedConnection{Config: c, Enabled: true, Origin: "config"}
		}
	}
	saved, err := s.Store.connectionRecords(workspace)
	if err != nil {
		return nil, err
	}
	for _, c := range saved {
		records[c.Config.Provider] = c
	}
	out := make([]managedConnection, 0, len(records))
	for _, c := range records {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Config.Provider < out[j].Config.Provider })
	return out, nil
}
func (s *Store) saveConnection(c managedConnection, actor string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("UPDATE workspaces SET name=name WHERE id=?", c.Config.WorkspaceID); err != nil {
		return err
	}
	var role string
	if err = tx.QueryRow("SELECT role FROM members WHERE workspace_id=? AND user_id=?", c.Config.WorkspaceID, actor).Scan(&role); err != nil || role != "admin" {
		return ErrForbidden
	}
	var revision int
	err = tx.QueryRow("SELECT revision FROM task_connections WHERE workspace_id=? AND provider=?", c.Config.WorkspaceID, c.Config.Provider).Scan(&revision)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if revision != c.Revision {
		return ErrConflict
	}
	_, err = tx.Exec(`INSERT INTO task_connections(workspace_id,provider,scope,site,email,token_secret,revision,enabled) VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(workspace_id,provider) DO UPDATE SET scope=excluded.scope,site=excluded.site,email=excluded.email,token_secret=excluded.token_secret,revision=excluded.revision,enabled=excluded.enabled`, c.Config.WorkspaceID, c.Config.Provider, c.Config.Scope, c.Config.Site, c.Config.Email, c.Config.TokenSecret, c.Revision+1, c.Enabled)
	if err != nil {
		return err
	}
	action := "connection_saved"
	if !c.Enabled {
		action = "connection_disabled"
	}
	if err = activity(tx, c.Config.WorkspaceID, "", actor, action, c.Config.Provider); err != nil {
		return err
	}
	return tx.Commit()
}

package collab

import (
	"context"
	"errors"
	"net/http"
	"net/mail"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/zdaniels/fathom/internal/security"
)

type connectionView struct {
	Provider        string `json:"provider"`
	Scope           string `json:"scope"`
	Site            string `json:"site,omitempty"`
	Email           string `json:"email,omitempty"`
	Revision        int    `json:"revision"`
	Enabled         bool   `json:"enabled"`
	CredentialSaved bool   `json:"credentialSaved"`
	Origin          string `json:"origin"`
}

func connectionViews(records []managedConnection) []connectionView {
	out := []connectionView{}
	for _, c := range records {
		out = append(out, connectionView{c.Config.Provider, c.Config.Scope, c.Config.Site, c.Config.Email, c.Revision, c.Enabled, c.Config.TokenSecret != "", c.Origin})
	}
	return out
}

var teamID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
var projectKey = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)

func validateConnection(c *Connection) error {
	if len(c.Scope) > 64 || len(c.Site) > 250 {
		return errors.New("connection fields are too long")
	}
	c.Scope = strings.TrimSpace(c.Scope)
	c.Site = strings.TrimRight(strings.TrimSpace(c.Site), "/")
	c.Email = strings.TrimSpace(c.Email)
	switch c.Provider {
	case "linear":
		if !teamID.MatchString(c.Scope) {
			return errors.New("enter the Linear team UUID")
		}
		c.Scope = strings.ToLower(c.Scope)
		c.Site = ""
		c.Email = ""
	case "jira":
		if !projectKey.MatchString(c.Scope) {
			return errors.New("enter the Jira project key, such as TEAM")
		}
		u, err := url.Parse(c.Site)
		if err != nil || u.Scheme != "https" || !strings.HasSuffix(u.Hostname(), ".atlassian.net") || u.Port() != "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" {
			return errors.New("Jira site must be https://your-site.atlassian.net")
		}
		c.Site = "https://" + strings.ToLower(u.Host)
		address, err := mail.ParseAddress(c.Email)
		if err != nil || address.Address != c.Email || len(c.Email) > 254 {
			return errors.New("enter the email address used for the Jira API token")
		}
	default:
		return errors.New("choose Linear or Jira")
	}
	return nil
}
func (s *Service) manageConnections(w http.ResponseWriter, r *http.Request, workspace, user, action string) {
	if s.Store.Role(workspace, user) != "admin" || s.ConnectionGuard == nil {
		fail(w, 403, ErrForbidden)
		return
	}
	if !s.ConnectionGuard(w, r, user) {
		return
	}
	if r.Method == "GET" && action == "" {
		records, err := s.connectionRecords(workspace)
		if err != nil {
			fail(w, 500, errors.New("connections unavailable"))
			return
		}
		respond(w, 200, connectionViews(records))
		return
	}
	if r.Method != "POST" || (action != "save" && action != "check" && action != "disable") {
		fail(w, 405, errors.New("unsupported connection action"))
		return
	}
	var in struct {
		Provider string `json:"provider"`
		Scope    string `json:"scope"`
		Site     string `json:"site"`
		Email    string `json:"email"`
		Token    string `json:"token"`
		Revision int    `json:"revision"`
	}
	if !decode(w, r, &in) {
		return
	}
	if len(in.Token) > 8192 || strings.ContainsAny(in.Token, "\r\n") {
		fail(w, 400, errors.New("invalid API token"))
		return
	}
	if in.Provider != "linear" && in.Provider != "jira" {
		fail(w, 400, errors.New("choose Linear or Jira"))
		return
	}
	// Serialize only connection management; provider checks must not block agent
	// progress, comments, or unrelated board actions behind the service mutex.
	s.connectionMu.Lock()
	defer s.connectionMu.Unlock()
	records, err := s.connectionRecords(workspace)
	if err != nil {
		fail(w, 500, errors.New("connections unavailable"))
		return
	}
	old := managedConnection{}
	for _, c := range records {
		if c.Config.Provider == in.Provider {
			old = c
		}
	}
	if old.Revision != in.Revision {
		fail(w, 409, errors.New("connection changed; close and reopen setup before saving"))
		return
	}
	next := managedConnection{Config: Connection{WorkspaceID: workspace, Provider: in.Provider, Scope: in.Scope, Site: in.Site, Email: in.Email}, Enabled: true, Revision: in.Revision}
	if action == "disable" {
		if old.Config.Provider == "" {
			fail(w, 404, errors.New("connection not found"))
			return
		}
		next = old
		next.Enabled = false
	} else {
		if err = validateConnection(&next.Config); err != nil {
			fail(w, 400, err)
			return
		}
		if in.Token == "" {
			if old.Config.TokenSecret == "" {
				fail(w, 400, errors.New("an API token is required"))
				return
			}
			if in.Provider == "jira" && (next.Config.Site != strings.TrimRight(old.Config.Site, "/") || next.Config.Email != old.Config.Email) {
				fail(w, 400, errors.New("enter a new API token when changing Jira site or email"))
				return
			}
			next.Config.TokenSecret = old.Config.TokenSecret
		}
	}
	if action == "check" {
		lookup := s.Lookup
		if in.Token != "" {
			lookup = func(string) (string, error) { return in.Token, nil }
		}
		connector := &Connector{Config: next.Config, Lookup: lookup, Client: s.ConnectionClient}
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		if err = connector.Check(ctx); err != nil {
			fail(w, 502, err)
			return
		}
		respond(w, 200, map[string]string{"status": "Connection verified: team/project is readable. Publishing permissions are checked when you publish."})
		return
	}
	secretName := ""
	if action == "save" && in.Token != "" {
		if s.SaveSecret == nil {
			fail(w, 503, errors.New("encrypted credential storage is unavailable"))
			return
		}
		secretName = "fathom-board-" + security.GenerateID()
		if err = s.SaveSecret(secretName, in.Token); err != nil {
			if s.DeleteSecret != nil {
				s.DeleteSecret(secretName)
			}
			fail(w, 503, errors.New("could not save credential to the encrypted vault"))
			return
		}
		next.Config.TokenSecret = secretName
	}
	if err = s.Store.saveConnection(next, user); err != nil {
		if secretName != "" && s.DeleteSecret != nil {
			s.DeleteSecret(secretName)
		}
		fail(w, 409, err)
		return
	}
	// Rotate only app-owned keys; startup config credentials may be shared.
	if secretName != "" && old.Origin == "board" && strings.HasPrefix(old.Config.TokenSecret, "fathom-board-") && s.DeleteSecret != nil {
		s.DeleteSecret(old.Config.TokenSecret)
	}
	if s.AuditConnection != nil {
		s.AuditConnection(workspace, user, in.Provider, action)
	}
	s.mu.Lock()
	s.notifyLocked(workspace)
	s.mu.Unlock()
	status := "Connection saved. Changes are active now."
	if action == "disable" {
		status = "Connection disabled. The saved credential is retained."
	}
	respond(w, 200, map[string]string{"status": status})
}

package cli

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/zdaniels/fathom/internal/agent/llm"
	"github.com/zdaniels/fathom/internal/agentfactory"
	"github.com/zdaniels/fathom/internal/collab"
	"github.com/zdaniels/fathom/internal/enterprise"
	"github.com/zdaniels/fathom/internal/enterprise/core"
	"github.com/zdaniels/fathom/internal/gateway"
	"github.com/zdaniels/fathom/pkg/types"
)

func mountBoard(gw *gateway.Gateway, cfg types.Config, result *agentfactory.Result, ent *enterprise.Mounted) (*collab.Service, error) {
	store, err := collab.Open(filepath.Join(cfg.DataDir, "collaboration.db"))
	if err != nil {
		return nil, err
	}
	if err = store.Recover(); err != nil {
		store.Close()
		return nil, err
	}
	lookup := func(name string) (string, error) {
		if result.Vault != nil {
			if v, err := result.Vault.Get(name, ""); err == nil && v != "" {
				return v, nil
			}
		}
		if v := os.Getenv(name); v != "" {
			return v, nil
		}
		return "", errors.New("credential not configured")
	}
	router := result.Router
	if router == nil {
		router, _ = llm.NewRouter(cfg.LLM, lookup)
	}
	service := &collab.Service{Store: store, Runner: &collab.Runner{Router: router}, Lookup: lookup}
	if cfg.Collaboration != nil {
		service.Runner.Image = cfg.Collaboration.Image
		service.Connections = cfg.Collaboration.Connections
	}
	service.Auth = func(r *http.Request) (string, error) {
		h := r.Header.Get("Authorization")
		if !strings.HasPrefix(h, "Bearer ") {
			return "", errors.New("missing token")
		}
		res, err := gw.Auth.Authenticate(strings.TrimPrefix(h, "Bearer "))
		if err != nil {
			return "", err
		}
		return res.UserID, nil
	}
	service.CanExecute = func(user string) bool {
		if ent == nil {
			return true
		}
		return ent.RBAC.HasPermission(user, "", func(p core.Permissions) bool { return p.ExecuteTools })
	}
	gw.SetBoardHandler(service)
	return service, nil
}

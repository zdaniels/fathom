// Package config loads Fathom's configuration from YAML and environment.
//
// The merge order, matching the TypeScript implementation:
//
//  1. Start from types.DefaultConfig().
//  2. Overlay anything found in fathom.config.yaml (cwd or given path).
//  3. Apply env-var overrides — FANTAZM_MODE is the canonical one for
//     container/helm deployments that ship a baseline file but want
//     orchestrator-controlled mode.
//
// Validation clamps obviously-wrong values (session timeout, port range)
// rather than failing — Fathom should boot even when the config has a
// typo, since the gateway exposes /api/v1/health which the operator can
// use to diagnose.
package config

import (
	"fmt"
	"github.com/zdaniels/fathom/internal/brandenv"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/zdaniels/fathom/pkg/types"
	"gopkg.in/yaml.v3"
)

// LoadConfig reads the Fathom config and merges defaults + env overrides.
//
// Discovery order when path is empty:
//
//  1. $FANTAZM_CONFIG (explicit override)
//  2. Upward search from cwd for fathom.config.yaml — like git looking for
//     .git/config. The first hit wins. Useful when you're inside a project
//     subdirectory and want per-project agent context.
//  3. ~/.config/fathom/config.yaml (XDG-style global fallback)
//  4. ~/.fantazm/config.yaml (legacy fallback, kept for users who upgraded
//     from earlier builds)
//
// When path is supplied explicitly, the search is skipped — that path is
// used directly.
func LoadConfig(path string) types.Config {
	cfg := types.DefaultConfig()
	if path == "" {
		path = DiscoverConfig()
	}
	if path == "" {
		// No config found anywhere. Boot on defaults with a quiet info log
		// so the caller (chat / start / doctor) can decide whether to nudge
		// the user.
		slog.Info("no fathom.config.yaml found; using secure defaults")
		applyEnvOverrides(&cfg)
		validate(&cfg)
		return cfg
	}
	if data, err := os.ReadFile(path); err == nil {
		var parsed types.Config
		if err := yaml.Unmarshal(data, &parsed); err != nil {
			slog.Error("failed to parse config file; using defaults", "path", path, "err", err)
		} else {
			cfg = mergeConfig(cfg, parsed)
			slog.Info("configuration loaded", "path", path)
		}
	} else if !os.IsNotExist(err) {
		slog.Error("failed to read config file; using defaults", "path", path, "err", err)
	}

	cfg.ConfigPath, _ = filepath.Abs(path)
	applyEnvOverrides(&cfg)
	if cfg.DataDir != "" && !filepath.IsAbs(cfg.DataDir) {
		cfg.DataDir = filepath.Join(filepath.Dir(cfg.ConfigPath), cfg.DataDir)
	}
	validate(&cfg)
	return cfg
}

// DiscoverConfig implements the discovery order documented on LoadConfig.
// Returns "" if nothing is found. Exported so the chat command can show
// the user which config is active and the doctor command can diagnose.
func DiscoverConfig() string { return DiscoverConfigFrom(cwd()) }

// DiscoverConfigFrom searches without changing the process working directory.
func DiscoverConfigFrom(dir string) string {
	if v := brandenv.Get("FATHOM_CONFIG"); v != "" {
		return v
	}
	// Upward search from cwd.
	d := dir
	for {
		for _, name := range []string{"fathom.config.yaml", "fantazm.config.yaml"} {
			candidate := filepath.Join(d, name)
			if _, err := os.Stat(candidate); err == nil {
				return candidate
			}
		}
		parent := filepath.Dir(d)
		if parent == d {
			break // hit filesystem root
		}
		d = parent
	}
	// XDG global.
	if home, err := os.UserHomeDir(); err == nil {
		for _, p := range []string{
			filepath.Join(home, ".config", "fathom", "config.yaml"),
			filepath.Join(home, ".fathom", "config.yaml"),
			filepath.Join(home, ".config", "fantazm", "config.yaml"),
			filepath.Join(home, ".fantazm", "config.yaml"),
		} {
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
	}
	return ""
}

// GlobalConfigPath returns the canonical "write here for --global" location.
// Used by `fathom init --global`.
func GlobalConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "fathom", "config.yaml")
}

// ResolvePolicyPath picks the policy file for a loaded config. Precedence:
//
//  1. cfg.PolicyFile as an absolute path
//  2. cfg.PolicyFile resolved relative to the config file's directory (so a
//     config that says `policyFile: ./fathom.policy.yaml` still works from
//     a subdir of the project)
//  3. Whatever DiscoverPolicy finds via upward search + global fallback
func ResolvePolicyPath(cfg types.Config) string {
	if path := brandenv.Get("FATHOM_POLICY"); path != "" {
		return path
	}
	if cfg.PolicyFile != "" {
		if filepath.IsAbs(cfg.PolicyFile) {
			return cfg.PolicyFile
		}
		if cfg.ConfigPath != "" {
			return filepath.Join(filepath.Dir(cfg.ConfigPath), cfg.PolicyFile)
		}
		if cfgPath := DiscoverConfig(); cfgPath != "" {
			return filepath.Join(filepath.Dir(cfgPath), cfg.PolicyFile)
		}
		return cfg.PolicyFile
	}
	return DiscoverPolicy()
}

// DiscoverPolicy returns the policy file path using the same upward-then-
// global search as DiscoverConfig.
func DiscoverPolicy() string {
	if v := brandenv.Get("FATHOM_POLICY"); v != "" {
		return v
	}
	d := cwd()
	for {
		for _, name := range []string{"fathom.policy.yaml", "fantazm.policy.yaml"} {
			candidate := filepath.Join(d, name)
			if _, err := os.Stat(candidate); err == nil {
				return candidate
			}
		}
		parent := filepath.Dir(d)
		if parent == d {
			break
		}
		d = parent
	}
	if home, err := os.UserHomeDir(); err == nil {
		for _, p := range []string{
			filepath.Join(home, ".config", "fathom", "policy.yaml"),
			filepath.Join(home, ".fathom", "policy.yaml"),
			filepath.Join(home, ".config", "fantazm", "policy.yaml"),
			filepath.Join(home, ".fantazm", "policy.yaml"),
		} {
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
	}
	return ""
}

// LoadPolicy reads fathom.policy.yaml, returning deny-by-default settings if
// the file is missing or malformed. The defaults match what `fathom init`
// writes — network deny, filesystem read-only, shell deny, secrets isolated.
func LoadPolicy(path string) types.PolicyConfig {
	defaults := types.PolicyConfig{
		Version: 1,
		Defaults: types.PermissionSet{
			Network:    "deny",
			Filesystem: "read-only",
			Shell:      "deny",
			Secrets:    "isolated",
		},
		Rules: nil,
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			slog.Info("no policy file found; using deny-all defaults", "path", path)
		} else {
			slog.Error("failed to read policy file; using deny-all defaults", "path", path, "err", err)
		}
		return defaults
	}

	var parsed types.PolicyConfig
	if err := yaml.Unmarshal(data, &parsed); err != nil {
		slog.Error("failed to parse policy file; using deny-all defaults", "path", path, "err", err)
		return defaults
	}
	if parsed.Version == 0 {
		parsed.Version = 1
	}
	// Take defaults from parsed where set, fill from baseline otherwise.
	if parsed.Defaults.Network == "" {
		parsed.Defaults.Network = defaults.Defaults.Network
	}
	if parsed.Defaults.Filesystem == "" {
		parsed.Defaults.Filesystem = defaults.Defaults.Filesystem
	}
	if parsed.Defaults.Shell == "" {
		parsed.Defaults.Shell = defaults.Defaults.Shell
	}
	if parsed.Defaults.Secrets == "" {
		parsed.Defaults.Secrets = defaults.Defaults.Secrets
	}
	slog.Info("policy loaded", "path", path, "ruleCount", len(parsed.Rules))
	return parsed
}

// mergeConfig overlays user-supplied fields onto a baseline. Nested structs
// (Auth, LLM, Enterprise) merge field-by-field so the user only has to set
// what they want to override.
func mergeConfig(base, overrides types.Config) types.Config {
	out := base
	if overrides.Mode != "" {
		out.Mode = overrides.Mode
	}
	if overrides.Profile != "" {
		out.Profile = overrides.Profile
	}
	if overrides.Host != "" {
		out.Host = overrides.Host
	}
	if overrides.Port != 0 {
		out.Port = overrides.Port
	}
	if overrides.DataDir != "" {
		out.DataDir = overrides.DataDir
	}
	if overrides.PolicyFile != "" {
		out.PolicyFile = overrides.PolicyFile
	}
	if overrides.LogLevel != "" {
		out.LogLevel = overrides.LogLevel
	}
	if overrides.Auth.Mode != "" {
		out.Auth.Mode = overrides.Auth.Mode
	}
	if overrides.Auth.SessionTimeout != 0 {
		out.Auth.SessionTimeout = overrides.Auth.SessionTimeout
	}
	if overrides.Auth.RequireMFA {
		out.Auth.RequireMFA = true
	}
	if overrides.LLM.Provider != "" {
		out.LLM.Provider = overrides.LLM.Provider
	}
	if overrides.LLM.Model != "" {
		out.LLM.Model = overrides.LLM.Model
	}
	if overrides.LLM.BaseURL != "" {
		out.LLM.BaseURL = overrides.LLM.BaseURL
	}
	if overrides.LLM.MaxTokens != 0 {
		out.LLM.MaxTokens = overrides.LLM.MaxTokens
	}
	if overrides.LLM.Temperature != 0 {
		out.LLM.Temperature = overrides.LLM.Temperature
	}
	// Multi-model registry: copy wholesale rather than per-key-merge, because
	// the file is meant to be the full statement of which models exist.
	if overrides.LLM.Default != "" {
		out.LLM.Default = overrides.LLM.Default
	}
	if len(overrides.LLM.Models) > 0 {
		out.LLM.Models = overrides.LLM.Models
	}
	// Routing: same treatment — config file owns the whole block when present.
	if overrides.Routing != nil {
		out.Routing = overrides.Routing
	}
	// Egress, Skills, Telemetry: same "wholesale copy" treatment as
	// Routing — the file is the canonical statement when present.
	if overrides.Egress != nil {
		out.Egress = overrides.Egress
	}
	if overrides.Skills != nil {
		out.Skills = overrides.Skills
	}
	if overrides.Telemetry != nil {
		out.Telemetry = overrides.Telemetry
	}
	if overrides.Enterprise != nil {
		out.Enterprise = overrides.Enterprise
	}
	// SubAgents: wholesale copy when the file declares the block.
	if overrides.SubAgents != nil {
		out.SubAgents = overrides.SubAgents
	}
	if overrides.Swarm != nil {
		out.Swarm = overrides.Swarm
	}
	if overrides.ClaudeCode != nil {
		out.ClaudeCode = overrides.ClaudeCode
	}
	if overrides.Takeover != nil {
		out.Takeover = overrides.Takeover
	}
	if overrides.Integrations != nil {
		out.Integrations = overrides.Integrations
	}
	return out
}

// applyEnvOverrides honours FANTAZM_MODE (and any other env keys we want to
// expose to orchestration). Invalid values are silently ignored — the file
// or default wins, which is what helm operators expect when they typo a value.
func applyEnvOverrides(cfg *types.Config) {
	if v := brandenv.Get("FATHOM_MODE"); v != "" {
		mode := types.Mode(v)
		if mode == types.ModePersonal || mode == types.ModeTeam || mode == types.ModeEnterprise {
			cfg.Mode = mode
		} else {
			slog.Warn("ignored invalid FANTAZM_MODE env value", "value", v)
		}
	}
	if v := brandenv.Get("FATHOM_PORT"); v != "" {
		var port int
		if _, err := fmt.Sscanf(v, "%d", &port); err == nil && port > 0 && port <= 65535 {
			cfg.Port = port
		}
	}
	if v := brandenv.Get("FATHOM_HOST"); v != "" {
		cfg.Host = v
	}
	if v := brandenv.Get("FATHOM_DATA_DIR"); v != "" {
		cfg.DataDir = v
	}
	if v := brandenv.Get("FATHOM_LOG_LEVEL"); v != "" {
		cfg.LogLevel = v
	}
}

// validate clamps obviously-wrong settings rather than failing — Fathom
// should boot when config is half-typed. The gateway's /api/v1/health
// endpoint is the operator's way to see "I'm up but using a defaulted port".
func validate(cfg *types.Config) {
	if cfg.Auth.SessionTimeout > 24*time.Hour {
		slog.Warn("session timeout exceeds 24h; clamping", "wanted", cfg.Auth.SessionTimeout)
		cfg.Auth.SessionTimeout = 24 * time.Hour
	}
	if cfg.Auth.SessionTimeout < 60*time.Second {
		slog.Warn("session timeout too short; setting to 60s minimum", "wanted", cfg.Auth.SessionTimeout)
		cfg.Auth.SessionTimeout = 60 * time.Second
	}
	if cfg.Port < 1 || cfg.Port > 65535 {
		slog.Warn("invalid port; resetting to default 8790", "wanted", cfg.Port)
		cfg.Port = 8790
	}
}

func cwd() string {
	d, _ := os.Getwd()
	return d
}

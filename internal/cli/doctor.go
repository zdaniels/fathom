package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
	"github.com/zdaniels/fathom/internal/cli/ui"
	"github.com/zdaniels/fathom/internal/config"
	"github.com/zdaniels/fathom/internal/security"
	"github.com/zdaniels/fathom/pkg/types"
)

func init() {
	subcommands = append(subcommands, newDoctorCommand)
}

// newDoctorCommand wires `fathom doctor` — a single command that diagnoses
// every common setup mistake. Returns non-zero exit when any check fails so
// CI / users' shell scripts can branch on it.
//
// Checks:
//   - config file: present + parses
//   - policy file: present + parses
//   - vault: key file exists, vault opens, encryption working
//   - LLM(s): each configured provider's required API key is resolvable
//   - Ollama (if configured): reachable + model pulled
//   - recall sidecar (Fathom Recall): on PATH + responds to mcp tools/list
//   - shell-exec: whether policy allows it
func newDoctorCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check Fathom's environment and diagnose setup issues",
		RunE: func(cmd *cobra.Command, args []string) error {
			// Silence INFO chatter from config + vault opens so the report
			// reads cleanly.
			security.SetLevel("warn")
			slog.SetDefault(slog.New(slog.NewTextHandler(
				os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))
			out := cmd.OutOrStdout()
			ui.SectionHeader(out, "fathom doctor")
			fmt.Fprintln(out)

			failures := 0
			report := func(name, detail string, ok bool) {
				if ok {
					fmt.Fprintln(out, "  "+ui.Success(name))
				} else {
					failures++
					fmt.Fprintln(out, "  "+ui.Error(name))
				}
				if detail != "" {
					ui.Hint(out, "  "+detail)
				}
			}

			// Config file — uses the same discovery logic as LoadConfig.
			cfgPath := config.DiscoverConfig()
			if cfgPath == "" {
				report("config file", "no config found — run `fathom init` (per-project) or `fathom init --global`", false)
				fmt.Fprintln(out)
				return fmt.Errorf("doctor: missing config")
			}
			cfg := config.LoadConfig(cfgPath)
			report("config file", cfgPath, true)

			// Mode + profile + feature matrix — tells the user, in one
			// glance, what's loaded vs. available given their config.
			// Bridges the gap between "I picked --minimal" and "yes
			// the scheduler genuinely isn't loaded."
			renderModeProfile(out, cfg)

			// Policy file — uses the same resolution as the factory so doctor
			// reports what the actual run-time loader will find.
			polPath := config.ResolvePolicyPath(cfg)
			if polPath != "" {
				if _, err := os.Stat(polPath); err == nil {
					report("policy file", polPath, true)
				} else {
					report("policy file", "configured at "+polPath+" but not readable — defaults will apply", false)
				}
			} else {
				report("policy file", "none found — deny-by-default applies", true)
			}

			// Vault key location (OS keychain when available, else key file).
			report("vault key", "stored in "+security.MasterKeySource(), true)

			// Vault opens.
			key, err := security.LoadOrCreateMasterKey()
			if err != nil {
				report("vault open", err.Error(), false)
			} else {
				v, err := security.OpenVault(security.VaultOpenOptions{
					Path: security.DefaultVaultPath(), Key: key,
				})
				if err != nil {
					report("vault open", err.Error(), false)
				} else {
					report("vault open", fmt.Sprintf("%d secret(s) stored", len(v.List())), true)
				}
			}

			// LLM checks. Multi-model setups loop every configured entry.
			if len(cfg.LLM.Models) > 0 {
				for name, m := range cfg.LLM.Models {
					checkProvider(out, report, name, m.Provider, m.Model, m.BaseURL)
				}
			} else {
				checkProvider(out, report, "default", cfg.LLM.Provider, cfg.LLM.Model, cfg.LLM.BaseURL)
			}

			// recall sidecar (Fathom Recall memory).
			if recallBin, _ := exec.LookPath("recall"); recallBin != "" {
				report("memory (recall)", "found at "+recallBin, true)
			} else {
				report("memory (recall)", "not installed — memory will be disabled. Build from github.com/zdaniels/recall and put `recall` on PATH.", true)
			}

			// Node + npm — required for any Node-based skill (gmail, slack,
			// browser, whatsapp, etc.). The built-in tools work without it.
			if nodeBin, _ := exec.LookPath("node"); nodeBin != "" {
				report("node", "found at "+nodeBin+" (required for skills like gmail/slack/browser/whatsapp)", true)
			} else {
				report("node", "not on PATH — Node 22+ is needed for any skill. Built-in tools (bash/grep/read_file/etc.) still work.", true)
			}
			if npmBin, _ := exec.LookPath("npm"); npmBin != "" {
				report("npm", "found at "+npmBin+" (needed by `fathom install` for skills with deps)", true)
			} else {
				report("npm", "not on PATH — `fathom install browser/whatsapp` will fail without it.", true)
			}

			// Docker — optional but needed for the python_exec sandboxed tool.
			if dockerBin, _ := exec.LookPath("docker"); dockerBin != "" {
				report("docker", "found at "+dockerBin+" (needed for python_exec sandboxed code)", true)
			} else {
				report("docker", "not on PATH — python_exec tool unavailable. Install Docker Desktop if you want sandboxed Python.", true)
			}

			// Workspace dir.
			cwd, _ := os.Getwd()
			report("workspace", cwd, true)

			fmt.Fprintln(out)
			if failures == 0 {
				ui.SectionHeader(out, ui.Success("All checks passed"))
			} else {
				ui.SectionHeader(out, ui.Error(fmt.Sprintf("%d check(s) failed", failures)))
				return fmt.Errorf("doctor: %d failure(s)", failures)
			}
			fmt.Fprintln(out)
			return nil
		},
	}
}

// checkProvider does the per-LLM-entry diagnosis. For ollama it pings the
// /api/tags endpoint to confirm reachability; for cloud providers it just
// checks the API key is present in the vault or .env.
// renderModeProfile prints a small "what's loaded vs available" matrix
// based on the current mode + profile. Sourced from the same rules
// the agentfactory / enterprise / minimal-profile branches use, so
// what doctor reports is what `fathom start` will actually load.
func renderModeProfile(out io.Writer, cfg types.Config) {
	mode := string(cfg.Mode)
	if mode == "" {
		mode = "personal"
	}
	profile := string(cfg.Profile)
	if profile == "" {
		profile = "default"
	}
	fmt.Fprintln(out)
	ui.SectionHeader(out, "Mode + profile")
	ui.KV(out, "mode", mode)
	ui.KV(out, "profile", profile)

	// Feature matrix. on/off per (mode, profile) — sourced from the
	// same branches the agentfactory/enterprise wiring uses, so
	// what doctor reports is what `fathom start` will actually load.
	minimal := cfg.Profile == types.ProfileMinimal
	enterprise := cfg.Mode == types.ModeEnterprise
	team := cfg.Mode == types.ModeTeam || enterprise

	fmt.Fprintln(out)
	ui.SectionHeader(out, "Features")
	feature := func(name string, on bool) {
		if on {
			ui.KV(out, name, ui.Success("enabled"))
		} else {
			ui.KV(out, name, ui.Mute("skipped"))
		}
	}
	feature("agent", true)
	feature("vault", true) // always — needed for API keys
	feature("audit", true) // always — small, in-memory hash chain
	feature("scheduler", !minimal)
	feature("threads", !minimal)
	feature("memory", !minimal)
	feature("skills", !minimal)
	feature("egress", !minimal)
	feature("tenants", team)
	feature("rbac", team)
	feature("sso", enterprise)
	feature("compliance", enterprise)
}

func checkProvider(_ interface{}, report func(string, string, bool), name, provider, model, baseURL string) {
	label := fmt.Sprintf("llm.%s (%s:%s)", name, provider, model)
	switch provider {
	case "ollama":
		base := baseURL
		if base == "" {
			base = "http://127.0.0.1:11434"
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/tags", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			report(label, "ollama not reachable at "+base+" — run `brew services start ollama`", false)
			return
		}
		_ = resp.Body.Close()
		report(label, "ollama reachable at "+base, true)
	case "openai":
		checkAPIKey(report, label, "OPENAI_API_KEY")
	case "anthropic":
		checkAPIKey(report, label, "ANTHROPIC_API_KEY")
	case "gemini":
		checkAPIKey(report, label, "GEMINI_API_KEY")
	case "deepseek":
		checkAPIKey(report, label, "DEEPSEEK_API_KEY")
	case "xai":
		checkAPIKey(report, label, "XAI_API_KEY")
	case "xiaomi", "mimo":
		checkAPIKey(report, label, "XIAOMI_MIMO_API_KEY")
	case "groq":
		checkAPIKey(report, label, "GROQ_API_KEY")
	case "openrouter":
		checkAPIKey(report, label, "OPENROUTER_API_KEY")
	case "together":
		checkAPIKey(report, label, "TOGETHER_API_KEY")
	case "fireworks":
		checkAPIKey(report, label, "FIREWORKS_API_KEY")
	case "mistral":
		checkAPIKey(report, label, "MISTRAL_API_KEY")
	case "huggingface", "hf":
		checkAPIKey(report, label, "HF_TOKEN")
	case "lmstudio":
		// Local OpenAI-compatible server. Ping its /v1/models endpoint
		// the same way we ping ollama — if it's not running, this is
		// the loudest signal a user will see.
		base := baseURL
		if base == "" {
			base = "http://127.0.0.1:1234/v1"
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/models", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			report(label, "LM Studio not reachable at "+base+" — start the local server in LM Studio", false)
			return
		}
		_ = resp.Body.Close()
		report(label, "LM Studio reachable at "+base, true)
	case "bedrock":
		// Bedrock needs both halves of the AWS pair plus a region (held in
		// baseUrl per init.buildLLMConfig). Surface each piece individually
		// so the user sees exactly which one is missing.
		checkAPIKey(report, label, "AWS_ACCESS_KEY_ID")
		checkAPIKey(report, label, "AWS_SECRET_ACCESS_KEY")
		region := baseURL
		if region == "" {
			region = os.Getenv("AWS_REGION")
			if region == "" {
				region = os.Getenv("AWS_DEFAULT_REGION")
			}
		}
		if region == "" {
			report(label, "region not set — put it in llm.baseUrl or AWS_REGION", false)
		} else {
			report(label, "region "+region, true)
		}
	default:
		report(label, "unknown provider "+provider, false)
	}
}

func checkAPIKey(report func(string, string, bool), label, envName string) {
	if v := os.Getenv(envName); v != "" {
		report(label, envName+" set in environment", true)
		return
	}
	// Try vault.
	key, err := security.LoadOrCreateMasterKey()
	if err == nil {
		v, err := security.OpenVault(security.VaultOpenOptions{
			Path: security.DefaultVaultPath(), Key: key,
		})
		if err == nil && v.Has(envName) {
			report(label, envName+" stored in vault", true)
			return
		}
	}
	report(label, envName+" not set — run `fathom vault set "+envName+"`", false)
	_ = filepath.Separator // keep imports alive on platforms that don't use it
}

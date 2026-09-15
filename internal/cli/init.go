package cli

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"github.com/zdaniels/fathom/internal/cli/ui"
	"github.com/zdaniels/fathom/internal/security"
	"gopkg.in/yaml.v3"
)

func init() {
	subcommands = append(subcommands, newInitCommand)
}

// boolCount returns how many of the args are true. Tiny helper so the
// "exactly one mode flag" check reads as a single line.
func boolCount(bs ...bool) int {
	n := 0
	for _, b := range bs {
		if b {
			n++
		}
	}
	return n
}

// newInitCommand: `fathom init` writes config + persona + policy, asks for
// agent name + LLM provider + API key.
//
// Mode is picked first-class via one of these flags (so users never
// have to hand-edit yaml to switch modes):
//
//	--minimal     — personal mode + ProfileMinimal (no scheduler,
//	                no threads, no audit log — lean chat brain)
//	--team        — team mode + tenants + minimal RBAC config
//	--enterprise  — enterprise mode + SSO/OIDC prompts
//
// With no flag (or `--mode=…`), the wizard asks interactively.
func newInitCommand() *cobra.Command {
	var (
		modeFlag       string
		globalFlag     bool
		minimalFlag    bool
		teamFlag       bool
		enterpriseFlag bool
	)
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Setup wizard (writes config, persona, policy)",
		Long: `Run the setup wizard. By default writes a per-project config to
the current directory — useful when each project has its own agent
context. Pass --global to write to ~/.config/fathom/ instead, creating
a single agent that works from anywhere.

Mode flags (pick one; defaults to interactive personal):
  --minimal     fastest "chat brain only" setup; no scheduler, threads,
                audit log, or skill bridging. Boots in ~150ms.
  --team        multi-user with tenants + minimal RBAC.
  --enterprise  team features + SSO/OIDC + compliance reports.

Discovery order at run time:
  1. $FANTAZM_CONFIG (explicit override)
  2. fathom.config.yaml walking up from cwd
  3. ~/.config/fathom/config.yaml`,
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			ui.Wordmark(out)
			if globalFlag {
				ui.SectionHeader(out, "Setup — global")
			} else {
				ui.SectionHeader(out, "Setup — project")
			}
			fmt.Fprintln(out)

			reader := bufio.NewReader(os.Stdin)
			ask := func(prompt string) string {
				ui.Field(out, prompt)
				line, _ := reader.ReadString('\n')
				return strings.TrimSpace(line)
			}

			// Mode resolution: explicit flags win; --mode is the
			// secondary explicit form; falling back to an interactive
			// prompt. Multiple mode flags are an obvious user error,
			// reject it loudly.
			modeFlagsSet := boolCount(minimalFlag, teamFlag, enterpriseFlag)
			if modeFlagsSet > 1 {
				return fmt.Errorf("pick only one of --minimal / --team / --enterprise")
			}
			var mode, profile string
			switch {
			case minimalFlag:
				mode, profile = "personal", "minimal"
			case teamFlag:
				mode = "team"
			case enterpriseFlag:
				mode = "enterprise"
			case modeFlag != "":
				mode = modeFlag
			default:
				m := ask("Mode (personal/team/enterprise) [personal]: ")
				if m == "team" || m == "enterprise" {
					mode = m
				} else {
					mode = "personal"
				}
			}

			agentName := ask("Agent name [fathom]: ")
			if agentName == "" {
				agentName = "fathom"
			}

			provider := ask("LLM provider (openai/anthropic/ollama/gemini/bedrock/deepseek/xai/xiaomi/groq/openrouter/together/fireworks/mistral/huggingface/lmstudio) [anthropic]: ")
			if provider == "" {
				provider = "anthropic"
			}
			defaultModel := "claude-sonnet-4-5"
			switch provider {
			case "openai":
				defaultModel = "gpt-4o"
			case "ollama":
				defaultModel = "llama3"
			case "gemini":
				defaultModel = "gemini-2.0-flash"
			case "bedrock":
				defaultModel = "anthropic.claude-3-5-sonnet-20241022-v2:0"
			case "deepseek":
				defaultModel = "deepseek-chat"
			case "xai":
				defaultModel = "grok-4"
			case "xiaomi", "mimo":
				defaultModel = "mimo-v2.5-pro"
			case "groq":
				defaultModel = "llama-3.3-70b-versatile"
			case "openrouter":
				defaultModel = "anthropic/claude-sonnet-latest"
			case "together":
				defaultModel = "meta-llama/Meta-Llama-3.1-70B-Instruct-Turbo"
			case "fireworks":
				defaultModel = "accounts/fireworks/models/llama-v3p3-70b-instruct"
			case "mistral":
				defaultModel = "mistral-large-latest"
			case "huggingface", "hf":
				defaultModel = "deepseek-ai/DeepSeek-R1"
			case "lmstudio":
				defaultModel = "local-model"
			}
			model := ask(fmt.Sprintf("Model [%s]: ", defaultModel))
			if model == "" {
				model = defaultModel
			}
			// Bedrock is region-scoped; capture the region up front and
			// stash it in baseUrl (config repurposes that field for
			// non-URL endpoints, per llm.NewBedrock).
			var bedrockRegion string
			if provider == "bedrock" {
				bedrockRegion = ask("AWS region [us-east-1]: ")
				if bedrockRegion == "" {
					bedrockRegion = "us-east-1"
				}
			}
			port := ask("Port [8790]: ")
			if port == "" {
				port = "8790"
			}
			dataDir := ask("Data directory [./data]: ")
			if dataDir == "" {
				dataDir = "./data"
			}

			var apiKeyEnv, apiKeyVal string
			switch provider {
			case "openai":
				apiKeyEnv = "OPENAI_API_KEY"
			case "anthropic":
				apiKeyEnv = "ANTHROPIC_API_KEY"
			case "gemini":
				apiKeyEnv = "GEMINI_API_KEY"
			case "deepseek":
				apiKeyEnv = "DEEPSEEK_API_KEY"
			case "xai":
				apiKeyEnv = "XAI_API_KEY"
			case "xiaomi", "mimo":
				apiKeyEnv = "XIAOMI_MIMO_API_KEY"
			case "groq":
				apiKeyEnv = "GROQ_API_KEY"
			case "openrouter":
				apiKeyEnv = "OPENROUTER_API_KEY"
			case "together":
				apiKeyEnv = "TOGETHER_API_KEY"
			case "fireworks":
				apiKeyEnv = "FIREWORKS_API_KEY"
			case "mistral":
				apiKeyEnv = "MISTRAL_API_KEY"
			case "huggingface", "hf":
				apiKeyEnv = "HF_TOKEN"
				// lmstudio is local + keyless — intentionally not listed.
			}
			if apiKeyEnv != "" {
				apiKeyVal = ask(fmt.Sprintf("%s (leave blank to set later): ", apiKeyEnv))
			}
			// Bedrock has two required secrets — handle separately and
			// stash both in the vault. Session token is optional.
			var bedrockAccessKey, bedrockSecretKey string
			if provider == "bedrock" {
				bedrockAccessKey = ask("AWS_ACCESS_KEY_ID (leave blank to set later): ")
				bedrockSecretKey = ask("AWS_SECRET_ACCESS_KEY (leave blank to set later): ")
			}

			// Per-mode follow-up prompts. team needs an admin email
			// + team name so the bootstrap RBAC + tenant assignment
			// land on something meaningful. enterprise needs real
			// SSO/OIDC values rather than placeholders the user
			// will forget to fix.
			var teamName, adminEmail string
			var ssoIssuer, ssoClient, ssoSecret string
			if mode == "team" || mode == "enterprise" {
				teamName = ask("Team name [Fathom Team]: ")
				if teamName == "" {
					teamName = "Fathom Team"
				}
				adminEmail = ask("Admin email (gets bootstrap admin role): ")
			}
			if mode == "enterprise" {
				ssoIssuer = ask("OIDC issuer URL (e.g. https://login.example.com): ")
				ssoClient = ask("OIDC client ID: ")
				ssoSecret = ask("OIDC client secret (leave blank to set later): ")
			}

			// config.yaml
			cfg := map[string]interface{}{
				"mode":       mode,
				"host":       "127.0.0.1",
				"port":       parseIntOr(port, 8790),
				"auth":       map[string]interface{}{"mode": "token", "sessionTimeout": "1h"},
				"llm":        buildLLMConfig(provider, model, bedrockRegion),
				"dataDir":    dataDir,
				"policyFile": "./fathom.policy.yaml",
				"logLevel":   "info",
			}
			if profile != "" {
				cfg["profile"] = profile
			}
			if mode == "team" || mode == "enterprise" {
				ent := map[string]interface{}{
					"teamName":   teamName,
					"adminEmail": adminEmail,
				}
				if mode == "enterprise" {
					// Use real values when provided; fall back to
					// placeholders for fields the user skipped so they
					// can fill them in later. Loud comment in the file
					// so they notice on first edit.
					issuer := ssoIssuer
					if issuer == "" {
						issuer = "REPLACE_ME_https://login.example.com"
					}
					client := ssoClient
					if client == "" {
						client = "REPLACE_ME_client_id"
					}
					secret := ssoSecret
					if secret == "" {
						secret = "REPLACE_ME_client_secret"
					}
					ent["sso"] = map[string]interface{}{
						"issuer":       issuer,
						"clientId":     client,
						"clientSecret": secret,
					}
				}
				cfg["enterprise"] = ent
			}
			// Pick destination root. --global writes to ~/.config/fathom/;
			// the default writes to cwd (per-project).
			cwd, _ := os.Getwd()
			root := cwd
			personaDir := filepath.Join(cwd, ".fantazm")
			cfgName := "fathom.config.yaml"
			policyName := "fathom.policy.yaml"
			if globalFlag {
				home, _ := os.UserHomeDir()
				root = filepath.Join(home, ".config", "fathom")
				if err := os.MkdirAll(root, 0o755); err != nil {
					return err
				}
				personaDir = root
				cfgName = "config.yaml"
				policyName = "policy.yaml"
			}
			cfgPath := filepath.Join(root, cfgName)
			cfgBuf, _ := yaml.Marshal(cfg)
			if err := os.WriteFile(cfgPath, cfgBuf, 0o644); err != nil {
				return err
			}
			fmt.Fprintln(out)
			ui.SectionHeader(out, "Files written")
			ui.KV(out, "config", cfgPath)

			// .env (only if user provided an API key, only in project mode)
			if !globalFlag && apiKeyVal != "" && apiKeyEnv != "" {
				envPath := filepath.Join(cwd, ".env")
				existing, _ := os.ReadFile(envPath)
				if !strings.Contains(string(existing), apiKeyEnv+"=") {
					line := apiKeyEnv + "=" + apiKeyVal + "\n"
					f, err := os.OpenFile(envPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
					if err != nil {
						return err
					}
					f.WriteString(line)
					f.Close()
					ui.KV(out, ".env", relPath(cwd, envPath)+" ("+apiKeyEnv+")")
				} else {
					ui.KV(out, ".env", "left as-is — already has "+apiKeyEnv)
				}
			} else if apiKeyVal != "" && apiKeyEnv != "" && globalFlag {
				// In global mode, ALWAYS store the key in the vault, not .env.
				// (Global users won't be cd'ing into a project dir for `.env`
				// to be picked up anyway.)
				if v, err := openVaultForInit(); err == nil {
					_ = v.Set(apiKeyEnv, apiKeyVal, nil)
					ui.KV(out, "vault", apiKeyEnv+" stored in encrypted vault")
				}
			} else if apiKeyEnv != "" {
				ui.KV(out, "key", ui.Warn("not set — `fathom vault set "+apiKeyEnv+"` before chat"))
			}

			// Bedrock: stash AWS_* secrets the same way single-key providers
			// do (.env in project mode, vault in global mode). Each secret
			// follows the same write rules as apiKeyVal above.
			if provider == "bedrock" {
				writeBedrockSecret := func(name, val string) {
					if val == "" {
						ui.KV(out, "key", ui.Warn("not set — `fathom vault set "+name+"` before chat"))
						return
					}
					if !globalFlag {
						envPath := filepath.Join(cwd, ".env")
						existing, _ := os.ReadFile(envPath)
						if !strings.Contains(string(existing), name+"=") {
							line := name + "=" + val + "\n"
							f, err := os.OpenFile(envPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
							if err == nil {
								f.WriteString(line)
								f.Close()
								ui.KV(out, ".env", relPath(cwd, envPath)+" ("+name+")")
							}
						} else {
							ui.KV(out, ".env", "left as-is — already has "+name)
						}
					} else if v, err := openVaultForInit(); err == nil {
						_ = v.Set(name, val, nil)
						ui.KV(out, "vault", name+" stored in encrypted vault")
					}
				}
				writeBedrockSecret("AWS_ACCESS_KEY_ID", bedrockAccessKey)
				writeBedrockSecret("AWS_SECRET_ACCESS_KEY", bedrockSecretKey)
			}

			// data dir (project mode only — global mode uses ~/.local/share)
			if !globalFlag {
				os.MkdirAll(filepath.Join(cwd, dataDir), 0o755)
			}

			// policy file
			policyPath := filepath.Join(root, policyName)
			if _, err := os.Stat(policyPath); err != nil {
				pol := map[string]interface{}{
					"version": 1,
					"defaults": map[string]string{
						"network": "deny", "filesystem": "read-only",
						"shell": "deny", "secrets": "isolated",
					},
					"rules": []interface{}{},
				}
				polBuf, _ := yaml.Marshal(pol)
				_ = os.WriteFile(policyPath, polBuf, 0o644)
				ui.KV(out, "policy", policyPath)
			}

			// persona.md
			personaPath := filepath.Join(personaDir, "persona.md")
			if _, err := os.Stat(personaPath); err != nil {
				os.MkdirAll(filepath.Dir(personaPath), 0o755)
				_ = os.WriteFile(personaPath, []byte(defaultPersona(agentName)), 0o644)
				ui.KV(out, "persona", personaPath)
			}

			fmt.Fprintln(out)
			ui.SectionHeader(out, "Mode: "+mode)
			switch mode {
			case "personal":
				ui.KV(out, "notes", "local note-taking with full-text search")
				ui.KV(out, "web-search", "DuckDuckGo lookups")
				ui.KV(out, "file-editor", "read/write files in the current workspace")
				fmt.Fprintln(out)
				ui.Hint(out, "Next: run 'fathom chat' to talk to your agent.")
				ui.Hint(out, "Add more skills: 'fathom install <name>'  (gmail, github, slack, …)")
			case "team":
				ui.KV(out, "admin api", "/api/v1/admin/*")
				fmt.Fprintln(out)
				ui.Hint(out, "Next: run 'fathom start' — admin token prints once on first boot.")
			default:
				ui.KV(out, "enterprise", "SSO + RBAC + compliance wired")
				fmt.Fprintln(out)
				ui.Hint(out, "Configure OIDC issuer/clientId/clientSecret in fathom.config.yaml.")
			}
			fmt.Fprintln(out)
			return nil
		},
	}
	cmd.Flags().StringVar(&modeFlag, "mode", "", "personal|team|enterprise (bypasses the interactive prompt)")
	cmd.Flags().BoolVar(&globalFlag, "global", false, "Write to ~/.config/fathom/ instead of the current directory (one agent works from anywhere)")
	cmd.Flags().BoolVar(&minimalFlag, "minimal", false, "Personal mode + lean profile: no scheduler, no threads, no audit log — fast chat brain")
	cmd.Flags().BoolVar(&teamFlag, "team", false, "Team mode: multi-user with tenants + minimal RBAC")
	cmd.Flags().BoolVar(&enterpriseFlag, "enterprise", false, "Enterprise mode: team features + SSO/OIDC + compliance")
	return cmd
}

// openVaultForInit opens the global vault so init can stash the API key.
// Used in --global mode; project-mode init writes the key to a project-local
// .env file instead. Returns the open vault or an error if unlock fails.
func openVaultForInit() (*security.SecretsVault, error) {
	key, err := security.LoadOrCreateMasterKey()
	if err != nil {
		return nil, err
	}
	return security.OpenVault(security.VaultOpenOptions{
		Path: security.DefaultVaultPath(), Key: key,
	})
}

func defaultPersona(name string) string {
	return fmt.Sprintf(`# %s

You are %s, the user's personal AI agent. You run locally on their machine.

## How to behave

- Be concise. The user is in a terminal; long blocks of prose are friction.
- Prefer doing over describing. If you can use a tool to answer, use it.
- When you write or edit files, summarize the change in one line, not a recap of the file.
- If a task is ambiguous, ask one clarifying question — don't fan out.

## Coding workflow

When the user asks you to write or change code:

1. **Read first.** Use grep or glob to find the relevant files, then read_file before editing. Don't guess what's there.
2. **Edit surgically.** For changes to existing files use edit_file with old_string + new_string. Use write_file only for brand-new files.
3. **Verify.** Run tests, linters, or builds via bash after a change (when shell-exec is allowed by policy).
4. **Stay scoped.** Don't fix unrelated things in the same change. Don't refactor on the side.

## What you can do today

- Search: grep (regex over file contents), glob (file patterns)
- Read/write: read_file, write_file (new files), edit_file (targeted string replace)
- Shell: bash (run tests, builds, git, package managers — policy permitting)
- Web: web_search for documentation and references
- Notes: create_note, search_notes, list_notes for your own scratchpad
- Memory: recall_search, record_decision, pin_for_session — durable cross-session memory if the Fathom Recall sidecar is installed

## Multi-model patterns

If the user has multiple models configured (visible via /models), the user can switch the active model with /use <name>. They can also /review the last answer through a different model.

When YOU should call the `+"`"+`critique`+"`"+` tool:
- Stakes are high — production code, security claims, anything irreversible
- You have competing options and want a second opinion before recommending one
- The user explicitly says "double-check" or "are you sure?"

Don't critique trivial answers — it wastes tokens. Lean on critique for genuinely hard calls.

Install more skills with `+"`"+`fathom install <name>`+"`"+`.
`, name, name)
}

func relPath(cwd, abs string) string {
	rel, err := filepath.Rel(cwd, abs)
	if err != nil {
		return abs
	}
	return "./" + rel
}

func parseIntOr(s string, d int) int {
	n := 0
	if _, err := fmt.Sscanf(s, "%d", &n); err == nil {
		return n
	}
	return d
}

// buildLLMConfig assembles the `llm:` block. Bedrock reuses the baseUrl
// field as a region holder (Bedrock is region-scoped, not endpoint-scoped),
// so we stash it there instead of inventing a new key.
func buildLLMConfig(provider, model, bedrockRegion string) map[string]interface{} {
	m := map[string]interface{}{"provider": provider, "model": model}
	if provider == "bedrock" && bedrockRegion != "" {
		m["baseUrl"] = bedrockRegion
	}
	return m
}

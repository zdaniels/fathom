package cli

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/spf13/cobra"
	"github.com/zdaniels/fathom/internal/cli/ui"
	"github.com/zdaniels/fathom/internal/security"
)

func init() {
	subcommands = append(subcommands, newVaultCommand)
}

// newVaultCommand: `fathom vault {list,set,get,delete,import-env}`. The
// vault lives at ~/.fantazm/vault and is auto-unlocked from ~/.fantazm/.master.key.
func newVaultCommand() *cobra.Command {
	root := &cobra.Command{
		Use:   "vault",
		Short: "Manage the encrypted secrets vault",
		PersistentPreRun: func(cmd *cobra.Command, args []string) {
			// Suppress the "vault opened" INFO log — it's not useful here
			// and clutters the visual layout. Errors still surface at warn+.
			security.SetLevel("warn")
		},
	}
	root.AddCommand(vaultListCmd(), vaultSetCmd(), vaultGetCmd(), vaultDeleteCmd(), vaultImportEnvCmd())
	return root
}

func openVault() (*security.SecretsVault, error) {
	key, err := security.LoadOrCreateMasterKey()
	if err != nil {
		return nil, err
	}
	return security.OpenVault(security.VaultOpenOptions{
		Path: security.DefaultVaultPath(),
		Key:  key,
	})
}

func vaultListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "Show secrets in the vault (names + scopes; never values)",
		RunE: func(cmd *cobra.Command, args []string) error {
			v, err := openVault()
			if err != nil {
				return err
			}
			entries := v.List()
			out := cmd.OutOrStdout()
			fmt.Fprintln(out)
			if len(entries) == 0 {
				ui.SectionHeader(out, "Vault is empty")
				ui.KV(out, "vault", security.DefaultVaultPath())
				ui.KV(out, "key", security.MasterKeySource())
				fmt.Fprintln(out)
				ui.Hint(out, "Add a secret: fathom vault set <NAME>")
				fmt.Fprintln(out)
				return nil
			}
			ui.SectionHeader(out, fmt.Sprintf("%d secret(s) in vault", len(entries)))
			ui.KV(out, "vault", security.DefaultVaultPath())
			fmt.Fprintln(out)
			for _, e := range entries {
				scope := ui.Mute("[any]")
				if len(e.AllowedSkills) > 0 {
					scope = ui.Brand("[" + strings.Join(e.AllowedSkills, ",") + "]")
				}
				fmt.Fprintf(out, "  %s  %s  %s\n",
					ui.Body(fmt.Sprintf("%-28s", e.Name)),
					scope,
					ui.Mute(e.CreatedAt.Format("2006-01-02T15:04:05Z")),
				)
			}
			fmt.Fprintln(out)
			return nil
		},
	}
}

func vaultSetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "set NAME [skills,csv]",
		Short: "Store a secret (prompts for value, scoped to optional skill list)",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			var allowed []string
			if len(args) == 2 {
				for _, s := range strings.Split(args[1], ",") {
					if t := strings.TrimSpace(s); t != "" {
						allowed = append(allowed, t)
					}
				}
			}
			out := cmd.OutOrStdout()
			fmt.Fprint(out, "  "+ui.Mute("›")+" "+ui.Bold(name)+ui.Mute(": "))
			reader := bufio.NewReader(os.Stdin)
			val, _ := reader.ReadString('\n')
			val = strings.TrimSpace(val)
			if val == "" {
				fmt.Fprintln(out)
				fmt.Fprintln(out, "  "+ui.Warn("empty value — nothing stored"))
				return nil
			}
			v, err := openVault()
			if err != nil {
				return err
			}
			if err := v.Set(name, val, allowed); err != nil {
				return err
			}
			scope := ""
			if len(allowed) > 0 {
				scope = " (scoped to: " + strings.Join(allowed, ", ") + ")"
			}
			fmt.Fprintln(out)
			fmt.Fprintln(out, "  "+ui.Success("Stored "+name+scope))
			fmt.Fprintln(out)
			return nil
		},
	}
	return cmd
}

func vaultGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get NAME",
		Short: "Print a secret's value (use sparingly — shoulder-surf risk)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			v, err := openVault()
			if err != nil {
				return err
			}
			val, err := v.Get(args[0], "")
			if err != nil {
				return err
			}
			fmt.Println(val)
			return nil
		},
	}
}

func vaultDeleteCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "delete NAME",
		Short: "Remove a secret from the vault",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			v, err := openVault()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintln(out)
			if v.Delete(args[0]) {
				fmt.Fprintln(out, "  "+ui.Success("Deleted "+args[0]))
			} else {
				fmt.Fprintln(out, "  "+ui.Warn(args[0]+" not found"))
			}
			fmt.Fprintln(out)
			return nil
		},
	}
}

// knownSecrets keys explicit names → optional skill-scopes.
var knownSecrets = map[string][]string{
	"OPENAI_API_KEY":                {},
	"ANTHROPIC_API_KEY":             {},
	"GMAIL_CLIENT_ID":               {"gmail"},
	"GMAIL_CLIENT_SECRET":           {"gmail"},
	"GMAIL_REFRESH_TOKEN":           {"gmail"},
	"GOOGLE_CLIENT_ID":              {"gmail", "calendar"},
	"GOOGLE_CLIENT_SECRET":          {"gmail", "calendar"},
	"GOOGLE_CALENDAR_REFRESH_TOKEN": {"calendar"},
	"GITHUB_TOKEN":                  {"github"},
	"SLACK_BOT_TOKEN":               {"slack"},
	"DISCORD_BOT_TOKEN":             {"discord"},
	"TELEGRAM_BOT_TOKEN":            {"telegram"},
}

var heuristicRE = regexp.MustCompile(`(API_KEY|TOKEN|CLIENT_ID|CLIENT_SECRET|REFRESH|ACCESS)$`)

func vaultImportEnvCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "import-env",
		Short: "Migrate known keys from .env into the vault",
		RunE: func(cmd *cobra.Command, args []string) error {
			cwd, _ := os.Getwd()
			envPath := filepath.Join(cwd, ".env")
			data, err := os.ReadFile(envPath)
			if err != nil {
				fmt.Printf("\n  No .env at %s\n\n", envPath)
				return nil
			}
			v, err := openVault()
			if err != nil {
				return err
			}
			existing := map[string]bool{}
			for _, e := range v.List() {
				existing[e.Name] = true
			}
			imported, already, skipped := 0, 0, 0
			for _, line := range strings.Split(string(data), "\n") {
				idx := strings.IndexByte(line, '=')
				if idx <= 0 {
					continue
				}
				key := strings.TrimSpace(line[:idx])
				val := strings.Trim(strings.TrimSpace(line[idx+1:]), `"'`)
				if val == "" {
					skipped++
					continue
				}
				if existing[key] {
					already++
					continue
				}
				scope, known := knownSecrets[key]
				if !known && !heuristicRE.MatchString(key) {
					skipped++
					continue
				}
				if err := v.Set(key, val, scope); err != nil {
					return err
				}
				imported++
				if len(scope) > 0 {
					fmt.Printf("  imported %s → [%s]\n", key, strings.Join(scope, ","))
				} else {
					fmt.Printf("  imported %s\n", key)
				}
			}
			fmt.Printf("\n  Imported %d, already-present %d, skipped %d.\n", imported, already, skipped)
			if imported > 0 {
				fmt.Printf("  Vault: %s\n  Safe to delete those keys from .env now.\n\n", security.DefaultVaultPath())
			}
			return nil
		},
	}
}

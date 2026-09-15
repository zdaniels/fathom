package cli

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"
	"github.com/zdaniels/fathom/internal/cli/ui"
	"github.com/zdaniels/fathom/internal/config"
	"github.com/zdaniels/fathom/internal/security"
)

func init() {
	subcommands = append(subcommands, newAuditCommand)
}

// newAuditCommand exposes the hash-chained audit log to the CLI. Two
// modes: `fathom audit` for human-readable tail, `fathom audit --json`
// for piping into jq.
//
// Reads durable state directly without constructing an agent or sidecars.
func newAuditCommand() *cobra.Command {
	var asJSON bool
	var limit int
	cmd := &cobra.Command{
		Use:   "audit",
		Short: "Show recent audit log entries",
		Long: `Print the most recent hash-chained audit entries. Tool calls, LLM
requests, policy decisions, canary trips — everything that runs through
the agent is recorded here.

Pipe through jq for filtering: fathom audit --json | jq '.[] | select(.action=="tool_call")'`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := config.LoadConfig("")
			if limit < 0 {
				return fmt.Errorf("limit must not be negative")
			}
			audit, err := security.OpenAuditLogger(security.AuditPath(cfg.DataDir))
			if err != nil {
				return err
			}
			defer audit.Close()
			entries := audit.Snapshot()
			if err := audit.Err(); err != nil {
				return err
			}
			if asJSON && len(entries) == 0 {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(entries)
			}
			if len(entries) == 0 {
				out := cmd.OutOrStdout()
				fmt.Fprintln(out)
				ui.SectionHeader(out, "Audit log is empty")
				ui.Hint(out, "Entries are written when the agent runs. Use `fathom chat`, then re-run audit.")
				fmt.Fprintln(out)
				return nil
			}
			if limit > 0 && limit < len(entries) {
				entries = entries[len(entries)-limit:]
			}
			if asJSON {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(entries)
			}
			out := cmd.OutOrStdout()
			fmt.Fprintln(out)
			ui.SectionHeader(out, fmt.Sprintf("%d audit entries", len(entries)))
			fmt.Fprintln(out)
			for _, e := range entries {
				decoration := ui.Mute("allow")
				switch string(e.PolicyResult) {
				case "deny":
					decoration = ui.Error("deny")
				case "escalate":
					decoration = ui.Warn("escalate")
				}
				ts := e.Timestamp.Format("15:04:05")
				fmt.Fprintf(out, "  %s  %s  %s  %s\n",
					ui.Mute(ts),
					ui.Brand(string(e.Action)),
					decoration,
					ui.Mute("user="+e.UserID),
				)
			}
			fmt.Fprintln(out)
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "Emit JSON for piping into jq")
	cmd.Flags().IntVar(&limit, "limit", 100, "Cap at the N most recent entries")
	return cmd
}

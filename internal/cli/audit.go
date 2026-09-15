package cli

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/zdaniels/fathom/internal/agentfactory"
	"github.com/zdaniels/fathom/internal/cli/ui"
	"github.com/zdaniels/fathom/internal/config"
)

func init() {
	subcommands = append(subcommands, newAuditCommand)
}

// newAuditCommand exposes the hash-chained audit log to the CLI. Two
// modes: `fathom audit` for human-readable tail, `fathom audit --json`
// for piping into jq.
//
// The audit log lives in-memory inside the gateway process, so running
// this command spins up a transient factory just to access the logger.
// That's intentional — there's no separate "audit daemon" to coordinate
// with.
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
			result, err := agentfactory.CreateDefault(cfg, agentfactory.Options{})
			if err != nil {
				return err
			}
			defer func() {
				if result.Memory != nil {
					result.Memory.Stop()
				}
			}()
			entries := result.Security.Audit.Snapshot()
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
				return json.NewEncoder(os.Stdout).Encode(entries)
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

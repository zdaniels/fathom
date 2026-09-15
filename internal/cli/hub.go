package cli

import (
	"encoding/json"
	"fmt"
	"github.com/zdaniels/fathom/internal/brandenv"
	"io"
	"net/http"
	"net/url"

	"github.com/spf13/cobra"
	"github.com/zdaniels/fathom/internal/cli/ui"
)

func init() {
	subcommands = append(subcommands, newHubCommand)
}

// newHubCommand wires `fathom hub {search,stats}` — thin HTTP clients
// pointing at the local hub (or whatever FANTAZM_HUB_URL says).
func newHubCommand() *cobra.Command {
	root := &cobra.Command{Use: "hub", Short: "Search the Fathom Secure Hub"}
	root.AddCommand(hubSearchCmd(), hubStatsCmd())
	return root
}

func hubBaseURL() string {
	if v := brandenv.Get("FATHOM_HUB_URL"); v != "" {
		return v
	}
	return "http://127.0.0.1:8791"
}

func hubSearchCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "search QUERY",
		Short: "Search the hub for skills matching QUERY",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			q := args[0]
			out := cmd.OutOrStdout()
			endpoint := hubBaseURL() + "/hub/v1/skills?q=" + url.QueryEscape(q)
			fmt.Fprintln(out)
			ui.SectionHeader(out, "Hub search: "+q)
			resp, err := http.Get(endpoint)
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			if resp.StatusCode != http.StatusOK {
				fmt.Fprintln(out, "  "+ui.Error(fmt.Sprintf("hub returned %d: %s", resp.StatusCode, string(body))))
				return nil
			}
			var data struct {
				Skills []map[string]interface{} `json:"skills"`
				Count  int                      `json:"count"`
			}
			_ = json.Unmarshal(body, &data)
			if data.Count == 0 {
				ui.Hint(out, "No matches.")
				fmt.Fprintln(out)
				return nil
			}
			fmt.Fprintln(out)
			for _, s := range data.Skills {
				fmt.Fprintf(out, "  %s  %s  %s\n",
					ui.Brand(fmt.Sprintf("%-16s", s["name"])),
					ui.Body(fmt.Sprintf("%v", s["version"])),
					ui.Mute("by "+fmt.Sprintf("%v", s["author"])),
				)
				if d, ok := s["description"].(string); ok && d != "" {
					fmt.Fprintf(out, "    %s\n", ui.Mute(d))
				}
			}
			fmt.Fprintln(out)
			ui.Hint(out, fmt.Sprintf("%d match(es)", data.Count))
			fmt.Fprintln(out)
			return nil
		},
	}
}

func hubStatsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stats",
		Short: "Show hub statistics",
		RunE: func(cmd *cobra.Command, args []string) error {
			resp, err := http.Get(hubBaseURL() + "/hub/v1/stats")
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
			var stats struct {
				Skills         int `json:"skills"`
				TotalDownloads int `json:"totalDownloads"`
			}
			_ = json.Unmarshal(body, &stats)
			out := cmd.OutOrStdout()
			fmt.Fprintln(out)
			ui.SectionHeader(out, "Fathom Secure Hub")
			ui.KV(out, "skills", fmt.Sprintf("%d", stats.Skills))
			ui.KV(out, "downloads", fmt.Sprintf("%d total", stats.TotalDownloads))
			fmt.Fprintln(out)
			return nil
		},
	}
}

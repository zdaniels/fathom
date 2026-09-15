package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/zdaniels/fathom/internal/brandenv"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/zdaniels/fathom/internal/cli/ui"
	"github.com/zdaniels/fathom/internal/relay"
)

func init() {
	subcommands = append(subcommands, newRelayCommand)
}

// newRelayCommand parents `fathom relay {enable,disable,status}` — the
// CLI surface for the optional Fantazm-hosted relay that lets a
// mobile app reach this Fathom gateway from outside the LAN without
// requiring Tailscale or a per-user Cloudflare Tunnel.
//
// Enable mints a relay_id + secret with the relay's /enroll endpoint
// and stashes them at ~/.fantazm/relay.token. The next `fathom start`
// sees the file and spins up the outbound WebSocket client.
func newRelayCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "relay",
		Short: "Enable / disable the Fantazm-hosted mobile relay",
		Long: `Bridge your local agent to mobile clients via a Cloudflare-hosted
relay at relay.fantazm.ai, so the phone app reaches you without
LAN, Tailscale, or your own Cloudflare Tunnel.

  fathom relay enable    one-time enroll; writes ~/.fantazm/relay.token
  fathom relay disable   delete the token (revoke is a separate API call)
  fathom relay status    show current relay state

The relay sees device tokens + request bodies in transit. v2 will add
end-to-end encryption; for now treat the relay like any other trusted
TLS hop.`,
	}
	cmd.AddCommand(newRelayEnableCommand())
	cmd.AddCommand(newRelayDisableCommand())
	cmd.AddCommand(newRelayStatusCommand())
	return cmd
}

func newRelayEnableCommand() *cobra.Command {
	var enrollURL string
	var apiKey string
	var name string
	cmd := &cobra.Command{
		Use:   "enable",
		Short: "Enroll this gateway with the relay and persist credentials",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if apiKey == "" {
				apiKey = brandenv.Get("FATHOM_RELAY_ENROLL_KEY")
			}
			if apiKey == "" {
				return fmt.Errorf("missing enroll key — pass --api-key or set FANTAZM_RELAY_ENROLL_KEY")
			}
			if name == "" {
				host, _ := os.Hostname()
				name = host
			}

			out := cmd.OutOrStdout()
			ui.SectionHeader(out, "Enrolling with relay")
			ui.KV(out, "endpoint", enrollURL)
			ui.KV(out, "name", name)

			cfg, err := enrollRelay(enrollURL, apiKey, name)
			if err != nil {
				return fmt.Errorf("enroll: %w", err)
			}
			path := relay.DefaultConfigPath()
			if err := relay.SaveConfig(path, cfg); err != nil {
				return fmt.Errorf("save token: %w", err)
			}
			fmt.Fprintln(out)
			ui.SectionHeader(out, "Done")
			ui.KV(out, "relay_id", cfg.RelayID)
			ui.KV(out, "agent_url", cfg.AgentURL)
			ui.KV(out, "stored", path)
			fmt.Fprintln(out)
			ui.Hint(out, "Restart `fathom start` for the relay client to come online.")
			return nil
		},
	}
	cmd.Flags().StringVar(&enrollURL, "endpoint", "https://relay.fantazm/enroll",
		"Relay enrollment endpoint (defaults to the Fantazm-hosted relay)")
	cmd.Flags().StringVar(&apiKey, "api-key", "",
		"Enrollment API key (or set FANTAZM_RELAY_ENROLL_KEY)")
	cmd.Flags().StringVar(&name, "name", "", "Human-readable name for this Fathom instance (default: hostname)")
	return cmd
}

func newRelayDisableCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "disable",
		Short: "Delete the local relay token (gateway will stop dialing on next start)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			path := relay.DefaultConfigPath()
			if _, err := os.Stat(path); os.IsNotExist(err) {
				ui.Hint(cmd.OutOrStdout(), "Relay is already disabled (no token at "+path+").")
				return nil
			}
			if err := os.Remove(path); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "  ✓ Deleted "+path)
			ui.Hint(cmd.OutOrStdout(), "Restart `fathom start` to drop the relay connection.")
			return nil
		},
	}
}

func newRelayStatusCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show the relay token + whether a connection is up",
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			path := relay.DefaultConfigPath()
			cfg, err := relay.LoadConfig(path)
			if os.IsNotExist(err) {
				ui.SectionHeader(out, "Relay disabled")
				fmt.Fprintln(out, "  No token at "+path)
				fmt.Fprintln(out)
				ui.Hint(out, "Run `fathom relay enable` to enroll.")
				return nil
			}
			if err != nil {
				return err
			}
			ui.SectionHeader(out, "Relay enabled")
			ui.KV(out, "relay_id", cfg.RelayID)
			ui.KV(out, "agent_url", cfg.AgentURL)
			ui.KV(out, "token_path", path)
			// Liveness check — hit the relay's /status/{rid} endpoint.
			statusURL := deriveStatusURL(cfg)
			if statusURL != "" {
				if live := fetchRelayStatus(statusURL); live != "" {
					ui.KV(out, "live", live)
				} else {
					ui.KV(out, "live", "unreachable")
				}
			}
			return nil
		},
	}
}

// enrollRelay POSTs the enrollment request and returns the relay
// Config to persist. The server responds with relay_id, secret,
// agent_url, client_url.
func enrollRelay(endpoint, apiKey, name string) (relay.Config, error) {
	body, _ := json.Marshal(map[string]string{"name": name})
	req, _ := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return relay.Config{}, err
	}
	defer res.Body.Close()
	respBody, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK {
		return relay.Config{}, fmt.Errorf("HTTP %s: %s", res.Status, strings.TrimSpace(string(respBody)))
	}
	var c relay.Config
	if err := json.Unmarshal(respBody, &c); err != nil {
		return relay.Config{}, fmt.Errorf("parse response: %w", err)
	}
	return c, nil
}

// deriveStatusURL turns wss://relay.../agent/{rid} → https://relay.../status/{rid}.
// Hand-rolled rather than parsing because the relay's URL structure is
// stable and the transform is one-off.
func deriveStatusURL(c relay.Config) string {
	u := c.AgentURL
	u = strings.Replace(u, "wss://", "https://", 1)
	u = strings.Replace(u, "ws://", "http://", 1)
	u = strings.Replace(u, "/agent/", "/status/", 1)
	return u
}

func fetchRelayStatus(url string) string {
	client := &http.Client{Timeout: 5 * time.Second}
	res, err := client.Get(url)
	if err != nil {
		return ""
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return ""
	}
	body, _ := io.ReadAll(res.Body)
	var s struct {
		AgentConnected bool `json:"agent_connected"`
		OpenStreams    int  `json:"open_streams"`
	}
	if err := json.Unmarshal(body, &s); err != nil {
		return ""
	}
	if s.AgentConnected {
		return fmt.Sprintf("connected (%d open stream(s))", s.OpenStreams)
	}
	return "offline"
}

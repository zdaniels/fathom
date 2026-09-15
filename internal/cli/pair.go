package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/zdaniels/fathom/internal/brandenv"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/skip2/go-qrcode"
	"github.com/spf13/cobra"
	"github.com/zdaniels/fathom/internal/cli/ui"
	"github.com/zdaniels/fathom/internal/config"
	"github.com/zdaniels/fathom/internal/relay"
)

func init() {
	subcommands = append(subcommands, newPairCommand)
	subcommands = append(subcommands, newDevicesCommand)
}

// newPairCommand prints a QR code + 6-digit number that a phone can use
// to pair against this Fathom instance. Blocks until the code is
// claimed (~60s) and then prints the device that joined.
//
// The QR encodes a deep link of the form
//
//	https://<host>/?pair_code=<code>
//
// so phones that scan it land directly on the chat UI with the pairing
// form pre-filled. Host detection priority:
//
//  1. --host flag (explicit)
//  2. --tunnel — pair against an active Cloudflare Tunnel (looks up
//     the URL Fathom printed at boot; not implemented in this pass)
//  3. Tailscale IP (interface starting with tailscale*/utun + Tailscale
//     magic-DNS suffix)
//  4. First non-loopback LAN IPv4
//  5. localhost (only useful for testing on the laptop itself)
func newPairCommand() *cobra.Command {
	var hostOverride string
	var port int
	cmd := &cobra.Command{
		Use:   "pair",
		Short: "Pair a phone or other device with this Fathom instance",
		Long: `Pair a phone or other device with this Fathom instance.

Generates a 6-digit pairing code, prints a QR code linking to the
mobile chat UI, and blocks until a device claims the code.

The QR encodes the Fathom URL + code, so phones with a camera-aware
browser jump straight to the chat with the code pre-filled.

Prerequisites:
  - 'fathom serve' must be running (this command talks to the gateway).
  - For remote pairing, the phone needs network reach: Tailscale,
    Cloudflare Tunnel (via 'fathom serve --tunnel'), or same LAN.

To revoke a paired device later:
  fathom devices list
  fathom devices revoke <id>`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg := config.LoadConfig("")
			if port == 0 {
				port = cfg.Port
			}
			if port == 0 {
				port = 8790
			}
			gatewayURL := fmt.Sprintf("http://127.0.0.1:%d", port)
			token, err := loadAdminToken()
			if err != nil {
				return fmt.Errorf("read admin token: %w (is `fathom serve` running and have you completed first-boot setup?)", err)
			}

			// Step 1: start the pairing on the gateway.
			pc, err := startPair(gatewayURL, token)
			if err != nil {
				return err
			}

			// Step 2: figure out the host to put in the QR code.
			displayHost := hostOverride
			if displayHost == "" {
				displayHost = detectAdvertiseHost(port)
			}
			pairURL := fmt.Sprintf("%s/?pair_code=%s", strings.TrimRight(displayHost, "/"), pc.Code)

			// Step 3: render the QR + code to the terminal.
			renderPairingScreen(cmd.OutOrStdout(), pairURL, pc.Code, pc.ExpiresAt, displayHost)

			// Step 4: long-poll until the code is claimed (or expired).
			dev, err := watchPair(gatewayURL, token, pc.Code)
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout())
			fmt.Fprintf(cmd.OutOrStdout(), "  ✓ Paired: %s (id=%s)\n", dev.Name, dev.ID)
			fmt.Fprintln(cmd.OutOrStdout())
			return nil
		},
	}
	cmd.Flags().StringVar(&hostOverride, "host", "", "Override the host URL encoded in the QR (e.g. https://abc.tunnel.app or http://100.x.y.z:8790)")
	cmd.Flags().IntVar(&port, "port", 0, "Gateway port (defaults to config)")
	return cmd
}

// newDevicesCommand parents `devices list` + `devices revoke <id>`.
func newDevicesCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "devices",
		Short: "Manage paired devices (phones, tablets, second laptops)",
	}
	cmd.AddCommand(newDevicesListCommand())
	cmd.AddCommand(newDevicesRevokeCommand())
	return cmd
}

func newDevicesListCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List paired devices",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg := config.LoadConfig("")
			port := cfg.Port
			if port == 0 {
				port = 8790
			}
			token, err := loadAdminToken()
			if err != nil {
				return err
			}
			devs, err := listDevices(fmt.Sprintf("http://127.0.0.1:%d", port), token)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(devs) == 0 {
				ui.SectionHeader(out, "No paired devices")
				fmt.Fprintln(out, "  Run `fathom pair` to pair one.")
				return nil
			}
			ui.SectionHeader(out, fmt.Sprintf("Paired devices (%d)", len(devs)))
			fmt.Fprintln(out)
			fmt.Fprintf(out, "  %-12s  %-18s  %-22s  %s\n", "ID", "NAME", "PAIRED", "LAST SEEN")
			fmt.Fprintf(out, "  %-12s  %-18s  %-22s  %s\n", "------------", "------------------", "----------------------", "----------------------")
			for _, d := range devs {
				fmt.Fprintf(out, "  %-12s  %-18s  %-22s  %s\n",
					d["id"], truncateStr(d["name"], 18), formatRelative(d["created_at"]), formatRelative(d["last_seen"]))
			}
			return nil
		},
	}
}

func newDevicesRevokeCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "revoke <id>",
		Short: "Revoke a paired device's access token",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := config.LoadConfig("")
			port := cfg.Port
			if port == 0 {
				port = 8790
			}
			token, err := loadAdminToken()
			if err != nil {
				return err
			}
			if err := revokeDevice(fmt.Sprintf("http://127.0.0.1:%d", port), token, args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "  ✓ Revoked device %s\n", args[0])
			return nil
		},
	}
}

// === HTTP helpers ===========================================================

type pairCodeResponse struct {
	Code      string    `json:"code"`
	ExpiresAt time.Time `json:"expires_at"`
}

func startPair(base, token string) (pairCodeResponse, error) {
	req, _ := http.NewRequest(http.MethodPost, base+"/api/v1/pair/start", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return pairCodeResponse{}, fmt.Errorf("contact gateway: %w", err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK {
		return pairCodeResponse{}, fmt.Errorf("pair start: %s — %s", res.Status, strings.TrimSpace(string(body)))
	}
	var pc pairCodeResponse
	if err := json.Unmarshal(body, &pc); err != nil {
		return pairCodeResponse{}, err
	}
	return pc, nil
}

type watchResponse struct {
	Claimed bool                   `json:"claimed"`
	Device  map[string]interface{} `json:"device"`
}

func watchPair(base, token, code string) (struct {
	ID   string
	Name string
}, error) {
	type out = struct {
		ID   string
		Name string
	}
	// Long-poll loop: server caps each call at ~90s; if the code's not
	// claimed by then we re-poll. Total wait bounded by code's own TTL
	// (60s default) — server returns 410 GONE after that.
	for {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet,
			base+"/api/v1/pair/watch?code="+url.QueryEscape(code), nil)
		req.Header.Set("Authorization", "Bearer "+token)
		res, err := (&http.Client{Timeout: 120 * time.Second}).Do(req)
		if err != nil {
			return out{}, fmt.Errorf("watch: %w", err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		switch res.StatusCode {
		case http.StatusOK:
			var w watchResponse
			if err := json.Unmarshal(body, &w); err != nil {
				return out{}, err
			}
			id, _ := w.Device["id"].(string)
			name, _ := w.Device["name"].(string)
			return out{ID: id, Name: name}, nil
		case http.StatusRequestTimeout:
			// Just our long-poll cap — keep waiting.
			continue
		case http.StatusGone:
			return out{}, fmt.Errorf("pairing code expired unclaimed (60s) — run `fathom pair` again")
		default:
			return out{}, fmt.Errorf("watch: %s — %s", res.Status, strings.TrimSpace(string(body)))
		}
	}
}

func listDevices(base, token string) ([]map[string]string, error) {
	req, _ := http.NewRequest(http.MethodGet, base+"/api/v1/devices", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(res.Body)
		return nil, fmt.Errorf("list devices: %s — %s", res.Status, strings.TrimSpace(string(body)))
	}
	var raw struct {
		Devices []map[string]interface{} `json:"devices"`
	}
	if err := json.NewDecoder(res.Body).Decode(&raw); err != nil {
		return nil, err
	}
	out := make([]map[string]string, 0, len(raw.Devices))
	for _, d := range raw.Devices {
		row := map[string]string{}
		for k, v := range d {
			row[k] = fmt.Sprintf("%v", v)
		}
		out = append(out, row)
	}
	return out, nil
}

func revokeDevice(base, token, id string) error {
	req, _ := http.NewRequest(http.MethodDelete, base+"/api/v1/devices/"+url.PathEscape(id), nil)
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(res.Body)
		return fmt.Errorf("revoke: %s — %s", res.Status, strings.TrimSpace(string(body)))
	}
	return nil
}

// === Local helpers ==========================================================

// loadAdminToken reads the bootstrap admin token from
// ~/.fantazm/api-token (the same file the gateway writes on first boot).
func loadAdminToken() (string, error) {
	if env := brandenv.Get("FATHOM_TOKEN_FILE"); env != "" {
		b, err := os.ReadFile(env)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(b)), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	p := filepath.Join(home, ".fantazm", "api-token")
	b, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// detectAdvertiseHost picks the URL the QR-code should encode. Priority:
//
//  1. Active fathom-hosted relay (~/.fantazm/relay.token from
//     `fathom relay enable`) — wins because it works from anywhere
//     without LAN, Tailscale, or per-user tunnel setup. Mobile clients
//     just hit relay.fantazm.ai; we proxy back to the local agent.
//  2. Active Cloudflare Tunnel (~/.fantazm/tunnel-url, from
//     `fathom start --tunnel`) — second because it also bypasses NAT,
//     but ties to one tunnel URL per launch.
//  3. Tailscale interface IP (100.64.0.0/10 CGNAT range) — secure
//     WireGuard mesh, works through NAT without exposing a public
//     surface. Requires Tailscale on both devices.
//  4. First non-loopback IPv4 LAN — fallback for same-wifi pairing.
//  5. http://localhost:<port> — only useful for self-testing.
func detectAdvertiseHost(port int) string {
	if u := readRelayClientURL(); u != "" {
		return u
	}
	if u := ReadTunnelURLFile(); u != "" {
		return u
	}
	if ip := tailscaleIP(); ip != "" {
		return fmt.Sprintf("http://%s:%d", ip, port)
	}
	if ip := lanIPv4(); ip != "" {
		return fmt.Sprintf("http://%s:%d", ip, port)
	}
	return fmt.Sprintf("http://localhost:%d", port)
}

// readRelayClientURL pulls the client-facing URL out of the relay
// token if it's enabled. Format example:
//
//	https://relay.fantazm/c/abc123…
//
// Mobile clients pointed at this URL reach the local Fathom gateway
// transparently — the Worker forwards to the outbound WS our relay
// Client holds open. See internal/relay/client.go.
func readRelayClientURL() string {
	cfg, err := relay.LoadConfig(relay.DefaultConfigPath())
	if err != nil {
		return ""
	}
	// AgentURL is wss://relay.../agent/{rid}; the client URL is
	// https://relay.../c/{rid}. Hand-roll the transform.
	u := cfg.AgentURL
	u = strings.Replace(u, "wss://", "https://", 1)
	u = strings.Replace(u, "ws://", "http://", 1)
	u = strings.Replace(u, "/agent/", "/c/", 1)
	return u
}

// tailscaleIP returns the first IP in Tailscale's CGNAT range
// (100.64.0.0/10) on any local interface, or "" if none.
func tailscaleIP() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	_, cgnat, _ := net.ParseCIDR("100.64.0.0/10")
	for _, a := range addrs {
		ipNet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip4 := ipNet.IP.To4()
		if ip4 != nil && cgnat.Contains(ip4) {
			return ip4.String()
		}
	}
	return ""
}

// lanIPv4 returns the first non-loopback, non-link-local IPv4 from any
// up interface. Best-effort — works for the common single-NIC laptop.
func lanIPv4() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, a := range addrs {
		ipNet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip4 := ipNet.IP.To4()
		if ip4 == nil || ip4.IsLoopback() || ip4.IsLinkLocalUnicast() {
			continue
		}
		// Skip CGNAT (Tailscale) — that's tailscaleIP()'s job; this
		// helper is the "ordinary LAN" fallback.
		if ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127 {
			continue
		}
		return ip4.String()
	}
	return ""
}

// renderPairingScreen prints the QR + code + instructions to the
// terminal. QR is ANSI-rendered via go-qrcode's String() helper. We
// also print the URL in case the user's terminal mangles the QR.
func renderPairingScreen(w io.Writer, pairURL, code string, expiresAt time.Time, host string) {
	ui.SectionHeader(w, "Pair a device")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "  Scan this QR with your phone (or open the URL):")
	fmt.Fprintln(w)

	qr, err := qrcode.New(pairURL, qrcode.Low)
	if err == nil {
		// Render at small size — terminal QRs are at most ~37 cells
		// wide for v6, fine for camera scanning.
		buf := &bytes.Buffer{}
		fmt.Fprintln(buf, qr.ToSmallString(false))
		// Indent each line for visual alignment.
		for _, line := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
			fmt.Fprintln(w, "  "+line)
		}
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "  URL:   %s\n", pairURL)
	fmt.Fprintf(w, "  Code:  %s\n", formatCodeWithSpaces(code))
	fmt.Fprintf(w, "  Host:  %s\n", host)
	fmt.Fprintf(w, "  Valid: %s\n", formatTTL(time.Until(expiresAt)))
	fmt.Fprintln(w)
	fmt.Fprintln(w, "  Waiting for a device to claim this code…")
}

// formatCodeWithSpaces turns "123456" into "123 456" for typing ease.
func formatCodeWithSpaces(code string) string {
	if len(code) == 6 {
		return code[0:3] + " " + code[3:6]
	}
	return code
}

func formatTTL(d time.Duration) string {
	if d <= 0 {
		return "expired"
	}
	return d.Truncate(time.Second).String()
}

// formatRelative turns an ISO 8601 timestamp string into "5 minutes ago".
// Returns the original on parse failure (so device-list rows never
// disappear due to a bad timestamp).
func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + "…"
}

func formatRelative(ts string) string {
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return ts
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

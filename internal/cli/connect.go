package cli

// `fathom connect <service>` — interactive, browser-based account
// linking. Today: GitHub via the OAuth Device Flow.
//
// Device flow fits a local CLI/agent: no redirect server, no client
// secret. We ask GitHub for a device code, pop the user's browser to
// github.com/login/device with the code pre-filled, poll until they
// authorize, then store the resulting token in the encrypted vault
// (scoped so only the github skill can read it).

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/zdaniels/fathom/internal/brandenv"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/zdaniels/fathom/internal/cli/ui"
	"github.com/zdaniels/fathom/internal/config"
)

func init() {
	subcommands = append(subcommands, newConnectCommand)
}

// Endpoint URLs are vars (not consts) so tests can point them at a mock
// server. Not meant to be reconfigured at runtime.
var (
	ghDeviceCodeURL = "https://github.com/login/device/code"
	ghTokenURL      = "https://github.com/login/oauth/access_token"
	ghUserURL       = "https://api.github.com/user"
)

func newConnectCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "connect",
		Short: "Connect external accounts (GitHub, …) via browser sign-in",
		Long: `Link an external account to this Fathom instance using a browser
sign-in. The resulting token is stored in the encrypted vault and scoped
to the matching skill.`,
	}
	cmd.AddCommand(newConnectGitHubCommand())
	return cmd
}

func newConnectGitHubCommand() *cobra.Command {
	var clientID, scopes string
	c := &cobra.Command{
		Use:   "github",
		Short: "Connect a GitHub account via browser (device flow)",
		Long: `Opens your browser to github.com/login/device, shows you a one-time
code to authorize, then stores the token as GITHUB_TOKEN in the vault
(scoped to the github skill).

Needs a GitHub OAuth App client ID with Device Flow enabled. Provide it
via --client-id, the FANTAZM_GITHUB_CLIENT_ID env var, or
integrations.github.oauthClientId in fathom.config.yaml. Register an
OAuth App at https://github.com/settings/developers (enable "Device Flow").`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return connectGitHub(cmd, clientID, scopes)
		},
	}
	c.Flags().StringVar(&clientID, "client-id", "", "GitHub OAuth App client ID (device-flow enabled)")
	c.Flags().StringVar(&scopes, "scopes", "repo read:org notifications", "Space-separated OAuth scopes to request")
	return c
}

// resolveGitHubClientID: flag > env > config.
func resolveGitHubClientID(flag string) string {
	if flag != "" {
		return flag
	}
	if v := brandenv.Get("FATHOM_GITHUB_CLIENT_ID"); v != "" {
		return v
	}
	cfg := config.LoadConfig("")
	if cfg.Integrations != nil && cfg.Integrations.GitHub != nil {
		return cfg.Integrations.GitHub.OAuthClientID
	}
	return ""
}

type ghDeviceCode struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
}

type ghTokenResp struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	Scope       string `json:"scope"`
	Error       string `json:"error"`
	ErrorDesc   string `json:"error_description"`
}

func connectGitHub(cmd *cobra.Command, clientIDFlag, scopes string) error {
	out := cmd.OutOrStdout()
	clientID := resolveGitHubClientID(clientIDFlag)
	if clientID == "" {
		return fmt.Errorf("no GitHub OAuth client ID configured.\n" +
			"  Register an OAuth App at https://github.com/settings/developers\n" +
			"  (enable \"Device Flow\"), then re-run with --client-id <id>,\n" +
			"  set FANTAZM_GITHUB_CLIENT_ID, or add integrations.github.oauthClientId to your config.")
	}

	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}

	// 1. Request a device + user code.
	dc, err := ghRequestDeviceCode(ctx, clientID, scopes)
	if err != nil {
		return fmt.Errorf("requesting device code: %w", err)
	}

	// 2. Show the code + open the browser (pre-filled when GitHub provides it).
	verifyURL := dc.VerificationURI
	openURL := verifyURL + "?user_code=" + url.QueryEscape(dc.UserCode)
	fmt.Fprintln(out)
	ui.Hint(out, "To connect GitHub, authorize this device:")
	fmt.Fprintln(out, "    code:  "+ui.Bold(dc.UserCode))
	fmt.Fprintln(out, "    open:  "+verifyURL)
	fmt.Fprintln(out)
	if err := openInBrowser(openURL); err != nil {
		ui.Hint(out, "Couldn't open your browser automatically — visit the URL above and enter the code.")
	} else {
		ui.Hint(out, "Opened your browser. Enter the code above if it isn't pre-filled.")
	}
	fmt.Fprintln(out)

	// 3. Poll for the token until the user authorizes (or the code expires).
	ui.Hint(out, "Waiting for authorization…")
	token, err := ghPollForToken(ctx, clientID, dc)
	if err != nil {
		return err
	}

	// 4. Confirm who we connected as, then store the token (scoped to github).
	login := ghWhoAmI(ctx, token)
	v, err := openVault()
	if err != nil {
		return fmt.Errorf("open vault: %w", err)
	}
	if err := v.Set("GITHUB_TOKEN", token, []string{"github"}); err != nil {
		return fmt.Errorf("store token: %w", err)
	}
	fmt.Fprintln(out)
	who := "your GitHub account"
	if login != "" {
		who = ui.Bold(login)
	}
	ui.Hint(out, "✓ Connected as "+who+" — token stored in the vault (scoped to the github skill).")
	ui.Hint(out, "Restart the gateway to pick it up: fathom start")
	return nil
}

func ghRequestDeviceCode(ctx context.Context, clientID, scopes string) (*ghDeviceCode, error) {
	form := url.Values{"client_id": {clientID}, "scope": {scopes}}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, ghDeviceCodeURL, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github returned status %d (is the client ID valid + device flow enabled?)", resp.StatusCode)
	}
	var dc ghDeviceCode
	if err := json.NewDecoder(resp.Body).Decode(&dc); err != nil {
		return nil, err
	}
	if dc.DeviceCode == "" || dc.UserCode == "" {
		return nil, fmt.Errorf("github returned an empty device code")
	}
	if dc.Interval <= 0 {
		dc.Interval = 5
	}
	return &dc, nil
}

func ghPollForToken(ctx context.Context, clientID string, dc *ghDeviceCode) (string, error) {
	interval := time.Duration(dc.Interval) * time.Second
	deadline := time.Now().Add(time.Duration(dc.ExpiresIn) * time.Second)
	if dc.ExpiresIn <= 0 {
		deadline = time.Now().Add(15 * time.Minute)
	}
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(interval):
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("the device code expired before authorization — run `fathom connect github` again")
		}
		tr, err := ghExchange(ctx, clientID, dc.DeviceCode)
		if err != nil {
			return "", err
		}
		switch {
		case tr.AccessToken != "":
			return tr.AccessToken, nil
		case tr.Error == "authorization_pending":
			// keep waiting
		case tr.Error == "slow_down":
			interval += 5 * time.Second
		case tr.Error == "expired_token":
			return "", fmt.Errorf("the device code expired — run `fathom connect github` again")
		case tr.Error == "access_denied":
			return "", fmt.Errorf("authorization was denied")
		case tr.Error != "":
			return "", fmt.Errorf("github error: %s (%s)", tr.Error, tr.ErrorDesc)
		}
	}
}

func ghExchange(ctx context.Context, clientID, deviceCode string) (*ghTokenResp, error) {
	form := url.Values{
		"client_id":   {clientID},
		"device_code": {deviceCode},
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, ghTokenURL, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var tr ghTokenResp
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return nil, err
	}
	return &tr, nil
}

// ghWhoAmI returns the authenticated login, or "" if the lookup fails
// (non-fatal — we still store the token).
func ghWhoAmI(ctx context.Context, token string) string {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ghUserURL, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	var u struct {
		Login string `json:"login"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&u)
	return u.Login
}

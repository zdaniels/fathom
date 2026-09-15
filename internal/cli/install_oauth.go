package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/zdaniels/fathom/internal/cli/ui"
	"github.com/zdaniels/fathom/internal/security"
	"github.com/zdaniels/fathom/internal/skills"
)

// oauthSkillSpec describes how to run the OAuth flow for one provider.
// We hard-code the consent + token endpoints per known provider since
// they're stable URLs and putting them in YAML config would just be a
// second source of truth.
type oauthSkillSpec struct {
	AuthURL  string
	TokenURL string
	Scopes   []string
}

var oauthSpecs = map[string]oauthSkillSpec{
	// Google-family — gmail, calendar, gchat. Same auth+token endpoints,
	// different scopes per skill.
	"gmail": {
		AuthURL:  "https://accounts.google.com/o/oauth2/v2/auth",
		TokenURL: "https://oauth2.googleapis.com/token",
		Scopes: []string{
			"https://www.googleapis.com/auth/gmail.readonly",
			"https://www.googleapis.com/auth/gmail.send",
			"https://www.googleapis.com/auth/gmail.modify",
		},
	},
	"calendar": {
		AuthURL:  "https://accounts.google.com/o/oauth2/v2/auth",
		TokenURL: "https://oauth2.googleapis.com/token",
		Scopes:   []string{"https://www.googleapis.com/auth/calendar"},
	},
	"gchat": {
		AuthURL:  "https://accounts.google.com/o/oauth2/v2/auth",
		TokenURL: "https://oauth2.googleapis.com/token",
		Scopes: []string{
			"https://www.googleapis.com/auth/chat.messages",
			"https://www.googleapis.com/auth/chat.spaces.readonly",
		},
	},
	// Microsoft — Teams uses the Microsoft identity platform. The tenant
	// in the URL is "common" by default (works for personal + work
	// accounts); the user can override by setting TEAMS_TENANT_ID in
	// their GCP equivalent. Scopes use the Graph .default style.
	"teams": {
		AuthURL:  "https://login.microsoftonline.com/common/oauth2/v2.0/authorize",
		TokenURL: "https://login.microsoftonline.com/common/oauth2/v2.0/token",
		Scopes: []string{
			"https://graph.microsoft.com/Chat.ReadWrite",
			"https://graph.microsoft.com/ChannelMessage.Send",
			"https://graph.microsoft.com/Team.ReadBasic.All",
			"offline_access",
		},
	},
}

// runOAuthFor walks the user through Google-style OAuth for a skill.
// Steps:
//
//  1. Prompt for CLIENT_ID + CLIENT_SECRET (from their GCP project).
//     Skipped if both are already in the vault.
//  2. Spin up the localhost callback + open the consent URL.
//  3. Exchange the code for a refresh_token.
//  4. Stash refresh_token in the vault scoped to this skill.
func runOAuthFor(ctx context.Context, out io.Writer, name string, skill *skills.BundledSkill, vault *security.SecretsVault) error {
	spec, ok := oauthSpecs[name]
	if !ok {
		return fmt.Errorf("oauth flow for %q is not implemented yet — set the secrets manually with `fathom vault set`", name)
	}

	// Pull the three secret names declared in the manifest.
	var clientIDKey, clientSecretKey, refreshTokenKey string
	for _, s := range skill.Manifest.Permissions.Secrets {
		switch {
		case strings.HasSuffix(s, "_CLIENT_ID"):
			clientIDKey = s
		case strings.HasSuffix(s, "_CLIENT_SECRET"):
			clientSecretKey = s
		case strings.HasSuffix(s, "_REFRESH_TOKEN"):
			refreshTokenKey = s
		}
	}
	if clientIDKey == "" || clientSecretKey == "" || refreshTokenKey == "" {
		return fmt.Errorf("oauth: manifest for %s is missing one of CLIENT_ID/CLIENT_SECRET/REFRESH_TOKEN secret names", name)
	}

	// Prompt for client credentials if not already in vault.
	clientID, err := getOrAskSecret(out, vault, clientIDKey, "OAuth client ID (from your GCP project): ", name)
	if err != nil {
		return err
	}
	clientSecret, err := getOrAskSecret(out, vault, clientSecretKey, "OAuth client secret: ", name)
	if err != nil {
		return err
	}

	fmt.Fprintln(out)
	ui.Hint(out, "Setup prerequisites in the Google Cloud Console:")
	ui.Hint(out, "  1. Create or pick an OAuth 2.0 Client ID of type \"Desktop app\".")
	ui.Hint(out, "  2. Add http://127.0.0.1 to authorized redirect URIs (any port — fathom picks one).")
	ui.Hint(out, "  3. Enable the relevant API for "+name+" in APIs & Services → Library.")

	flow := &oauthFlow{
		AuthURL:      spec.AuthURL,
		TokenURL:     spec.TokenURL,
		ClientID:     clientID,
		ClientSecret: clientSecret,
		Scopes:       spec.Scopes,
		AccessType:   "offline",
		Prompt:       "consent",
	}
	result, err := flow.Run(ctx, out)
	if err != nil {
		return fmt.Errorf("oauth: %w", err)
	}
	if err := vault.Set(refreshTokenKey, result.RefreshToken, []string{name}); err != nil {
		return fmt.Errorf("vault set %s: %w", refreshTokenKey, err)
	}
	fmt.Fprintln(out, "  "+ui.Success("Stored "+refreshTokenKey+" in vault"))
	return nil
}

func getOrAskSecret(out io.Writer, vault *security.SecretsVault, key, prompt, scope string) (string, error) {
	if vault.Has(key) {
		val, err := vault.Get(key, scope)
		if err == nil && val != "" {
			fmt.Fprintln(out, "  "+ui.Mute(key+" already in vault — reusing"))
			return val, nil
		}
	}
	ui.Field(out, prompt)
	reader := bufio.NewReader(os.Stdin)
	line, _ := reader.ReadString('\n')
	val := strings.TrimSpace(line)
	if val == "" {
		return "", fmt.Errorf("%s required", key)
	}
	if err := vault.Set(key, val, []string{scope}); err != nil {
		return "", fmt.Errorf("vault set %s: %w", key, err)
	}
	return val, nil
}

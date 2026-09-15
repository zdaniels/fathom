package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/zdaniels/fathom/internal/brandenv"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/zdaniels/fathom/internal/cli/ui"
	"github.com/zdaniels/fathom/internal/security"
	"github.com/zdaniels/fathom/internal/skills"
)

func init() {
	subcommands = append(subcommands, newInstallCommand, newSkillsCommand)
}

// newInstallCommand: `fathom install [skill]`. With no args, lists every
// skill bundled in the binary. With a skill name, runs the install flow:
// extracts the skill files to ./skills/<name>/ (or
// ~/.local/share/fantazm/skills/<name>/ with --global), prompts for the
// secrets declared in the manifest, runs OAuth where applicable, stashes
// everything in the vault.
func newInstallCommand() *cobra.Command {
	var globalFlag bool
	cmd := &cobra.Command{
		Use:   "install [skill]",
		Short: "Install a bundled skill (gmail, slack, github, …)",
		Long: `Install a skill that ships with Fathom. With no argument, lists every
available skill. With a skill name, copies it to your skills directory
and walks you through the credential setup.

Where it installs:
  default     ./skills/<name>/         (per-project)
  --global    ~/.local/share/fantazm/skills/<name>/  (works from anywhere)

For OAuth-based skills (gmail, calendar) you'll need to set up an
OAuth client in the provider's developer console first — the install
flow tells you what to do.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			security.SetLevel("warn")
			slog.SetDefault(slog.New(slog.NewTextHandler(
				os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))

			out := cmd.OutOrStdout()
			if len(args) == 0 {
				return listAvailable(out)
			}
			return installSkill(cmd.Context(), out, args[0], globalFlag)
		},
	}
	cmd.Flags().BoolVar(&globalFlag, "global", false, "Install to ~/.local/share/fantazm/skills/ (works from anywhere) instead of ./skills/")
	return cmd
}

// newSkillsCommand: `fathom skills {list,remove}` — manage installed skills.
func newSkillsCommand() *cobra.Command {
	root := &cobra.Command{Use: "skills", Short: "Manage installed skills"}
	root.AddCommand(skillsListCmd(), skillsRemoveCmd())
	return root
}

func listAvailable(out interface {
	Write(p []byte) (n int, err error)
}) error {
	bundled, err := skills.ListBundled()
	if err != nil {
		return err
	}
	fmt.Fprintln(out)
	ui.SectionHeader(out, fmt.Sprintf("%d skills available", len(bundled)))
	fmt.Fprintln(out)
	for _, s := range bundled {
		name := ui.Brand(fmt.Sprintf("%-14s", s.Name))
		desc := s.Manifest.Description
		if desc == "" {
			desc = "no description"
		}
		fmt.Fprintf(out, "  %s  %s\n", name, ui.Body(desc))
	}
	fmt.Fprintln(out)
	ui.Hint(out, "Install with: fathom install <name>           (per-project)")
	ui.Hint(out, "             fathom install --global <name>   (works anywhere)")
	fmt.Fprintln(out)
	return nil
}

func installSkill(ctx context.Context, out interface {
	Write(p []byte) (n int, err error)
}, name string, global bool) error {
	skill, err := skills.GetBundled(name)
	if err != nil {
		return err
	}
	dest, err := resolveInstallDest(name, global)
	if err != nil {
		return err
	}

	fmt.Fprintln(out)
	ui.SectionHeader(out, "Installing "+name)
	ui.KV(out, "target", dest)
	ui.KV(out, "version", skill.Manifest.Version)
	ui.KV(out, "author", skill.Manifest.Author)
	if skill.Manifest.Description != "" {
		ui.KV(out, "what", skill.Manifest.Description)
	}
	fmt.Fprintln(out)

	if _, err := os.Stat(dest); err == nil {
		ui.Hint(out, "Already installed at "+dest+". Re-running will refresh source files and re-prompt for secrets.")
	}

	if err := skills.ExtractBundled(name, dest); err != nil {
		return fmt.Errorf("extract: %w", err)
	}
	fmt.Fprintln(out, "  "+ui.Success("Files copied"))

	// If the skill ships a package.json, run npm install so its deps land.
	// We pipe npm's output through so the user sees the long-running install
	// (Baileys, Playwright, etc. are not small).
	if _, err := os.Stat(filepath.Join(dest, "package.json")); err == nil {
		fmt.Fprintln(out, "  "+ui.Mute("installing npm dependencies (this takes a minute on first install)…"))
		if err := runNpmInstall(ctx, out, dest); err != nil {
			return fmt.Errorf("npm install: %w", err)
		}
		fmt.Fprintln(out, "  "+ui.Success("Dependencies installed"))

		// Some skills have a post-install hook (e.g. Playwright needs to
		// download Chromium). Detect via "fantazm:postinstall" script.
		if err := runFathomPostInstall(ctx, out, dest); err != nil {
			return fmt.Errorf("postinstall: %w", err)
		}
	}
	fmt.Fprintln(out)

	// Open vault for secret storage.
	v, err := openVaultForInit()
	if err != nil {
		return fmt.Errorf("open vault: %w", err)
	}

	// OAuth providers need orchestration. Detect by the secret names.
	hasOAuth := false
	for _, s := range skill.Manifest.Permissions.Secrets {
		if strings.HasSuffix(s, "_REFRESH_TOKEN") {
			hasOAuth = true
			break
		}
	}
	switch {
	case name == "whatsapp":
		// WhatsApp has no secrets in the manifest — it pairs by QR. After
		// npm install we spawn the skill's pair() entrypoint, which prints
		// the QR to scan from the phone.
		if err := runWhatsAppPair(ctx, out, dest); err != nil {
			return err
		}
	case hasOAuth:
		if err := runOAuthFor(ctx, out, name, skill, v); err != nil {
			return err
		}
	default:
		// Plain secret prompts.
		reader := bufio.NewReader(os.Stdin)
		for _, secretName := range skill.Manifest.Permissions.Secrets {
			if v.Has(secretName) {
				fmt.Fprintln(out, "  "+ui.Mute(secretName+" already in vault — keeping current value"))
				continue
			}
			ui.Field(out, secretName+": ")
			line, _ := reader.ReadString('\n')
			val := strings.TrimSpace(line)
			if val == "" {
				fmt.Fprintln(out, "  "+ui.Warn(secretName+" left empty — skill won't work until you `fathom vault set "+secretName+"`"))
				continue
			}
			if err := v.Set(secretName, val, []string{name}); err != nil {
				return fmt.Errorf("vault set %s: %w", secretName, err)
			}
			fmt.Fprintln(out, "  "+ui.Success("Stored "+secretName))
		}
	}

	fmt.Fprintln(out)
	ui.SectionHeader(out, ui.Success(name+" ready"))
	ui.Hint(out, "Restart `fathom chat` to pick up the new skill.")
	fmt.Fprintln(out)
	return nil
}

func resolveInstallDest(name string, global bool) (string, error) {
	if global {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, ".local", "share", "fantazm", "skills", name), nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return filepath.Join(cwd, "skills", name), nil
}

func skillsListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List installed skills (project + global)",
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			fmt.Fprintln(out)
			ui.SectionHeader(out, "Installed skills")
			fmt.Fprintln(out)
			seen := map[string]bool{}
			report := func(dir, label string) {
				entries, err := os.ReadDir(dir)
				if err != nil {
					return
				}
				for _, e := range entries {
					if !e.IsDir() || seen[e.Name()] {
						continue
					}
					manifestPath := filepath.Join(dir, e.Name(), "skill.manifest.yaml")
					if _, err := os.Stat(manifestPath); err != nil {
						continue
					}
					seen[e.Name()] = true
					fmt.Fprintf(out, "  %s  %s  %s\n",
						ui.Brand(fmt.Sprintf("%-14s", e.Name())),
						ui.Body(label),
						ui.Mute(filepath.Join(dir, e.Name())),
					)
				}
			}
			cwd, _ := os.Getwd()
			report(filepath.Join(cwd, "skills"), "project")
			if home, err := os.UserHomeDir(); err == nil {
				report(filepath.Join(home, ".local", "share", "fantazm", "skills"), "global")
			}
			if len(seen) == 0 {
				ui.Hint(out, "No skills installed yet. Run `fathom install` to see what's available.")
			}
			fmt.Fprintln(out)
			return nil
		},
	}
}

// runWhatsAppPair invokes the WhatsApp skill in pair mode so the QR shows
// up in the terminal. Auth state lands at ~/.fantazm/whatsapp-auth/; once
// paired, normal tool calls reconnect silently using that state.
//
// The driver script DOES NOT call the skill's pair() export — that
// indirection bit us when an `import()` rejection silently exited code
// 0 and the CLI reported "paired" when nothing happened. Instead we
// inline the connect + wait-for-success loop in the driver so success
// is unambiguous: process exits 0 only when the connection transitions
// to "open" AND we see a `sock.user` populated. Any other exit is
// surfaced as a failure.
func runWhatsAppPair(ctx context.Context, out interface {
	Write(p []byte) (n int, err error)
}, skillDir string) error {
	fmt.Fprintln(out, "  "+ui.Mute("starting WhatsApp pair flow…"))
	fmt.Fprintln(out, "  "+ui.Mute("a QR code will appear — scan it with WhatsApp → Settings → Linked Devices → Link a device"))
	fmt.Fprintln(out, "  "+ui.Mute("the QR may refresh once or twice while you scan; that's normal"))
	fmt.Fprintln(out)

	// Clear any partial creds from previous failed attempts. Baileys saves
	// noise-handshake state to creds.json before the pair completes, and a
	// half-paired state will confuse the next attempt (WhatsApp sees a
	// "returning device" with no completed pair and rejects with 401/405).
	authDir := brandenv.Get("FATHOM_WHATSAPP_AUTH_DIR")
	if authDir == "" {
		if home, err := os.UserHomeDir(); err == nil {
			authDir = filepath.Join(home, ".fantazm", "whatsapp-auth")
		}
	}
	if authDir != "" && !hasValidPairedCreds(authDir) {
		// No valid pair on disk — safe to wipe pre-flight junk. If creds.json
		// exists with > 0 bytes the pair completed on a previous run and
		// we leave the dir alone (re-running install is a no-op the user
		// can use to re-verify, but we must not destroy a working session).
		_ = os.RemoveAll(authDir)
	}

	cctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	driver := `
import makeWASocket, { useMultiFileAuthState, DisconnectReason } from "@whiskeysockets/baileys";
import qrcode from "qrcode-terminal";
import pino from "pino";
import { join } from "node:path";

const authDir = process.env.FANTAZM_WHATSAPP_AUTH_DIR
  || join(process.env.HOME ?? ".", ".fantazm", "whatsapp-auth");

// connectOnce opens a Baileys socket and waits for one of three terminal
// states:
//   • connection.open  → success, return { ok: true }
//   • close, code 515  → stream-restart-required: WhatsApp wants us to
//     reconnect to finalize the pair. Return { ok: "retry" } so the
//     outer loop creates a fresh socket. This is the documented happy
//     path immediately after a successful QR scan; treating it as
//     failure would orphan the pair (creds saved client-side, but
//     WhatsApp never finishes registering the linked device).
//   • close, other     → real failure. Return { ok: false, code, msg }.
async function connectOnce(showQR) {
  const { state, saveCreds } = await useMultiFileAuthState(authDir);
  const sock = makeWASocket({
    auth: state,
    printQRInTerminal: false,
    logger: pino({ level: "silent" }),
  });
  // Track every in-flight saveCreds() promise. Baileys' saveCreds is an
  // async fs.writeFile under the hood; if process.exit fires while a save
  // is in flight, creds.json gets truncated to 0 bytes (which is exactly
  // what was happening: PAIR_SUCCESS, then empty creds.json, then the
  // wrapper would say "did not finalize").
  const pendingSaves = new Set();
  sock.ev.on("creds.update", () => {
    const p = (async () => {
      try { await saveCreds(); } finally { pendingSaves.delete(p); }
    })();
    pendingSaves.add(p);
  });
  const result = await new Promise((resolve) => {
    sock.ev.on("connection.update", (u) => {
      if (u.qr && showQR) {
        qrcode.generate(u.qr, { small: true });
        process.stderr.write("\n  scan the QR above with WhatsApp on your phone.\n  if the QR refreshes you can scan either one.\n\n");
      }
      if (u.connection === "open") {
        resolve({ ok: true, sock, user: sock.user?.id ?? "unknown" });
      }
      if (u.connection === "close") {
        const code = u.lastDisconnect?.error?.output?.statusCode;
        const msg = u.lastDisconnect?.error?.message ?? "unknown";
        if (code === DisconnectReason.restartRequired || code === 515) {
          resolve({ ok: "retry", code, msg });
        } else {
          resolve({ ok: false, code, msg });
        }
      }
    });
  });
  // Drain any saveCreds writes that started but haven't finished yet,
  // then do one explicit save to capture anything emitted between the
  // last creds.update event and our resolve. Without this drain the
  // process.exit below would race the fs.writeFile and zero the file.
  await Promise.all([...pendingSaves]);
  await saveCreds();
  return result;
}

// Outer loop: first attempt shows the QR; reconnect attempts after 515
// reuse the saved state silently. Cap at 3 reconnect retries — if WhatsApp
// keeps cycling us after that, something is genuinely wrong.
let result = await connectOnce(/* showQR */ true);
let retries = 0;
while (result.ok === "retry" && retries < 3) {
  retries++;
  process.stderr.write("  reconnecting to finalize pair (" + retries + "/3)...\n");
  // Tiny backoff lets Baileys flush the prior socket cleanly.
  await new Promise(r => setTimeout(r, 1500));
  result = await connectOnce(/* showQR */ false);
}

if (result.ok === true) {
  // Final grace period: some Baileys versions emit one more creds.update
  // a beat after connection===open (the "final-keys" event). Give it a
  // moment, drain any save, then exit.
  await new Promise((r) => setTimeout(r, 1500));
  console.log("PAIR_SUCCESS:" + result.user);
  process.exit(0);
} else if (result.ok === "retry") {
  // Burned through retries — WhatsApp kept sending restartRequired.
  console.error("PAIR_FAILED:" + JSON.stringify({ ok: false, code: 515, msg: "exhausted reconnect retries", retries }));
  process.exit(2);
} else {
  console.error("PAIR_FAILED:" + JSON.stringify(result));
  process.exit(2);
}`
	cmd := exec.CommandContext(cctx, "node",
		"--experimental-strip-types", "--no-warnings",
		"--input-type=module", "-e", driver,
	)
	cmd.Dir = skillDir
	// Tee stderr so the user sees output live AND we keep a copy to parse
	// the PAIR_FAILED line for a code-specific error message.
	var stderrBuf bytes.Buffer
	cmd.Stdout = newPrefixedWriter(out, "    ")
	cmd.Stderr = io.MultiWriter(newPrefixedWriter(out, ""), &stderrBuf)
	cmd.Stdin = nil
	err := cmd.Run()
	if err != nil {
		if cctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("pair: timed out after 3 minutes — open WhatsApp → Settings → Linked Devices and scan within the time limit next try")
		}
		return fmt.Errorf("pair: %s", classifyPairFailure(stderrBuf.String()))
	}
	// Sanity check: a real PAIR_SUCCESS writes a non-empty creds.json
	// (the Baileys noise-handshake protocol persists the new device
	// keys via creds.update right before WhatsApp signals "open"). If
	// creds.json is missing or 0 bytes after the driver returned 0,
	// the success was reported prematurely and we should not claim the
	// pair finalized.
	//
	// Earlier versions checked for app-state-sync-key-*.json files
	// instead. Those only appear after the *next* session sync (which
	// can be seconds or minutes later), not at pair completion, so
	// the check produced false-negative errors on successful pairs.
	if authDir != "" && !hasValidPairedCreds(authDir) {
		return fmt.Errorf("pair: driver reported success but creds.json was not written to %s. This usually means the QR was scanned but the noise handshake didn't finalize — retry now (no waiting needed)", authDir)
	}
	fmt.Fprintln(out, "  "+ui.Success("WhatsApp paired"))
	return nil
}

// hasValidPairedCreds returns true when authDir contains a non-empty
// creds.json — the Baileys signal that a pair handshake completed and
// the session is usable. We deliberately do NOT require
// app-state-sync-key files (those only appear after subsequent session
// sync, which races against this check).
func hasValidPairedCreds(authDir string) bool {
	info, err := os.Stat(filepath.Join(authDir, "creds.json"))
	if err != nil {
		return false
	}
	return info.Size() > 0
}

// pairFailedRE finds the PAIR_FAILED:{...} line the driver writes to stderr
// on every non-success exit. We scrape the JSON to give a code-specific
// error instead of a generic "wait 30-60 min" message that's wrong for
// half the failure modes (timeouts, protocol mismatches, etc).
var pairFailedRE = regexp.MustCompile(`PAIR_FAILED:({[^\n]+})`)

type pairFailure struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	// Older driver versions emit "message" instead of "msg".
	Message string `json:"message,omitempty"`
}

// classifyPairFailure parses the driver's PAIR_FAILED JSON and returns
// a human-readable explanation specific to the Baileys/WhatsApp close
// code. Falls back to a generic message if no JSON line is present.
func classifyPairFailure(stderr string) string {
	m := pairFailedRE.FindStringSubmatch(stderr)
	if len(m) < 2 {
		return "WhatsApp pairing failed and no PAIR_FAILED diagnostic was emitted — check that node is installed and Baileys could load (re-run with verbose output if needed)"
	}
	var p pairFailure
	if err := json.Unmarshal([]byte(m[1]), &p); err != nil {
		return "WhatsApp pairing failed; could not parse driver diagnostic: " + m[1]
	}
	msg := p.Msg
	if msg == "" {
		msg = p.Message
	}
	switch p.Code {
	case 401:
		return "WhatsApp returned 401 (loggedOut) — the saved auth state is invalid. The auth dir has been cleared; retry now (no waiting needed)"
	case 405:
		return "WhatsApp returned 405 (anti-abuse) — you've made too many recent pair attempts. WhatsApp's cooldown EXTENDS with each retry, often to several hours after multiple failures. Wait at least 24 hours before trying again. Also verify in WhatsApp → Linked Devices that there are no orphaned 'Chrome' entries occupying device slots"
	case 408:
		return "WhatsApp connection timed out (408) — network blip, not anti-abuse. Try again now"
	case 428:
		return "WhatsApp returned 428 (precondition / protocol mismatch) — usually means the Baileys version is out of sync with WhatsApp's current protocol. Update the @whiskeysockets/baileys dep in the whatsapp skill and retry"
	case 440:
		return "WhatsApp returned 440 (conflict) — another client is using this session. Log out of any existing 'Chrome' device in WhatsApp → Linked Devices and try again"
	case 500, 503:
		return fmt.Sprintf("WhatsApp returned %d (server error) — transient, retry in a few minutes", p.Code)
	case 515:
		return "WhatsApp returned 515 (restart required) but the driver's reconnect retries were exhausted. This is unusual — the auth dir has been cleared, retry once more"
	case 0:
		if msg != "" {
			return "WhatsApp pair failed: " + msg
		}
		return "WhatsApp pair failed; the driver did not report a close code"
	default:
		return fmt.Sprintf("WhatsApp returned an unexpected close code %d (%s). Try again in 30 minutes; if it repeats, report the code", p.Code, msg)
	}
}

// runNpmInstall invokes `npm install` in dir, streaming stdout/stderr to out
// so the user sees progress on slow installs (Playwright/Baileys take a bit).
func runNpmInstall(ctx context.Context, out interface {
	Write(p []byte) (n int, err error)
}, dir string) error {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	// --ignore-scripts is a deliberate supply-chain control: a freshly pulled
	// dependency (or transitive dependency) must not be able to run arbitrary
	// pre/post-install lifecycle scripts as the user just because a skill was
	// installed. The skill's OWN setup runs separately and explicitly via
	// runFathomPostInstall ("npm run fantazm:postinstall"), which is our
	// audited script — `npm run` of a named script is unaffected by this flag.
	//
	// Reproducibility caveat: the bundled skills pin their DIRECT deps to exact
	// versions, but without a committed package-lock.json the TRANSITIVE deps
	// still resolve to the latest semver-compatible release at install time.
	// Full reproducibility is a follow-up: commit lockfiles and switch this to
	// `npm ci`. --ignore-scripts is the bigger security win and lands now.
	cmd := exec.CommandContext(cctx, "npm", "install",
		"--ignore-scripts", "--no-audit", "--no-fund", "--loglevel=error")
	cmd.Dir = dir
	cmd.Stdout = io.Discard // npm errors come on stderr; suppress chatter
	cmd.Stderr = newPrefixedWriter(out, "    ")
	return cmd.Run()
}

// runFathomPostInstall executes the skill's "fantazm:postinstall" npm script
// if defined. Used by browser to trigger `npx playwright install chromium`
// without the host caring about the specifics.
func runFathomPostInstall(ctx context.Context, out interface {
	Write(p []byte) (n int, err error)
}, dir string) error {
	pkgPath := filepath.Join(dir, "package.json")
	data, err := os.ReadFile(pkgPath)
	if err != nil {
		return nil
	}
	if !strings.Contains(string(data), `"fantazm:postinstall"`) {
		return nil
	}
	fmt.Fprintln(out, "  "+ui.Mute("running fantazm:postinstall (may download browser binary)…"))
	cctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(cctx, "npm", "run", "fantazm:postinstall", "--silent")
	cmd.Dir = dir
	cmd.Stdout = newPrefixedWriter(out, "    ")
	cmd.Stderr = newPrefixedWriter(out, "    ")
	return cmd.Run()
}

// prefixedWriter prepends a fixed indent to every line written through it.
// Lets us show subprocess output inside Fathom's indented layout without
// each line of npm's progress jumping back to column 0.
type prefixedWriter struct {
	out     io.Writer
	prefix  string
	atStart bool
}

func newPrefixedWriter(out interface {
	Write(p []byte) (n int, err error)
}, prefix string) *prefixedWriter {
	return &prefixedWriter{out: out.(io.Writer), prefix: prefix, atStart: true}
}

func (p *prefixedWriter) Write(b []byte) (int, error) {
	written := 0
	for _, c := range b {
		if p.atStart {
			if _, err := p.out.Write([]byte(p.prefix)); err != nil {
				return written, err
			}
			p.atStart = false
		}
		n, err := p.out.Write([]byte{c})
		written += n
		if err != nil {
			return written, err
		}
		if c == '\n' {
			p.atStart = true
		}
	}
	return written, nil
}

func skillsRemoveCmd() *cobra.Command {
	var globalFlag bool
	cmd := &cobra.Command{
		Use:   "remove NAME",
		Short: "Remove an installed skill",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			dest, err := resolveInstallDest(name, globalFlag)
			if err != nil {
				return err
			}
			if err := os.RemoveAll(dest); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintln(out)
			fmt.Fprintln(out, "  "+ui.Success("Removed "+name+" from "+dest))
			fmt.Fprintln(out)
			return nil
		},
	}
	cmd.Flags().BoolVar(&globalFlag, "global", false, "Remove from ~/.local/share/fantazm/skills/")
	return cmd
}

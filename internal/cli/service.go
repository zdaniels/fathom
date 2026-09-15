package cli

import (
	"fmt"
	"github.com/zdaniels/fathom/internal/brandenv"
	"html"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/zdaniels/fathom/internal/cli/ui"
	"github.com/zdaniels/fathom/internal/config"
)

func init() {
	subcommands = append(subcommands, newServiceCommand)
}

const launchdLabel = "ai.fantazm.fantazm-gateway"

// newServiceCommand: `fathom service {install,uninstall,status,restart,log}`.
//
// Wraps macOS launchd via launchctl(1). Writes a LaunchAgent plist to the
// user's ~/Library/LaunchAgents/ai.fantazm.fantazm-gateway.plist; launchd
// auto-starts `fathom start` at login + restarts it on crash. No GUI app,
// no sudo. Per-user agent — runs as the logged-in user, not root.
//
// Linux/Windows: returns a clear error. systemd-user wrappers are a
// reasonable follow-up but the v0.5 audience is macOS-first.
func newServiceCommand() *cobra.Command {
	root := &cobra.Command{
		Use:   "service",
		Short: "Manage the gateway's auto-start service (macOS launchd)",
		Long: `Install Fathom's gateway as a launchd LaunchAgent so it starts
at login and restarts itself on crash — no terminal required after the
initial install.

Where files land:
  plist: ~/Library/LaunchAgents/ai.fantazm.fantazm-gateway.plist
  logs:  ~/Library/Logs/fantazm-gateway.log

The service runs as your user (not root) with your environment, the
config it finds via standard discovery (cwd ↑ project, then global),
and the same vault/keys you'd use interactively.`,
	}
	root.AddCommand(
		serviceInstallCmd(),
		serviceUninstallCmd(),
		serviceStatusCmd(),
		serviceStartCmd(),
		serviceStopCmd(),
		serviceRestartCmd(),
		serviceLogCmd(),
	)
	return root
}

// serviceStartCmd: `fathom service start` — bring the service up in
// the background without rebuilding the bootstrap. If the agent is
// already loaded, this is a no-op kickstart (no -k, so it only starts
// when stopped). If not loaded, suggests `service install` instead.
func serviceStartCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "start",
		Short: "Start the gateway service in the background",
		RunE: func(cmd *cobra.Command, args []string) error {
			if runtime.GOOS != "darwin" {
				return fmt.Errorf("fathom service start is macOS-only")
			}
			uid := strconv.Itoa(os.Getuid())
			out := cmd.OutOrStdout()
			// kickstart (no -k) starts the job if it's stopped and
			// no-ops if it's already running. If launchd has no
			// record of the label, the user needs `service install`.
			if err := exec.Command("launchctl", "kickstart", "gui/"+uid+"/"+launchdLabel).Run(); err != nil {
				return fmt.Errorf("kickstart: %w — run `fathom service install` first", err)
			}
			fmt.Fprintln(out)
			fmt.Fprintln(out, "  "+ui.Success("Service started in background"))
			fmt.Fprintln(out, "  "+ui.Mute("Logs: ~/Library/Logs/fantazm-gateway.log  ·  fathom service log -f"))
			fmt.Fprintln(out)
			return nil
		},
	}
}

// serviceStopCmd: `fathom service stop` — bring the service down
// without uninstalling. `kill SIGTERM` lets in-flight requests drain;
// launchd will NOT auto-restart because we exited cleanly (the plist's
// KeepAlive.SuccessfulExit=false only restarts on crash).
func serviceStopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Stop the gateway service (uninstall keeps the plist; this just stops the process)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if runtime.GOOS != "darwin" {
				return fmt.Errorf("fathom service stop is macOS-only")
			}
			uid := strconv.Itoa(os.Getuid())
			out := cmd.OutOrStdout()
			if err := exec.Command("launchctl", "kill", "SIGTERM", "gui/"+uid+"/"+launchdLabel).Run(); err != nil {
				return fmt.Errorf("launchctl kill: %w — is the service installed + running?", err)
			}
			fmt.Fprintln(out)
			fmt.Fprintln(out, "  "+ui.Success("Service stopped"))
			fmt.Fprintln(out, "  "+ui.Mute("Restart with `fathom service start`."))
			fmt.Fprintln(out)
			return nil
		},
	}
}

func serviceInstallCmd() *cobra.Command {
	var workdir string
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Register fathom-gateway as a launchd agent (starts at login)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if runtime.GOOS != "darwin" {
				return fmt.Errorf("fathom service install is macOS-only (you're on %s)", runtime.GOOS)
			}
			out := cmd.OutOrStdout()
			fmt.Fprintln(out)

			// Resolve the working directory. Default = current cwd; user
			// can override with --workdir. Verify a config is discoverable
			// from there before committing.
			if workdir == "" {
				wd, err := os.Getwd()
				if err != nil {
					return err
				}
				workdir = wd
			}
			absWorkdir, err := filepath.Abs(workdir)
			if err != nil {
				return err
			}
			cfgPath := discoverFromDir(absWorkdir)
			if cfgPath == "" {
				return fmt.Errorf("no fathom.config.yaml discoverable from %s — run `fathom init` (or `fathom init --global`) first, or pass --workdir", absWorkdir)
			}

			// Locate our own binary so the plist refers to a stable path.
			// os.Executable returns the running binary's actual location;
			// EvalSymlinks resolves any ~/.local/bin → real-path indirection
			// so brew upgrades don't strand the agent on a broken symlink.
			binPath, err := os.Executable()
			if err != nil {
				return fmt.Errorf("locate fathom binary: %w", err)
			}
			binPath, _ = filepath.EvalSymlinks(binPath)

			home, err := os.UserHomeDir()
			if err != nil {
				return err
			}
			plistDir := filepath.Join(home, "Library", "LaunchAgents")
			plistPath := filepath.Join(plistDir, launchdLabel+".plist")
			logDir := filepath.Join(home, "Library", "Logs")
			logPath := filepath.Join(logDir, "fantazm-gateway.log")
			if err := os.MkdirAll(plistDir, 0o755); err != nil {
				return err
			}
			if err := os.MkdirAll(logDir, 0o755); err != nil {
				return err
			}

			plist := renderPlist(binPath, absWorkdir, logPath, cfgPath)
			logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
			if err != nil {
				return err
			}
			if err = logFile.Chmod(0600); err != nil {
				logFile.Close()
				return err
			}
			logFile.Close()
			if err := os.WriteFile(plistPath, []byte(plist), 0o600); err != nil {
				return err
			}

			// Bootstrap into the gui/<uid> domain — that's the per-user
			// session, what we want for an LSUIElement-adjacent service.
			// Bootout first in case there's a stale registration; ignore
			// its error.
			uid := strconv.Itoa(os.Getuid())
			_ = exec.Command("launchctl", "bootout", "gui/"+uid+"/"+launchdLabel).Run()
			if output, err := exec.Command("launchctl", "enable", "gui/"+uid+"/"+launchdLabel).CombinedOutput(); err != nil {
				return fmt.Errorf("launchctl enable: %w: %s", err, strings.TrimSpace(string(output)))
			}
			if output, err := exec.Command("launchctl", "bootstrap", "gui/"+uid, plistPath).CombinedOutput(); err != nil {
				return fmt.Errorf("launchctl bootstrap: %w: %s", err, strings.TrimSpace(string(output)))
			}
			// Bootstrap starts RunAtLoad jobs. A plain kickstart is idempotent;
			// -k would kill a gateway that just started successfully.
			if output, err := exec.Command("launchctl", "kickstart", "gui/"+uid+"/"+launchdLabel).CombinedOutput(); err != nil {
				return fmt.Errorf("launchctl kickstart: %w: %s", err, strings.TrimSpace(string(output)))
			}

			ui.SectionHeader(out, ui.Success("Service installed"))
			ui.KV(out, "plist", plistPath)
			ui.KV(out, "log", logPath)
			ui.KV(out, "binary", binPath)
			ui.KV(out, "workdir", absWorkdir)
			ui.KV(out, "config", cfgPath)
			fmt.Fprintln(out)
			ui.Hint(out, "Gateway is running in the background and will auto-restart on login.")
			ui.Hint(out, "Status:    fathom service status")
			ui.Hint(out, "Logs:      fathom service log")
			ui.Hint(out, "Uninstall: fathom service uninstall")
			fmt.Fprintln(out)
			return nil
		},
	}
	cmd.Flags().StringVar(&workdir, "workdir", "", "Directory the gateway runs in (defaults to cwd). Config is discovered from here.")
	return cmd
}

func serviceUninstallCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "uninstall",
		Short: "Stop the gateway service and remove the launchd plist",
		RunE: func(cmd *cobra.Command, args []string) error {
			if runtime.GOOS != "darwin" {
				return fmt.Errorf("fathom service uninstall is macOS-only")
			}
			home, _ := os.UserHomeDir()
			plistPath := filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist")
			uid := strconv.Itoa(os.Getuid())
			_ = exec.Command("launchctl", "bootout", "gui/"+uid+"/"+launchdLabel).Run()
			_ = os.Remove(plistPath)
			out := cmd.OutOrStdout()
			fmt.Fprintln(out)
			fmt.Fprintln(out, "  "+ui.Success("Service removed"))
			fmt.Fprintln(out)
			return nil
		},
	}
}

func serviceStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show whether the launchd agent is loaded + the gateway is up",
		RunE: func(cmd *cobra.Command, args []string) error {
			if runtime.GOOS != "darwin" {
				return fmt.Errorf("fathom service status is macOS-only")
			}
			uid := strconv.Itoa(os.Getuid())
			out := cmd.OutOrStdout()
			fmt.Fprintln(out)
			ui.SectionHeader(out, "Service status")

			// `launchctl print` is the modern, verbose status query. We
			// only show whether the agent is registered + its last exit
			// code; the full output is for `launchctl print` directly.
			data, err := exec.Command("launchctl", "print", "gui/"+uid+"/"+launchdLabel).CombinedOutput()
			if err != nil {
				ui.KV(out, "agent", ui.Warn("not installed — run `fathom service install`"))
				fmt.Fprintln(out)
				return nil
			}
			ui.KV(out, "agent", ui.Success("registered"))
			for _, line := range strings.Split(string(data), "\n") {
				t := strings.TrimSpace(line)
				switch {
				case strings.HasPrefix(t, "state ="):
					ui.KV(out, "state", strings.TrimPrefix(t, "state = "))
				case strings.HasPrefix(t, "pid ="):
					ui.KV(out, "pid", strings.TrimPrefix(t, "pid = "))
				case strings.HasPrefix(t, "last exit code ="):
					ui.KV(out, "last exit", strings.TrimPrefix(t, "last exit code = "))
				}
			}

			// Also probe the gateway directly.
			cfg := config.LoadConfig("")
			port := cfg.Port
			if port == 0 {
				port = 8790
			}
			gatewayURL := fmt.Sprintf("http://127.0.0.1:%d/api/v1/health", port)
			if err := exec.Command("curl", "-fsS", "--max-time", "2", gatewayURL).Run(); err == nil {
				ui.KV(out, "gateway", ui.Success("responding on :"+strconv.Itoa(port)))
			} else {
				ui.KV(out, "gateway", ui.Warn("not responding — check `fathom service log`"))
			}
			fmt.Fprintln(out)
			return nil
		},
	}
}

func serviceRestartCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "restart",
		Short: "Restart the gateway service (kickstart -k)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if runtime.GOOS != "darwin" {
				return fmt.Errorf("fathom service restart is macOS-only")
			}
			uid := strconv.Itoa(os.Getuid())
			out := cmd.OutOrStdout()
			if err := exec.Command("launchctl", "kickstart", "-k", "gui/"+uid+"/"+launchdLabel).Run(); err != nil {
				return fmt.Errorf("kickstart: %w — is the service installed?", err)
			}
			fmt.Fprintln(out)
			fmt.Fprintln(out, "  "+ui.Success("Service restarted"))
			fmt.Fprintln(out)
			return nil
		},
	}
}

func serviceLogCmd() *cobra.Command {
	var follow bool
	cmd := &cobra.Command{
		Use:   "log",
		Short: "Show recent gateway log output",
		RunE: func(cmd *cobra.Command, args []string) error {
			home, _ := os.UserHomeDir()
			logPath := filepath.Join(home, "Library", "Logs", "fantazm-gateway.log")
			if _, err := os.Stat(logPath); err != nil {
				return fmt.Errorf("no log at %s — has the service ever run?", logPath)
			}
			args = []string{"-n", "100", logPath}
			if follow {
				args = append([]string{"-f"}, args...)
			}
			c := exec.Command("tail", args...)
			c.Stdout = cmd.OutOrStdout()
			c.Stderr = cmd.ErrOrStderr()
			return c.Run()
		},
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "Follow new log lines (tail -f)")
	return cmd
}

// renderPlist builds the LaunchAgent XML. We do it as a template string —
// the plist format is stable and a real XML library would be overkill
// for ~30 lines of static structure.
func renderPlist(binPath, workdir, logPath string, configPaths ...string) string {
	configPath := ""
	if len(configPaths) > 0 {
		configPath, _ = filepath.Abs(configPaths[0])
	}
	pathEnv := os.Getenv("PATH")
	if pathEnv == "" {
		pathEnv = "/usr/local/bin:/opt/homebrew/bin:/usr/bin:/bin"
	}
	home, _ := os.UserHomeDir()
	var extraEnv strings.Builder
	for _, key := range []string{"FATHOM_MODE", "FATHOM_HOST", "FATHOM_PORT", "FATHOM_DATA_DIR", "FATHOM_WORKSPACE_ROOT", "FATHOM_POLICY", "FATHOM_SKILL_SANDBOX", "FATHOM_VAULT_PATH", "FATHOM_VAULT_KEY", "FATHOM_DEVICES_DB", "FATHOM_THREADS_DB", "FATHOM_SCHEDULE_DB", "FATHOM_TOKEN_FILE", "FATHOM_LOG_LEVEL"} {
		if value := brandenv.Get(key); value != "" {
			fmt.Fprintf(&extraEnv, "<key>%s</key><string>%s</string>\n", key, html.EscapeString(value))
		}
	}
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>%s</string>
    <key>ProgramArguments</key>
    <array>
        <string>%s</string>
        <string>start</string>
        <string>%s</string>
    </array>
    <key>WorkingDirectory</key>
    <string>%s</string>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <dict>
        <key>SuccessfulExit</key>
        <false/>
    </dict>
    <key>StandardOutPath</key>
    <string>%s</string>
    <key>StandardErrorPath</key>
    <string>%s</string>
    <key>EnvironmentVariables</key>
    <dict>
        <key>PATH</key>
        <string>%s</string>
        <key>HOME</key>
        <string>%s</string>
        %s
    </dict>
    <key>ThrottleInterval</key>
    <integer>10</integer>
</dict>
</plist>
`, launchdLabel, html.EscapeString(binPath), html.EscapeString(configPath), html.EscapeString(workdir), html.EscapeString(logPath), html.EscapeString(logPath), html.EscapeString(pathEnv), html.EscapeString(home), extraEnv.String())
}

// discoverFromDir runs config discovery as if cwd were dir. Returns the
// first config file found via the standard precedence: upward walk +
// global fallback. Returns "" if nothing is found.
func discoverFromDir(dir string) string {
	return config.DiscoverConfigFrom(dir)
}

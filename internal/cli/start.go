package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/zdaniels/fathom/internal/agent/llm"
	"github.com/zdaniels/fathom/internal/agentfactory"
	"github.com/zdaniels/fathom/internal/auth"
	"github.com/zdaniels/fathom/internal/brandenv"
	"github.com/zdaniels/fathom/internal/cli/ui"
	"github.com/zdaniels/fathom/internal/config"
	"github.com/zdaniels/fathom/internal/enterprise"
	"github.com/zdaniels/fathom/internal/gateway"
	"github.com/zdaniels/fathom/internal/relay"
	"github.com/zdaniels/fathom/internal/scheduler"
	"github.com/zdaniels/fathom/internal/security"
	"github.com/zdaniels/fathom/internal/threads"
	"github.com/zdaniels/fathom/pkg/types"
)

func init() {
	subcommands = append(subcommands, newStartCommand)
}

// routerAdapter wraps the agent's llm.Router (which returns a typed
// Provider alongside the default name) so the gateway sees only the
// minimal ModelRegistry surface — names + the default name.
type routerAdapter struct{ r *llm.Router }

func (a routerAdapter) Names() []string     { return a.r.Names() }
func (a routerAdapter) DefaultName() string { _, n := a.r.Default(); return n }

// auditAdapter wraps *security.AuditLogger so the gateway can record
// pairing-flow events through it. Pairing events are informational
// (PolicyAllow); the gateway already enforces rate-limiting upstream.
type auditAdapter struct{ logger *security.AuditLogger }

func (a auditAdapter) Log(sessionID, userID, action string, detail map[string]interface{}) error {
	a.logger.Log(sessionID, userID, types.AuditAction(action), detail, types.PolicyAllow)
	return nil
}

// newStartCommand: `fathom start` — boots the gateway, agent, scheduler,
// and (in team/enterprise mode) mounts the admin API. Blocks until SIGINT.
func newStartCommand() *cobra.Command {
	var cfgPath string
	var withTunnel bool
	cmd := &cobra.Command{
		Use:   "start [config]",
		Short: "Start the gateway (HTTP + WebSocket) with the agent loop + scheduler",
		Long: `Start the Fathom gateway with the agent loop + scheduler.

Use --tunnel to expose the gateway over a Cloudflare Tunnel so a paired
device can reach it from outside your LAN. Requires the cloudflared
binary in PATH (brew install cloudflared on macOS).

Alternative: install Tailscale on the laptop and your phone. No CLI
changes needed; ` + "`fathom pair`" + ` auto-detects the Tailscale interface
and encodes that IP in the QR. Works through NAT, no public surface.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				cfgPath = args[0]
			}
			cfg := config.LoadConfig(cfgPath)

			// Persistent device tokens — keeps paired phones / CLIs /
			// browsers working across `fathom service restart`. Failure
			// here is non-fatal: we fall back to the ephemeral in-memory
			// auth manager and log it.
			ds, dsErr := auth.OpenDeviceStore(auth.DefaultDeviceDBPath())
			if dsErr != nil {
				slog.Warn("device store unavailable; paired devices will not survive restart", "err", dsErr)
			}
			gw, err := gateway.NewWithDeviceStore(cfg, ds)
			if err != nil {
				return fmt.Errorf("gateway init: %w", err)
			}

			// Persistent threads — used by both the mobile UI and any
			// thread-aware Mac client. Failure here isn't fatal: the
			// gateway still works for legacy single-shot messages.
			// Minimal profile skips this entirely — chat brain only.
			if cfg.Profile != types.ProfileMinimal {
				if ts, err := threads.OpenStore(threads.DefaultDBPath()); err == nil {
					gw.SetThreadStore(ts)
				} else {
					slog.Warn("thread store unavailable; cross-device sync disabled", "err", err)
				}
			}

			result, err := agentfactory.CreateDefault(cfg, agentfactory.Options{})
			if err != nil {
				return err
			}
			if result.Security != nil {
				defer result.Security.Audit.Close()
			}

			gw.SetMessageHandler(result.Handler)
			// HandlerN + router enable per-thread `/model` overrides
			// in the threads API. Both are no-ops when the agent
			// factory didn't wire them (single-model legacy setup).
			if result.HandlerN != nil {
				gw.SetMessageHandlerN(gateway.MessageHandlerN(result.HandlerN))
			}
			// HandlerNU is preferred when available — it lets the
			// gateway persist token usage onto each agent message so
			// the threads API can surface cost info to clients.
			if result.HandlerNU != nil {
				gw.SetMessageHandlerNU(func(ctx context.Context, msg types.ChannelMessage, sess types.Session, model string) (string, *gateway.UsageSnapshot, string, error) {
					reply, u, resolved, herr := result.HandlerNU(ctx, msg, sess, model)
					if herr != nil {
						return "", nil, "", herr
					}
					var snap *gateway.UsageSnapshot
					if u != nil {
						snap = &gateway.UsageSnapshot{
							PromptTokens:     u.PromptTokens,
							CompletionTokens: u.CompletionTokens,
						}
					}
					return reply, snap, resolved, nil
				})
			}
			if result.Router != nil {
				gw.SetModelRegistry(routerAdapter{r: result.Router})
			}
			// Pairing-flow audit. The gateway's recordAudit hook is a
			// no-op until we wire a recorder; this connects pairing
			// claims / device list / revokes into the same hash-chained
			// log the agent already writes to.
			if result.Security != nil && result.Security.Audit != nil {
				gw.SetAuditRecorder(auditAdapter{logger: result.Security.Audit})
			}

			// Settings page (/api/v1/settings + the web UI's Settings panel).
			// Wire the live policy engine so policy toggles hot-reload, and
			// the resolved config/policy paths so edits persist to the same
			// files `fathom init` writes. In personal mode the gateway's
			// default edit-auth (always-true) applies; team/enterprise mode
			// gets the RBAC gate from enterprise.Mount below.
			if result.Security != nil && result.Security.Policy != nil {
				gw.SetPolicyController(result.Security.Policy)
			}
			gw.SetSettingsPaths(cfg.ConfigPath, config.ResolvePolicyPath(cfg))

			// Mount enterprise features in team/enterprise mode. Pass the
			// agent's own mesh so the AdminAPI's audit endpoint queries the
			// same logger the agent writes to (NOT a fresh one — that was
			// the bug from PR #1 of the TS impl).
			ent, err := enterprise.Mount(gw, cfg, result.Security)
			if err != nil {
				return err
			}

			if ent != nil {
				defer ent.DB.Close()
			}
			gw.SetReady(result.Ready)

			// Scheduler — only fires when there's a real LLM behind the agent
			// AND the profile isn't minimal (NanoClaw-style chat-brain mode
			// has no use for cron jobs).
			var sched *scheduler.Scheduler
			if result.Ready && cfg.Profile != types.ProfileMinimal {
				store, err := scheduler.OpenStore(scheduler.DefaultDBPath())
				if err != nil {
					return err
				}
				// Composite deliverer: print to console AND publish an
				// event so the macOS app (and future web UI / SSE clients)
				// can show native notifications.
				eventDeliverer := func(job scheduler.Job, reply string) {
					scheduler.ConsoleDeliverer(job, reply)
					gw.Events.Publish("scheduled_job_done", map[string]interface{}{
						"jobId":  job.ID,
						"cron":   job.Cron,
						"prompt": job.Prompt,
						"reply":  reply,
					})
				}
				sched = scheduler.New(scheduler.Options{
					Store: store,
					Invoker: func(ctx context.Context, prompt string) (string, error) {
						msg := types.ChannelMessage{
							ChannelType: "scheduler", ChannelID: "scheduler",
							SenderID: "scheduler", Text: prompt, Timestamp: time.Now().UTC(),
						}
						sess := makeLocalSession()
						sess.ID = "scheduler-session"
						sess.UserID = "scheduler"
						if ent != nil {
							sess.UserID = brandenv.Get("FATHOM_BOOTSTRAP_ADMIN")
							if sess.UserID == "" {
								sess.UserID = "admin"
							}
						}
						return result.Handler(ctx, msg, sess)
					},
					Deliverer: eventDeliverer,
				})
				sched.Start()
			}

			if err := gw.Start(); err != nil {
				return err
			}

			// Optional Cloudflare Tunnel — exposes the gateway publicly.
			// Refused unless at least one non-initial token exists, so
			// the bootstrap admin token can't be guessed/leaked over a
			// public URL. The right unlock sequence is:
			//   1. fathom start                  (LAN only)
			//   2. fathom pair                   (creates a device token)
			//   3. fathom start --tunnel         (now allowed)
			var tunnel *CloudflareTunnel
			var tunnelURL string
			if withTunnel {
				if !gw.Auth.HasNonInitialTokens() {
					return fmt.Errorf("--tunnel refuses to start with only the bootstrap admin token (too risky over a public URL).\nFirst, in another terminal run `fathom pair` to create a device token, then restart with --tunnel.")
				}
				tunnel = NewCloudflareTunnel(cfg.Port)
				if err := tunnel.Start(context.Background()); err != nil {
					return err
				}
				u, err := tunnel.URL()
				if err != nil {
					_ = tunnel.Stop()
					return err
				}
				tunnelURL = u
				_ = WriteTunnelURLFile(u)
			}

			// Spawn the relay client when ~/.fantazm/relay.token exists.
			// Holds an outbound WebSocket to relay.fantazm.ai and
			// forwards mobile-client requests into the in-process
			// gateway handler. Absence of the token == relay disabled
			// (default state); presence == auto-enroll done already.
			var relayClient *relay.Client
			var relayConnected bool
			if relayCfg, err := relay.LoadConfig(relay.DefaultConfigPath()); err == nil {
				relayClient = relay.New(relayCfg, gw.Handler())
				go relayClient.Run(context.Background())
				relayConnected = true
			} else if !os.IsNotExist(err) {
				slog.Warn("relay token unreadable; relay disabled", "err", err)
			}

			out := cmd.OutOrStdout()
			ui.Welcome(out, result.Description, "")
			ui.SectionHeader(out, "Gateway")
			ui.KV(out, "mode", string(cfg.Mode))
			ui.KV(out, "agent", result.Description)
			ui.KV(out, "listen", fmt.Sprintf("%s:%d", cfg.Host, cfg.Port))
			if relayConnected {
				ui.KV(out, "relay", ui.Success("enabled (relay.fantazm.ai)"))
			}
			if tunnelURL != "" {
				ui.KV(out, "public URL", ui.Success(tunnelURL))
			}
			if sched != nil {
				ui.KV(out, "scheduler", ui.Success("active"))
			} else {
				ui.KV(out, "scheduler", ui.Mute("idle (no LLM configured)"))
			}
			if ent != nil {
				ssoNote := "token auth"
				if ent.SSO != nil {
					ssoNote = "SSO active"
				}
				ui.KV(out, "admin", "/api/v1/admin/*  ("+ssoNote+")")
				ui.KV(out, "tenants", fmt.Sprintf("%d configured", len(ent.Tenants.List())))
			}
			fmt.Fprintln(out)
			ui.Hint(out, "Press Ctrl+C to stop.")
			fmt.Fprintln(out)

			sigCh := make(chan os.Signal, 1)
			signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
			<-sigCh
			fmt.Fprintln(out)
			ui.Hint(out, "Shutting down…")
			if sched != nil {
				sched.Stop()
			}
			if result.EgressProxy != nil {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = result.EgressProxy.Stop(ctx)
			}
			if result.Memory != nil {
				result.Memory.Stop()
			}
			if result.Beacon != nil {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = result.Beacon.Stop(ctx)
			}
			if tunnel != nil {
				_ = tunnel.Stop()
				ClearTunnelURLFile()
			}
			if relayClient != nil {
				relayClient.Stop()
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			return gw.Stop(ctx)
		},
	}
	cmd.Flags().StringVar(&cfgPath, "config", "", "Path to fathom.config.yaml")
	cmd.Flags().BoolVar(&withTunnel, "tunnel", false, "Expose the gateway over a Cloudflare Tunnel (requires cloudflared in PATH)")
	return cmd
}

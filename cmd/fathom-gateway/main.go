// Fathom Gateway — standalone OpenAI-compatible LLM router.
//
// Same codebase, separate binary. Where `fathom` is the agent-on-your-
// laptop product, `fathom-gateway` is the per-VPC multi-tenant LLM
// proxy: holds vendor keys in a vault, authenticates apps via per-team
// bearer tokens, enforces quotas, writes hash-chained audit.
//
// Run mode is intentionally simple — one config file, one listener,
// no subcommands. Tenant CRUD lives in the admin API. The binary
// itself just boots, serves, and exits cleanly on SIGTERM.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/zdaniels/fathom/internal/llmproxy"
	"github.com/zdaniels/fathom/internal/security"
	"github.com/zdaniels/fathom/pkg/types"
)

var (
	version   = "dev"
	commit    = "none"
	buildDate = "unknown"
)

func main() {
	var (
		cfgPath     = flag.String("config", "/etc/fathom-gateway/config.yaml", "path to config file")
		showVersion = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println(versionString())
		return
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	cfg, err := llmproxy.LoadConfig(*cfgPath)
	if err != nil {
		fatal("config: %v", err)
	}

	// Audit sink. v0.1 only wires the in-memory hash-chained logger;
	// the AuditConfig.Sink field is parsed but only "stdout" is
	// observed today (entries echo to stderr via OnEntry). OTLP +
	// file sinks land in PR 4.
	audit := security.NewAuditLogger(cfg.Audit.MaxEntries)
	if cfg.Audit.Sink == "stdout" {
		audit.OnEntry = func(e types.AuditEntry) {
			slog.Info("audit",
				"action", e.Action, "userId", e.UserID, "policy", e.PolicyResult, "detail", e.Detail)
		}
	}

	// Secrets resolver. v0.1 reads from env + literal only; vault
	// integration follows the same pattern as fathom's start.go and
	// lands when an enterprise customer asks for it.
	resolver := llmproxy.EnvResolver

	router, err := llmproxy.NewRouter(cfg.Providers, resolver)
	if err != nil {
		fatal("router: %v", err)
	}
	tenants, err := llmproxy.NewTenantStore(cfg.Tenants, resolver)
	if err != nil {
		fatal("tenants: %v", err)
	}

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           llmproxy.NewServer(router, tenants, audit),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		slog.Info("fathom-gateway listening",
			"addr", cfg.Listen,
			"providers", len(cfg.Providers),
			"tenants", len(cfg.Tenants))
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("listen failed", "err", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	slog.Info("shutdown signal received")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	slog.Info("fathom-gateway stopped")
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

func versionString() string {
	if version != "dev" {
		return fmt.Sprintf("fathom-gateway %s (commit %s, built %s)", version, commit, buildDate)
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		v := info.Main.Version
		if v == "" || v == "(devel)" {
			v = "dev"
		}
		return fmt.Sprintf("fathom-gateway %s", v)
	}
	return "fathom-gateway dev"
}

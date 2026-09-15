package agentfactory

import (
	"context"
	"fmt"
	"time"

	"github.com/zdaniels/fathom/internal/agent"
	"github.com/zdaniels/fathom/internal/integrations/beaconclient"
	"github.com/zdaniels/fathom/internal/integrations/charonclient"
	"github.com/zdaniels/fathom/internal/integrations/chasmclient"
	"github.com/zdaniels/fathom/internal/skills"
	"github.com/zdaniels/fathom/pkg/types"
)

// resolveExternalEgress reads cfg.Egress and returns a configured
// charonclient.Config if the user wants external egress proxying. Returns
// nil, nil when the block is absent — fall back to in-process proxy.
func resolveExternalEgress(cfg *types.EgressConfig) (*charonclient.Config, error) {
	if cfg == nil || cfg.Proxy == "" {
		return nil, nil
	}
	token := cfg.Token
	if token == "" && cfg.TokenFile != "" {
		t, err := charonclient.LoadTokenFromFile(cfg.TokenFile)
		if err != nil {
			return nil, fmt.Errorf("egress: %w", err)
		}
		token = t
	}
	out := &charonclient.Config{URL: cfg.Proxy, Token: token}
	if err := out.Validate(); err != nil {
		return nil, err
	}
	return out, nil
}

// resolveChasmRuntime reads cfg.Skills and returns a configured
// ChasmRuntime if the user wants external skill execution. Returns
// nil, nil when runtime is unset or != "chasm".
//
// On non-nil return, the function also performs a quick /healthz ping
// against the Chasm server — fail-fast at boot beats a confusing
// runtime error on the first skill call.
func resolveChasmRuntime(cfg *types.SkillsConfig) (*skills.ChasmRuntime, error) {
	if cfg == nil || cfg.Runtime != "chasm" {
		return nil, nil
	}
	token := cfg.ChasmToken
	if token == "" && cfg.ChasmTokenFile != "" {
		t, err := chasmclient.LoadTokenFromFile(cfg.ChasmTokenFile)
		if err != nil {
			return nil, fmt.Errorf("skills.chasm: %w", err)
		}
		token = t
	}
	cc := chasmclient.Config{
		URL:          cfg.ChasmURL,
		Token:        token,
		DefaultImage: cfg.DefaultImage,
	}
	if err := cc.Validate(); err != nil {
		return nil, err
	}
	client := chasmclient.New(cc)
	pingCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := client.Healthz(pingCtx); err != nil {
		return nil, fmt.Errorf("skills.chasm: cannot reach %s — is `chasm serve` running? (%w)", cfg.ChasmURL, err)
	}
	return skills.NewChasmRuntime(client, cfg.DefaultImage), nil
}

// resolveBeaconClient reads cfg.Telemetry and returns a configured
// *beaconclient.Client when beacon_url is set. Returns nil when the
// block is absent — Loop falls back to its no-op tracer in that case.
//
// Unlike the Chasm resolver we don't ping at boot: traces are
// fire-and-forget, the background flush logs warns on failure, and a
// down Beacon should never gate Fathom startup.
func resolveBeaconClient(cfg *types.TelemetryConfig) (*beaconclient.Client, error) {
	if cfg == nil || cfg.BeaconURL == "" {
		return nil, nil
	}
	token := cfg.BeaconToken
	if token == "" && cfg.BeaconTokenFile != "" {
		t, err := beaconclient.LoadTokenFromFile(cfg.BeaconTokenFile)
		if err != nil {
			return nil, fmt.Errorf("telemetry: %w", err)
		}
		token = t
	}
	bc := beaconclient.Config{
		URL:         cfg.BeaconURL,
		Token:       token,
		ServiceName: cfg.ServiceName,
	}
	if err := bc.Validate(); err != nil {
		return nil, fmt.Errorf("telemetry: %w", err)
	}
	return beaconclient.New(bc, 0), nil
}

// beaconTracer adapts *beaconclient.Client to the agent.Tracer interface.
// Necessary because beaconclient returns concrete *SpanHandle (with chained
// builder methods that also return *SpanHandle) — Go doesn't allow covariant
// return types for interface satisfaction, so we wrap.
type beaconTracer struct{ c *beaconclient.Client }

type beaconSpanAdapter struct {
	c *beaconclient.Client
	h *beaconclient.SpanHandle
}

func (t beaconTracer) StartSpan(ctx context.Context, name string) agent.Span {
	return beaconSpanAdapter{c: t.c, h: t.c.StartSpan(ctx, name)}
}

func (t beaconTracer) StartChild(parent agent.Span, name string) agent.Span {
	p, _ := parent.(beaconSpanAdapter)
	return beaconSpanAdapter{c: t.c, h: t.c.StartChild(p.h, name)}
}

func (s beaconSpanAdapter) SetAttr(key string, val any) agent.Span {
	s.h.SetAttr(key, val)
	return s
}
func (s beaconSpanAdapter) SetError(msg string) agent.Span {
	s.h.SetError(msg)
	return s
}
func (s beaconSpanAdapter) End()            { s.h.End() }
func (s beaconSpanAdapter) TraceID() string { return s.h.TraceID() }

// tracerOrNil returns a working agent.Tracer when bc is non-nil; otherwise
// returns nil so NewLoop's noop default kicks in.
func tracerOrNil(bc *beaconclient.Client) agent.Tracer {
	if bc == nil {
		return nil
	}
	return beaconTracer{c: bc}
}

// urlOrEmpty + tokenOrEmpty are tiny helpers for the factory body. The
// nil-check pattern would otherwise pollute the bridge wiring with `if`
// branches; collapse it here.
func urlOrEmpty(c *charonclient.Config) string {
	if c == nil {
		return ""
	}
	return c.ProxyURL()
}

func tokenOrEmpty(c *charonclient.Config) string {
	if c == nil {
		return ""
	}
	return c.Token
}

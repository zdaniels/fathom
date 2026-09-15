package skills

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/zdaniels/fathom/internal/integrations/chasmclient"
)

// ChasmRuntime satisfies Runtime by delegating skill invocations to an
// external Chasm sandbox server. Each Invoke maps to one POST /run on
// Chasm; the container starts, the function runs, the JSON result comes
// back.
//
// Constructed via NewChasmRuntime. Use Registry.SetRuntime to install
// it as the active executor in place of the subprocess Sandbox.
type ChasmRuntime struct {
	client *chasmclient.Client
	image  string // default container image (override per skill later)
}

// NewChasmRuntime wires up a runtime around the configured Chasm client.
// defaultImage is the container image used when a skill's manifest
// doesn't declare its own (which v0 manifests never do — that's a
// future field).
func NewChasmRuntime(client *chasmclient.Client, defaultImage string) *ChasmRuntime {
	return &ChasmRuntime{client: client, image: defaultImage}
}

// Invoke maps SandboxInvocation → chasmclient.RunRequest.
//
// The skill's code directory is the directory containing EntryPoint;
// Chasm mounts it read-only at /skill and invokes the named function
// with the input + secrets. Egress (charon proxy URL + token) flows
// through unchanged.
func (c *ChasmRuntime) Invoke(ctx context.Context, inv SandboxInvocation) (interface{}, error) {
	codeDir := filepath.Dir(inv.EntryPoint)
	entryRel := filepath.Base(inv.EntryPoint)

	req := &chasmclient.RunRequest{
		Image:      c.image,
		CodeDir:    codeDir,
		Entrypoint: entryRel,
		Function:   inv.FunctionName,
		Input:      inv.Input,
		Secrets:    inv.Secrets,
	}
	if inv.Egress != nil {
		req.EgressProxy = inv.Egress.URL
		req.EgressToken = inv.Egress.Token
		req.Network = "egress" // open network only via the proxy
	} else {
		req.Network = "none"
	}

	resp, err := c.client.Run(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("chasm runtime: %w", err)
	}
	if !resp.OK {
		// Surface the container's error verbatim so the LLM (or operator)
		// sees the real reason rather than a generic "tool failed".
		msg := resp.Error
		if msg == "" {
			msg = fmt.Sprintf("chasm container exited with code %d", resp.ExitCode)
		}
		return nil, errors.New(msg)
	}
	return resp.Output, nil
}

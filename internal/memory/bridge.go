// Package memory bridges the Fathom Recall (formerly agent-memory; binary `recall`) Go sidecar into
// Fathom's tool registry. recall is itself a Go binary; we spawn it as an
// MCP stdio child once at startup, register its 8 tools (recall_search,
// recall_decisions, record_decision, etc.) as Fathom ToolDefinitions, and
// each tool call round-trips through the MCP JSON-RPC protocol.
//
// Why a sidecar vs a library: recall is a complete product on its own — it
// reads Claude Code / Codex / Cursor session storage too, exposes its own
// CLI, runs a polling watch daemon, ships its own ONNX embedder. Embedding
// would couple Fathom's lifecycle to recall's; sidecar keeps both projects
// independently versioned.
package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"sync/atomic"
)

// Bridge holds the recall subprocess + JSON-RPC framing. Lifetime is the
// gateway process; Stop on shutdown.
type Bridge struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	stdout  io.ReadCloser
	mu      sync.Mutex
	nextID  int64
	pending map[int64]chan rpcResponse
}

type rpcRequest struct {
	JSONRPC string                 `json:"jsonrpc"`
	ID      int64                  `json:"id"`
	Method  string                 `json:"method"`
	Params  map[string]interface{} `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Spawn starts the recall binary as an MCP stdio subprocess. The binary
// must be on PATH (we'll resolve it from FANTAZM_RECALL_BIN if set).
//
// If recall isn't installed, Spawn returns nil + an explanatory error —
// the agent factory treats this as "memory sidecar unavailable", not a
// fatal startup error.
func Spawn(ctx context.Context, recallBin string) (*Bridge, error) {
	if recallBin == "" {
		recallBin = "recall"
	}
	cmd := exec.CommandContext(ctx, recallBin, "mcp", "stdio")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("recall stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("recall stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("recall start (is it installed? get it from github.com/zdaniels/recall): %w", err)
	}
	b := &Bridge{
		cmd:     cmd,
		stdin:   stdin,
		stdout:  stdout,
		pending: make(map[int64]chan rpcResponse),
	}
	go b.readLoop()
	return b, nil
}

// Stop terminates the sidecar.
func (b *Bridge) Stop() {
	if b.cmd == nil || b.cmd.Process == nil {
		return
	}
	_ = b.cmd.Process.Kill()
	_, _ = b.cmd.Process.Wait()
}

// Call invokes a recall MCP method with the given params. Blocks until the
// sidecar replies or ctx expires.
func (b *Bridge) Call(ctx context.Context, method string, params map[string]interface{}) (json.RawMessage, error) {
	id := atomic.AddInt64(&b.nextID, 1)
	ch := make(chan rpcResponse, 1)
	b.mu.Lock()
	b.pending[id] = ch
	b.mu.Unlock()

	req := rpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params}
	buf, _ := json.Marshal(req)
	buf = append(buf, '\n')
	if _, err := b.stdin.Write(buf); err != nil {
		b.mu.Lock()
		delete(b.pending, id)
		b.mu.Unlock()
		return nil, err
	}

	select {
	case <-ctx.Done():
		b.mu.Lock()
		delete(b.pending, id)
		b.mu.Unlock()
		return nil, ctx.Err()
	case resp := <-ch:
		if resp.Error != nil {
			return nil, errors.New(resp.Error.Message)
		}
		return resp.Result, nil
	}
}

// readLoop demultiplexes MCP responses back to per-call channels.
func (b *Bridge) readLoop() {
	dec := json.NewDecoder(b.stdout)
	for {
		var resp rpcResponse
		if err := dec.Decode(&resp); err != nil {
			return
		}
		b.mu.Lock()
		ch, ok := b.pending[resp.ID]
		delete(b.pending, resp.ID)
		b.mu.Unlock()
		if ok {
			ch <- resp
		}
	}
}

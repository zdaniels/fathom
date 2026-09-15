package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// gatewayClient is the CLI's thin HTTP/SSE client to a locally running
// `fathom start` gateway. It activates when the gateway is reachable
// on 127.0.0.1:<port>; otherwise the chat REPL falls back to the
// in-process agent path. The auto-detect happens once at chat startup;
// we don't try to reconnect mid-session if the gateway comes up later.
//
// Why this exists: the in-process path can't see what other devices
// (phone, web) are doing, and other devices can't see in-process
// activity either, because ThreadHub.Publish is only called from
// gateway HTTP handlers. Routing the CLI through the same HTTP/SSE
// surface the phone uses fixes both directions for free — every
// device sees every other device's "thinking" and reply events.
type gatewayClient struct {
	baseURL string
	token   string
	http    *http.Client
}

const (
	defaultGatewayHost     = "127.0.0.1"
	defaultGatewayPort     = "8790"
	gatewayHealthDeadline  = 250 * time.Millisecond
	gatewayRequestDeadline = 5 * time.Minute
)

// detectGateway returns a configured client if a fathom gateway is
// reachable on this host. Returns nil + nil if no gateway is up — the
// caller should treat that as "fall back to in-process" rather than as
// an error. Real errors (bad config etc.) come back as nil + err.
func detectGateway() (*gatewayClient, error) {
	host := envOr("FANTAZM_GATEWAY_HOST", defaultGatewayHost)
	port := envOr("FANTAZM_GATEWAY_PORT", defaultGatewayPort)
	base := "http://" + host + ":" + port

	c := &http.Client{Timeout: gatewayHealthDeadline}
	resp, err := c.Get(base + "/api/v1/health")
	if err != nil {
		// Connection refused, timeout, no route — all of these mean
		// "no gateway here", not a configuration problem. Fall back.
		return nil, nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil
	}

	token, err := loadAPIToken()
	if err != nil {
		// Gateway is up but we couldn't find a token to talk to it.
		// Surface this — the user's running gateway will be invisible
		// from the CLI until they figure out their token state.
		return nil, fmt.Errorf("gateway is up but no api-token: %w", err)
	}

	return &gatewayClient{
		baseURL: base,
		token:   token,
		http:    &http.Client{Timeout: gatewayRequestDeadline},
	}, nil
}

// loadAPIToken pulls the bootstrap admin token the gateway persists at
// ~/.fantazm/api-token on first start. This is the same token the
// gateway uses internally and is single-user-only — multi-user mode
// will need a different flow (CLI fetches a per-user token from the
// auth manager).
func loadAPIToken() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	path := filepath.Join(home, ".fantazm", "api-token")
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	tok := strings.TrimSpace(string(raw))
	if tok == "" {
		return "", errors.New("api-token file is empty")
	}
	return tok, nil
}

// sendMessageWithIDs POSTs a user message to /api/v1/threads/{id}/messages
// and waits for the synchronous response. The gateway publishes
// agent_thinking + agent_done events to ThreadHub during processing —
// those reach this CLI via the SSE stream (see streamThread below)
// and other devices via their own SSE subscriptions.
//
// Returns the assigned user-message id, agent-message id, and the
// agent's reply content. The caller uses the IDs to dedup the SSE
// echo against the synchronous response so the same turn isn't
// rendered twice.
func (c *gatewayClient) sendMessageWithIDs(ctx context.Context, threadID, text string) (userMsgID, agentMsgID, reply string, err error) {
	body, _ := json.Marshal(map[string]string{"text": text})
	url := c.baseURL + "/api/v1/threads/" + threadID + "/messages"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", "", "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return "", "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", "", "", fmt.Errorf("gateway status %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var parsed struct {
		UserMessage struct {
			ID string `json:"id"`
		} `json:"user_message"`
		AgentMessage struct {
			ID      string `json:"id"`
			Content string `json:"content"`
		} `json:"agent_message"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return "", "", "", fmt.Errorf("decode reply: %w", err)
	}
	return parsed.UserMessage.ID, parsed.AgentMessage.ID, parsed.AgentMessage.Content, nil
}

// createThread asks the gateway to mint a fresh thread. Returns the
// new thread id. Used when the CLI starts a chat in gateway mode but
// no thread is being resumed.
func (c *gatewayClient) createThread(ctx context.Context, title string) (string, error) {
	body, _ := json.Marshal(map[string]string{"title": title})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/v1/threads", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return "", fmt.Errorf("create thread status %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var parsed struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return "", err
	}
	if parsed.ID == "" {
		return "", errors.New("gateway returned empty thread id")
	}
	return parsed.ID, nil
}

// gatewayEventMsg is dispatched as a tea.Msg whenever the SSE stream
// produces an event. The chat model handles user_message, agent_done,
// agent_thinking, etc. — see chatModel.Update for the dispatch.
type gatewayEventMsg struct {
	EventType string
	Role      string // for *_message events: "user" | "agent"
	Content   string
	MessageID string
	Err       error // non-nil → stream ended; usually means reconnect
}

// streamThread opens SSE on /api/v1/threads/{id}/stream and emits
// gatewayEventMsg per event. Returns a tea.Cmd you can attach to the
// model so bubbletea handles delivery. Runs forever until ctx is
// cancelled; on disconnect emits a final msg with Err set so the
// caller can decide whether to retry.
func (c *gatewayClient) streamThread(ctx context.Context, threadID string, dispatch func(tea.Msg)) {
	url := c.baseURL + "/api/v1/threads/" + threadID + "/stream"
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+c.token)

	// Use a fresh http.Client without a request timeout — SSE
	// connections are intentionally long-lived. The default client
	// inherits gatewayRequestDeadline which would cut us off mid-stream.
	streamer := &http.Client{Timeout: 0}
	resp, err := streamer.Do(req)
	if err != nil {
		dispatch(gatewayEventMsg{Err: err})
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		dispatch(gatewayEventMsg{Err: fmt.Errorf("stream status %d: %s", resp.StatusCode, string(raw))})
		return
	}

	// SSE wire format: per-line `data: <json>` records, separated by
	// blank lines. The gateway's emitter writes one event per line
	// (no multi-line data block), so a plain line-scan with the
	// "data: " prefix is enough.
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return
		default:
		}
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" {
			continue
		}
		var ev struct {
			Type    string `json:"type"`
			Message *struct {
				ID      string `json:"id"`
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message,omitempty"`
		}
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			continue
		}
		msg := gatewayEventMsg{EventType: ev.Type}
		if ev.Message != nil {
			msg.Role = ev.Message.Role
			msg.Content = ev.Message.Content
			msg.MessageID = ev.Message.ID
		}
		dispatch(msg)
	}
	if err := scanner.Err(); err != nil {
		dispatch(gatewayEventMsg{Err: err})
	} else {
		dispatch(gatewayEventMsg{Err: io.EOF})
	}
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

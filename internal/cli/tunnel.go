package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"github.com/zdaniels/fathom/internal/brandenv"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sync"
	"syscall"
	"time"
)

// CloudflareTunnel manages a child `cloudflared` process that exposes
// the local gateway at a public *.trycloudflare.com URL.
//
// We use the anonymous "quick tunnel" mode (no Cloudflare account,
// ephemeral URL) — fastest possible first-run UX. Users with a CF
// account can run a named tunnel themselves and pass --host to
// `fathom pair` to point at it; that's documented but not automated
// here.
//
// Lifecycle:
//
//	t := NewCloudflareTunnel(8790)
//	if err := t.Start(ctx); err != nil { ... }
//	defer t.Stop()
//	url := t.URL()  // blocks up to ~15s waiting for cloudflared to report
//
// On Stop we send SIGTERM, wait briefly, then SIGKILL. Cloudflared
// handles SIGTERM cleanly.
type CloudflareTunnel struct {
	port int
	cmd  *exec.Cmd

	mu     sync.Mutex
	url    string
	urlCh  chan string
	doneCh chan struct{}
}

// NewCloudflareTunnel constructs a tunnel object pointed at the local
// gateway on `port`. Doesn't start anything until Start is called.
func NewCloudflareTunnel(port int) *CloudflareTunnel {
	return &CloudflareTunnel{
		port:   port,
		urlCh:  make(chan string, 1),
		doneCh: make(chan struct{}),
	}
}

// Start launches cloudflared. Returns immediately once the child is
// spawned; the URL is discovered asynchronously via parseURL on the
// stderr stream. Use URL() to wait for it.
func (t *CloudflareTunnel) Start(ctx context.Context) error {
	bin, err := exec.LookPath("cloudflared")
	if err != nil {
		return errors.New("cloudflared not found in PATH — install it first:\n" +
			"  macOS:  brew install cloudflared\n" +
			"  Linux:  see https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/downloads/\n" +
			"\nOr skip --tunnel and use Tailscale instead (works without any install).")
	}
	t.cmd = exec.CommandContext(ctx, bin, "tunnel", "--url",
		fmt.Sprintf("http://localhost:%d", t.port),
		"--no-autoupdate")
	// Cloudflared writes its progress (including the public URL) to
	// stderr, not stdout — counter-intuitive but documented. Capture
	// both, parse for the URL, also forward to our own stderr so the
	// user sees connection state.
	stderr, err := t.cmd.StderrPipe()
	if err != nil {
		return err
	}
	t.cmd.Stdout = os.Stderr
	if err := t.cmd.Start(); err != nil {
		return fmt.Errorf("spawn cloudflared: %w", err)
	}
	go t.parseURL(stderr)
	go func() {
		_ = t.cmd.Wait()
		close(t.doneCh)
	}()
	return nil
}

// URL blocks up to 20s for the tunnel URL to appear, then returns it.
// Returns ("", error) on timeout. Subsequent calls return immediately
// from the cached value.
func (t *CloudflareTunnel) URL() (string, error) {
	t.mu.Lock()
	cached := t.url
	t.mu.Unlock()
	if cached != "" {
		return cached, nil
	}
	select {
	case u := <-t.urlCh:
		t.mu.Lock()
		t.url = u
		t.mu.Unlock()
		return u, nil
	case <-time.After(20 * time.Second):
		return "", errors.New("cloudflared didn't report a URL within 20s — check `cloudflared` is reachable and the network is up")
	case <-t.doneCh:
		return "", errors.New("cloudflared exited before reporting a URL")
	}
}

// Stop sends SIGTERM, waits up to 5s, then SIGKILL.
func (t *CloudflareTunnel) Stop() error {
	if t.cmd == nil || t.cmd.Process == nil {
		return nil
	}
	_ = t.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-t.doneCh:
		return nil
	case <-time.After(5 * time.Second):
		return t.cmd.Process.Kill()
	}
}

// parseURL scans cloudflared's stderr for the announcement line, which
// looks like:
//
//	2026-05-24T01:23:45Z INF +-------------+
//	...
//	|  https://random-words.trycloudflare.com  |
//	...
//
// We also pass stderr through to our own stderr so the user sees the
// progress.
var tunnelURLRegex = regexp.MustCompile(`https://[a-z0-9-]+\.trycloudflare\.com`)

func (t *CloudflareTunnel) parseURL(stderr io.ReadCloser) {
	defer stderr.Close()
	scanner := bufio.NewScanner(stderr)
	// cloudflared's banner lines can be wide; bump the buffer so we
	// don't drop them.
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		// Forward to user terminal.
		fmt.Fprintln(os.Stderr, "[cloudflared] "+line)
		if t.url != "" {
			continue
		}
		if m := tunnelURLRegex.FindString(line); m != "" {
			select {
			case t.urlCh <- m:
			default:
			}
		}
	}
}

// WriteTunnelURLFile persists the tunnel URL at ~/.fantazm/tunnel-url so
// `fathom pair` (which runs in a separate process) can pick it up and
// encode it in the QR. File is rewritten on each call; cleared by
// ClearTunnelURLFile on shutdown.
func WriteTunnelURLFile(url string) error {
	p, err := tunnelURLPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, []byte(url+"\n"), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// ClearTunnelURLFile removes the persisted URL — call on shutdown so a
// stale URL doesn't leak into a future `fathom pair` after the tunnel
// is gone.
func ClearTunnelURLFile() {
	if p, err := tunnelURLPath(); err == nil {
		_ = os.Remove(p)
	}
}

// ReadTunnelURLFile is called by `fathom pair` to discover the active
// tunnel URL when no --host flag is set.
func ReadTunnelURLFile() string {
	p, err := tunnelURLPath()
	if err != nil {
		return ""
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	url := string(b)
	// Trim trailing newline + any whitespace.
	for len(url) > 0 && (url[len(url)-1] == '\n' || url[len(url)-1] == ' ' || url[len(url)-1] == '\r') {
		url = url[:len(url)-1]
	}
	return url
}

func tunnelURLPath() (string, error) {
	if env := brandenv.Get("FATHOM_TUNNEL_URL_FILE"); env != "" {
		return env, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".fantazm", "tunnel-url"), nil
}

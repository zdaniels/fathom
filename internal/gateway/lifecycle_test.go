package gateway

import (
	"context"
	"net"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zdaniels/fathom/internal/threads"
	"github.com/zdaniels/fathom/pkg/types"
)

func TestStartReportsOccupiedPort(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	_, port, _ := net.SplitHostPort(l.Addr().String())
	n, _ := strconv.Atoi(port)
	g := New(types.Config{Host: "127.0.0.1", Port: n})
	defer g.Pairing.Close()
	if err = g.Start(); err == nil {
		g.Stop(context.Background())
		t.Fatal("Start succeeded on occupied port")
	}
}
func TestStopDrainsRequestsBeforeClosingStores(t *testing.T) {
	t.Setenv("FATHOM_TOKEN_FILE", filepath.Join(t.TempDir(), "token"))
	g, tok := newTestGateway(t)
	g.cfg.Host = "127.0.0.1"
	g.cfg.Port = 0
	s, err := threads.OpenStore(filepath.Join(t.TempDir(), "threads.db"))
	if err != nil {
		t.Fatal(err)
	}
	g.SetThreadStore(s)
	entered, release := make(chan struct{}), make(chan struct{})
	g.SetMessageHandler(func(ctx context.Context, _ types.ChannelMessage, _ types.Session) (string, error) {
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
			return "", ctx.Err()
		}
		if _, err := s.Create("u", "last write"); err != nil {
			t.Error("store closed before handler drained", err)
		}
		return "ok", nil
	})
	if err = g.Start(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/api/v1/message", strings.NewReader(`{"text":"hello"}`))
	req.Header.Set("Authorization", "Bearer "+tok)
	done := make(chan struct{})
	go func() { g.Handler().ServeHTTP(httptest.NewRecorder(), req); close(done) }()
	<-entered
	stopped := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		stopped <- g.Stop(ctx)
	}()
	close(release)
	<-done
	if err = <-stopped; err != nil {
		t.Fatal(err)
	}
}
func TestHubConcurrentUnsubscribeAndOverflow(t *testing.T) {
	h := NewThreadHub()
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		ch, stop := h.Subscribe("t")
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.Publish(ThreadEvent{Type: "agent_delta", ThreadID: "t", Delta: "x"})
			stop()
			stop()
			for range ch {
			}
		}()
	}
	wg.Wait()
	ch, stop := h.Subscribe("t")
	for i := 0; i < 40; i++ {
		h.Publish(ThreadEvent{Type: "agent_delta", ThreadID: "t"})
	}
	stop()
	for range ch {
	}
	h.Close()
}

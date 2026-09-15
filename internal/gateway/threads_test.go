package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zdaniels/fathom/internal/threads"
	"github.com/zdaniels/fathom/pkg/types"
)

func newTestGatewayWithThreads(t *testing.T) (*Gateway, string) {
	t.Helper()
	g, tok := newTestGateway(t)
	ts, err := threads.OpenStore(filepath.Join(t.TempDir(), "threads.db"))
	if err != nil {
		t.Fatalf("open thread store: %v", err)
	}
	g.SetThreadStore(ts)
	t.Cleanup(func() { _ = ts.Close() })
	return g, tok
}

func TestThreadsCreateAndList(t *testing.T) {
	g, tok := newTestGatewayWithThreads(t)
	defer g.Pairing.Close()

	// Create.
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/threads",
		strings.NewReader(`{"title":"first thread"}`))
	r.Header.Set("Authorization", "Bearer "+tok)
	g.handleThreadsRoot(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("create status %d: %s", w.Code, w.Body.String())
	}
	var th map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &th)
	if th["id"] == nil || th["title"] != "first thread" {
		t.Fatalf("create resp: %+v", th)
	}

	// List.
	w = httptest.NewRecorder()
	r = httptest.NewRequest(http.MethodGet, "/api/v1/threads", nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	g.handleThreadsRoot(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("list status %d: %s", w.Code, w.Body.String())
	}
	var list struct {
		Threads []map[string]interface{} `json:"threads"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &list)
	if len(list.Threads) != 1 || list.Threads[0]["title"] != "first thread" {
		t.Errorf("list: %+v", list.Threads)
	}
}

func TestThreadSendInvokesHandlerAndPersists(t *testing.T) {
	g, tok := newTestGatewayWithThreads(t)
	defer g.Pairing.Close()

	g.SetMessageHandler(func(ctx context.Context, msg types.ChannelMessage, sess types.Session) (string, error) {
		if msg.Text != "hello agent" {
			t.Errorf("agent got msg %q, want 'hello agent'", msg.Text)
		}
		return "hello human", nil
	})

	th, _ := g.Threads.Create("admin", "")
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost,
		"/api/v1/threads/"+th.ID+"/messages",
		strings.NewReader(`{"text":"hello agent"}`))
	r.Header.Set("Authorization", "Bearer "+tok)
	g.handleThreadItem(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("send status %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		UserMessage  map[string]interface{} `json:"user_message"`
		AgentMessage map[string]interface{} `json:"agent_message"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.UserMessage["content"] != "hello agent" || resp.AgentMessage["content"] != "hello human" {
		t.Errorf("send: %+v", resp)
	}

	// Both messages should now be in the store, tail in order.
	msgs, err := g.Threads.MessagesTail(th.ID, 10)
	if err != nil {
		t.Fatalf("tail: %v", err)
	}
	if len(msgs) != 2 || msgs[0].Role != "user" || msgs[1].Role != "agent" {
		t.Errorf("persisted: %+v", msgs)
	}
}

func TestThreadAutoTitleOnFirstMessage(t *testing.T) {
	g, tok := newTestGatewayWithThreads(t)
	defer g.Pairing.Close()
	g.SetMessageHandler(func(ctx context.Context, msg types.ChannelMessage, _ types.Session) (string, error) {
		return "ok", nil
	})

	th, _ := g.Threads.Create("admin", "") // no title
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost,
		"/api/v1/threads/"+th.ID+"/messages",
		strings.NewReader(`{"text":"summarize today's PRs"}`))
	r.Header.Set("Authorization", "Bearer "+tok)
	g.handleThreadItem(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("send: %d %s", w.Code, w.Body.String())
	}
	got, _ := g.Threads.Get(th.ID)
	if got.Title != "summarize today's PRs" {
		t.Errorf("title = %q, want auto-derived from first message", got.Title)
	}
}

func TestThreadAuthorizationScopedToOwner(t *testing.T) {
	g, tok := newTestGatewayWithThreads(t)
	defer g.Pairing.Close()
	// Pretend a thread belongs to someone else.
	other, _ := g.Threads.Create("not-admin", "secret")
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/threads/"+other.ID, nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	g.handleThreadItem(w, r)
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", w.Code)
	}
}

func TestThreadHubFansOutToMultipleSubscribers(t *testing.T) {
	hub := NewThreadHub()
	ch1, unsub1 := hub.Subscribe("t1")
	ch2, unsub2 := hub.Subscribe("t1")
	defer unsub1()
	defer unsub2()
	ch3, unsub3 := hub.Subscribe("t2")
	defer unsub3()

	hub.Publish(ThreadEvent{Type: "user_message", ThreadID: "t1", Delta: "x"})

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); <-ch1 }()
	go func() { defer wg.Done(); <-ch2 }()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("subscribers didn't receive within 1s")
	}

	// t2 subscriber should NOT have received the t1 event.
	select {
	case e := <-ch3:
		t.Errorf("t2 received cross-topic event: %+v", e)
	case <-time.After(50 * time.Millisecond):
		// good
	}
}

func TestThreadStreamSSEEmitsPublishedEvents(t *testing.T) {
	g, tok := newTestGatewayWithThreads(t)
	defer g.Pairing.Close()

	th, _ := g.Threads.Create("admin", "")

	// Run the SSE handler in a goroutine; cancel the request after we've
	// captured the events we expected.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Use a pipe so the SSE handler writes to one end and our test
	// reads from the other.
	pr, pw := io.Pipe()
	rec := &flushableRecorder{Writer: pw, header: http.Header{}}

	r := httptest.NewRequest(http.MethodGet,
		"/api/v1/threads/"+th.ID+"/stream", nil).WithContext(ctx)
	r.Header.Set("Authorization", "Bearer "+tok)
	go func() {
		g.handleThreadItem(rec, r)
		pw.Close()
	}()

	// Wait for the `ready` event so we know the subscription is live.
	if !waitForLine(t, pr, "event: ready", 2*time.Second) {
		t.Fatal("never saw the ready event")
	}

	// Publish a user_message and verify the subscriber sees it.
	hub := g.ThreadHub
	hub.Publish(ThreadEvent{Type: "user_message", ThreadID: th.ID, Delta: "hi"})

	if !waitForLine(t, pr, "event: user_message", 2*time.Second) {
		t.Fatal("never saw the user_message event")
	}
}

// === test helpers ===

type flushableRecorder struct {
	io.Writer
	header http.Header
	status int
}

func (f *flushableRecorder) Header() http.Header { return f.header }
func (f *flushableRecorder) WriteHeader(s int)   { f.status = s }
func (f *flushableRecorder) Flush()              {}

func waitForLine(t *testing.T, r io.Reader, want string, timeout time.Duration) bool {
	t.Helper()
	done := make(chan bool, 1)
	go func() {
		buf := make([]byte, 4096)
		var acc strings.Builder
		for {
			n, err := r.Read(buf)
			if n > 0 {
				acc.Write(buf[:n])
				if strings.Contains(acc.String(), want) {
					done <- true
					return
				}
			}
			if err != nil {
				done <- false
				return
			}
		}
	}()
	select {
	case ok := <-done:
		return ok
	case <-time.After(timeout):
		return false
	}
}

func TestThreadMessagesPreserveClientCorrelationID(t *testing.T) {
	for _, text := range []string{"run the task", "who are you?"} {
		t.Run(text, func(t *testing.T) {
			g, tok := newTestGatewayWithThreads(t)
			defer g.Pairing.Close()
			g.SetMessageHandler(func(context.Context, types.ChannelMessage, types.Session) (string, error) { return "done", nil })
			th, err := g.Threads.Create("admin", "")
			if err != nil {
				t.Fatal(err)
			}
			body, _ := json.Marshal(map[string]string{"text": text, "clientMessageId": "local-request-123"})
			r := httptest.NewRequest("POST", "/api/v1/threads/"+th.ID+"/messages", strings.NewReader(string(body)))
			r.Header.Set("Authorization", "Bearer "+tok)
			w := httptest.NewRecorder()
			g.handleThreadItem(w, r)
			if w.Code != 200 {
				t.Fatalf("%d %s", w.Code, w.Body.String())
			}
			var response struct {
				User threads.Message `json:"user_message"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if response.User.Metadata["clientMessageId"] != "local-request-123" {
				t.Fatal("response lost correlation ID")
			}
		})
	}
}

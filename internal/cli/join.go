package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"
	"github.com/zdaniels/fathom/internal/cli/ui"
	"github.com/zdaniels/fathom/internal/config"
)

func init() {
	subcommands = append(subcommands, newJoinCommand)
}

// newJoinCommand wires `fathom join` — a terminal client that
// connects to the running gateway and joins a persistent thread.
// Same threads/messages your phone sees; messages flow live in both
// directions via SSE.
//
// In-session UI is bubbletea-styled to match `fathom chat` — boxed
// user turns, indented agent prose, spinner while the agent thinks.
func newJoinCommand() *cobra.Command {
	var threadID string
	var createNew bool
	var listOnly bool
	var port int

	cmd := &cobra.Command{
		Use:   "join",
		Short: "Join a thread from this terminal (same threads your phone sees)",
		Long: `Open a persistent thread from your terminal and chat in real time
with anyone else connected to it (your phone, the menubar app, another
browser). Requires ` + "`fathom start`" + ` to be running.

Examples:
  fathom join                 # pick from a list (or open the only one)
  fathom join --new           # start a new thread
  fathom join --thread <id>   # open a specific thread
  fathom join --list          # show threads + exit`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg := config.LoadConfig("")
			if port == 0 {
				port = cfg.Port
			}
			if port == 0 {
				port = 8790
			}
			base := fmt.Sprintf("http://127.0.0.1:%d", port)
			token, err := loadAdminToken()
			if err != nil {
				return fmt.Errorf("read admin token: %w (is `fathom start` running?)", err)
			}
			client := &joinClient{base: base, token: token, http: &http.Client{Timeout: 30 * time.Second}}

			if listOnly {
				return client.printThreads(cmd.OutOrStdout())
			}

			// Pick which thread to join.
			var thread joinThread
			switch {
			case threadID != "":
				thread, err = client.getThread(threadID)
				if err != nil {
					return fmt.Errorf("open thread: %w", err)
				}
			case createNew:
				thread, err = client.createThread("")
				if err != nil {
					return fmt.Errorf("create thread: %w", err)
				}
			default:
				thread, err = pickThreadInteractively(cmd.OutOrStdout(), client)
				if err != nil {
					return err
				}
			}

			return runJoinTUI(client, thread)
		},
	}
	cmd.Flags().StringVar(&threadID, "thread", "", "Open a specific thread by ID")
	cmd.Flags().BoolVar(&createNew, "new", false, "Create a fresh thread")
	cmd.Flags().BoolVar(&listOnly, "list", false, "List threads + exit")
	cmd.Flags().IntVar(&port, "port", 0, "Gateway port (defaults to config)")
	return cmd
}

// pickThreadInteractively shows a numbered list of threads and reads
// a selection from stdin. "n" / "N" creates a fresh one. Returns the
// chosen thread.
func pickThreadInteractively(out io.Writer, client *joinClient) (joinThread, error) {
	list, err := client.listThreads()
	if err != nil {
		return joinThread{}, fmt.Errorf("list threads: %w", err)
	}
	if len(list) == 0 {
		fmt.Fprintln(out, "No threads yet — creating a new one.")
		return client.createThread("")
	}
	ui.SectionHeader(out, fmt.Sprintf("Pick a thread (%d available)", len(list)))
	fmt.Fprintln(out)
	for i, t := range list {
		fmt.Fprintf(out, "  %2d.  %s\n",
			i+1, formatThreadRow(t))
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, "  n.   New thread")
	fmt.Fprintln(out)
	fmt.Fprint(out, "  → ")

	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil {
		return joinThread{}, fmt.Errorf("read selection: %w", err)
	}
	choice := strings.TrimSpace(line)
	if choice == "" || strings.EqualFold(choice, "n") {
		return client.createThread("")
	}
	var idx int
	if _, err := fmt.Sscanf(choice, "%d", &idx); err != nil || idx < 1 || idx > len(list) {
		return joinThread{}, fmt.Errorf("invalid selection %q", choice)
	}
	return list[idx-1], nil
}

func formatThreadRow(t joinThread) string {
	title := truncateStr(t.title(), 38)
	updated := relativeAgo(t.UpdatedAt)
	id := short(t.ID, 8)
	return fmt.Sprintf("%-38s  %-12s  id=%s", title, updated, id)
}

func relativeAgo(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// === Bubbletea TUI for the joined-thread session ===========================

// joinSentMsg comes back when a POST /threads/{id}/messages returns. The
// SSE stream will redeliver the same messages — dedup-by-id in renderMsg
// keeps things idempotent. Carrying the messages on the POST response is
// belt + suspenders: we render immediately rather than wait for SSE.
type joinSentMsg struct {
	user  joinMessage
	agent joinMessage
	err   error
}

// joinStreamMsg is one event from the SSE subscriber goroutine, marshalled
// into a tea.Msg via Program.Send.
type joinStreamMsg struct {
	event joinStreamEvent
}

// joinStreamDropMsg signals the SSE goroutine permanently gave up (the
// thread was deleted etc.). Surfaced as a subtle status note.
type joinStreamDropMsg struct{ err error }

type joinModel struct {
	client        *joinClient
	thread        joinThread
	messages      []joinMessage
	seen          map[string]struct{}
	input         textinput.Model
	spinner       spinner.Model
	busy          bool
	busyStartedAt time.Time
	width         int
	height        int
	quitting      bool
	statusErr     string
	streamCancel  context.CancelFunc
}

func newJoinModel(client *joinClient, thread joinThread, history []joinMessage) joinModel {
	ti := textinput.New()
	ti.Placeholder = "say something to the agent…"
	ti.Focus()
	ti.Prompt = ""
	ti.CharLimit = 4096

	sp := spinner.New()
	sp.Spinner = spinner.Dot // matches `fathom chat` — classic Braille rotation
	sp.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("67"))

	seen := make(map[string]struct{}, len(history))
	for _, m := range history {
		seen[m.ID] = struct{}{}
	}

	return joinModel{
		client:   client,
		thread:   thread,
		messages: history,
		seen:     seen,
		input:    ti,
		spinner:  sp,
		width:    100,
	}
}

func (m joinModel) Init() tea.Cmd {
	return tea.Batch(textinput.Blink, m.spinner.Tick)
}

func (m joinModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.input.Width = m.inputWidth()
		return m, nil

	case tea.KeyMsg:
		switch msg.Type {
		case tea.KeyCtrlC, tea.KeyEsc:
			m.quitting = true
			if m.streamCancel != nil {
				m.streamCancel()
			}
			return m, tea.Quit
		}
		if m.busy {
			return m, nil
		}
		if msg.Type == tea.KeyEnter {
			text := strings.TrimSpace(m.input.Value())
			if text == "" {
				return m, nil
			}
			if text == "/quit" || text == "/exit" {
				m.quitting = true
				if m.streamCancel != nil {
					m.streamCancel()
				}
				return m, tea.Quit
			}
			m.input.SetValue("")
			m.busy = true
			m.busyStartedAt = time.Now()
			m.statusErr = ""
			return m, m.sendMessage(text)
		}

	case joinSentMsg:
		m.busy = false
		if msg.err != nil {
			m.statusErr = msg.err.Error()
			return m, nil
		}
		m.appendIfNew(msg.user)
		m.appendIfNew(msg.agent)
		return m, nil

	case joinStreamMsg:
		evt := msg.event
		switch evt.Type {
		case "agent_thinking":
			// Cross-device spinner — another device (or this one) sent
			// a message; show the agent-working state until agent_done.
			m.busy = true
			if m.busyStartedAt.IsZero() {
				m.busyStartedAt = time.Now()
			}
		case "agent_done":
			m.busy = false
			m.busyStartedAt = time.Time{}
			if evt.Message != nil {
				m.appendIfNew(*evt.Message)
			}
		case "agent_error":
			m.busy = false
			m.busyStartedAt = time.Time{}
			if evt.Error != "" {
				m.statusErr = "stream: " + evt.Error
			}
		default:
			if evt.Message != nil {
				m.appendIfNew(*evt.Message)
			}
			if evt.Error != "" {
				m.statusErr = "stream: " + evt.Error
			}
		}
		return m, nil

	case joinStreamDropMsg:
		if msg.err != nil {
			m.statusErr = "stream closed: " + msg.err.Error()
		}
		return m, nil

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

// appendIfNew renders a message if we haven't seen its id yet.
func (m *joinModel) appendIfNew(msg joinMessage) {
	if _, ok := m.seen[msg.ID]; ok {
		return
	}
	m.seen[msg.ID] = struct{}{}
	m.messages = append(m.messages, msg)
}

func (m joinModel) sendMessage(text string) tea.Cmd {
	return func() tea.Msg {
		body, _ := json.Marshal(map[string]string{"text": text})
		var resp struct {
			User  joinMessage `json:"user_message"`
			Agent joinMessage `json:"agent_message"`
		}
		err := m.client.postJSONRaw("/api/v1/threads/"+m.thread.ID+"/messages", body, &resp)
		if err != nil {
			return joinSentMsg{err: err}
		}
		return joinSentMsg{user: resp.User, agent: resp.Agent}
	}
}

// === View === ==============================================================

func (m joinModel) View() string {
	if m.quitting {
		return ""
	}
	var b strings.Builder
	b.WriteString(m.renderWelcome())
	for _, msg := range m.messages {
		switch msg.Role {
		case "user":
			b.WriteString(m.renderUserBox(msg))
		case "agent":
			b.WriteString(m.renderAgentReply(msg.Content))
		default:
			// system or unknown — render as agent prose for now
			b.WriteString(m.renderAgentReply(msg.Content))
		}
	}
	if m.busy {
		thinking := lipgloss.NewStyle().Foreground(lipgloss.Color("244")).Italic(true)
		b.WriteString(renderSpinnerWithLabel(
			m.spinner.View(),
			thinking.Render(spinnerWord(m.busyStartedAt)),
			"  ",
		))
	}
	if m.statusErr != "" {
		err := lipgloss.NewStyle().Foreground(lipgloss.Color("203")).Render("error: " + m.statusErr)
		b.WriteString("  " + err + "\n\n")
	}
	b.WriteString(m.renderInputBox())
	return b.String()
}

func (m joinModel) renderWelcome() string {
	var b strings.Builder
	b.WriteString("\n")
	// Wordmark + "by fathom" subtitle — shared with `fathom chat` and the
	// gateway splash via ui.BannerBlock so the three never drift apart.
	b.WriteString(ui.BannerBlock(m.width))
	b.WriteString("\n")
	dot := lipgloss.NewStyle().Foreground(lipgloss.Color("244")).Render("·")
	tag := lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	body := lipgloss.NewStyle().Foreground(lipgloss.Color("253"))
	b.WriteString("  " + body.Render("Joined thread") + "  " + dot + "  " + tag.Render("github.com/zdaniels/fathom") + "\n\n")
	b.WriteString(kvJoin("thread", m.thread.title()))
	b.WriteString(kvJoin("id", short(m.thread.ID, 16)))
	b.WriteString(kvJoin("commands", "/quit  · Esc to exit"))
	b.WriteString("\n")
	return b.String()
}

func (m joinModel) boxContentWidth() int {
	w := m.width - 4
	if w < 16 {
		w = 16
	}
	return w
}
func (m joinModel) inputWidth() int { return m.boxContentWidth() - 2 }
func (m joinModel) contentWidth() int {
	w := m.width - 4
	if w < 16 {
		w = 16
	}
	return w
}
func (m joinModel) boxStyle() lipgloss.Style {
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("67")).
		Padding(0, 1).
		Width(m.boxContentWidth())
}

func (m joinModel) renderUserBox(msg joinMessage) string {
	cursor := lipgloss.NewStyle().Foreground(lipgloss.Color("67")).Render("›")
	label := msg.who()
	if label != "" {
		// Small grey "from <device>" tag right-aligned over the bubble.
		from := lipgloss.NewStyle().Foreground(lipgloss.Color("244")).Render(label)
		out := m.boxStyle().Render(cursor+" "+msg.Content) + "\n"
		return out + "  " + from + "\n\n"
	}
	return m.boxStyle().Render(cursor+" "+msg.Content) + "\n\n"
}

func (m joinModel) renderAgentReply(text string) string {
	w := m.contentWidth()
	style := lipgloss.NewStyle().Foreground(lipgloss.Color("253")).Width(w)
	out := style.Render(text)
	lines := strings.Split(out, "\n")
	for i := range lines {
		lines[i] = "  " + lines[i]
	}
	return strings.Join(lines, "\n") + "\n\n"
}

func (m joinModel) renderInputBox() string {
	prompt := lipgloss.NewStyle().Foreground(lipgloss.Color("67")).Bold(true).Render(">")
	return m.boxStyle().Render(prompt + " " + m.input.View())
}

// joinMessage.who renders the source label that floats under the bubble:
// blank for messages typed in this terminal, "from iPhone" for others.
// Heuristic: if DeviceID is non-empty AND doesn't match ours, label it.
// (No reliable "this is me" signal yet; for now we just show every
// device's name so the user can tell which messages came from where.)
func (m joinMessage) who() string {
	if m.DeviceID == "" {
		return ""
	}
	return "from " + short(m.DeviceID, 8)
}

// short returns the first n chars of s (no ellipsis) — used for device
// IDs and thread IDs in compact UI labels.
func short(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func kvJoin(key, value string) string {
	const maxKey = 12
	pad := strings.Repeat(" ", maxKey-len(key))
	if len(key) > maxKey {
		pad = ""
	}
	return fmt.Sprintf("  %s%s  %s\n",
		lipgloss.NewStyle().Foreground(lipgloss.Color("244")).Render(key),
		pad,
		lipgloss.NewStyle().Foreground(lipgloss.Color("253")).Render(value),
	)
}

// runJoinTUI sets up the bubbletea program + the SSE side-channel that
// pushes incoming events as tea.Msgs.
func runJoinTUI(client *joinClient, thread joinThread) error {
	// Bootstrap history.
	meta, err := client.getThreadWithHistory(thread.ID)
	var history []joinMessage
	if err == nil {
		history = meta.Messages
	}

	model := newJoinModel(client, thread, history)

	streamCtx, streamCancel := context.WithCancel(context.Background())
	model.streamCancel = streamCancel

	p := tea.NewProgram(model, tea.WithAltScreen())

	// SSE goroutine — pump events into the program via p.Send. Reconnects
	// silently with backoff on drops.
	go func() {
		defer streamCancel()
		backoff := 500 * time.Millisecond
		for {
			if err := streamCtx.Err(); err != nil {
				return
			}
			err := client.streamOnce(streamCtx, thread.ID, func(evt joinStreamEvent) {
				p.Send(joinStreamMsg{event: evt})
			})
			if err == nil || errors.Is(err, context.Canceled) {
				return
			}
			// Backoff + retry. Cap at 5s.
			select {
			case <-streamCtx.Done():
				return
			case <-time.After(backoff):
			}
			if backoff < 5*time.Second {
				backoff *= 2
			}
		}
	}()

	_, err = p.Run()
	streamCancel()
	return err
}

// === Client === ============================================================

type joinClient struct {
	base  string
	token string
	http  *http.Client
}

type joinThread struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (t joinThread) title() string {
	if t.Title != "" {
		return t.Title
	}
	return "(untitled)"
}

type joinMessage struct {
	ID        string    `json:"id"`
	ThreadID  string    `json:"thread_id"`
	Role      string    `json:"role"`
	Content   string    `json:"content"`
	DeviceID  string    `json:"device_id,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

type joinStreamEvent struct {
	Type    string       `json:"type"`
	Message *joinMessage `json:"message,omitempty"`
	Error   string       `json:"error,omitempty"`
}

func (c *joinClient) printThreads(out io.Writer) error {
	threads, err := c.listThreads()
	if err != nil {
		return err
	}
	if len(threads) == 0 {
		ui.SectionHeader(out, "No threads yet")
		fmt.Fprintln(out, "  fathom join --new")
		return nil
	}
	ui.SectionHeader(out, fmt.Sprintf("Threads (%d)", len(threads)))
	fmt.Fprintln(out)
	fmt.Fprintf(out, "  %-32s  %-30s  %s\n", "ID", "TITLE", "UPDATED")
	for _, t := range threads {
		fmt.Fprintf(out, "  %-32s  %-30s  %s\n",
			t.ID, truncateStr(t.title(), 30), t.UpdatedAt.Local().Format("2006-01-02 15:04"))
	}
	return nil
}

func (c *joinClient) listThreads() ([]joinThread, error) {
	var resp struct {
		Threads []joinThread `json:"threads"`
	}
	if err := c.getJSON("/api/v1/threads", &resp); err != nil {
		return nil, err
	}
	return resp.Threads, nil
}

func (c *joinClient) createThread(title string) (joinThread, error) {
	body, _ := json.Marshal(map[string]string{"title": title})
	var t joinThread
	if err := c.postJSONRaw("/api/v1/threads", body, &t); err != nil {
		return joinThread{}, err
	}
	return t, nil
}

func (c *joinClient) getThread(id string) (joinThread, error) {
	resp, err := c.getThreadWithHistory(id)
	if err != nil {
		return joinThread{}, err
	}
	return resp.Thread, nil
}

type threadMetaResp struct {
	Thread   joinThread    `json:"thread"`
	Messages []joinMessage `json:"messages"`
}

func (c *joinClient) getThreadWithHistory(id string) (threadMetaResp, error) {
	var r threadMetaResp
	if err := c.getJSON("/api/v1/threads/"+id, &r); err != nil {
		return threadMetaResp{}, err
	}
	return r, nil
}

func (c *joinClient) streamOnce(ctx context.Context, threadID string, onEvent func(joinStreamEvent)) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.base+"/api/v1/threads/"+threadID+"/stream", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "text/event-stream")
	client := &http.Client{Timeout: 0}
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("stream: HTTP %s", res.Status)
	}
	scanner := bufio.NewScanner(res.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var (
		evtName string
		dataBuf strings.Builder
	)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if dataBuf.Len() > 0 {
				var data joinStreamEvent
				if json.Unmarshal([]byte(dataBuf.String()), &data) == nil {
					if data.Type == "" {
						data.Type = evtName
					}
					onEvent(data)
				}
			}
			evtName = ""
			dataBuf.Reset()
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		if strings.HasPrefix(line, "event:") {
			evtName = strings.TrimSpace(line[len("event:"):])
			continue
		}
		if strings.HasPrefix(line, "data:") {
			if dataBuf.Len() > 0 {
				dataBuf.WriteByte('\n')
			}
			dataBuf.WriteString(strings.TrimSpace(line[len("data:"):]))
		}
	}
	return scanner.Err()
}

func (c *joinClient) getJSON(path string, out interface{}) error {
	req, _ := http.NewRequest(http.MethodGet, c.base+path, nil)
	req.Header.Set("Authorization", "Bearer "+c.token)
	res, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 2048))
		return fmt.Errorf("%s: HTTP %s — %s", path, res.Status, strings.TrimSpace(string(body)))
	}
	return json.NewDecoder(res.Body).Decode(out)
}

func (c *joinClient) postJSONRaw(path string, body []byte, out interface{}) error {
	req, _ := http.NewRequest(http.MethodPost, c.base+path, strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	res, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(res.Body, 2048))
		return fmt.Errorf("%s: HTTP %s — %s", path, res.Status, strings.TrimSpace(string(respBody)))
	}
	if out != nil {
		return json.NewDecoder(res.Body).Decode(out)
	}
	return nil
}

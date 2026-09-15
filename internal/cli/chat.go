package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"
	"github.com/zdaniels/fathom/internal/agentfactory"
	"github.com/zdaniels/fathom/internal/cli/ui"
	"github.com/zdaniels/fathom/internal/config"
	"github.com/zdaniels/fathom/internal/security"
	"github.com/zdaniels/fathom/internal/threads"
	"github.com/zdaniels/fathom/pkg/types"
)

func init() {
	subcommands = append(subcommands, newChatCommand)
}

// newChatCommand wires `fathom chat`: a local TUI that talks straight to an
// in-process agent. Bubbletea handles the input box + history rendering;
// the agent loop runs in a goroutine and emits a tea.Msg back.
//
// --thread <id>   resume an existing thread (preloads message history)
// --resume / -r   resume the most recent thread (no id lookup needed)
//
// Without either flag, a fresh thread is created and turns are
// persisted to ~/.fantazm/threads.db. So Ctrl+C and then
// `fathom threads -r` will pick up where you left off.
func newChatCommand() *cobra.Command {
	var cfgPath, threadIDFlag string
	var resumeFlag bool
	cmd := &cobra.Command{
		Use:   "chat",
		Short: "Local chat REPL with the agent",
		Long: `Open an interactive chat session. The agent runs in-process,
loads the persona from .fantazm/persona.md if present, and uses the
configured LLM. Press Esc or Ctrl+C to exit.

Sessions persist to ~/.fantazm/threads.db. Resume with --thread <id>
or --resume (the most recent); list with 'fathom threads'.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runChatSession(cmd, cfgPath, threadIDFlag, resumeFlag)
		},
	}
	cmd.Flags().StringVar(&cfgPath, "config", "", "Path to fathom.config.yaml")
	cmd.Flags().StringVar(&threadIDFlag, "thread", "", "Resume the chat for this thread id (see `fathom threads`)")
	cmd.Flags().BoolVarP(&resumeFlag, "resume", "r", false, "Resume the most recent thread")
	return cmd
}

// runChatSession is the shared startup path used by both `fathom chat`
// and `fathom threads` (after the picker resolves a thread id). Keeps
// the chat lifecycle logic in one place so resume-via-picker and
// resume-via-flag both go through the same cleanup chain.
func runChatSession(cmd *cobra.Command, cfgPath, threadIDFlag string, resumeFlag bool) error {
	security.SetLevel("warn")
	slog.SetDefault(slog.New(slog.NewTextHandler(
		os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))

	if cfgPath == "" && config.DiscoverConfig() == "" {
		out := cmd.OutOrStdout()
		ui.Wordmark(out)
		ui.Hint(out,
			"No Fathom config found.",
			"",
			"Two ways to set up:",
			"  fathom init           — per-project (writes to this directory)",
			"  fathom init --global  — single agent, works from anywhere",
			"",
			"Then `fathom doctor` will confirm everything is wired.",
		)
		fmt.Fprintln(out)
		return nil
	}
	cfg := config.LoadConfig(cfgPath)
	result, err := agentfactory.CreateDefault(cfg, agentfactory.Options{})
	if err != nil {
		return err
	}
	defer func() {
		if result.EgressProxy != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = result.EgressProxy.Stop(ctx)
		}
		if result.Memory != nil {
			result.Memory.Stop()
		}
		if result.Beacon != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = result.Beacon.Stop(ctx)
		}
	}()

	// Takeover: when enabled, an in-process chat must behave like the gateway
	// — every turn goes to the external coding agent (Claude Code / Codex),
	// the local routed agent is bypassed. We swap the handler and clear
	// HandlerN/NU so runAgent's `/model`-aware path can't route around it, and
	// rewrite the banner description so the splash reflects takeover instead of
	// advertising local models that never see a message.
	if cfg.Takeover != nil && cfg.Takeover.Enabled {
		th, terr := agentfactory.BuildTakeoverHandler(cfg)
		if terr != nil {
			return fmt.Errorf("takeover mode: %w", terr)
		}
		result.Handler = th
		result.HandlerN = nil
		result.HandlerNU = nil
		result.Description = "takeover → " + agentfactory.TakeoverDesc(cfg.Takeover)
	}

	cwd, _ := os.Getwd()
	ts, terr := threads.OpenStore(threads.DefaultDBPath())
	if terr != nil {
		fmt.Fprintln(cmd.ErrOrStderr(), "warn: threads store unavailable:", terr)
	} else {
		defer ts.Close()
	}

	m := newChatModel(result, cwd, ts, threadIDFlag, resumeFlag)
	p := tea.NewProgram(m)
	_, err = p.Run()
	return err
}

// chatTurn is one round-trip in the conversation history.
type chatTurn struct {
	user, agent string
}

// chatResponseMsg is dispatched when the agent loop finishes.
type chatResponseMsg struct {
	reply string
	err   error
}

type chatModel struct {
	result        *agentfactory.Result
	cwd           string
	session       types.Session
	history       []chatTurn
	input         textinput.Model
	spinner       spinner.Model
	busy          bool
	busyStartedAt time.Time
	width         int
	height        int
	quitting      bool
	// quitArm holds which quit key was pressed once and is now "armed":
	// "esc" or "ctrl+c". A second press of either quit key actually exits;
	// any other keystroke disarms it. Empty means not armed. This guards
	// against a single accidental Esc/Ctrl+C tearing down the session.
	quitArm      string
	lastError    string
	currentModel string // "" → router default; set by /use
	slashIndex   int    // selected row in the slash-command popup

	// Thread persistence. threadStore is the shared SQLite store the
	// gateway uses too — so CLI sessions show up in the web/mobile
	// thread drawer + are resumable via `fathom threads`. nil means
	// persistence is disabled (store wouldn't open); chat still works.
	threadStore *threads.Store
	threadID    string

	// Gateway routing (Path C). When a local `fathom start` gateway is
	// up, the CLI auto-becomes an HTTP/SSE client of it — same surface
	// the phone uses — so every device sees every other device's
	// activity in realtime. gw == nil means the gateway probe failed or
	// no gateway is running; in that case we fall back to the in-process
	// agent path (the default historical behavior).
	gw             *gatewayClient
	gwEvents       chan gatewayEventMsg
	gwStreamCancel context.CancelFunc
	// seenMsgIDs holds message IDs we've already rendered (either via
	// POST response or via SSE). When SSE redelivers a message we
	// already rendered through the POST path, we skip it. Sized for one
	// session's worth of messages — no eviction, the bound is fine.
	seenMsgIDs map[string]bool

	// Input history (shell-style ↑/↓). historyCommands holds everything
	// the user has submitted in this session, oldest first. historyIndex
	// is -1 when not navigating (live input). 0 = most-recent command,
	// 1 = next-older, etc. When the user presses ↑ for the first time
	// we stash the current input in draftBeforeHistory so ↓ all the way
	// back restores whatever they were typing before they started
	// scrolling. Bounded at historyMax to keep memory predictable.
	historyCommands    []string
	historyIndex       int
	draftBeforeHistory string
}

const historyMax = 200

// icebergSpinner is a 3-line iceberg: peak above, waterline in the
// middle, inverted reflection below. Waterline is the visual connector
// so the peak and reflection read as one shape (without it, terminal
// glyph padding makes ▲ and ▼ look like two unrelated triangles).
// Both triangles are FILLED at every pulse step (▴/▾ when small, ▲/▼
// when full) so they grow in sync rather than one looking hollow.
var icebergSpinner = spinner.Spinner{
	Frames: []string{
		" ▴ \n▔▔▔\n ▾ ",
		" ▲ \n▔▔▔\n ▼ ",
		" ▲ \n▔▔▔\n ▼ ",
		" ▴ \n▔▔▔\n ▾ ",
	},
	FPS: 250 * time.Millisecond,
}

// renderSpinnerWithLabel splits a (possibly multi-line) spinner View()
// into rows and stitches the label onto the first row, so a 2-line
// iceberg spinner reads as
//
//	▲  thinking…
//	▽
//
// rather than collapsing the second line under the first. Returns
// already-indented + newline-terminated text the caller can write
// straight into the chat history.
func renderSpinnerWithLabel(spinnerView, label, indent string) string {
	lines := strings.Split(spinnerView, "\n")
	var b strings.Builder
	for i, line := range lines {
		b.WriteString(indent + line)
		if i == 0 {
			b.WriteString("  " + label)
		}
		b.WriteString("\n")
	}
	b.WriteString("\n")
	return b.String()
}

// spinnerWord returns the label to show next to the spinner. Cycles
// every ~2.5s — mostly "thinking…", with "fathoming…" surfacing every
// fourth cycle so it feels like a wink rather than a tic.
func spinnerWord(since time.Time) string {
	if since.IsZero() {
		return "thinking…"
	}
	cycle := int(time.Since(since).Seconds() / 2.5)
	if cycle%4 == 2 {
		return "fathoming…"
	}
	return "thinking…"
}

// slashCommand is one row in the popup. Name is the literal command (used to
// filter as the user types). Template is what gets inserted on Tab —
// includes placeholder for any argument. Desc is the one-line hint.
type slashCommand struct {
	name, template, desc string
}

var slashCommands = []slashCommand{
	{"/use", "/use ", "Switch active model for following turns"},
	{"/models", "/models", "List configured models and current selection"},
	{"/review", "/review", "Critique the last answer with a second model"},
	{"/rename", "/rename ", "Rename the current thread (no arg shows current)"},
	{"/help", "/help", "Show this list"},
	{"/quit", "/quit", "Leave the chat"},
}

// filteredSlash returns the commands whose name prefix-matches what the
// user has typed. Used to drive the popup.
func (m chatModel) filteredSlash() []slashCommand {
	q := m.input.Value()
	if !strings.HasPrefix(q, "/") {
		return nil
	}
	// Only filter while the user is still typing the COMMAND token — once
	// they've typed past the first space (filling in an argument), the
	// popup gets out of the way.
	if strings.Contains(q, " ") {
		return nil
	}
	var out []slashCommand
	for _, c := range slashCommands {
		if strings.HasPrefix(c.name, q) {
			out = append(out, c)
		}
	}
	return out
}

func newChatModel(r *agentfactory.Result, cwd string, ts *threads.Store, threadIDFlag string, resume bool) chatModel {
	ti := textinput.New()
	ti.Placeholder = "ask anything…"
	ti.Focus()
	ti.Prompt = ""
	ti.CharLimit = 4096

	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("67"))

	session := makeLocalSession()
	m := chatModel{
		result:       r,
		cwd:          cwd,
		session:      session,
		input:        ti,
		spinner:      sp,
		width:        100,
		threadStore:  ts,
		historyIndex: -1,
	}

	if ts == nil {
		return m
	}

	// Resume / create thread. Order: explicit --thread > -r (most recent) > new.
	switch {
	case threadIDFlag != "":
		m.threadID = threadIDFlag
		m.loadHistoryInto(threadIDFlag)
	case resume:
		if list, err := ts.List(session.UserID, 1); err == nil && len(list) > 0 {
			m.threadID = list[0].ID
			m.loadHistoryInto(list[0].ID)
		} else {
			m.threadID = m.createThreadOrEmpty(session.UserID)
		}
	default:
		m.threadID = m.createThreadOrEmpty(session.UserID)
	}

	// Path C: if a local gateway is up, switch to gateway-routed mode so
	// the CLI is just another HTTP/SSE client. Failures here are silent:
	// the in-process path stays usable whether the probe succeeds or not.
	if gw, _ := detectGateway(); gw != nil && m.threadID != "" {
		m.gw = gw
		m.gwEvents = make(chan gatewayEventMsg, 64)
		m.seenMsgIDs = map[string]bool{}
		ctx, cancel := context.WithCancel(context.Background())
		m.gwStreamCancel = cancel
		// SSE goroutine pushes events into gwEvents; chat REPL drains
		// the channel via a recurring tea.Cmd (see waitGatewayEvent).
		eventsCh := m.gwEvents
		go gw.streamThread(ctx, m.threadID, func(msg tea.Msg) {
			if ev, ok := msg.(gatewayEventMsg); ok {
				select {
				case eventsCh <- ev:
				case <-ctx.Done():
				}
			}
		})
	}
	return m
}

// waitGatewayEvent returns a tea.Cmd that blocks until the next SSE
// event arrives, then dispatches it as a tea.Msg. The Update handler
// for gatewayEventMsg re-issues this Cmd so we keep listening for the
// session's lifetime. Returns nil when there's no gateway wired.
func waitGatewayEvent(ch <-chan gatewayEventMsg) tea.Cmd {
	if ch == nil {
		return nil
	}
	return func() tea.Msg {
		ev, ok := <-ch
		if !ok {
			return gatewayEventMsg{Err: fmt.Errorf("gateway event channel closed")}
		}
		return ev
	}
}

// createThreadOrEmpty starts a new thread on the store; on any error
// the model continues with an empty threadID so persistence is silently
// skipped without crashing the user's session.
func (m *chatModel) createThreadOrEmpty(userID string) string {
	t, err := m.threadStore.Create(userID, "")
	if err != nil {
		return ""
	}
	return t.ID
}

// loadHistoryInto pulls every persisted message for the thread and
// pairs them into chatTurn{user, agent} rows so the rendered history
// matches what a fresh REPL would have shown. Unbalanced trailing user
// messages (waiting on an agent reply) stay in the buffer.
func (m *chatModel) loadHistoryInto(threadID string) {
	if m.threadStore == nil {
		return
	}
	msgs, err := m.threadStore.AllMessages(threadID)
	if err != nil {
		return
	}
	var turn chatTurn
	for _, msg := range msgs {
		switch msg.Role {
		case "user":
			if turn.user != "" || turn.agent != "" {
				m.history = append(m.history, turn)
				turn = chatTurn{}
			}
			turn.user = msg.Content
		case "assistant", "agent":
			turn.agent = msg.Content
			m.history = append(m.history, turn)
			turn = chatTurn{}
		}
	}
	if turn.user != "" || turn.agent != "" {
		m.history = append(m.history, turn)
	}
}

func (m chatModel) Init() tea.Cmd {
	cmds := []tea.Cmd{textinput.Blink, m.spinner.Tick}
	if c := waitGatewayEvent(m.gwEvents); c != nil {
		cmds = append(cmds, c)
	}
	return tea.Batch(cmds...)
}

func (m chatModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.input.Width = m.inputWidth()
		return m, nil

	case tea.KeyMsg:
		// Quit needs two consecutive presses of a quit key (Esc or Ctrl+C):
		// the first arms quit and shows a hint, the second confirms. Any other
		// keystroke disarms (handled below). Esc still closes the slash menu
		// first when it's open. This stops a single accidental press from
		// tearing down the session.
		switch msg.Type {
		case tea.KeyCtrlC:
			if m.quitArm != "" {
				m.quitting = true
				return m, tea.Quit
			}
			m.quitArm = "ctrl+c"
			return m, nil
		case tea.KeyEsc:
			if len(m.filteredSlash()) > 0 {
				// Dismiss the slash popup by clearing the input.
				m.input.SetValue("")
				m.slashIndex = 0
				m.quitArm = ""
				return m, nil
			}
			if m.quitArm != "" {
				m.quitting = true
				return m, tea.Quit
			}
			m.quitArm = "esc"
			return m, nil
		default:
			// Any other key disarms a pending quit — an accidental Esc/Ctrl+C
			// is cancelled the moment the user does anything else.
			m.quitArm = ""
		}
		if m.busy {
			// Swallow keys during in-flight LLM call (except exit above).
			return m, nil
		}
		// Slash-popup navigation: ↑/↓ to move, Tab to accept.
		if cmds := m.filteredSlash(); len(cmds) > 0 {
			switch msg.Type {
			case tea.KeyUp:
				if m.slashIndex > 0 {
					m.slashIndex--
				}
				return m, nil
			case tea.KeyDown:
				if m.slashIndex < len(cmds)-1 {
					m.slashIndex++
				}
				return m, nil
			case tea.KeyTab:
				if m.slashIndex < len(cmds) {
					m.input.SetValue(cmds[m.slashIndex].template)
					m.input.CursorEnd()
					m.slashIndex = 0
				}
				return m, nil
			}
		}
		// Shell-style input history. ↑ walks backward through previous
		// commands; ↓ walks forward and eventually restores whatever
		// draft the user was typing before they started scrolling.
		// Only fires when the slash popup is closed (handled above).
		if msg.Type == tea.KeyUp && len(m.historyCommands) > 0 {
			if m.historyIndex == -1 {
				m.draftBeforeHistory = m.input.Value()
				m.historyIndex = 0
			} else if m.historyIndex < len(m.historyCommands)-1 {
				m.historyIndex++
			}
			cmd := m.historyCommands[len(m.historyCommands)-1-m.historyIndex]
			m.input.SetValue(cmd)
			m.input.CursorEnd()
			return m, nil
		}
		if msg.Type == tea.KeyDown && m.historyIndex >= 0 {
			if m.historyIndex == 0 {
				m.input.SetValue(m.draftBeforeHistory)
				m.input.CursorEnd()
				m.historyIndex = -1
				return m, nil
			}
			m.historyIndex--
			cmd := m.historyCommands[len(m.historyCommands)-1-m.historyIndex]
			m.input.SetValue(cmd)
			m.input.CursorEnd()
			return m, nil
		}
		switch msg.Type {
		case tea.KeyEnter:
			text := strings.TrimSpace(m.input.Value())
			if text == "" {
				return m, nil
			}
			// Record into in-session history before we dispatch. Skip
			// consecutive duplicates (same as zsh's HIST_IGNORE_DUPS)
			// and cap at historyMax. Always done — even for /quit, so
			// the user can reopen and ↑ back to their last command…
			// well, only within a single session, but consistent.
			if n := len(m.historyCommands); n == 0 || m.historyCommands[n-1] != text {
				m.historyCommands = append(m.historyCommands, text)
				if len(m.historyCommands) > historyMax {
					m.historyCommands = m.historyCommands[len(m.historyCommands)-historyMax:]
				}
			}
			m.historyIndex = -1
			m.draftBeforeHistory = ""
			if text == "/quit" || text == "/exit" {
				m.quitting = true
				return m, tea.Quit
			}
			if text == "/help" {
				m.history = append(m.history, chatTurn{
					user:  text,
					agent: m.helpText(),
				})
				m.input.SetValue("")
				return m, nil
			}
			// Local-only commands (no gateway equivalent): /review is the
			// "send the last turn through a second model" critique flow
			// that lives entirely client-side.
			if strings.HasPrefix(text, "/review") {
				return m.handleReview(strings.TrimSpace(strings.TrimPrefix(text, "/review")))
			}
			// Commands the gateway also knows (/use, /rename, /models —
			// see gateway/threads.go maybeHandleSlashCommand). In gateway
			// mode we DELIBERATELY skip the local short-circuit and let
			// these flow through sendViaGateway, so the gateway can
			// persist server-side state (per-thread model pin, thread
			// rename) in the shared DB. Without this, /use haiku only
			// updated m.currentModel locally — but in gateway mode the
			// LLM dispatch goes through the gateway, which reads t.Model
			// from the DB, so the pin was invisible to the actual
			// request flow.
			if m.gw == nil {
				if strings.HasPrefix(text, "/use") {
					return m.handleUse(strings.TrimSpace(strings.TrimPrefix(text, "/use")))
				}
				if strings.HasPrefix(text, "/rename") {
					return m.handleRename(strings.TrimSpace(strings.TrimPrefix(text, "/rename")))
				}
				if text == "/models" {
					m.history = append(m.history, chatTurn{
						user:  text,
						agent: m.modelsText(),
					})
					m.input.SetValue("")
					return m, nil
				}
			}
			// Hand off to the agent in a goroutine. Bubbletea expects the
			// returned tea.Cmd to be a func() tea.Msg, so we wrap.
			userText := text
			m.input.SetValue("")
			m.busy = true
			m.busyStartedAt = time.Now()
			m.lastError = ""
			pending := chatTurn{user: userText}
			m.history = append(m.history, pending)
			// In gateway mode, the gateway is the one persisting + the
			// SSE stream is what surfaces messages on this CLI. Calling
			// persistMessage here too would double-write the same row
			// (CLI store and gateway store are the same SQLite file).
			if m.gw == nil {
				m.persistMessage("user", userText)
				return m, m.runAgent(userText)
			}
			return m, m.sendViaGateway(userText)
		}

	case chatResponseMsg:
		m.busy = false
		if len(m.history) == 0 {
			return m, nil
		}
		last := &m.history[len(m.history)-1]
		if msg.err != nil {
			last.agent = "error: " + msg.err.Error()
			m.lastError = msg.err.Error()
		} else {
			last.agent = msg.reply
			// Gateway mode: the gateway already persisted both messages
			// when it processed the POST. Writing again here would
			// duplicate rows in the shared SQLite store.
			if m.gw == nil {
				// threads.Store accepts only "user" | "agent" | "system" —
				// passing "assistant" silently fails validation, which is
				// why early builds lost the LLM reply on resume.
				m.persistMessage("agent", msg.reply)
			}
		}
		// Mark the just-rendered IDs as seen so the SSE echo of our own
		// post doesn't double-render. The POST response carries both
		// IDs; we recorded them in sendViaGateway already.
		return m, nil

	case gatewayEventMsg:
		return m.applyGatewayEvent(msg)

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	}

	// Forward everything else to the input.
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

// persistMessage appends a single message to the threads store if
// persistence is wired and a thread exists. Failures are silent — the
// REPL keeps working with an unflushed transcript rather than aborting
// the user's session over a disk hiccup.
func (m *chatModel) persistMessage(role, content string) {
	if m.threadStore == nil || m.threadID == "" || content == "" {
		return
	}
	_, _ = m.threadStore.Append(m.threadID, role, content, "", nil)
}

// sendViaGateway POSTs the user's message to the local gateway. The
// gateway persists it, runs the agent, and returns the synchronous
// reply — we map that into a chatResponseMsg so the existing render
// path lights up identically to the in-process flow. The SSE stream
// echoes both the user_message and agent_done events back at us; the
// IDs we capture here are recorded in seenMsgIDs so applyGatewayEvent
// can skip the echoes (they're the same messages we already rendered).
func (m chatModel) sendViaGateway(text string) tea.Cmd {
	gw := m.gw
	threadID := m.threadID
	seen := m.seenMsgIDs
	return func() tea.Msg {
		// Long-running send: no internal deadline — the gateway has its
		// own per-handler timeout, and a Ctrl+C still works thanks to
		// bubbletea's signal handling.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		userMsgID, agentMsgID, reply, err := gw.sendMessageWithIDs(ctx, threadID, text)
		if userMsgID != "" {
			seen[userMsgID] = true
		}
		if agentMsgID != "" {
			seen[agentMsgID] = true
		}
		return chatResponseMsg{reply: reply, err: err}
	}
}

// applyGatewayEvent handles one SSE event from the gateway and
// re-issues the listener Cmd so we keep consuming. Three event types
// matter to the CLI render:
//
//   - user_message: a phone (or other device) sent a message; render it
//     as a new turn so the user sees what's happening on the gateway.
//   - agent_thinking: any device is being processed by the agent right
//     now; show the spinner so this CLI reflects cross-device activity.
//   - agent_done: the agent's reply for some message landed. When it's
//     the reply for OUR own outgoing message, the POST path already
//     handled it (we cached the ID in seenMsgIDs); skip. Otherwise it's
//     the reply to a phone-sent message — render it.
func (m chatModel) applyGatewayEvent(ev gatewayEventMsg) (tea.Model, tea.Cmd) {
	relisten := waitGatewayEvent(m.gwEvents)
	if ev.Err != nil {
		// Stream ended (EOF, network blip, gateway restart). Don't
		// crash the chat — the in-process fall-through is gone for
		// this session, but we can still type and have the POST path
		// keep working. Stop relistening on EOF; if it's a transient
		// error, a future iteration can wire reconnect logic.
		return m, nil
	}
	switch ev.EventType {
	case "agent_thinking":
		// Another device kicked off agent work — show a spinner so this
		// CLI reflects that something's happening. Idempotent vs. our
		// own busy state from sendViaGateway.
		m.busy = true
		if m.busyStartedAt.IsZero() {
			m.busyStartedAt = time.Now()
		}
	case "agent_done", "agent_message":
		if m.seenMsgIDs != nil && ev.MessageID != "" && m.seenMsgIDs[ev.MessageID] {
			break
		}
		if ev.MessageID != "" {
			m.seenMsgIDs[ev.MessageID] = true
		}
		// A reply we didn't originate (phone-sent message). Render as a
		// fresh turn whose user side is whatever the most recent
		// user_message was — but we may have already added the user
		// turn when the user_message arrived, in which case fill its
		// agent slot instead.
		if n := len(m.history); n > 0 && m.history[n-1].agent == "" {
			m.history[n-1].agent = ev.Content
		} else {
			m.history = append(m.history, chatTurn{agent: ev.Content})
		}
		m.busy = false
	case "user_message":
		if m.seenMsgIDs != nil && ev.MessageID != "" && m.seenMsgIDs[ev.MessageID] {
			break
		}
		if ev.MessageID != "" {
			m.seenMsgIDs[ev.MessageID] = true
		}
		// A phone-sent user message. Append a pending turn so the next
		// agent_done lands in its agent slot. If the most recent turn
		// already has a user but no agent (a race where we appended
		// pending locally and then SSE caught up), don't double-add.
		if n := len(m.history); n > 0 && m.history[n-1].agent == "" && m.history[n-1].user == ev.Content {
			break
		}
		m.history = append(m.history, chatTurn{user: ev.Content})
	case "agent_error":
		m.busy = false
		m.lastError = ev.Content
		if n := len(m.history); n > 0 {
			m.history[n-1].agent = "error: " + ev.Content
		}
	}
	return m, relisten
}

func (m chatModel) runAgent(text string) tea.Cmd {
	model := m.currentModel
	return func() tea.Msg {
		msg := types.ChannelMessage{
			ChannelType: "cli",
			ChannelID:   "local",
			SenderID:    m.session.UserID,
			Text:        text,
			Timestamp:   time.Now().UTC(),
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		var reply string
		var err error
		if m.result.HandlerN != nil {
			reply, err = m.result.HandlerN(ctx, msg, m.session, model)
		} else {
			reply, err = m.result.Handler(ctx, msg, m.session)
		}
		return chatResponseMsg{reply: reply, err: err}
	}
}

// handleRename implements `/rename <title>` — renames the active thread
// in the shared SQLite store so it shows up under the new label in the
// CLI picker and the web/mobile thread drawer. Bare `/rename` prints the
// current title rather than blanking it (you can blank with /rename -).
func (m chatModel) handleRename(title string) (tea.Model, tea.Cmd) {
	m.input.SetValue("")
	if m.threadStore == nil || m.threadID == "" {
		m.history = append(m.history, chatTurn{
			user:  "/rename " + title,
			agent: "Thread persistence is off — nothing to rename. Re-run `fathom` to start a persisted thread.",
		})
		return m, nil
	}
	if title == "" {
		t, err := m.threadStore.Get(m.threadID)
		current := "(untitled)"
		if err == nil && t.Title != "" {
			current = t.Title
		}
		m.history = append(m.history, chatTurn{
			user:  "/rename",
			agent: "Current title: " + current + "\n\nUsage: /rename <new title>\n        /rename -        (clears the title)",
		})
		return m, nil
	}
	newTitle := title
	if newTitle == "-" {
		newTitle = "" // user-facing way to blank the title
	}
	if err := m.threadStore.Rename(m.threadID, newTitle); err != nil {
		m.history = append(m.history, chatTurn{
			user:  "/rename " + title,
			agent: "Rename failed: " + err.Error(),
		})
		return m, nil
	}
	shown := newTitle
	if shown == "" {
		shown = "(untitled)"
	}
	m.history = append(m.history, chatTurn{
		user:  "/rename " + title,
		agent: "Thread renamed → " + shown,
	})
	return m, nil
}

// handleUse implements `/use <model>` — switches the current model for
// subsequent turns. `/use default` reverts. Unknown names are reported
// rather than silently dropped.
func (m chatModel) handleUse(name string) (tea.Model, tea.Cmd) {
	m.input.SetValue("")
	if name == "" {
		m.history = append(m.history, chatTurn{
			user:  "/use",
			agent: "Usage: /use <model-name>\n\n" + m.modelsText(),
		})
		return m, nil
	}
	if name == "default" {
		m.currentModel = ""
		_, defName := m.result.Router.Default()
		m.history = append(m.history, chatTurn{
			user:  "/use default",
			agent: "Reverted to default model: " + defName,
		})
		return m, nil
	}
	if m.result.Router == nil {
		m.history = append(m.history, chatTurn{
			user:  "/use " + name,
			agent: "Only one model is configured. To enable /use, add an `llm.models` map to your fathom.config.yaml.",
		})
		return m, nil
	}
	if _, err := m.result.Router.Get(name); err != nil {
		m.history = append(m.history, chatTurn{
			user:  "/use " + name,
			agent: "Unknown model: " + name + "\n\n" + m.modelsText(),
		})
		return m, nil
	}
	m.currentModel = name
	m.history = append(m.history, chatTurn{
		user:  "/use " + name,
		agent: "Switched to model: " + name,
	})
	return m, nil
}

// handleReview runs the last assistant reply (or the prior user prompt
// + reply) through a different model with a critique prompt. The critic
// has full tool access — it can grep, read files, run tests — to verify
// the primary's claims rather than just paraphrasing them.
func (m chatModel) handleReview(criticOverride string) (tea.Model, tea.Cmd) {
	m.input.SetValue("")
	if len(m.history) == 0 {
		m.history = append(m.history, chatTurn{
			user:  "/review",
			agent: "Nothing to review yet — ask something first, then /review the answer.",
		})
		return m, nil
	}
	last := m.history[len(m.history)-1]
	if last.agent == "" || m.busy {
		m.history = append(m.history, chatTurn{
			user:  "/review",
			agent: "Wait for the current answer to finish before /review.",
		})
		return m, nil
	}
	// Pick the critic model.
	var criticName string
	if m.result.Router != nil {
		if criticOverride != "" {
			if _, err := m.result.Router.Get(criticOverride); err != nil {
				m.history = append(m.history, chatTurn{
					user:  "/review " + criticOverride,
					agent: "Unknown critic model: " + criticOverride + "\n\n" + m.modelsText(),
				})
				return m, nil
			}
			criticName = criticOverride
		} else {
			_, criticName = m.result.Router.PickCritic(m.currentModel)
		}
	}
	primaryName := m.currentModel
	if primaryName == "" && m.result.Router != nil {
		_, primaryName = m.result.Router.Default()
	}
	if criticName == primaryName && m.result.Router != nil {
		m.history = append(m.history, chatTurn{
			user:  "/review",
			agent: "Only one model is available; /review needs a second model. Add an `llm.models` entry named 'critic' (or any second model) and try again.",
		})
		return m, nil
	}

	// Compose the critique prompt. Pass the user's question + primary's
	// answer, ask for a rigorous critique with full tool access. The critic
	// runs the SAME agent loop with the SAME tool registry — so it can
	// grep code, read files, run tests to validate the answer.
	prompt := buildReviewPrompt(last.user, last.agent, primaryName)
	m.busy = true
	m.history = append(m.history, chatTurn{user: "/review (via " + criticName + ")"})
	return m, m.runReview(prompt, criticName)
}

func (m chatModel) runReview(prompt, criticModel string) tea.Cmd {
	return func() tea.Msg {
		msg := types.ChannelMessage{
			ChannelType: "cli",
			ChannelID:   "local",
			SenderID:    m.session.UserID,
			Text:        prompt,
			Timestamp:   time.Now().UTC(),
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		reply, err := m.result.HandlerN(ctx, msg, m.session, criticModel)
		return chatResponseMsg{reply: reply, err: err}
	}
}

func buildReviewPrompt(userQ, primaryAnswer, primaryModel string) string {
	pm := primaryModel
	if pm == "" {
		pm = "the primary model"
	}
	return "You are reviewing an answer for accuracy and quality. Be rigorous and critical — your job is to find what's wrong, missing, or worth challenging. You have full tool access: grep the repo, read files, run tests, check facts. Don't just paraphrase the answer.\n\nUser asked:\n" + userQ + "\n\nAnswer from " + pm + ":\n" + primaryAnswer + "\n\nProvide:\n1. What's correct (briefly)\n2. What's wrong, missing, or hand-waved (with evidence you verified)\n3. Concrete suggestions to improve\n\nIf the answer is solid, say so plainly — don't manufacture critique."
}

func (m chatModel) helpText() string {
	lines := []string{
		"Commands (type / for the popup menu):",
		"  /use <model>     — switch active model for subsequent turns",
		"  /use default     — revert to the default model",
		"  /models          — list configured models",
		"  /review [critic] — critique the last answer with a second model",
		"  /rename <title>  — name the current thread (bare /rename shows current; /rename - clears)",
		"  /help            — this message",
		"  /quit, /exit     — leave the chat",
		"",
		"Popup keys:",
		"  ↑ / ↓            — navigate the slash-command menu",
		"  Tab              — complete the highlighted command",
		"  Esc              — dismiss the popup (second Esc exits chat)",
		"",
		"Tip: type /use<space>NAME to switch models — the popup gets out of the way",
		"once you've picked the command, so you can read what you're typing.",
	}
	return strings.Join(lines, "\n")
}

func (m chatModel) modelsText() string {
	if m.result.Router == nil {
		return "Single-model setup; no /use available. Add an `llm.models` map to fathom.config.yaml to enable model switching."
	}
	names := m.result.Router.Names()
	_, defName := m.result.Router.Default()
	cur := m.currentModel
	if cur == "" {
		cur = defName + " (default)"
	}
	var b strings.Builder
	b.WriteString("Current: " + cur + "\n\nConfigured:")
	for _, n := range names {
		marker := "  "
		if n == defName {
			marker = "* "
		}
		b.WriteString("\n" + marker + n)
	}
	return b.String()
}

// View renders the whole chat surface: banner, history, input box.
func (m chatModel) View() string {
	if m.quitting {
		return ""
	}
	var b strings.Builder

	// Banner + welcome — printed at the top, never scrolls out.
	b.WriteString(m.renderWelcome())

	// History: each turn renders the user message in a rounded box, then
	// the agent reply as indented prose. Empty agent reply means the call
	// is still in-flight; show the spinner instead.
	thinking := lipgloss.NewStyle().Foreground(lipgloss.Color("244")).Italic(true)
	for i, turn := range m.history {
		b.WriteString(m.renderUserBox(turn.user))
		if turn.agent == "" && i == len(m.history)-1 && m.busy {
			b.WriteString(renderSpinnerWithLabel(
				m.spinner.View(),
				thinking.Render(spinnerWord(m.busyStartedAt)),
				"  ",
			))
		} else if turn.agent != "" {
			b.WriteString(m.renderAgentReply(turn.agent))
		}
	}

	// Slash-command popup floats just above the input box when the user is
	// composing a /command and hasn't typed past the command token yet.
	if popup := m.renderSlashPopup(); popup != "" {
		b.WriteString(popup)
	}

	// Input box at the bottom — always closed on all four sides.
	b.WriteString(m.renderInputBox())
	return b.String()
}

// renderSlashPopup draws the floating command menu above the input. Returns
// "" when no slash command is active.
func (m chatModel) renderSlashPopup() string {
	cmds := m.filteredSlash()
	if len(cmds) == 0 {
		return ""
	}
	// Clamp slashIndex into range — filter may have shrunk under us.
	idx := m.slashIndex
	if idx >= len(cmds) {
		idx = len(cmds) - 1
	}
	if idx < 0 {
		idx = 0
	}
	// Two columns: command (padded) + description.
	nameW := 14
	for _, c := range cmds {
		if len(c.name) > nameW {
			nameW = len(c.name)
		}
	}
	var lines []string
	for i, c := range cmds {
		name := lipgloss.NewStyle().Foreground(lipgloss.Color("215")).Bold(true).Render(pad(c.name, nameW))
		desc := lipgloss.NewStyle().Foreground(lipgloss.Color("244")).Render(c.desc)
		row := name + "  " + desc
		if i == idx {
			row = lipgloss.NewStyle().
				Foreground(lipgloss.Color("253")).
				Background(lipgloss.Color("236")).
				Render(" " + lipgloss.NewStyle().Foreground(lipgloss.Color("215")).Bold(true).Render(pad(c.name, nameW)) +
					"  " + lipgloss.NewStyle().Foreground(lipgloss.Color("253")).Render(c.desc) + " ")
		} else {
			row = " " + row + " "
		}
		lines = append(lines, row)
	}
	hint := lipgloss.NewStyle().Foreground(lipgloss.Color("244")).Italic(true).
		Render("  ↑/↓ navigate · Tab to complete · Esc to dismiss")
	style := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("67")).
		Padding(0, 1).
		Width(m.boxContentWidth())
	return style.Render(strings.Join(lines, "\n")+"\n"+hint) + "\n"
}

func pad(s string, w int) string {
	if len(s) >= w {
		return s
	}
	return s + strings.Repeat(" ", w-len(s))
}

func (m chatModel) renderWelcome() string {
	var b strings.Builder
	b.WriteString("\n")
	// Wordmark + "by fathom" subtitle — shared with `fathom join` and the
	// gateway splash via ui.BannerBlock so the three never drift apart.
	b.WriteString(ui.BannerBlock(m.width))
	b.WriteString("\n")
	dot := lipgloss.NewStyle().Foreground(lipgloss.Color("244")).Render("·")
	tag := lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	body := lipgloss.NewStyle().Foreground(lipgloss.Color("253"))
	b.WriteString("  " + body.Render("Local agent") + "  " + dot + "  " + tag.Render("github.com/zdaniels/fathom") + "\n\n")
	b.WriteString(kv("agent", m.result.Description))
	if m.cwd != "" {
		b.WriteString(kv("workspace", m.cwd))
	}
	b.WriteString(kv("commands", "/quit  /help  · Esc to exit"))
	b.WriteString("\n")
	return b.String()
}

// boxStyle is the rounded box used for both the user's submitted messages
// and the live input. lipgloss adds 1 char of border on each side; we want
// the rendered box to span the FULL terminal width (m.width). So:
//
//	content width inside box = m.width - 2 (left border, right border)
//
// With Padding(0, 1) lipgloss reserves another 1 char each side for inner
// space, leaving m.width - 4 cols of usable content area.
func (m chatModel) boxStyle() lipgloss.Style {
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("67")).
		Padding(0, 1).
		Width(m.boxContentWidth())
}

// boxContentWidth is the value to pass to lipgloss .Width() so the FINAL
// rendered line (including borders) equals m.width.
func (m chatModel) boxContentWidth() int {
	// lipgloss.Width(N) means "content area is N cols"; the final render
	// is borders(2) + padding(2) + N = N + 4 chars. To fill m.width we
	// need N = m.width - 4.
	w := m.width - 4
	if w < 16 {
		w = 16
	}
	return w
}

// inputWidth is the editable area inside the input box (subtract "> " prompt).
func (m chatModel) inputWidth() int {
	return m.boxContentWidth() - 2 // "> " takes 2 cols
}

// contentWidth is how wide unbordered prose (agent replies) wraps to.
// Two-col indent on each side keeps the prose visually inset from the boxes.
func (m chatModel) contentWidth() int {
	w := m.width - 4
	if w < 16 {
		w = 16
	}
	return w
}

func (m chatModel) renderUserBox(text string) string {
	cursor := lipgloss.NewStyle().Foreground(lipgloss.Color("67")).Render("›")
	return m.boxStyle().Render(cursor+" "+text) + "\n\n"
}

func (m chatModel) renderAgentReply(text string) string {
	w := m.contentWidth()
	style := lipgloss.NewStyle().Foreground(lipgloss.Color("253")).Width(w)
	out := style.Render(text)
	// Indent every line two cols to inset agent prose from the full-width
	// box edges — reads as "inside" the conversation rather than alongside.
	lines := strings.Split(out, "\n")
	for i := range lines {
		lines[i] = "  " + lines[i]
	}
	return strings.Join(lines, "\n") + "\n\n"
}

func (m chatModel) renderInputBox() string {
	prompt := lipgloss.NewStyle().Foreground(lipgloss.Color("67")).Bold(true).Render(">")
	box := m.boxStyle().Render(prompt + " " + m.input.View())
	// When a quit key is armed, show a dim one-line hint under the box so the
	// user knows a second press exits — or that any other key cancels.
	if m.quitArm != "" {
		key := "Esc"
		if m.quitArm == "ctrl+c" {
			key = "Ctrl+C"
		}
		hint := lipgloss.NewStyle().Foreground(lipgloss.Color("244")).Italic(true).
			Render("  press " + key + " again to quit · any other key cancels")
		return box + "\n" + hint
	}
	return box
}

func kv(key, value string) string {
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

func makeLocalSession() types.Session {
	now := time.Now().UTC()
	return types.Session{
		ID: "local-chat-session",
		// "admin" matches what the gateway hands out to paired devices
		// (see internal/auth/manager.go SetupInitialToken). Unifying
		// the userID across CLI + gateway means a thread started on the
		// phone shows up in `fathom threads` on the laptop and vice
		// versa — same SQLite, same row, same filter bucket.
		UserID:    "admin",
		DeviceID:  "local-device",
		CreatedAt: now,
		ExpiresAt: now.Add(24 * time.Hour),
		Permissions: types.PermissionSet{
			Network: "allow", Filesystem: "read-write", Shell: "deny", Secrets: "accessible",
		},
	}
}

// Package threads persists chat threads + messages so a conversation
// outlives any one device session.
//
// Two tables (one SQLite file at ~/.fantazm/threads.db):
//
//	threads  — user-facing conversation. Title, created/updated, soft-delete.
//	messages — append-only ordered messages, foreign-keyed to threads.
//
// Multi-device continuity falls out: the Mac menubar, the CLI chat, and
// the phone can all subscribe to the same thread, each rendering history
// from the same source. Live updates use the gateway's pub-sub (see
// internal/gateway/threads.go), not this package — Store is purely the
// durable spine.
package threads

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/zdaniels/fathom/internal/brandenv"
	"os"
	"path/filepath"
	"time"

	"github.com/zdaniels/fathom/internal/security"
	_ "modernc.org/sqlite"
)

// Thread is one persistent conversation.
type Thread struct {
	ID        string     `json:"id"`
	UserID    string     `json:"user_id"`
	Title     string     `json:"title"`
	Model     string     `json:"model,omitempty"` // per-thread model override (matches an llm.models key); empty → user default
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	DeletedAt *time.Time `json:"deleted_at,omitempty"`
}

// Message is one turn — user or agent. ToolCalls + Usage land in Metadata
// as JSON; we don't promote them to columns because (a) they're variable
// shape and (b) the messages table stays simple, with anything richer
// handled at render time.
type Message struct {
	ID        string                 `json:"id"`
	ThreadID  string                 `json:"thread_id"`
	Role      string                 `json:"role"` // "user" | "agent" | "system"
	Content   string                 `json:"content"`
	DeviceID  string                 `json:"device_id,omitempty"`
	CreatedAt time.Time              `json:"created_at"`
	Metadata  map[string]interface{} `json:"metadata,omitempty"`
}

// Store is the persistence handle. Safe for concurrent use — SQLite
// serialises writes, and we use WAL so concurrent reads don't block.
type Store struct {
	db   *sql.DB
	path string
}

// Sentinel errors so HTTP handlers can map cleanly to status codes.
var (
	ErrNotFound = errors.New("thread not found")
	ErrDeleted  = errors.New("thread deleted")
)

// DefaultDBPath returns ~/.fantazm/threads.db (or $FANTAZM_THREADS_DB).
// Lives alongside schedule.db + audit.db; same trust model.
func DefaultDBPath() string {
	if env := brandenv.Get("FATHOM_THREADS_DB"); env != "" {
		return env
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".fantazm", "threads.db")
}

// OpenStore opens or creates the SQLite database. Idempotent schema
// init. Uses WAL so readers don't block writers (mobile UI scrolling
// history while the agent is streaming a response, etc.).
func OpenStore(dbPath string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		return nil, err
	}
	if _, err := db.Exec("PRAGMA foreign_keys=ON"); err != nil {
		return nil, err
	}
	schema := `
		CREATE TABLE IF NOT EXISTS threads (
			id         TEXT PRIMARY KEY,
			user_id    TEXT NOT NULL,
			title      TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			deleted_at TEXT
		);
		CREATE INDEX IF NOT EXISTS idx_threads_user_updated
			ON threads(user_id, updated_at DESC);

		CREATE TABLE IF NOT EXISTS messages (
			id         TEXT PRIMARY KEY,
			thread_id  TEXT NOT NULL REFERENCES threads(id) ON DELETE CASCADE,
			role       TEXT NOT NULL,
			content    TEXT NOT NULL,
			device_id  TEXT,
			created_at TEXT NOT NULL,
			metadata   TEXT
		);
		CREATE INDEX IF NOT EXISTS idx_messages_thread_time
			ON messages(thread_id, created_at);
	`
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("threads: schema create: %w", err)
	}
	// Idempotent column add — `threads.model` ships in a later
	// schema revision than the original threads table. SQLite has
	// no IF NOT EXISTS for ALTER, so we probe + add when missing.
	if err := ensureColumn(db, "threads", "model", "TEXT NOT NULL DEFAULT ''"); err != nil {
		db.Close()
		return nil, fmt.Errorf("threads: migrate model column: %w", err)
	}
	// FTS5 index over messages.content + triggers to keep it in sync.
	// External-content table — the FTS table doesn't duplicate the
	// content column on disk; it just indexes what's in `messages`.
	// Triggers keep the index up to date on insert / update / delete.
	if err := ensureFTS(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("threads: setup FTS: %w", err)
	}
	return &Store{db: db, path: dbPath}, nil
}

// Close releases the underlying DB. Safe to call multiple times.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// Create inserts a new thread for userID. title is optional; empty
// means "auto-title later from the first user message."
func (s *Store) Create(userID, title string) (Thread, error) {
	now := time.Now().UTC()
	t := Thread{
		ID:        security.GenerateID(),
		UserID:    userID,
		Title:     title,
		CreatedAt: now,
		UpdatedAt: now,
	}
	_, err := s.db.Exec(
		`INSERT INTO threads (id, user_id, title, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
		t.ID, t.UserID, t.Title, t.CreatedAt.Format(time.RFC3339Nano), t.UpdatedAt.Format(time.RFC3339Nano))
	if err != nil {
		return Thread{}, fmt.Errorf("threads: insert: %w", err)
	}
	return t, nil
}

// Get returns a single thread by ID. ErrNotFound when missing.
func (s *Store) Get(id string) (Thread, error) {
	row := s.db.QueryRow(
		`SELECT id, user_id, title, model, created_at, updated_at, deleted_at
		 FROM threads WHERE id = ?`, id)
	return scanThread(row)
}

// List returns up to `limit` non-deleted threads for userID, newest
// first. limit ≤ 0 → 50.
func (s *Store) List(userID string, limit int) ([]Thread, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.Query(
		`SELECT id, user_id, title, model, created_at, updated_at, deleted_at
		 FROM threads
		 WHERE user_id = ? AND deleted_at IS NULL
		 ORDER BY updated_at DESC
		 LIMIT ?`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Thread
	for rows.Next() {
		t, err := scanThread(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// SetModel pins this thread to a specific named LLM model. Empty
// string clears the override and the thread reverts to the user
// default at send time. Doesn't bump updated_at — the model is a
// metadata choice, not a content edit.
func (s *Store) SetModel(id, model string) error {
	res, err := s.db.Exec(
		`UPDATE threads SET model = ? WHERE id = ? AND deleted_at IS NULL`,
		model, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ensureFTS sets up the FTS5 virtual table + insert/update/delete
// triggers, then backfills existing rows on first run. Idempotent:
// re-running is a no-op if the table already exists.
func ensureFTS(db *sql.DB) error {
	// External-content FTS5: the virtual table doesn't store
	// content separately; it indexes what's in `messages`. Saves disk
	// space at the cost of a triggered sync on writes.
	stmts := []string{
		`CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts USING fts5(
			content,
			content='messages',
			content_rowid='rowid',
			tokenize='porter unicode61'
		)`,
		`CREATE TRIGGER IF NOT EXISTS messages_fts_ai AFTER INSERT ON messages BEGIN
			INSERT INTO messages_fts(rowid, content) VALUES (new.rowid, new.content);
		END`,
		`CREATE TRIGGER IF NOT EXISTS messages_fts_ad AFTER DELETE ON messages BEGIN
			INSERT INTO messages_fts(messages_fts, rowid, content)
			VALUES('delete', old.rowid, old.content);
		END`,
		`CREATE TRIGGER IF NOT EXISTS messages_fts_au AFTER UPDATE ON messages BEGIN
			INSERT INTO messages_fts(messages_fts, rowid, content)
			VALUES('delete', old.rowid, old.content);
			INSERT INTO messages_fts(rowid, content) VALUES (new.rowid, new.content);
		END`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			return err
		}
	}
	// Backfill if the FTS table is empty but messages isn't (i.e. an
	// existing DB getting the FTS retrofit). One-time cost on upgrade.
	var ftsCount, msgCount int
	_ = db.QueryRow(`SELECT COUNT(*) FROM messages_fts`).Scan(&ftsCount)
	_ = db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&msgCount)
	if ftsCount == 0 && msgCount > 0 {
		if _, err := db.Exec(
			`INSERT INTO messages_fts(rowid, content) SELECT rowid, content FROM messages`,
		); err != nil {
			return fmt.Errorf("fts backfill: %w", err)
		}
	}
	return nil
}

// ThreadUsage is the aggregated token spend across one thread.
type ThreadUsage struct {
	ThreadID         string `json:"thread_id"`
	PromptTokens     int    `json:"prompt_tokens"`
	CompletionTokens int    `json:"completion_tokens"`
	TotalTokens      int    `json:"total_tokens"`
	MessageCount     int    `json:"message_count"` // messages with usage data (not all messages)
}

// SumThreadUsage walks every message in a thread that has usage in
// its metadata and returns the cumulative token counts. Messages
// from local providers (Ollama, etc.) that didn't report usage are
// skipped — no faking zeroes into the average.
func (s *Store) SumThreadUsage(threadID string) (ThreadUsage, error) {
	out := ThreadUsage{ThreadID: threadID}
	rows, err := s.db.Query(
		`SELECT metadata FROM messages
		 WHERE thread_id = ? AND metadata IS NOT NULL AND metadata != ''`,
		threadID)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var raw sql.NullString
		if err := rows.Scan(&raw); err != nil {
			return out, err
		}
		if !raw.Valid || raw.String == "" {
			continue
		}
		var meta struct {
			Usage *struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(raw.String), &meta); err != nil {
			continue
		}
		if meta.Usage == nil {
			continue
		}
		out.PromptTokens += meta.Usage.PromptTokens
		out.CompletionTokens += meta.Usage.CompletionTokens
		out.MessageCount++
	}
	out.TotalTokens = out.PromptTokens + out.CompletionTokens
	return out, rows.Err()
}

// SearchHit is one match returned by SearchMessages.
type SearchHit struct {
	ThreadID    string    `json:"thread_id"`
	ThreadTitle string    `json:"thread_title"`
	MessageID   string    `json:"message_id"`
	Role        string    `json:"role"`
	Snippet     string    `json:"snippet"` // FTS5-generated, contains <mark> around matches
	CreatedAt   time.Time `json:"created_at"`
}

// SearchMessages runs an FTS5 query and returns matching messages
// across every non-deleted thread for the given user. Results are
// ordered by relevance (FTS rank), then by recency as a tie-breaker.
//
// `userID` MUST be passed — search results never cross user boundaries
// even on a multi-tenant gateway. Pass the empty string in tests if
// you've only created one user's worth of data.
//
// The snippet has `<mark>…</mark>` around matched terms — clients can
// strip the markup or render them.
func (s *Store) SearchMessages(userID, query string, limit int) ([]SearchHit, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if query == "" {
		return nil, nil
	}
	// 64-char snippet, 3-token window on each side of a match.
	rows, err := s.db.Query(
		`SELECT
			t.id,
			t.title,
			m.id,
			m.role,
			snippet(messages_fts, 0, '<mark>', '</mark>', '…', 12),
			m.created_at
		 FROM messages_fts
		 JOIN messages t_msg ON messages_fts.rowid = t_msg.rowid
		 JOIN messages m    ON m.rowid = messages_fts.rowid
		 JOIN threads t     ON t.id = m.thread_id
		 WHERE messages_fts MATCH ?
		   AND t.user_id = ?
		   AND t.deleted_at IS NULL
		 ORDER BY rank, m.created_at DESC
		 LIMIT ?`,
		query, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SearchHit
	for rows.Next() {
		var h SearchHit
		var ts string
		if err := rows.Scan(&h.ThreadID, &h.ThreadTitle, &h.MessageID, &h.Role, &h.Snippet, &ts); err != nil {
			return nil, err
		}
		if t, perr := time.Parse(time.RFC3339Nano, ts); perr == nil {
			h.CreatedAt = t
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// ensureColumn is a tiny ALTER-IF-MISSING helper for sqlite, which
// doesn't support ALTER TABLE ADD COLUMN IF NOT EXISTS. We probe the
// existing column list via pragma_table_info and add only when the
// column doesn't already exist. Idempotent on every startup.
func ensureColumn(db *sql.DB, table, col, decl string) error {
	rows, err := db.Query(
		`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		if name == col {
			return nil
		}
	}
	_, err = db.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, col, decl))
	return err
}

// Rename updates a thread's title and bumps updated_at.
func (s *Store) Rename(id, title string) error {
	now := time.Now().UTC()
	res, err := s.db.Exec(
		`UPDATE threads SET title = ?, updated_at = ? WHERE id = ? AND deleted_at IS NULL`,
		title, now.Format(time.RFC3339Nano), id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// SoftDelete marks the thread deleted but keeps the row + messages so
// undo is possible. Callers can hard-purge later via a sweeper.
func (s *Store) SoftDelete(id string) error {
	now := time.Now().UTC()
	res, err := s.db.Exec(
		`UPDATE threads SET deleted_at = ? WHERE id = ? AND deleted_at IS NULL`,
		now.Format(time.RFC3339Nano), id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// Restore clears deleted_at so a soft-deleted thread reappears in
// List. Returns ErrNotFound if no such thread exists OR if the
// thread is already non-deleted.
func (s *Store) Restore(id string) error {
	res, err := s.db.Exec(
		`UPDATE threads SET deleted_at = NULL, updated_at = ?
		 WHERE id = ? AND deleted_at IS NOT NULL`,
		time.Now().UTC().Format(time.RFC3339Nano), id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// Append writes one message and bumps the thread's updated_at so
// List() returns it at the top.
//
// Returns the persisted Message — with its generated ID + timestamp —
// so the caller can immediately publish it to subscribers.
func (s *Store) Append(threadID, role, content, deviceID string, metadata map[string]interface{}) (Message, error) {
	if role != "user" && role != "agent" && role != "system" {
		return Message{}, fmt.Errorf("threads: invalid role %q", role)
	}
	t, err := s.Get(threadID)
	if err != nil {
		return Message{}, err
	}
	if t.DeletedAt != nil {
		return Message{}, ErrDeleted
	}
	now := time.Now().UTC()
	m := Message{
		ID:        security.GenerateID(),
		ThreadID:  threadID,
		Role:      role,
		Content:   content,
		DeviceID:  deviceID,
		CreatedAt: now,
		Metadata:  metadata,
	}
	var metaJSON sql.NullString
	if len(metadata) > 0 {
		b, err := json.Marshal(metadata)
		if err != nil {
			return Message{}, fmt.Errorf("threads: metadata marshal: %w", err)
		}
		metaJSON = sql.NullString{String: string(b), Valid: true}
	}
	tx, err := s.db.Begin()
	if err != nil {
		return Message{}, err
	}
	if _, err := tx.Exec(
		`INSERT INTO messages (id, thread_id, role, content, device_id, created_at, metadata)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		m.ID, m.ThreadID, m.Role, m.Content, m.DeviceID,
		m.CreatedAt.Format(time.RFC3339Nano), metaJSON,
	); err != nil {
		tx.Rollback()
		return Message{}, fmt.Errorf("threads: insert message: %w", err)
	}
	if _, err := tx.Exec(
		`UPDATE threads SET updated_at = ? WHERE id = ?`,
		now.Format(time.RFC3339Nano), threadID,
	); err != nil {
		tx.Rollback()
		return Message{}, fmt.Errorf("threads: bump updated_at: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Message{}, err
	}
	return m, nil
}

// All returns every non-deleted thread across every user, oldest first.
// Used by export tooling (`fathom export`) — production read paths
// should stay on List() which scopes to a user.
func (s *Store) All() ([]Thread, error) {
	rows, err := s.db.Query(
		`SELECT id, user_id, title, model, created_at, updated_at, deleted_at
		 FROM threads
		 WHERE deleted_at IS NULL
		 ORDER BY created_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Thread
	for rows.Next() {
		t, err := scanThread(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// AllMessages returns every message in threadID in chronological order.
// No limit; used by export tooling.
func (s *Store) AllMessages(threadID string) ([]Message, error) {
	return s.queryMessages(
		`SELECT id, thread_id, role, content, device_id, created_at, metadata
		 FROM messages
		 WHERE thread_id = ?
		 ORDER BY created_at ASC`,
		threadID)
}

// MessagesAfter returns messages added to threadID after `since` (exclusive),
// in chronological order. Used by clients reconnecting after a network
// hiccup to catch up on missed messages.
func (s *Store) MessagesAfter(threadID string, since time.Time, limit int) ([]Message, error) {
	if limit <= 0 {
		limit = 200
	}
	return s.queryMessages(
		`SELECT id, thread_id, role, content, device_id, created_at, metadata
		 FROM messages
		 WHERE thread_id = ? AND created_at > ?
		 ORDER BY created_at ASC
		 LIMIT ?`,
		threadID, since.Format(time.RFC3339Nano), limit)
}

// MessagesTail returns the last `limit` messages in chronological order.
// Used to bootstrap a client showing a thread for the first time.
func (s *Store) MessagesTail(threadID string, limit int) ([]Message, error) {
	if limit <= 0 {
		limit = 50
	}
	// Subquery to grab last-N then re-sort ASC for the client.
	return s.queryMessages(
		`SELECT id, thread_id, role, content, device_id, created_at, metadata
		 FROM (
		   SELECT * FROM messages WHERE thread_id = ?
		   ORDER BY created_at DESC LIMIT ?
		 )
		 ORDER BY created_at ASC`,
		threadID, limit)
}

// MessagesBefore returns the page of `limit` messages immediately before
// `until` (exclusive), in chronological order. Used for backward
// scrollback in the mobile UI.
func (s *Store) MessagesBefore(threadID string, until time.Time, limit int) ([]Message, error) {
	if limit <= 0 {
		limit = 50
	}
	return s.queryMessages(
		`SELECT id, thread_id, role, content, device_id, created_at, metadata
		 FROM (
		   SELECT * FROM messages
		   WHERE thread_id = ? AND created_at < ?
		   ORDER BY created_at DESC LIMIT ?
		 )
		 ORDER BY created_at ASC`,
		threadID, until.Format(time.RFC3339Nano), limit)
}

func (s *Store) queryMessages(query string, args ...interface{}) ([]Message, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		var (
			m      Message
			device sql.NullString
			meta   sql.NullString
			ts     string
		)
		if err := rows.Scan(&m.ID, &m.ThreadID, &m.Role, &m.Content, &device, &ts, &meta); err != nil {
			return nil, err
		}
		m.DeviceID = device.String
		t, err := time.Parse(time.RFC3339Nano, ts)
		if err != nil {
			return nil, fmt.Errorf("threads: parse created_at: %w", err)
		}
		m.CreatedAt = t
		if meta.Valid && meta.String != "" {
			_ = json.Unmarshal([]byte(meta.String), &m.Metadata)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// rowScanner is the subset of sql.Row / sql.Rows we use for thread
// scanning — lets us share code between Get (Row) and List (Rows).
type rowScanner interface {
	Scan(dest ...interface{}) error
}

func scanThread(r rowScanner) (Thread, error) {
	var (
		t                Thread
		title, model     string
		created, updated string
		deleted          sql.NullString
	)
	err := r.Scan(&t.ID, &t.UserID, &title, &model, &created, &updated, &deleted)
	if err == sql.ErrNoRows {
		return Thread{}, ErrNotFound
	}
	if err != nil {
		return Thread{}, err
	}
	t.Title = title
	t.Model = model
	if t.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
		return Thread{}, fmt.Errorf("threads: parse created_at: %w", err)
	}
	if t.UpdatedAt, err = time.Parse(time.RFC3339Nano, updated); err != nil {
		return Thread{}, fmt.Errorf("threads: parse updated_at: %w", err)
	}
	if deleted.Valid && deleted.String != "" {
		td, err := time.Parse(time.RFC3339Nano, deleted.String)
		if err != nil {
			return Thread{}, fmt.Errorf("threads: parse deleted_at: %w", err)
		}
		t.DeletedAt = &td
	}
	return t, nil
}

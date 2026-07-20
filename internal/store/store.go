package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"AI-agent/internal/mistral"
)

type Store struct {
	db *sql.DB
	mu sync.Mutex
}

type Task struct {
	ID              string    `json:"id"`
	UserID          int64     `json:"user_id"`
	ChatID          int64     `json:"chat_id,omitempty"`
	Kind            string    `json:"kind"`
	Text            string    `json:"text,omitempty"`
	URL             string    `json:"url,omitempty"`
	IntervalSeconds int64     `json:"interval_seconds"`
	NextRunAt       time.Time `json:"next_run_at"`
	LastFingerprint string    `json:"last_fingerprint,omitempty"`
	LastSnapshot    string    `json:"last_snapshot,omitempty"`
	Active          bool      `json:"active"`
	CreatedAt       time.Time `json:"created_at"`
}

type Document struct {
	ID        string    `json:"id"`
	UserID    int64     `json:"user_id"`
	Name      string    `json:"name"`
	MimeType  string    `json:"mime_type"`
	URL       string    `json:"url"`
	CreatedAt time.Time `json:"created_at"`
}

type UserRecord struct {
	UserID           int64              `json:"user_id"`
	Timezone         string             `json:"timezone,omitempty"`
	HomeLocation     string             `json:"home_location,omitempty"`
	FirstSeenAt      time.Time          `json:"first_seen_at"`
	LastSeenAt       time.Time          `json:"last_seen_at"`
	PromptTokens     int                `json:"prompt_tokens"`
	CompletionTokens int                `json:"completion_tokens"`
	TotalTokens      int                `json:"total_tokens"`
	Messages         []mistral.Message  `json:"messages"`
	Accounts         map[string]Account `json:"accounts,omitempty"`
}

type Account struct {
	Provider              string    `json:"provider"`
	ConnectedAt           time.Time `json:"connected_at"`
	Scopes                []string  `json:"scopes"`
	EncryptedAccessToken  string    `json:"encrypted_access_token"`
	EncryptedRefreshToken string    `json:"encrypted_refresh_token,omitempty"`
	TokenType             string    `json:"token_type"`
	Expiry                time.Time `json:"expiry"`
}

type legacyData struct {
	Users map[string]*UserRecord `json:"users"`
	Tasks map[string]*Task       `json:"tasks"`
}

func Open(path string) (*Store, error) {
	if path == "" {
		path = "data/agent.db"
	}
	if filepath.Ext(path) == ".json" {
		path = filepath.Join(filepath.Dir(path), "agent.db")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	st := &Store{db: db}
	if err := st.initSchema(); err != nil {
		db.Close()
		return nil, err
	}
	_ = os.Chmod(path, 0o600)
	if err := st.migrateLegacyJSON(path); err != nil {
		db.Close()
		return nil, err
	}
	return st, nil
}

func (s *Store) initSchema() error {
	_, err := s.db.Exec(`
PRAGMA journal_mode = WAL;
PRAGMA foreign_keys = ON;
CREATE TABLE IF NOT EXISTS users (
    user_id INTEGER PRIMARY KEY,
	timezone TEXT NOT NULL DEFAULT '',
	home_location TEXT NOT NULL DEFAULT '',
    first_seen_at TEXT NOT NULL,
    last_seen_at TEXT NOT NULL,
    prompt_tokens INTEGER NOT NULL DEFAULT 0,
    completion_tokens INTEGER NOT NULL DEFAULT 0,
    total_tokens INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS messages (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id INTEGER NOT NULL REFERENCES users(user_id) ON DELETE CASCADE,
    role TEXT NOT NULL,
    content TEXT NOT NULL,
    name TEXT,
    tool_call_id TEXT,
    tool_calls TEXT,
    created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_messages_user_id ON messages(user_id, id);
CREATE TABLE IF NOT EXISTS accounts (
    user_id INTEGER NOT NULL REFERENCES users(user_id) ON DELETE CASCADE,
    provider TEXT NOT NULL,
    connected_at TEXT NOT NULL,
    scopes TEXT NOT NULL DEFAULT '[]',
    encrypted_access_token TEXT NOT NULL,
    encrypted_refresh_token TEXT,
    token_type TEXT,
    expiry TEXT,
    PRIMARY KEY (user_id, provider)
);
CREATE TABLE IF NOT EXISTS tasks (
    id TEXT PRIMARY KEY,
    user_id INTEGER NOT NULL REFERENCES users(user_id) ON DELETE CASCADE,
    chat_id INTEGER NOT NULL DEFAULT 0,
    kind TEXT NOT NULL,
    text TEXT,
    url TEXT,
    interval_seconds INTEGER NOT NULL DEFAULT 0,
    next_run_at TEXT NOT NULL,
    last_fingerprint TEXT,
    last_snapshot TEXT,
    active INTEGER NOT NULL DEFAULT 1,
    created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS documents (
    id TEXT PRIMARY KEY,
    user_id INTEGER NOT NULL REFERENCES users(user_id) ON DELETE CASCADE,
    name TEXT NOT NULL,
    mime_type TEXT,
    url TEXT NOT NULL,
    created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_documents_user_id ON documents(user_id, created_at DESC
);`)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`ALTER TABLE users ADD COLUMN timezone TEXT NOT NULL DEFAULT ''`)
	if err != nil && !strings.Contains(err.Error(), "duplicate column name") {
		return err
	}
	_, err = s.db.Exec(`ALTER TABLE users ADD COLUMN home_location TEXT NOT NULL DEFAULT ''`)
	if err != nil && !strings.Contains(err.Error(), "duplicate column name") {
		return err
	}
	_, err = s.db.Exec(`ALTER TABLE tasks ADD COLUMN last_snapshot TEXT`)
	if err != nil && !strings.Contains(err.Error(), "duplicate column name") {
		return err
	}
	return nil
}

func (s *Store) History(userID int64) []mistral.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureUserLocked(userID)
	rows, err := s.db.Query(`SELECT role, content, name, tool_call_id, tool_calls FROM messages WHERE user_id = ? ORDER BY id`, userID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var history []mistral.Message
	for rows.Next() {
		var msg mistral.Message
		var name, toolCallID, toolCalls sql.NullString
		if rows.Scan(&msg.Role, &msg.Content, &name, &toolCallID, &toolCalls) != nil {
			continue
		}
		msg.Name = name.String
		msg.ToolCallID = toolCallID.String
		if toolCalls.Valid && toolCalls.String != "" {
			_ = json.Unmarshal([]byte(toolCalls.String), &msg.ToolCalls)
		}
		if msg.Role == "tool" || len(msg.ToolCalls) > 0 || msg.ToolCallID != "" {
			continue
		}
		history = append(history, msg)
	}
	return history
}

func (s *Store) AppendMessages(userID int64, messages ...mistral.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	if err := s.ensureUserLocked(userID); err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, msg := range messages {
		if msg.Role == "tool" || len(msg.ToolCalls) > 0 || msg.ToolCallID != "" {
			continue
		}
		toolCalls, _ := json.Marshal(msg.ToolCalls)
		if _, err := tx.Exec(`INSERT INTO messages(user_id, role, content, name, tool_call_id, tool_calls, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`, userID, msg.Role, msg.Content, msg.Name, msg.ToolCallID, string(toolCalls), now.Format(time.RFC3339Nano)); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`DELETE FROM messages WHERE user_id = ? AND id NOT IN (SELECT id FROM messages WHERE user_id = ? ORDER BY id DESC LIMIT 24)`, userID, userID); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE users SET last_seen_at = ? WHERE user_id = ?`, now.Format(time.RFC3339Nano), userID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ClearHistory(userID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureUserLocked(userID); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM messages WHERE user_id = ?`, userID)
	return err
}

func (s *Store) SaveDocument(document Document) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureUserLocked(document.UserID); err != nil {
		return err
	}
	if document.CreatedAt.IsZero() {
		document.CreatedAt = time.Now().UTC()
	}
	_, err := s.db.Exec(`INSERT INTO documents(id, user_id, name, mime_type, url, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		document.ID, document.UserID, document.Name, document.MimeType, document.URL, document.CreatedAt.UTC().Format(time.RFC3339Nano))
	return err
}

func (s *Store) ActiveDocument(userID int64) (Document, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureUserLocked(userID)
	var document Document
	var createdAt string
	err := s.db.QueryRow(`SELECT id, user_id, name, mime_type, url, created_at FROM documents WHERE user_id = ? ORDER BY created_at DESC LIMIT 1`, userID).
		Scan(&document.ID, &document.UserID, &document.Name, &document.MimeType, &document.URL, &createdAt)
	if err != nil {
		return Document{}, false
	}
	document.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAt)
	return document, true
}

func (s *Store) ClearDocuments(userID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`DELETE FROM documents WHERE user_id = ?`, userID)
	return err
}

func (s *Store) AddUsage(userID int64, usage mistral.Usage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureUserLocked(userID); err != nil {
		return err
	}
	_, err := s.db.Exec(`UPDATE users SET prompt_tokens = prompt_tokens + ?, completion_tokens = completion_tokens + ?, total_tokens = total_tokens + ?, last_seen_at = ? WHERE user_id = ?`, usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens, time.Now().UTC().Format(time.RFC3339Nano), userID)
	return err
}

func (s *Store) SaveTask(task Task) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureUserLocked(task.UserID); err != nil {
		return err
	}
	_, err := s.db.Exec(`INSERT INTO tasks(id, user_id, chat_id, kind, text, url, interval_seconds, next_run_at, last_fingerprint, last_snapshot, active, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(id) DO UPDATE SET user_id=excluded.user_id, chat_id=excluded.chat_id, kind=excluded.kind, text=excluded.text, url=excluded.url, interval_seconds=excluded.interval_seconds, next_run_at=excluded.next_run_at, last_fingerprint=excluded.last_fingerprint, last_snapshot=excluded.last_snapshot, active=excluded.active, created_at=excluded.created_at`, task.ID, task.UserID, task.ChatID, task.Kind, task.Text, task.URL, task.IntervalSeconds, task.NextRunAt.UTC().Format(time.RFC3339Nano), task.LastFingerprint, task.LastSnapshot, boolInt(task.Active), task.CreatedAt.UTC().Format(time.RFC3339Nano))
	return err
}

func (s *Store) DueTasks(now time.Time) []Task {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`SELECT id, user_id, chat_id, kind, text, url, interval_seconds, next_run_at, last_fingerprint, last_snapshot, active, created_at FROM tasks WHERE active = 1 AND next_run_at <= ?`, now.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return nil
	}
	defer rows.Close()
	return scanTasks(rows)
}

func (s *Store) Tasks(userID int64) []Task {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`SELECT id, user_id, chat_id, kind, text, url, interval_seconds, next_run_at, last_fingerprint, last_snapshot, active, created_at FROM tasks WHERE user_id = ? AND active = 1 ORDER BY next_run_at`, userID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	return scanTasks(rows)
}

func (s *Store) TasksForDate(userID int64, date time.Time) []Task {
	s.mu.Lock()
	defer s.mu.Unlock()
	start := time.Date(date.Year(), date.Month(), date.Day(), 0, 0, 0, 0, date.Location()).UTC()
	end := start.AddDate(0, 0, 1)
	rows, err := s.db.Query(`SELECT id, user_id, chat_id, kind, text, url, interval_seconds, next_run_at, last_fingerprint, last_snapshot, active, created_at FROM tasks WHERE user_id = ? AND active = 1 AND kind != 'daily_digest' AND next_run_at >= ? AND next_run_at < ? ORDER BY next_run_at`, userID, start.Format(time.RFC3339Nano), end.Format(time.RFC3339Nano))
	if err != nil {
		return nil
	}
	defer rows.Close()
	return scanTasks(rows)
}

func (s *Store) DeleteTask(userID int64, taskID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	result, err := s.db.Exec(`DELETE FROM tasks WHERE id = ? AND user_id = ?`, taskID, userID)
	if err != nil {
		return false
	}
	n, _ := result.RowsAffected()
	return n > 0
}

func (s *Store) DeleteTasksByKind(userID int64, kind string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	result, err := s.db.Exec(`DELETE FROM tasks WHERE user_id = ? AND kind = ? AND active = 1`, userID, kind)
	if err != nil {
		return 0
	}
	n, _ := result.RowsAffected()
	return int(n)
}

func (s *Store) Usage(userID int64) UserRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ensureUserLocked(userID) != nil {
		return UserRecord{}
	}
	var rec UserRecord
	_ = s.db.QueryRow(`SELECT user_id, timezone, home_location, first_seen_at, last_seen_at, prompt_tokens, completion_tokens, total_tokens FROM users WHERE user_id = ?`, userID).Scan(&rec.UserID, &rec.Timezone, &rec.HomeLocation, (*databaseTime)(&rec.FirstSeenAt), (*databaseTime)(&rec.LastSeenAt), &rec.PromptTokens, &rec.CompletionTokens, &rec.TotalTokens)
	return rec
}

func (s *Store) Timezone(userID int64) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureUserLocked(userID); err != nil {
		return ""
	}
	var timezone string
	_ = s.db.QueryRow(`SELECT timezone FROM users WHERE user_id = ?`, userID).Scan(&timezone)
	return timezone
}

func (s *Store) SetTimezone(userID int64, timezone string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureUserLocked(userID); err != nil {
		return err
	}
	_, err := s.db.Exec(`UPDATE users SET timezone = ?, last_seen_at = ? WHERE user_id = ?`, timezone, time.Now().UTC().Format(time.RFC3339Nano), userID)
	return err
}

func (s *Store) HomeLocation(userID int64) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureUserLocked(userID); err != nil {
		return ""
	}
	var location string
	_ = s.db.QueryRow(`SELECT home_location FROM users WHERE user_id = ?`, userID).Scan(&location)
	return location
}

func (s *Store) SetHomeLocation(userID int64, location string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureUserLocked(userID); err != nil {
		return err
	}
	_, err := s.db.Exec(`UPDATE users SET home_location = ?, last_seen_at = ? WHERE user_id = ?`, location, time.Now().UTC().Format(time.RFC3339Nano), userID)
	return err
}

func (s *Store) SaveAccount(userID int64, account Account) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureUserLocked(userID); err != nil {
		return err
	}
	scopes, err := json.Marshal(account.Scopes)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO accounts(user_id, provider, connected_at, scopes, encrypted_access_token, encrypted_refresh_token, token_type, expiry) VALUES (?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(user_id, provider) DO UPDATE SET connected_at=excluded.connected_at, scopes=excluded.scopes, encrypted_access_token=excluded.encrypted_access_token, encrypted_refresh_token=excluded.encrypted_refresh_token, token_type=excluded.token_type, expiry=excluded.expiry`, userID, account.Provider, account.ConnectedAt.UTC().Format(time.RFC3339Nano), string(scopes), account.EncryptedAccessToken, account.EncryptedRefreshToken, account.TokenType, account.Expiry.UTC().Format(time.RFC3339Nano))
	return err
}

func (s *Store) Account(userID int64, provider string) (Account, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var account Account
	var scopes string
	var connectedAt, expiry databaseTime
	err := s.db.QueryRow(`SELECT provider, connected_at, scopes, encrypted_access_token, encrypted_refresh_token, token_type, expiry FROM accounts WHERE user_id = ? AND provider = ?`, userID, provider).Scan(&account.Provider, &connectedAt, &scopes, &account.EncryptedAccessToken, &account.EncryptedRefreshToken, &account.TokenType, &expiry)
	if err != nil {
		return Account{}, false
	}
	account.ConnectedAt = time.Time(connectedAt)
	account.Expiry = time.Time(expiry)
	_ = json.Unmarshal([]byte(scopes), &account.Scopes)
	return account, true
}

func (s *Store) DeleteAccount(userID int64, provider string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`DELETE FROM accounts WHERE user_id = ? AND provider = ?`, userID, provider)
	return err
}

func (s *Store) ensureUserLocked(userID int64) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := s.db.Exec(`INSERT OR IGNORE INTO users(user_id, first_seen_at, last_seen_at) VALUES (?, ?, ?)`, userID, now, now)
	return err
}

func scanTasks(rows *sql.Rows) []Task {
	var tasks []Task
	for rows.Next() {
		var task Task
		var nextRunAt, createdAt databaseTime
		var lastSnapshot sql.NullString
		var active int
		if rows.Scan(&task.ID, &task.UserID, &task.ChatID, &task.Kind, &task.Text, &task.URL, &task.IntervalSeconds, &nextRunAt, &task.LastFingerprint, &lastSnapshot, &active, &createdAt) != nil {
			continue
		}
		task.LastSnapshot = lastSnapshot.String
		task.NextRunAt = time.Time(nextRunAt)
		task.CreatedAt = time.Time(createdAt)
		task.Active = active != 0
		tasks = append(tasks, task)
	}
	return tasks
}

type databaseTime time.Time

func (t *databaseTime) Scan(value any) error {
	s, ok := value.(string)
	if !ok {
		return fmt.Errorf("database time is %T", value)
	}
	parsed, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return err
	}
	*t = databaseTime(parsed)
	return nil
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func (s *Store) migrateLegacyJSON(dbPath string) error {
	legacyPath := filepath.Join(filepath.Dir(dbPath), "store.json")
	if filepath.Clean(legacyPath) == filepath.Clean(dbPath) {
		return nil
	}
	data, err := os.ReadFile(legacyPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	var legacy legacyData
	if err := json.Unmarshal(data, &legacy); err != nil {
		return fmt.Errorf("read legacy JSON store: %w", err)
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&count); err != nil || count > 0 {
		return err
	}
	for _, user := range legacy.Users {
		if err := s.ensureUserLocked(user.UserID); err != nil {
			return err
		}
		_, err := s.db.Exec(`UPDATE users SET timezone = ?, home_location = ?, first_seen_at = ?, last_seen_at = ?, prompt_tokens = ?, completion_tokens = ?, total_tokens = ? WHERE user_id = ?`, user.Timezone, user.HomeLocation, user.FirstSeenAt.UTC().Format(time.RFC3339Nano), user.LastSeenAt.UTC().Format(time.RFC3339Nano), user.PromptTokens, user.CompletionTokens, user.TotalTokens, user.UserID)
		if err != nil {
			return err
		}
		if err := s.AppendMessages(user.UserID, user.Messages...); err != nil {
			return err
		}
		for _, account := range user.Accounts {
			if err := s.SaveAccount(user.UserID, account); err != nil {
				return err
			}
		}
	}
	for _, task := range legacy.Tasks {
		if err := s.SaveTask(*task); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

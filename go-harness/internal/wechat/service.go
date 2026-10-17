package wechat

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/internal/automation"
)

type Config struct {
	Enabled      bool     `yaml:"enabled" json:"enabled"`
	Workspace    string   `yaml:"workspace" json:"workspace"`
	AllowedUsers []string `yaml:"allowed_users" json:"allowed_users"`
	Approval     string   `yaml:"approval" json:"approval"`
}
type Runner func(context.Context, string, string, func(string) error) (automation.Outcome, error)
type Service struct {
	db           *sql.DB
	client       *Client
	cfg          Config
	run          Runner
	mu           sync.Mutex
	qr           QR
	loginID      string
	loginOrigin  string
	loginVerify  string
	expires      time.Time
	lastError    string
	activeCancel context.CancelFunc
	cancel       context.CancelFunc
	done         chan struct{}
	once         sync.Once
}

func New(ctx context.Context, db *sql.DB, cfg Config, runner Runner) (*Service, error) {
	if cfg.Approval == "" {
		cfg.Approval = "read_only"
	}
	if cfg.Enabled && (!filepath.IsAbs(cfg.Workspace) || len(cfg.AllowedUsers) == 0) {
		return nil, errors.New("WeChat requires an absolute workspace and explicit allowed_users (default deny)")
	}
	if cfg.Approval != "read_only" && cfg.Approval != "ask" && cfg.Approval != "allow_always" {
		return nil, errors.New("invalid WeChat approval policy")
	}
	if len(cfg.AllowedUsers) > 100 {
		return nil, errors.New("too many WeChat allowed users")
	}
	for _, id := range cfg.AllowedUsers {
		if id == "" || len(id) > 512 || strings.TrimSpace(id) != id {
			return nil, errors.New("invalid WeChat allowed user")
		}
	}
	_, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS harness_wechat_auth(id INTEGER PRIMARY KEY CHECK(id=1),credentials BLOB NOT NULL,cursor TEXT NOT NULL DEFAULT '');
CREATE TABLE IF NOT EXISTS harness_wechat_threads(bot TEXT NOT NULL,sender TEXT NOT NULL,session_id TEXT NOT NULL,PRIMARY KEY(bot,sender));
CREATE TABLE IF NOT EXISTS harness_wechat_messages(bot TEXT NOT NULL,id TEXT NOT NULL,sender TEXT NOT NULL,context_token TEXT NOT NULL,text TEXT NOT NULL,status TEXT NOT NULL,reply TEXT NOT NULL DEFAULT '',session_id TEXT NOT NULL DEFAULT '',client_id TEXT NOT NULL,created_ms INTEGER NOT NULL,PRIMARY KEY(bot,id));
CREATE INDEX IF NOT EXISTS harness_wechat_pending ON harness_wechat_messages(status,created_ms);
UPDATE harness_wechat_messages SET status='unknown',reply='Daemon stopped before a terminal receipt. Do not replay this message; inspect the session.' WHERE status IN ('running','sending');`)
	if err != nil {
		return nil, err
	}
	return &Service{db: db, cfg: cfg, run: runner, client: NewClient()}, nil
}
func (s *Service) allowed(sender string) bool {
	for _, id := range s.cfg.AllowedUsers {
		if id == sender {
			return true
		}
	}
	return false
}
func (s *Service) credentials(ctx context.Context) (Credentials, string, error) {
	var raw []byte
	var cursor string
	err := s.db.QueryRowContext(ctx, `SELECT credentials,cursor FROM harness_wechat_auth WHERE id=1`).Scan(&raw, &cursor)
	if errors.Is(err, sql.ErrNoRows) {
		return Credentials{}, "", nil
	}
	if err != nil {
		return Credentials{}, "", err
	}
	var v Credentials
	raw, err = openCredentials(raw)
	if err != nil {
		return Credentials{}, "", err
	}
	err = json.Unmarshal(raw, &v)
	return v, cursor, err
}
func (s *Service) Status(ctx context.Context) (any, error) {
	cred, _, err := s.credentials(ctx)
	if err != nil {
		return nil, err
	}
	var unknown, queued int
	if err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM harness_wechat_messages WHERE status='unknown'`).Scan(&unknown); err != nil {
		return nil, err
	}
	if err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM harness_wechat_messages WHERE status IN ('queued','ready')`).Scan(&queued); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return map[string]any{"enabled": s.cfg.Enabled, "connected": cred.Token != "", "bot_id": cred.BotID, "allowed_users": s.cfg.AllowedUsers, "approval": s.cfg.Approval, "unknown": unknown, "queued": queued, "error": s.lastError}, nil
}
func (s *Service) LoginStart(ctx context.Context) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 7*time.Second)
	defer cancel()
	// A new attempt fences the previous QR even when the provider is unavailable.
	s.loginID = ""
	s.loginVerify = ""
	qr, err := s.client.QR(ctx)
	if err != nil {
		return nil, err
	}
	s.qr = qr
	s.loginID = rand.Text()
	s.loginOrigin = DefaultOrigin
	s.expires = time.Now().Add(180 * time.Second)
	return map[string]any{"id": s.loginID, "qrcode_content": qr.Content, "expires_in": 180}, nil
}
func (s *Service) LoginPoll(ctx context.Context, id, verify string) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id == "" || id != s.loginID || time.Now().After(s.expires) {
		if time.Now().After(s.expires) {
			s.loginID, s.loginVerify = "", ""
		}
		return nil, errors.New("WeChat login expired or not found; start again")
	}
	if err := validateVerificationCode(verify); err != nil {
		return nil, err
	}
	if verify != "" {
		s.loginVerify = verify
	}
	ctx, cancel := context.WithTimeout(ctx, 7*time.Second)
	defer cancel()
	status, err := s.client.loginAt(ctx, s.loginOrigin, s.qr.Code, s.loginVerify)
	if err != nil {
		return nil, err
	}
	// Waits, network timeouts and IDC redirects do not acknowledge the code.
	// Retain it only in memory until accepted, rejected, expired or cancelled.
	switch status.Status {
	case "scaned_but_redirect":
		s.loginOrigin = "https://" + status.RedirectHost
		status.Status = "scanned"
	case "wait", "pending":
		status.Status = "pending"
	case "need_verifycode":
		status.Status = "verification_required"
		s.loginVerify = ""
	case "scaned", "scanned":
		status.Status = "scanned"
		s.loginVerify = ""
	case "confirmed":
		s.loginVerify = ""
	case "expired":
		s.loginID, s.loginVerify = "", ""
	default:
		status.Status = "failed"
		s.loginID, s.loginVerify = "", ""
	}
	if status.Status == "confirmed" {
		raw, err := json.Marshal(status.Credentials)
		if err != nil {
			return nil, err
		}
		raw, err = sealCredentials(raw)
		if err != nil {
			return nil, err
		}
		if s.activeCancel != nil {
			s.activeCancel()
		}
		tx, e := s.db.BeginTx(ctx, nil)
		if e != nil {
			return nil, e
		}
		defer tx.Rollback()
		_, err = tx.ExecContext(ctx, `UPDATE harness_wechat_messages SET status='unknown' WHERE status IN ('queued','ready','running','sending')`)
		if err != nil {
			return nil, err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO harness_wechat_auth(id,credentials,cursor) VALUES(1,?,'') ON CONFLICT(id) DO UPDATE SET credentials=excluded.credentials,cursor=''`, raw)
		if err != nil {
			return nil, err
		}
		if err = tx.Commit(); err != nil {
			return nil, err
		}
		s.loginID = ""
		s.lastError = ""
	}
	return map[string]any{"status": status.Status, "user_id": status.UserID, "bot_id": status.BotID}, nil
}
func (s *Service) Logout(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loginID = ""
	s.loginVerify = ""
	if s.activeCancel != nil {
		s.activeCancel()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `UPDATE harness_wechat_messages SET status='unknown' WHERE status IN ('queued','ready','running','sending')`); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM harness_wechat_auth`); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Service) Start(ctx context.Context) {
	ctx, s.cancel = context.WithCancel(ctx)
	s.done = make(chan struct{})
	go func() {
		defer close(s.done)
		for {
			if ctx.Err() != nil {
				return
			}
			err := s.step(ctx)
			delay := time.Second
			if err != nil {
				delay = 10 * time.Second
				s.mu.Lock()
				s.lastError = err.Error()
				s.mu.Unlock()
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(delay):
			}
		}
	}()
}
func (s *Service) Close() {
	s.once.Do(func() {
		if s.cancel != nil {
			s.cancel()
			<-s.done
		}
	})
}
func (s *Service) Stop() {
	if s.cancel != nil {
		s.cancel()
	}
}

func (s *Service) step(ctx context.Context) error {
	cred, cursor, err := s.credentials(ctx)
	if err != nil {
		return err
	}
	if !s.cfg.Enabled || cred.Token == "" {
		return nil
	}
	handled, err := s.process(ctx, cred)
	if err != nil || handled {
		return err
	}
	updates, err := s.client.Poll(ctx, cred, cursor)
	if errors.Is(err, ErrExpired) {
		s.mu.Lock()
		defer s.mu.Unlock()
		current, _, readErr := s.credentials(ctx)
		if readErr != nil {
			return readErr
		}
		if current == cred {
			_, _ = s.db.ExecContext(ctx, `DELETE FROM harness_wechat_auth`)
		}
		return err
	}
	if err != nil {
		return err
	}
	return s.accept(ctx, cred, updates)
}

// Cursor advancement and deduplicated inputs are one transaction. No network
// side effect occurs inside it, and untrusted senders never create sessions.
func (s *Service) accept(ctx context.Context, cred Credentials, u Updates) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, _, err := s.credentials(ctx)
	if err != nil || current != cred {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, m := range u.Messages {
		if m.From == "" {
			m.From = m.UserID
		}
		id := m.key()
		if m.Type != 1 || m.Group != "" || !s.allowed(m.From) || id == "" || m.Context == "" || len(m.Context) > 8192 {
			continue
		}
		var text strings.Builder
		unsupported := false
		for _, item := range m.Items {
			if item.Type != 1 {
				unsupported = true
				continue
			}
			text.WriteString(item.Text.Text)
			text.WriteByte('\n')
		}
		status, reply := "queued", ""
		prompt := strings.TrimSpace(text.String())
		if unsupported || prompt == "" || len(prompt) > 16384 {
			status = "ready"
			reply = "This WeChat channel currently accepts text only (up to 16 KiB). Use the desktop for images and PDF/Office attachments."
			prompt = ""
		}
		if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO harness_wechat_messages(bot,id,sender,context_token,text,status,reply,client_id,created_ms) VALUES(?,?,?,?,?,?,?,?,?)`, cred.BotID, id, m.From, m.Context, prompt, status, reply, "deerflow-"+rand.Text(), time.Now().UnixMilli()); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE harness_wechat_auth SET cursor=? WHERE id=1`, u.Cursor); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Service) process(ctx context.Context, cred Credentials) (bool, error) {
	s.mu.Lock()
	current, _, err := s.credentials(ctx)
	if err != nil || current != cred {
		s.mu.Unlock()
		return false, err
	}
	ctx, cancel := context.WithCancel(ctx)
	s.activeCancel = cancel
	s.mu.Unlock()
	defer func() { cancel(); s.mu.Lock(); s.activeCancel = nil; s.mu.Unlock() }()
	var id, sender, token, text, status, reply, session, clientID string
	// Provider IDs are opaque; insertion order preserves messages within a batch.
	err = s.db.QueryRowContext(ctx, `SELECT id,sender,context_token,text,status,reply,session_id,client_id FROM harness_wechat_messages m WHERE bot=? AND status IN ('queued','ready') AND NOT EXISTS(SELECT 1 FROM harness_wechat_messages u WHERE u.bot=m.bot AND u.sender=m.sender AND u.status IN ('unknown','running','sending')) ORDER BY m.rowid LIMIT 1`, cred.BotID).Scan(&id, &sender, &token, &text, &status, &reply, &session, &clientID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	current, _, err = s.credentials(ctx)
	if err != nil || current != cred {
		return true, err
	}
	if !s.allowed(sender) {
		_, err = s.db.ExecContext(ctx, `UPDATE harness_wechat_messages SET status='ignored' WHERE bot=? AND id=?`, cred.BotID, id)
		return true, err
	}
	if status == "queued" {
		claim, claimErr := s.db.ExecContext(ctx, `UPDATE harness_wechat_messages SET status='running' WHERE bot=? AND id=? AND status='queued'`, cred.BotID, id)
		err = claimErr
		if err != nil {
			return true, err
		}
		if n, e := claim.RowsAffected(); e != nil || n != 1 {
			return true, e
		}
		err = s.db.QueryRowContext(ctx, `SELECT session_id FROM harness_wechat_threads WHERE bot=? AND sender=?`, cred.BotID, sender).Scan(&session)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return true, err
		}
		out, runErr := s.run(ctx, session, text, func(bound string) error {
			tx, e := s.db.BeginTx(ctx, nil)
			if e != nil {
				return e
			}
			defer tx.Rollback()
			if _, e = tx.ExecContext(ctx, `INSERT INTO harness_wechat_threads VALUES(?,?,?) ON CONFLICT(bot,sender) DO UPDATE SET session_id=excluded.session_id`, cred.BotID, sender, bound); e != nil {
				return e
			}
			if _, e = tx.ExecContext(ctx, `UPDATE harness_wechat_messages SET session_id=? WHERE bot=? AND id=?`, bound, cred.BotID, id); e != nil {
				return e
			}
			session = bound
			return tx.Commit()
		})
		reply = out.Text
		if runErr != nil || out.Status != "completed" {
			reply = "Execution needs attention. Open desktop session " + session + " to review permissions, failure or recovery. It will not be automatically retried.\n" + reply
		}
		if reply == "" {
			reply = "Completed. Open desktop session " + session + " for details."
		}
		if len(reply) > 10000 {
			runes := []rune(reply)
			reply = string(runes[:min(2500, len(runes))]) + "\n[truncated; see desktop session " + session + "]"
		}
		persist, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if _, err = s.db.ExecContext(persist, `UPDATE harness_wechat_messages SET status='ready',reply=? WHERE bot=? AND id=? AND status='running'`, reply, cred.BotID, id); err != nil {
			return true, err
		}
	}
	if ctx.Err() != nil {
		return true, ctx.Err()
	}
	// Claim send before network I/O. A timeout is an unknown effect, not proof
	// of failure: retain it for explicit human review instead of duplicate sends.
	current, _, err = s.credentials(ctx)
	if err != nil || current != cred {
		return true, err
	}
	claim, err := s.db.ExecContext(ctx, `UPDATE harness_wechat_messages SET status='sending' WHERE bot=? AND id=? AND status='ready'`, cred.BotID, id)
	if err != nil {
		return true, err
	}
	if n, e := claim.RowsAffected(); e != nil || n != 1 {
		return true, e
	}
	err = s.client.Send(ctx, cred, sender, token, clientID, reply)
	terminal := "sent"
	if err != nil {
		terminal = "unknown"
	}
	persist, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	_, saveErr := s.db.ExecContext(persist, `UPDATE harness_wechat_messages SET status=? WHERE bot=? AND id=? AND status='sending'`, terminal, cred.BotID, id)
	return true, errors.Join(err, saveErr)
}

// Review only acknowledges an unknown outcome. It never resends or reruns it.
func (s *Service) Review(ctx context.Context, id string) error {
	cred, _, err := s.credentials(ctx)
	if err != nil {
		return err
	}
	r, err := s.db.ExecContext(ctx, `UPDATE harness_wechat_messages SET status='reviewed' WHERE bot=? AND id=? AND status='unknown'`, cred.BotID, id)
	if err != nil {
		return err
	}
	n, err := r.RowsAffected()
	if err == nil && n != 1 {
		return errors.New("unknown message not found for current account")
	}
	return err
}

func (s *Service) CanNotify(sender string) bool { return s.cfg.Enabled && s.allowed(sender) }

// EnqueueNotificationTx shares the schedule's terminal transaction. No network
// call occurs until the durable outbox is committed. Missing conversation
// context leaves a visible unknown item rather than guessing a recipient.
func (s *Service) EnqueueNotificationTx(ctx context.Context, tx *sql.Tx, spec automation.Spec, occ automation.Occurrence) error {
	if !s.CanNotify(spec.NotifyWeChat) {
		return nil
	}
	var raw []byte
	err := tx.QueryRowContext(ctx, `SELECT credentials FROM harness_wechat_auth WHERE id=1`).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	raw, err = openCredentials(raw)
	if err != nil {
		return err
	}
	var cred Credentials
	if err = json.Unmarshal(raw, &cred); err != nil {
		return err
	}
	var token string
	err = tx.QueryRowContext(ctx, `SELECT context_token FROM harness_wechat_messages WHERE bot=? AND sender=? AND context_token<>'' ORDER BY rowid DESC LIMIT 1`, cred.BotID, spec.NotifyWeChat).Scan(&token)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	status := "ready"
	if token == "" {
		status = "unknown"
	}
	reply := "Scheduled task: " + spec.Name + "\nStatus: " + occ.Outcome.Status + "\nSession: " + occ.Outcome.SessionID + "\n" + occ.Outcome.Text
	if len(reply) > 10000 {
		r := []rune(reply)
		reply = string(r[:min(2400, len(r))]) + "\n[see desktop session for full output]"
	}
	_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO harness_wechat_messages(bot,id,sender,context_token,text,status,reply,session_id,client_id,created_ms) VALUES(?,?,?,?,'',?,?,?,?,?)`, cred.BotID, "schedule-"+occ.ID, spec.NotifyWeChat, token, status, reply, occ.Outcome.SessionID, "deerflow-schedule-"+occ.ID, time.Now().UnixMilli())
	return err
}

func (s *Service) History(ctx context.Context) (any, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,bot,sender,status,session_id,created_ms FROM harness_wechat_messages ORDER BY rowid DESC LIMIT 100`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, bot, sender, status, session string
		var ms int64
		if err = rows.Scan(&id, &bot, &sender, &status, &session, &ms); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"id": id, "bot_id": bot, "sender": sender, "status": status, "session_id": session, "created_ms": ms})
	}
	return out, rows.Err()
}

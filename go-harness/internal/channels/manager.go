// Package channels owns trusted plugin lifecycles and durable channel admission.
package channels

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/channelproto"
	"github.com/omengye/deerflow-acp/go-harness/internal/automation"
	"github.com/omengye/deerflow-acp/go-harness/internal/session"
)

type Config struct {
	Enabled    bool              `json:"enabled"`
	Workspace  string            `json:"workspace"`
	Options    map[string]string `json:"options,omitempty"`
	Approval   string            `json:"approval,omitempty"`
	FullAccess *FullAccessGrant  `json:"full_access,omitempty"`
	// Legacy policy is preserved on migration, but never editable by plugins.
	LegacyAllowed  []string `json:"legacy_allowed,omitempty"`
	LegacyApproval string   `json:"legacy_approval,omitempty"`
}
type Runner func(context.Context, string, string, string, string, func(string) error) (automation.Outcome, error)
type instance struct {
	op          sync.Mutex
	mu          sync.Mutex
	id          string
	manifest    Manifest
	cfg         Config
	revision    int64
	auth        channelproto.Auth
	cursor      string
	proc        transport
	cancel      context.CancelFunc
	done        chan struct{}
	login       string
	pairCode    string
	pairExpires time.Time
	connected   bool
	lastSuccess time.Time
	lastError   string
	active      *activeDispatch
}
type Manager struct {
	db     *sql.DB
	run    Runner
	mu     sync.Mutex
	items  map[string]*instance
	closed bool
	start  func(context.Context, Manifest) (transport, channelproto.Descriptor, error)
}

func New(ctx context.Context, db *sql.DB, run Runner) (*Manager, error) {
	_, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS harness_channels(id TEXT PRIMARY KEY,manifest BLOB NOT NULL,config BLOB NOT NULL,revision INTEGER NOT NULL,auth BLOB,cursor TEXT NOT NULL DEFAULT '');
CREATE TABLE IF NOT EXISTS harness_channel_messages(instance TEXT NOT NULL,account TEXT NOT NULL,id TEXT NOT NULL,conversation TEXT NOT NULL,sender TEXT NOT NULL,context TEXT NOT NULL,text TEXT NOT NULL,status TEXT NOT NULL,reply TEXT NOT NULL DEFAULT '',session_id TEXT NOT NULL DEFAULT '',client_id TEXT NOT NULL,workspace TEXT NOT NULL,created_ms INTEGER NOT NULL,PRIMARY KEY(instance,account,id));
CREATE INDEX IF NOT EXISTS harness_channel_pending ON harness_channel_messages(instance,status,created_ms);
CREATE INDEX IF NOT EXISTS harness_channel_message_id ON harness_channel_messages(id);
CREATE TABLE IF NOT EXISTS harness_channel_threads(instance TEXT NOT NULL,account TEXT NOT NULL,conversation TEXT NOT NULL,sender TEXT NOT NULL,workspace TEXT NOT NULL,session_id TEXT NOT NULL,PRIMARY KEY(instance,account,conversation,sender,workspace));
CREATE TABLE IF NOT EXISTS harness_channel_admissions(instance TEXT NOT NULL,account TEXT NOT NULL,id TEXT NOT NULL,approval TEXT NOT NULL,PRIMARY KEY(instance,account,id));
CREATE TABLE IF NOT EXISTS harness_channel_confirmations(code TEXT PRIMARY KEY,scope TEXT NOT NULL,session_id TEXT NOT NULL,run_id TEXT NOT NULL,execution_version INTEGER NOT NULL,intent_id TEXT NOT NULL,intent_version INTEGER NOT NULL,digest TEXT NOT NULL,config_version INTEGER NOT NULL,policy_revision INTEGER NOT NULL,expires_ms INTEGER NOT NULL,decision TEXT NOT NULL DEFAULT '',used INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS harness_channel_policy_audit(id INTEGER PRIMARY KEY,instance TEXT NOT NULL,account TEXT NOT NULL,owner TEXT NOT NULL,workspace TEXT NOT NULL,approval TEXT NOT NULL,source TEXT NOT NULL,revision INTEGER NOT NULL,created_ms INTEGER NOT NULL);
UPDATE harness_channel_messages SET status='unknown' WHERE status IN ('running','sending');`)
	if err != nil {
		return nil, err
	}
	m := &Manager{db: db, run: run, items: map[string]*instance{}, start: startProcess}
	rows, err := db.QueryContext(ctx, `SELECT id,manifest,config,revision,auth,cursor FROM harness_channels`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		i := &instance{}
		var manifest, cfg, auth []byte
		if err = rows.Scan(&i.id, &manifest, &cfg, &i.revision, &auth, &i.cursor); err != nil {
			return nil, err
		}
		if json.Unmarshal(manifest, &i.manifest) != nil || json.Unmarshal(cfg, &i.cfg) != nil {
			return nil, userError("invalid stored channel configuration")
		}
		if len(auth) > 0 {
			plain, e := openCredentials(auth)
			if e != nil || json.Unmarshal(plain, &i.auth) != nil {
				i.lastError = "Credentials unavailable; sign in again"
				i.auth = channelproto.Auth{}
			}
		}
		m.items[i.id] = i
	}
	return m, rows.Err()
}
func (m *Manager) Start() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	for _, i := range m.items {
		m.startWorker(i)
	}
}
func (m *Manager) Stop() {
	m.mu.Lock()
	m.closed = true
	items := make([]*instance, 0, len(m.items))
	for _, i := range m.items {
		items = append(items, i)
	}
	m.mu.Unlock()
	for _, i := range items {
		i.op.Lock()
		i.mu.Lock()
		if i.cancel != nil {
			i.cancel()
		}
		i.mu.Unlock()
		i.op.Unlock()
	}
}
func (m *Manager) Close() {
	m.Stop()
	for _, i := range m.instances() {
		i.op.Lock()
		m.stopWorker(i)
		i.op.Unlock()
	}
}
func (m *Manager) instances() []*instance {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*instance, 0, len(m.items))
	for _, i := range m.items {
		out = append(out, i)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].id < out[b].id })
	return out
}
func (m *Manager) get(id string) (*instance, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, userError("channels stopped")
	}
	i := m.items[id]
	if i == nil {
		return nil, userError("channel not installed")
	}
	return i, nil
}
func (m *Manager) available() bool { m.mu.Lock(); defer m.mu.Unlock(); return !m.closed }

// Bundled binaries are discovered only beside the host executable, never in cwd/PATH.
func (m *Manager) EnsureBundled(ctx context.Context) error {
	m.mu.Lock()
	_, exists := m.items["wechat"]
	m.mu.Unlock()
	if exists {
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	name := "deerflow-channel-wechat"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	path := filepath.Join(filepath.Dir(exe), name)
	bytes, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	sum := sha256.Sum256(bytes)
	// Registration of the bundled transport must not make an unrelated desktop
	// startup depend on a plugin process. Validate/handshake on first use instead.
	m.mu.Lock()
	defer m.mu.Unlock()
	manifest := Manifest{ID: "wechat", Command: path, SHA256: hex.EncodeToString(sum[:]), Descriptor: channelproto.Descriptor{ID: "wechat", Name: "WeChat", Version: "1.0.0", Protocol: 1, Login: []string{"qr"}, Messages: []string{"text"}, Fields: []channelproto.Field{}, RequiresReplyContext: true}}
	i := &instance{id: "wechat", manifest: manifest, revision: 1, cfg: Config{Options: map[string]string{}}}
	if err = m.persist(ctx, i); err != nil {
		return err
	}
	m.items[i.id] = i
	return nil
}
func (m *Manager) Install(ctx context.Context, id string, manifest Manifest) (any, error) {
	manifest.Previous = nil
	if !validID(id) {
		return nil, userError("invalid channel instance ID")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, userError("channels stopped")
	}
	if len(m.items) >= 32 {
		return nil, userError("channel instance limit reached")
	}
	if m.items[id] != nil {
		return nil, userError("channel already installed")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	p, d, err := m.start(ctx, manifest)
	if err != nil {
		return nil, err
	}
	p.Close()
	manifest.Descriptor = d
	i := &instance{id: id, manifest: manifest, revision: 1, cfg: Config{Options: map[string]string{}}}
	if err = m.persist(ctx, i); err != nil {
		return nil, err
	}
	m.items[id] = i
	return map[string]any{"id": id, "descriptor": d}, nil
}
func (m *Manager) persist(ctx context.Context, i *instance) error {
	return m.persistWith(ctx, m.db, i)
}

type executor interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func (m *Manager) persistWith(ctx context.Context, db executor, i *instance) error {
	manifest, err := json.Marshal(i.manifest)
	if err != nil {
		return err
	}
	cfg, err := json.Marshal(i.cfg)
	if err != nil {
		return err
	}
	var auth []byte
	if len(i.auth.Secret) > 0 {
		plain, e := json.Marshal(i.auth)
		if e != nil {
			return e
		}
		auth, err = sealCredentials(plain)
		if err != nil {
			return err
		}
	}
	_, err = db.ExecContext(ctx, `INSERT INTO harness_channels(id,manifest,config,revision,auth,cursor) VALUES(?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET manifest=excluded.manifest,config=excluded.config,revision=excluded.revision,auth=excluded.auth,cursor=excluded.cursor`, i.id, manifest, cfg, i.revision, auth, i.cursor)
	return err
}
func (m *Manager) List(ctx context.Context) (any, error) {
	out := []any{}
	for _, i := range m.instances() {
		v, err := m.status(ctx, i)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return map[string]any{"channels": out, "protocol": channelproto.Version}, nil
}
func (m *Manager) Status(ctx context.Context, id string) (any, error) {
	i, err := m.get(id)
	if err != nil {
		return nil, err
	}
	return m.status(ctx, i)
}
func (m *Manager) status(ctx context.Context, i *instance) (any, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	var queued, unknown int
	if err := m.db.QueryRowContext(ctx, `SELECT COUNT(CASE WHEN status IN ('queued','ready') THEN 1 END),COUNT(CASE WHEN status='unknown' THEN 1 END) FROM harness_channel_messages WHERE instance=? AND account=?`, i.id, i.auth.Account).Scan(&queued, &unknown); err != nil {
		return nil, err
	}
	phase := "disabled"
	if i.cfg.Enabled {
		phase = "login_required"
		if len(i.auth.Secret) > 0 {
			phase = "connecting"
			if i.auth.Owner == "" && len(i.cfg.LegacyAllowed) == 0 {
				phase = "binding_required"
			}
			if i.connected && (i.auth.Owner != "" || len(i.cfg.LegacyAllowed) > 0) {
				phase = "connected"
			}
			if i.lastError != "" {
				phase = "error"
			}
		}
	}
	if i.login != "" {
		phase = "login_pending"
	}
	options := map[string]string{}
	for k, v := range i.cfg.Options {
		options[k] = v
	}
	for _, f := range i.manifest.Descriptor.Fields {
		if f.Login {
			continue
		}
		if f.Secret && options[f.Key] != "" {
			options[f.Key] = "__REDACTED__"
		}
	}
	permissions := "global defaults"
	if i.cfg.LegacyApproval != "" {
		permissions = "preserved legacy policy"
	}
	if i.cfg.Approval != "" {
		permissions = approvalLabel(effectiveApproval(i.cfg, i.auth))
	}
	authenticated := len(i.auth.Secret) > 0
	bound := authenticated && (i.auth.Owner != "" || len(i.cfg.LegacyAllowed) > 0)
	return map[string]any{"id": i.id, "descriptor": i.manifest.Descriptor, "enabled": i.cfg.Enabled, "workspace": i.cfg.Workspace, "options": options, "revision": i.revision, "authenticated": authenticated, "bound": bound, "connected": i.cfg.Enabled && i.connected, "account": i.auth.Account, "owner": i.auth.Owner, "phase": phase, "last_success": i.lastSuccess, "error": i.lastError, "queued": queued, "unknown": unknown, "legacy_policy": len(i.cfg.LegacyAllowed) > 0 || i.cfg.LegacyApproval != "", "permissions": permissions, "approval": effectiveApproval(i.cfg, i.auth), "full_access_authorized": fullAccessAuthorized(i.cfg, i.auth)}, nil
}
func (m *Manager) Configure(ctx context.Context, id string, revision int64, workspace string, options map[string]string, enabled bool) (any, error) {
	i, err := m.get(id)
	if err != nil {
		return nil, err
	}
	i.op.Lock()
	defer i.op.Unlock()
	if !m.available() {
		return nil, userError("channels stopped")
	}
	if enabled && workspace != "" {
		workspace, err = session.NormalizeWorkspace(workspace)
		if err != nil {
			return nil, userError("choose an existing workspace directory")
		}
	}
	if enabled && workspace == "" {
		return nil, userError("workspace is required")
	}
	i.mu.Lock()
	if revision != i.revision {
		i.mu.Unlock()
		return nil, userError("channel changed; refresh before saving")
	}
	cfg := i.cfg
	descriptor := i.manifest.Descriptor
	oldWorkspace := cfg.Workspace
	i.mu.Unlock()
	if options == nil {
		options = cfg.Options
	}
	next := map[string]string{}
	for _, f := range descriptor.Fields {
		if f.Login {
			continue
		}
		v := options[f.Key]
		if v == "__REDACTED__" {
			v = cfg.Options[f.Key]
		}
		if len(v) > 8192 || strings.ContainsRune(v, 0) || enabled && f.Required && v == "" {
			return nil, userError("invalid or missing plugin setting")
		}
		if f.Secret && v != "" {
			return nil, userError("secret plugin settings must use the login flow")
		}
		next[f.Key] = v
	}
	for k := range options {
		found := false
		for _, f := range descriptor.Fields {
			found = found || (k == f.Key && !f.Login)
		}
		if !found {
			return nil, userError("unknown plugin setting")
		}
	}
	m.stopWorker(i)
	i.mu.Lock()
	defer i.mu.Unlock()
	previous := i.cfg
	i.cfg.Enabled = enabled
	i.cfg.Workspace = workspace
	if oldWorkspace != workspace {
		i.cfg.FullAccess = nil
		i.cfg.Approval = "global"
	}
	i.cfg.Options = next
	i.revision++
	tx, err := m.db.BeginTx(ctx, nil)
	if err == nil {
		err = m.persistWith(ctx, tx, i)
		if err == nil && (!enabled || oldWorkspace != workspace) {
			_, err = tx.ExecContext(ctx, `UPDATE harness_channel_messages SET status='unknown' WHERE instance=? AND status IN ('queued','ready','running','sending')`, id)
		}
		if err == nil {
			err = tx.Commit()
		} else {
			_ = tx.Rollback()
		}
	}
	if err != nil {
		i.cfg = previous
		i.revision--
		m.startWorkerLocked(i)
		return nil, err
	}
	i.login = ""
	i.pairCode = ""
	i.lastError = ""
	m.startWorkerLocked(i)
	return map[string]any{"saved": true, "revision": i.revision}, nil
}
func (m *Manager) ensureProcess(ctx context.Context, i *instance) error {
	if i.proc != nil {
		return nil
	}
	p, d, err := m.start(ctx, i.manifest)
	if err != nil {
		return err
	}
	i.proc = p
	i.manifest.Descriptor = d
	return nil
}
func (m *Manager) startWorker(i *instance) { i.mu.Lock(); defer i.mu.Unlock(); m.startWorkerLocked(i) }
func (m *Manager) startWorkerLocked(i *instance) {
	if i.cancel != nil || !i.cfg.Enabled || len(i.auth.Secret) == 0 {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	i.cancel = cancel
	i.done = make(chan struct{})
	done := i.done
	go func() {
		defer close(done)
		defer m.joinActive(i)
		for {
			if ctx.Err() != nil {
				return
			}
			err := m.workerStep(ctx, i)
			if ctx.Err() != nil {
				return
			}
			delay := time.Second
			i.mu.Lock()
			if err != nil {
				i.connected = false
				i.lastError = "Channel unavailable; retrying"
				delay = 10 * time.Second
				if errors.Is(err, channelproto.ErrAuthExpired) {
					if i.active != nil {
						i.active.cancel()
					}
					i.auth = channelproto.Auth{}
					i.lastError = "Login expired; sign in again"
					_ = m.persist(context.Background(), i)
				}
			}
			i.mu.Unlock()
			select {
			case <-ctx.Done():
				return
			case <-time.After(delay):
			}
		}
	}()
}
func (m *Manager) stopWorker(i *instance) {
	i.mu.Lock()
	cancel, done := i.cancel, i.done
	if cancel != nil {
		cancel()
	}
	i.mu.Unlock()
	if done != nil {
		<-done
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	i.cancel = nil
	i.done = nil
	i.connected = false
	if i.proc != nil {
		i.proc.Close()
		i.proc = nil
	}
}
func (m *Manager) Login(ctx context.Context, id, operation, loginID, verify string) (any, error) {
	return m.LoginWithFields(ctx, id, operation, loginID, verify, nil)
}
func (m *Manager) LoginWithFields(ctx context.Context, id, operation, loginID, verify string, fields map[string]string) (any, error) {
	i, err := m.get(id)
	if err != nil {
		return nil, err
	}
	i.op.Lock()
	defer i.op.Unlock()
	if !m.available() {
		return nil, userError("channels stopped")
	}
	if operation != "login.start" && operation != "login.poll" && operation != "login.cancel" && operation != "logout" {
		return nil, userError("unsupported login operation")
	}
	i.mu.Lock()
	if operation == "login.cancel" && i.login == "" && i.pairCode == "" {
		i.mu.Unlock()
		return map[string]bool{"cancelled": true}, nil
	}
	for key, value := range fields {
		found := false
		for _, f := range i.manifest.Descriptor.Fields {
			found = found || (f.Key == key && f.Login)
		}
		if !found || len(value) > 8192 || strings.ContainsRune(value, 0) {
			i.mu.Unlock()
			return nil, userError("invalid login field")
		}
	}
	if operation == "login.start" {
		for _, f := range i.manifest.Descriptor.Fields {
			if f.Login && f.Required && fields[f.Key] == "" {
				i.mu.Unlock()
				return nil, userError("required login field is missing")
			}
		}
	}
	i.mu.Unlock()
	if operation == "login.start" || operation == "logout" || operation == "login.cancel" {
		m.stopWorker(i)
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if operation == "logout" {
		oldAuth, oldCfg, oldCursor := i.auth, i.cfg, i.cursor
		i.auth = channelproto.Auth{}
		i.cursor = ""
		i.login = ""
		i.pairCode = ""
		i.cfg.Enabled = false
		i.cfg.LegacyAllowed = nil
		i.cfg.LegacyApproval = ""
		i.cfg.FullAccess = nil
		i.cfg.Approval = "global"
		i.revision++
		tx, e := m.db.BeginTx(ctx, nil)
		if e != nil {
			i.auth, i.cfg, i.cursor = oldAuth, oldCfg, oldCursor
			i.revision--
			return nil, e
		}
		err = m.persistWith(ctx, tx, i)
		if err == nil {
			_, err = tx.ExecContext(ctx, `UPDATE harness_channel_messages SET status='unknown' WHERE instance=? AND status IN ('queued','ready','running','sending')`, id)
		}
		if err == nil && id == "wechat" {
			var legacy bool
			err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='harness_wechat_auth')`).Scan(&legacy)
			if err == nil && legacy {
				_, err = tx.ExecContext(ctx, `DELETE FROM harness_wechat_auth`)
			}
		}
		if err == nil {
			err = tx.Commit()
		} else {
			_ = tx.Rollback()
		}
		if err != nil {
			i.auth, i.cfg, i.cursor = oldAuth, oldCfg, oldCursor
			i.revision--
			i.lastError = "Logout could not be saved; retry"
		}
		return map[string]bool{"logged_out": true}, err
	}
	if operation == "login.cancel" {
		i.login = ""
		i.pairCode = ""
		m.startWorkerLocked(i)
		return map[string]bool{"cancelled": true}, nil
	}
	if operation == "login.start" {
		i.login = ""
		i.pairCode = ""
		_, err = m.db.ExecContext(ctx, `UPDATE harness_channel_messages SET status='unknown' WHERE instance=? AND status IN ('queued','ready','running','sending')`, id)
		if err != nil {
			return nil, err
		}
	}
	if operation == "login.poll" && (loginID == "" || i.login != loginID) {
		return nil, userError("login attempt expired; start again")
	}
	if err = m.ensureProcess(ctx, i); err != nil {
		i.lastError = "Plugin unavailable; check its registered path and checksum"
		m.startWorkerLocked(i)
		return nil, err
	}
	var v channelproto.Login
	if err = i.proc.Call(ctx, operation, map[string]any{"id": loginID, "verify_code": verify, "options": i.cfg.Options, "fields": fields}, &v); err != nil {
		i.proc.Close()
		i.proc = nil
		i.login = ""
		m.startWorkerLocked(i)
		return nil, err
	}
	if operation == "login.start" && v.State != "confirmed" {
		if v.ID == "" || len(v.ID) > 256 || len(v.QR) > 4096 || v.ExpiresIn < 1 || v.ExpiresIn > 600 {
			m.startWorkerLocked(i)
			return nil, userError("invalid login challenge")
		}
		i.login = v.ID
	}
	if len(v.ID) > 256 || len(v.QR) > 4096 || (v.ID != "" && operation == "login.poll" && v.ID != loginID) {
		i.proc.Close()
		i.proc = nil
		i.login = ""
		m.startWorkerLocked(i)
		return nil, userError("invalid login response")
	}
	switch v.State {
	case "pending", "scanned", "verification_required", "confirmed", "expired", "failed":
	default:
		i.proc.Close()
		i.proc = nil
		i.login = ""
		m.startWorkerLocked(i)
		return nil, userError("unsupported login state")
	}
	if v.ID == "" {
		v.ID = i.login
	}
	if v.State == "confirmed" {
		if v.Auth == nil || v.Auth.Account == "" || len(v.Auth.Account) > 512 || len(v.Auth.Owner) > 512 || len(v.Auth.Secret) == 0 || len(v.Auth.Secret) > 32768 {
			return nil, userError("invalid plugin credentials")
		}
		previousAuth, previousCfg, previousCursor := i.auth, i.cfg, i.cursor
		i.auth = *v.Auth
		if !fullAccessAuthorized(i.cfg, i.auth) {
			i.cfg.FullAccess = nil
			i.cfg.Approval = "global"
		}
		i.cursor = ""
		i.login = ""
		i.cfg.LegacyAllowed = nil
		i.cfg.LegacyApproval = ""
		i.revision++
		i.lastError = ""
		tx, e := m.db.BeginTx(ctx, nil)
		if e != nil {
			i.auth, i.cfg, i.cursor = previousAuth, previousCfg, previousCursor
			i.revision--
			return nil, e
		}
		err = m.persistWith(ctx, tx, i)
		if err == nil {
			_, err = tx.ExecContext(ctx, `UPDATE harness_channel_messages SET status='unknown' WHERE instance=? AND status IN ('queued','ready','running','sending')`, id)
		}
		if err == nil {
			err = tx.Commit()
		} else {
			_ = tx.Rollback()
		}
		if err != nil {
			i.auth, i.cfg, i.cursor = previousAuth, previousCfg, previousCursor
			i.revision--
			return nil, err
		}
		if i.auth.Owner == "" {
			i.pairCode = rand.Text()
			i.pairExpires = time.Now().Add(3 * time.Minute)
			v.State = "binding_required"
		}
		m.startWorkerLocked(i)
	}
	if v.State == "expired" || v.State == "failed" {
		i.login = ""
		m.startWorkerLocked(i)
	}
	// The unredacted Auth response never crosses the host management boundary.
	return map[string]any{"id": v.ID, "state": v.State, "qrcode_content": v.QR, "expires_in": v.ExpiresIn, "account": i.auth.Account, "owner": i.auth.Owner, "pairing_code": i.pairCode}, nil
}

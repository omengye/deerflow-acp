package deerflow

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/automation"
	"github.com/omengye/deerflow-acp/go-harness/internal/channels"
	"github.com/omengye/deerflow-acp/go-harness/internal/localhost"
	hr "github.com/omengye/deerflow-acp/go-harness/internal/runtime"
	"github.com/omengye/deerflow-acp/go-harness/internal/session"
)

type scheduledRunKey struct{}

type buzzSchedulePolicy struct {
	Account    string   `json:"account"`
	Workspace  string   `json:"workspace"`
	Allowed    []string `json:"allowed_senders"`
	FullAccess bool     `json:"full_access"`
}

func (c *Client) bindScheduleSource(ctx context.Context, sessionID string, source automation.Source) error {
	x, err := c.service.Store.Session(ctx, sessionID)
	if err != nil {
		return err
	}
	workspace, err := session.NormalizeWorkspace(source.Workspace)
	if err != nil || !session.SameWorkspace(workspace, x.CWD) {
		return harness.ErrPermissionDenied
	}
	// Resolve aliases only at attachment. Persist the session's pinned path so
	// later symlink changes cannot transfer a saved grant to another workspace.
	source.SessionID, source.Workspace = sessionID, x.CWD
	raw, err := json.Marshal(source)
	if err == nil {
		_, err = c.store.DB().ExecContext(ctx, `INSERT INTO harness_schedule_sources(session_id,source) VALUES(?,?) ON CONFLICT(session_id) DO UPDATE SET source=excluded.source`, sessionID, raw)
	}
	return err
}

// Each admitted run pins its source once. Resume cannot turn a channel or
// scheduled execution into a local desktop administrator after context loss.
func (c *Client) scheduleSource(ctx context.Context, req harness.RunRequest) (automation.Source, error) {
	db := c.store.DB()
	var source automation.Source
	var raw []byte
	err := db.QueryRowContext(ctx, `SELECT source FROM harness_schedule_run_sources WHERE run_id=?`, req.RunID).Scan(&raw)
	if err == nil {
		err = json.Unmarshal(raw, &source)
		return source, err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return source, err
	}
	if d, ok := channels.DispatchFromContext(ctx); ok {
		source = c.channels.ScheduleSource(d, req.Session.ID)
		if req.Session.ApprovalMode != harness.ApprovalAllowAlways {
			source.Grant = ""
		}
	} else {
		err = db.QueryRowContext(ctx, `SELECT source FROM harness_schedule_sources WHERE session_id=?`, req.Session.ID).Scan(&raw)
		if err == nil {
			if err = json.Unmarshal(raw, &source); err != nil {
				return source, err
			}
		} else if errors.Is(err, sql.ErrNoRows) {
			source = automation.Source{Kind: "desktop", Workspace: req.Session.CWD, SessionID: req.Session.ID}
		} else {
			return source, err
		}
	}
	if !session.SameWorkspace(source.Workspace, req.Session.CWD) {
		return source, harness.ErrPermissionDenied
	}
	raw, err = json.Marshal(source)
	if err == nil {
		_, err = db.ExecContext(ctx, `INSERT OR IGNORE INTO harness_schedule_run_sources(run_id,source) VALUES(?,?)`, req.RunID, raw)
	}
	if err != nil {
		return source, err
	}
	err = db.QueryRowContext(ctx, `SELECT source FROM harness_schedule_run_sources WHERE run_id=?`, req.RunID).Scan(&raw)
	if err != nil {
		return source, err
	}
	err = json.Unmarshal(raw, &source)
	return source, err
}

func (c *Client) authorizeScheduleSource(ctx context.Context, source automation.Source, full bool) error {
	switch source.Kind {
	case "desktop":
		return nil
	case "scheduled", "untrusted":
		return harness.ErrPermissionDenied
	case "buzz":
		var raw []byte
		var version string
		var expires int64
		err := c.store.DB().QueryRowContext(ctx, `SELECT policy,version,expires_ms FROM harness_schedule_buzz_grants WHERE account=? AND workspace=?`, source.Account, source.Workspace).Scan(&raw, &version, &expires)
		var policy buzzSchedulePolicy
		if err != nil || json.Unmarshal(raw, &policy) != nil || expires < time.Now().UnixMilli() || !slices.Contains(policy.Allowed, source.Sender) || full && (!policy.FullAccess || source.Grant == "" || source.Grant != version) {
			return fmt.Errorf("%w: Buzz scheduling grant unavailable, expired or revoked; check the local adapter configuration", harness.ErrPermissionDenied)
		}
		return nil
	default:
		if c.channels.AuthorizeSchedule(source, full) {
			return nil
		}
		return fmt.Errorf("%w: channel scheduling requires a current local Full Access grant", harness.ErrPermissionDenied)
	}
}

func (c *Client) ownsSchedule(ctx context.Context, source automation.Source, spec automation.Spec) bool {
	if !session.SameWorkspace(source.Workspace, spec.Workspace) {
		return false
	}
	if source.Kind == "desktop" {
		return true
	}
	if spec.Source == nil {
		return false
	}
	if source.Principal() == spec.Source.Principal() {
		return true
	}
	var matched bool
	err := c.store.DB().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM harness_schedule_owner_links a JOIN harness_schedule_owner_links b ON a.owner=b.owner WHERE a.principal=? AND b.principal=?)`, source.Principal(), spec.Source.Principal()).Scan(&matched)
	return err == nil && matched
}

// These operations are local-admin only, never ACP/model tools. The adapter
// uses the protected loopback management channel, not a prompt text identity.
func (c *Client) managementScheduleSources(ctx context.Context, r localhost.ManagementRequest) (any, error) {
	switch r.Operation {
	case "schedule.buzz.register":
		var in buzzSchedulePolicy
		if err := decodeManagement(r, &in, "account", "workspace", "allowed_senders", "full_access"); err != nil {
			return nil, err
		}
		if in.Account == "" || len(in.Account) > 256 || !filepath.IsAbs(in.Workspace) || len(in.Allowed) > 100 || in.FullAccess && len(in.Allowed) == 0 {
			return nil, managementInvalid("invalid Buzz schedule policy")
		}
		workspace, err := session.NormalizeWorkspace(in.Workspace)
		if err != nil {
			return nil, managementInvalid("choose an existing workspace directory")
		}
		in.Workspace = workspace
		for _, sender := range in.Allowed {
			if sender == "" || len(sender) > 256 {
				return nil, managementInvalid("invalid sender")
			}
		}
		slices.Sort(in.Allowed)
		in.Allowed = slices.Compact(in.Allowed)
		raw, _ := json.Marshal(in)
		version := hr.NewID()
		var old []byte
		var oldVersion string
		if err := c.store.DB().QueryRowContext(ctx, `SELECT policy,version FROM harness_schedule_buzz_grants WHERE account=? AND workspace=?`, in.Account, in.Workspace).Scan(&old, &oldVersion); err == nil && string(old) == string(raw) {
			version = oldVersion
		}
		_, err = c.store.DB().ExecContext(ctx, `INSERT INTO harness_schedule_buzz_grants(account,workspace,policy,version,expires_ms) VALUES(?,?,?,?,?) ON CONFLICT(account,workspace) DO UPDATE SET policy=excluded.policy,version=excluded.version,expires_ms=excluded.expires_ms`, in.Account, in.Workspace, raw, version, time.Now().Add(2*time.Minute).UnixMilli())
		return map[string]any{"version": version}, err
	case "schedule.buzz.bind":
		var in struct {
			Session string            `json:"session_id"`
			Source  automation.Source `json:"source"`
		}
		if err := decodeManagement(r, &in, "session_id", "source"); err != nil {
			return nil, err
		}
		s := in.Source
		s.Kind, s.SessionID = "buzz", in.Session
		if s.Account == "" || s.Sender == "" || len(s.Account) > 256 || len(s.Sender) > 256 || len(s.Grant) > 256 || s.MessageID == "" || s.Conversation == "" || len(s.MessageID) > 256 || len(s.Conversation) > 512 {
			return nil, managementInvalid("invalid Buzz source")
		}
		return map[string]any{"bound": true}, c.bindScheduleSource(ctx, in.Session, s)
	case "schedule.owner.link":
		var in struct {
			Sources []automation.Source `json:"sources"`
			Owner   string              `json:"owner"`
		}
		if err := decodeManagement(r, &in, "sources", "owner"); err != nil {
			return nil, err
		}
		if len(in.Sources) < 1 || len(in.Sources) > 20 || len(in.Owner) > 200 {
			return nil, managementInvalid("invalid owner link")
		}
		for index, source := range in.Sources {
			if source.Kind == "" || source.Account == "" || source.Sender == "" || !filepath.IsAbs(source.Workspace) {
				return nil, managementInvalid("incomplete source identity")
			}
			workspace, err := session.NormalizeWorkspace(source.Workspace)
			if err != nil {
				return nil, managementInvalid("choose an existing workspace directory")
			}
			in.Sources[index].Workspace = workspace
		}
		tx, err := c.store.DB().BeginTx(ctx, nil)
		if err != nil {
			return nil, err
		}
		defer tx.Rollback()
		for _, source := range in.Sources {
			if strings.TrimSpace(in.Owner) == "" {
				_, err = tx.ExecContext(ctx, `DELETE FROM harness_schedule_owner_links WHERE principal=?`, source.Principal())
			} else {
				_, err = tx.ExecContext(ctx, `INSERT INTO harness_schedule_owner_links VALUES(?,?) ON CONFLICT(principal) DO UPDATE SET owner=excluded.owner`, source.Principal(), in.Owner)
			}
			if err != nil {
				return nil, err
			}
		}
		return map[string]any{"saved": true}, tx.Commit()
	default:
		return c.managementScheduleDelivery(ctx, r)
	}
}

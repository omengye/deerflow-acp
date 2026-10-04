package skills

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

type exportTarget struct {
	root *os.Root
	key  string
}

func openExportTarget(directory string) (*exportTarget, error) {
	s, err := openSource(harness.SkillSource{ID: "exports", Root: directory, Scope: harness.SkillScopeGlobal})
	if err != nil {
		return nil, invalid("export root must be an existing ordinary absolute directory")
	}
	return &exportTarget{root: s.root, key: s.identity}, nil
}

func (p *PublicationTx) queueExport(head SkillHead) error {
	if p.export == nil || !head.Managed {
		return nil
	}
	data, _ := json.Marshal(head)
	_, err := p.tx.ExecContext(p.ctx, `INSERT INTO df_skill_exports(source_key,name,export_key,state_version,head,status,view_directory,attempts,error_code,updated_at,retry_after_ns) VALUES(?,?,?,?,?,'pending','',0,'',?,0) ON CONFLICT(source_key,name,export_key) DO UPDATE SET state_version=excluded.state_version,head=excluded.head,status='pending',view_directory='',attempts=0,error_code='',updated_at=excluded.updated_at,retry_after_ns=0 WHERE df_skill_exports.state_version<>excluded.state_version`, p.source.identity, p.name, p.export.key, head.StateVersion, string(data), time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

// ReconcileExports restores missing outbox entries and requests verification of
// synced views after restart. Failed/pending attempts keep their retry state,
// unless an operator explicitly requests retryFailed.
func (r *Registry) ReconcileExports(ctx context.Context, retryFailed bool) error {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		return ErrClosed
	}
	if r.export == nil {
		return nil
	}
	for _, s := range r.sources {
		rows, err := r.db.QueryContext(ctx, `SELECT name FROM df_skill_control WHERE source_key=? AND managed=1 ORDER BY name`, s.identity)
		if err != nil {
			return err
		}
		var names []string
		for rows.Next() {
			var name string
			if err = rows.Scan(&name); err != nil {
				break
			}
			names = append(names, name)
		}
		if err == nil {
			err = rows.Err()
		}
		_ = rows.Close()
		if err != nil {
			return err
		}
		for _, name := range names {
			if !validName(name) {
				return errors.New("skill export target is inconsistent")
			}
			err = r.withPublication(ctx, s, name, func(tx *PublicationTx) error {
				head, err := tx.Current()
				if err != nil {
					return err
				}
				if err = tx.queueExport(head); err != nil {
					return err
				}
				_, err = tx.SQL().ExecContext(ctx, `UPDATE df_skill_exports SET status='pending',attempts=0,error_code='',retry_after_ns=0,updated_at=? WHERE source_key=? AND name=? AND export_key=? AND (status='synced' OR ?)`, time.Now().UTC().Format(time.RFC3339Nano), s.identity, name, r.export.key, retryFailed)
				return err
			})
			if err != nil {
				return err
			}
		}
	}
	return ctx.Err()
}

// StartExportWorker is called by the owning SDK after construction. It performs
// local materialization only, has no model budget and never invokes scripts.
// Close cancels and joins it before closing roots/database dependencies.
func (r *Registry) StartExportWorker() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return ErrClosed
	}
	if r.export == nil || r.exportDone != nil {
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.exportCancel, r.exportDone = cancel, make(chan struct{})
	go func() {
		defer close(r.exportDone)
		warned := false
		for {
			err := r.SyncExports(ctx)
			if ctx.Err() != nil || errors.Is(err, ErrClosed) {
				return
			}
			delay := time.Second
			if err != nil {
				if !warned {
					slog.Warn("skill export synchronization failed; publications remain committed")
				}
				warned, delay = true, 5*time.Second
			} else {
				warned = false
			}
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}()
	return nil
}

type exportItem struct {
	source  *source
	state   harness.SkillExportState
	head    string
	updated string
}

type exportRow interface{ Scan(...any) error }

func readExport(row exportRow, s *source, exportKey string) (exportItem, error) {
	var item exportItem
	var name, updated string
	var version, retry int64
	item.source = s
	o := &item.state
	err := row.Scan(&name, &version, &item.head, &o.Status, &o.Directory, &o.Attempts, &o.ErrorCode, &updated, &retry)
	if err != nil {
		return item, err
	}
	o.Target = harness.SkillTarget{SourceID: s.config.ID, SourceIdentity: s.identity, Name: name, Scope: s.config.Scope, Workspace: s.config.Workspace}
	if json.Unmarshal([]byte(item.head), &o.Head) != nil || !validName(name) || version < 1 || o.Head.StateVersion != version || !o.Head.Managed || o.Head.Exists != (o.Head.Version != "") || o.Head.Exists && !validHash(o.Head.Version) || !o.Head.Exists && o.Head.Enabled || o.Attempts < 0 || o.Attempts > 3 {
		return item, errors.New("skill export record is inconsistent")
	}
	if o.Status != "pending" && o.Status != "synced" && o.Status != "failed" || o.Status == "synced" && (o.Directory == "" || o.ErrorCode != "" || retry != 0) || o.Status == "failed" && (o.Attempts != 3 || retry != 0) || o.Status == "pending" && o.Attempts >= 3 {
		return item, errors.New("skill export status is inconsistent")
	}
	if o.ErrorCode != "" && o.ErrorCode != "filesystem_unavailable" {
		return item, errors.New("skill export error is inconsistent")
	}
	if o.Directory != "" && !validExportDirectory(o.Target, o.Head, o.Directory) {
		return item, errors.New("skill export directory is inconsistent")
	}
	o.UpdatedAt, err = time.Parse(time.RFC3339Nano, updated)
	if err != nil || retry < 0 {
		return item, errors.New("skill export timestamp is inconsistent")
	}
	if retry != 0 {
		t := time.Unix(0, retry).UTC()
		o.RetryAfter = &t
	}
	item.updated = updated
	data, _ := json.Marshal([]any{exportKey, *o})
	o.StateToken = digest(data)
	return item, nil
}

const exportColumns = `name,state_version,head,status,view_directory,attempts,error_code,updated_at,retry_after_ns`

// SyncExports processes one bounded batch. File failures are recorded separately
// and retried at most three times. SQL/content integrity failures are returned.
// A directory is never authoritative before a CAS settles this exact head.
func (r *Registry) SyncExports(ctx context.Context) error {
	r.exportMu.Lock()
	defer r.exportMu.Unlock()
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		return ErrClosed
	}
	if r.export == nil {
		return nil
	}
	remaining := 32
	for _, s := range r.sources {
		rows, err := r.db.QueryContext(ctx, `SELECT `+exportColumns+` FROM df_skill_exports WHERE source_key=? AND export_key=? AND status='pending' AND retry_after_ns<=? ORDER BY retry_after_ns,name LIMIT ?`, s.identity, r.export.key, time.Now().UnixNano(), remaining)
		if err != nil {
			return err
		}
		var items []exportItem
		for rows.Next() {
			var item exportItem
			item, err = readExport(rows, s, r.export.key)
			if err != nil {
				break
			}
			items = append(items, item)
		}
		if err == nil {
			err = rows.Err()
		}
		_ = rows.Close()
		if err != nil {
			return err
		}
		for _, item := range items {
			if err = r.syncExport(ctx, item); err != nil {
				return err
			}
		}
		remaining -= len(items)
		if remaining == 0 {
			break
		}
	}
	return ctx.Err()
}

func (r *Registry) syncExport(ctx context.Context, item exportItem) error {
	var candidate *Candidate
	err := r.withPublication(ctx, item.source, item.state.Target.Name, func(tx *PublicationTx) error {
		head, err := tx.Current()
		if err != nil {
			return err
		}
		if head != item.state.Head {
			if head.StateVersion == item.state.Head.StateVersion {
				return errors.New("skill export head differs from its publication state")
			}
			return ErrPublicationConflict
		}
		if head.Exists {
			candidate, err = tx.ReadVersion(head.Version)
		}
		return err
	})
	if errors.Is(err, ErrPublicationConflict) {
		return nil
	}
	if err != nil {
		return err
	}
	directory, fileErr := r.materializeExport(ctx, item.state, candidate)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	status, code, attempts, retry := "synced", "", item.state.Attempts, int64(0)
	if fileErr != nil {
		status, code, directory = "pending", "filesystem_unavailable", ""
		attempts++
		if attempts >= 3 {
			status = "failed"
		} else {
			retry = time.Now().Add(time.Duration(attempts*attempts) * time.Second).UnixNano()
		}
	}
	_, err = r.db.ExecContext(ctx, `UPDATE df_skill_exports SET status=?,view_directory=?,attempts=?,error_code=?,updated_at=?,retry_after_ns=? WHERE source_key=? AND name=? AND export_key=? AND state_version=? AND head=? AND status='pending' AND attempts=? AND view_directory=? AND updated_at=?`, status, directory, attempts, code, time.Now().UTC().Format(time.RFC3339Nano), retry, item.source.identity, item.state.Target.Name, r.export.key, item.state.Head.StateVersion, item.head, item.state.Attempts, item.state.Directory, item.updated)
	return err
}

// ListExports returns states for the current export root and configured source
// identities, including tombstones. It performs no filesystem I/O.
func (r *Registry) ListExports(ctx context.Context, selection harness.SkillSelection) ([]harness.SkillExportState, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		return nil, ErrClosed
	}
	sources, err := r.selectedSources(selection)
	if err != nil {
		return nil, err
	}
	result := make([]harness.SkillExportState, 0)
	if r.export == nil {
		return result, nil
	}
	for _, s := range sources {
		rows, err := r.db.QueryContext(ctx, `SELECT `+exportColumns+` FROM df_skill_exports WHERE source_key=? AND export_key=? ORDER BY name`, s.identity, r.export.key)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var item exportItem
			item, err = readExport(rows, s, r.export.key)
			if err != nil {
				break
			}
			if selectedName(selection.Names, item.state.Target.Name) {
				result = append(result, item.state)
			}
		}
		if err == nil {
			err = rows.Err()
		}
		_ = rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return result, nil
}

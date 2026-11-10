package skills

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

// readRecoveryHistory opens only an explicitly supplied historical database,
// with SQLite mode=ro. It never creates/migrates it, reads conversations, copies
// jobs, or revives automatic candidates whose routing/observation was not moved.
func readRecoveryHistory(ctx context.Context, target harness.SkillTarget, history string, limits harness.SkillLimits) (harness.SkillRecoveryPlan, error) {
	var plan harness.SkillRecoveryPlan
	if !filepath.IsAbs(history) {
		return plan, invalid("recovery requires an absolute historical database path")
	}
	if err := noSymlinkPath(history); err != nil {
		return plan, err
	}
	info, err := os.Lstat(history)
	if err != nil || !info.Mode().IsRegular() {
		return plan, invalid("historical database must be an existing regular file")
	}
	p := filepath.ToSlash(history)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	u := url.URL{Scheme: "file", Path: p}
	q := url.Values{"mode": {"ro"}, "_pragma": {"query_only(1)", "busy_timeout(1500)"}}
	db, err := sql.Open("sqlite", u.String()+"?"+q.Encode())
	if err != nil {
		return plan, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return plan, err
	}
	defer tx.Rollback()
	head, err := ReadPublicationHead(ctx, tx, target.SourceIdentity, target.Name)
	if err != nil {
		return plan, err
	}
	if !head.Exists || !head.Managed {
		return plan, invalid("history has no managed published installation")
	}
	if pending, e := publicationPending(ctx, tx, target.SourceIdentity, target.Name); e != nil {
		return plan, e
	} else if pending {
		return plan, harness.ErrReconciliationRequired
	}
	if pending, e := editPending(ctx, tx, target.SourceIdentity, target.Name, ""); e != nil {
		return plan, e
	} else if pending {
		return plan, harness.ErrReconciliationRequired
	}
	var raw []byte
	err = tx.QueryRowContext(ctx, `SELECT substr(payload,1,65537) FROM df_skill_publications WHERE source_key=? AND name=? ORDER BY revision DESC LIMIT 1`, target.SourceIdentity, target.Name).Scan(&raw)
	if err != nil {
		return plan, err
	}
	var publication harness.SkillPublication
	if len(raw) > 65536 || json.Unmarshal(raw, &publication) != nil || publication.Target != target || publication.Name != target.Name || publication.Deleted || publication.ContentVersion != head.Version || publication.Head.Version != head.Version || !publication.Head.Exists || publication.ProposalID == "" {
		return plan, invalid("historical publication does not match the installed version")
	}
	var fileStatus string
	if err = tx.QueryRowContext(ctx, `SELECT status FROM df_skill_file_publication_results WHERE source_key=? AND name=? AND state_version=?`, target.SourceIdentity, target.Name, publication.Head.StateVersion).Scan(&fileStatus); err != nil || fileStatus != "applied" {
		return plan, invalid("historical publication has no applied file receipt")
	}
	var proposal harness.SkillProposal
	if err = tx.QueryRowContext(ctx, `SELECT payload FROM df_skill_proposals WHERE id=? AND source_key=? AND name=? AND status='published' AND length(payload)<=8388608`, publication.ProposalID, target.SourceIdentity, target.Name).Scan(&raw); err != nil {
		return plan, err
	}
	if json.Unmarshal(raw, &proposal) != nil || proposal.ID != publication.ProposalID || proposal.Target != target || proposal.Status != "published" || proposal.CandidateVersion != head.Version || proposal.Origin == "automatic" {
		return plan, invalid("recovery requires a matching historical publication; automatic candidates need their original lifecycle state")
	}
	var meta, manifest, status, observed string
	var size int64
	if err = tx.QueryRowContext(ctx, `SELECT metadata,manifest,total_bytes FROM df_skill_versions WHERE source_key=? AND name=? AND version=?`, target.SourceIdentity, target.Name, head.Version).Scan(&meta, &manifest, &size); err != nil {
		return plan, err
	}
	fm, files, err := decodeVersion(meta, manifest, head.Version, size, limits)
	if err != nil || fm.Name != target.Name {
		return plan, invalid("historical file manifest is invalid")
	}
	if err = tx.QueryRowContext(ctx, `SELECT status,observed_version FROM df_skill_source_status WHERE source_key=? AND name=?`, target.SourceIdentity, target.Name).Scan(&status, &observed); err != nil || status != "ready" || observed != head.Version {
		return plan, invalid("historical source was not ready")
	}
	publication.FileStatus = fileStatus
	plan = harness.SkillRecoveryPlan{Target: target, Version: head.Version, Enabled: head.Enabled, Files: files, Publication: publication}
	encoded, _ := json.Marshal(plan)
	plan.ReceiptSHA256 = digest(encoded)
	return plan, tx.Commit()
}

func (r *Registry) recoveryPackage(ctx context.Context, s *source, plan harness.SkillRecoveryPlan) (skillPackage, error) {
	first, err := r.readPackage(ctx, s, plan.Target.Name)
	if err != nil {
		return first, err
	}
	if first.meta.Name != plan.Target.Name || (&Candidate{pkg: first}).Version() != plan.Version {
		return first, harness.ErrSkillConflict
	}
	second, err := r.readPackage(ctx, s, plan.Target.Name)
	if err != nil {
		return first, err
	}
	if (&Candidate{pkg: second}).Version() != plan.Version {
		return first, harness.ErrSkillConflict
	}
	return first, nil
}

func (r *Registry) PlanRecovery(ctx context.Context, target harness.SkillTarget, history string) (harness.SkillRecoveryPlan, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var plan harness.SkillRecoveryPlan
	if r.closed {
		return plan, ErrClosed
	}
	s, err := r.catalogTarget(target)
	if err != nil {
		return plan, err
	}
	if !s.managed || s.builtin {
		return plan, harness.ErrPermissionDenied
	}
	// Do not open another connection to the live database as recovery history.
	var seq int
	var dbName, current string
	if err = r.db.QueryRowContext(ctx, `PRAGMA database_list`).Scan(&seq, &dbName, &current); err != nil {
		return plan, err
	}
	if a, e := os.Stat(current); e == nil {
		if b, e := os.Stat(history); e == nil && os.SameFile(a, b) {
			return plan, invalid("recovery history must not be the active database")
		}
	}
	var exists bool
	if err = r.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM df_skill_control WHERE source_key=? AND name=?)`, target.SourceIdentity, target.Name).Scan(&exists); err != nil {
		return plan, err
	}
	if exists {
		return plan, harness.ErrSkillConflict
	}
	plan, err = readRecoveryHistory(ctx, target, history, r.limits)
	if err != nil {
		return plan, err
	}
	_, err = r.recoveryPackage(ctx, s, plan)
	return plan, err
}

// RecoverPublished is an explicit host repair, with a checked historical
// receipt and exact installed file manifest. It never writes live skill files.
// Idempotent replays do not need the history database (it may be archived).
func (r *Registry) RecoverPublished(ctx context.Context, in harness.SkillRecovery) (harness.SkillManagementResult, error) {
	if !validHash(in.Version) || !validHash(in.ReceiptSHA256) {
		return harness.SkillManagementResult{}, invalid("recovery requires the previewed version and receipt")
	}
	return r.catalogCommand(ctx, in.Target, in.RequestKey, in.Actor, "recover_installation", in, func(p *PublicationTx) (SkillHead, error) {
		head, err := p.Current()
		if err != nil {
			return head, err
		}
		if head.StateVersion != 0 || !p.source.managed || p.source.builtin {
			return head, harness.ErrSkillConflict
		}
		var guarded bool
		if err = p.tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM df_skill_creation_guards WHERE source_key=? AND name=?)`, in.Target.SourceIdentity, in.Target.Name).Scan(&guarded); err != nil {
			return head, err
		}
		if guarded {
			return head, harness.ErrPermissionDenied
		}
		plan, err := readRecoveryHistory(ctx, in.Target, in.HistoryDatabase, r.limits)
		if err != nil {
			return head, err
		}
		if plan.Version != in.Version || plan.Enabled != in.Enabled || plan.ReceiptSHA256 != in.ReceiptSHA256 {
			return head, harness.ErrSkillConflict
		}
		pkg, err := r.recoveryPackage(ctx, p.source, plan)
		if err != nil {
			return head, err
		}
		if err = guardAutomaticContent(ctx, p.tx, in.Version); err != nil {
			return head, err
		}
		if _, err = p.stagePackage(pkg); err != nil {
			return head, err
		}
		next, err := p.moveHead(head, in.Version, in.Enabled, true, in.Version, in.Target.Name)
		if err != nil {
			return head, err
		}
		if err = p.recordSourceStatus(in.Target.Name, "ready", in.Version, ""); err != nil {
			return head, err
		}
		proof, _ := json.Marshal(plan)
		_, err = p.tx.ExecContext(ctx, `INSERT INTO df_skill_recoveries(source_key,name,request_key,receipt_sha,payload,created_at) VALUES(?,?,?,?,?,?)`, in.Target.SourceIdentity, in.Target.Name, in.RequestKey, digest(proof), proof, time.Now().UTC().Format(time.RFC3339Nano))
		return next, err
	})
}

package skills

import (
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

// PublishReviewed installs the exact package admitted by the host's safety review.
func (p *PublicationTx) PublishReviewed(approval harness.SkillApproval, enabled bool) (SkillHead, error) {
	head, err := p.Current()
	if err != nil {
		return head, err
	}
	if head != approval.Base {
		return head, ErrPublicationConflict
	}
	if approval.Target != p.Target() || approval.SecurityReview == nil || approval.UserConfirmation != nil || approval.CandidateVersion == "" ||
		approval.SecurityReview.Workspace == "" && (approval.SecurityReview.WorkspaceOnly || approval.Target.Scope != harness.SkillScopeGlobal) {
		return head, harness.ErrPermissionDenied
	}
	var data []byte
	var sha string
	if err = p.tx.QueryRowContext(p.ctx, `SELECT payload,payload_sha FROM df_skill_approvals WHERE proposal_id=?`, approval.ProposalID).Scan(&data, &sha); err != nil {
		return head, err
	}
	expected, err := json.Marshal(approval)
	if err != nil || digest(data) != sha || digest(expected) != sha {
		return head, harness.ErrPermissionDenied
	}
	if _, err = p.ReadVersion(approval.CandidateVersion); err != nil {
		return head, err
	}
	if err = checkScopedVersion(p.ctx, p.tx, harness.SkillRef{SourceIdentity: p.source.identity, Name: p.name, Version: approval.CandidateVersion}, approval.SecurityReview.Workspace); err != nil {
		return head, err
	}
	if approval.SecurityReview.WorkspaceOnly {
		return p.publishScoped(approval.CandidateVersion, head, enabled, approval.SecurityReview.Workspace, "stable")
	}
	return p.preparePublication(approval.CandidateVersion, head, enabled)
}

// Source refresh may recognize only the exact head installed by this decision.
// A copied automatic package at another target or after a newer head remains
// blocked. This is not permission to import or republish arbitrary bytes.
func (p *PublicationTx) isReviewedHead(head SkillHead, version string) (bool, error) {
	if !head.Exists || head.Version != version {
		return false, nil
	}
	var tables int
	if err := p.tx.QueryRowContext(p.ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name IN ('df_skill_publications','df_skill_approvals')`).Scan(&tables); err != nil || tables != 2 {
		return false, err
	}
	var decision, publication []byte
	var sha, requestSHA, key string
	err := p.tx.QueryRowContext(p.ctx, `SELECT a.payload,a.payload_sha,h.payload,h.request_sha,h.request_key FROM df_skill_publications h
 JOIN df_skill_approvals a ON a.proposal_id=json_extract(h.payload,'$.source_proposal_id')
 WHERE h.source_key=? AND h.name=? AND json_extract(h.payload,'$.head.state_version')<=?
 AND json_extract(h.payload,'$.sha256')=? AND h.request_key IN ('confirm/'||a.proposal_id,'security/'||a.proposal_id)
 ORDER BY h.revision DESC LIMIT 1`, p.source.identity, p.name, head.StateVersion, version).Scan(&decision, &sha, &publication, &requestSHA, &key)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var approval harness.SkillApproval
	var pub harness.SkillPublication
	if digest(decision) != sha || sha != requestSHA || json.Unmarshal(decision, &approval) != nil || json.Unmarshal(publication, &pub) != nil {
		return false, invalid("confirmed publication integrity failure")
	}
	return (approval.SecurityReview == nil || !approval.SecurityReview.WorkspaceOnly) && approval.FilePublicationKey() == key && approval.Target == p.Target() && approval.CandidateVersion == version &&
		pub.Target == p.Target() && pub.Head.Version == head.Version && pub.Head.Exists && pub.ProposalID == approval.ProposalID && pub.ContentVersion == version, nil
}

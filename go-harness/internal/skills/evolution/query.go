package evolution

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills"
)

type pageCursor struct {
	Kind, Filter, Key, Name string
	Revision                int64
}

func validStatus(status string) bool {
	switch status {
	case "", "generating", "validating", "pending_review", "publishing", "published", "rejected", "failed", "stale":
		return true
	}
	return false
}

func (s *Service) querySources(q harness.SkillQuery, kind string) (map[string]harness.SkillTarget, string, []any, pageCursor, error) {
	cursor := pageCursor{Kind: kind}
	if q.NameContains != "" {
		return nil, "", nil, cursor, invalid("name substring filter is only supported for the catalog")
	}
	if q.Limit < 1 || q.Limit > 100 || !validStatus(q.Status) || len(q.Cursor) > 2048 || !bounded(q.SourceID, 128) || !bounded(q.Name, 128) {
		return nil, "", nil, cursor, invalid("query is invalid")
	}
	sources, err := s.registry.PublicationSources()
	if err != nil {
		return nil, "", nil, cursor, err
	}
	if q.SourceID != "" {
		selected := sources[:0]
		for _, source := range sources {
			if source.SourceID == q.SourceID {
				selected = append(selected, source)
			}
		}
		sources = selected
		if len(sources) == 0 {
			return nil, "", nil, cursor, harness.ErrPermissionDenied
		}
	}
	cursor.Filter = encodedHash(struct {
		Sources      []harness.SkillTarget
		Name, Status string
	}{sources, q.Name, q.Status})
	if q.NeedsAttention != nil {
		if kind != "proposals" {
			return nil, "", nil, cursor, invalid("attention filter is only supported for proposals")
		}
		cursor.Filter = encodedHash([]any{cursor.Filter, *q.NeedsAttention, s.autoPatchPolicyIdentity()})
	}
	if q.Cursor != "" {
		var saved pageCursor
		data, err := base64.RawURLEncoding.DecodeString(q.Cursor)
		if err != nil || json.Unmarshal(data, &saved) != nil || saved.Kind != kind || saved.Filter != cursor.Filter || !bounded(saved.Key, 256) || !bounded(saved.Name, 128) || saved.Revision < 0 {
			return nil, "", nil, cursor, invalid("cursor does not match query")
		}
		cursor = saved
	}
	allowed := make(map[string]harness.SkillTarget, len(sources))
	args := make([]any, 0, len(sources))
	for _, source := range sources {
		allowed[source.SourceIdentity] = source
		args = append(args, source.SourceIdentity)
	}
	where := "0=1"
	if len(args) > 0 {
		where = "source_key IN (" + strings.TrimSuffix(strings.Repeat("?,", len(args)), ",") + ")"
	}
	if q.Name != "" {
		where += " AND name=?"
		args = append(args, q.Name)
	}
	return allowed, where, args, cursor, nil
}

func encodeCursor(c pageCursor) string {
	data, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(data)
}

func checkTarget(allowed map[string]harness.SkillTarget, target harness.SkillTarget) bool {
	source, ok := allowed[target.SourceIdentity]
	source.Name = target.Name
	return ok && source == target
}

func (s *Service) QueryProposals(ctx context.Context, q harness.SkillQuery) (harness.SkillProposalPage, error) {
	page := harness.SkillProposalPage{Proposals: make([]Proposal, 0)}
	if q.NeedsAttention != nil && q.Status != "pending_review" {
		return page, invalid("attention filter requires pending_review")
	}
	allowed, where, args, cursor, err := s.querySources(q, "proposals")
	if err != nil {
		return page, err
	}
	where += " AND id>?"
	args = append(args, cursor.Key)
	if q.Status != "" {
		where += " AND status=?"
		args = append(args, q.Status)
	}
	if q.NeedsAttention != nil {
		expression, parameters := s.proposalReasonSQL()
		where += " AND ((" + expression + ") NOT IN ('waiting_evidence','automatic_checks','waiting_observation','waiting_quota'))=?"
		args = append(args, parameters...)
		args = append(args, *q.NeedsAttention)
	}
	args = append(args, q.Limit+1)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return page, err
	}
	defer tx.Rollback()
	if err = tx.QueryRowContext(ctx, `SELECT revision FROM df_skill_catalog WHERE id=1`).Scan(&page.CatalogVersion); err != nil {
		return page, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id FROM df_skill_proposals p WHERE `+where+` ORDER BY id LIMIT ?`, args...)
	if err != nil {
		return page, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			break
		}
		ids = append(ids, id)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return page, err
	}
	bytesUsed := 0
	for _, id := range ids {
		if len(page.Proposals) == q.Limit {
			page.Next = encodeCursor(cursor)
			break
		}
		p, err := readProposal(ctx, tx, id)
		if err != nil {
			return page, err
		}
		if !checkTarget(allowed, p.Target) {
			return page, harness.ErrPermissionDenied
		}
		if p.Status == "pending_review" {
			p.Disposition, err = s.proposalDispositionTx(ctx, tx, id)
			if err != nil {
				return page, err
			}
		}
		p.Diff = "" // Detail is fetched explicitly; do not duplicate full candidate text in lists.
		data, err := json.Marshal(p)
		if err != nil {
			return page, err
		}
		if bytesUsed+len(data) > 2<<20 && len(page.Proposals) > 0 {
			page.Next = encodeCursor(cursor)
			break
		}
		bytesUsed += len(data)
		page.Proposals = append(page.Proposals, p)
		cursor.Key = id
	}
	return page, tx.Commit()
}

func (s *Service) QueryHistory(ctx context.Context, q harness.SkillQuery) (harness.SkillHistoryPage, error) {
	page := harness.SkillHistoryPage{Publications: make([]Publication, 0)}
	if q.Status != "" {
		return page, invalid("history does not accept a proposal status")
	}
	allowed, where, args, cursor, err := s.querySources(q, "history")
	if err != nil {
		return page, err
	}
	where += ` AND (source_key>? OR (source_key=? AND name>?) OR (source_key=? AND name=? AND revision>?))`
	args = append(args, cursor.Key, cursor.Key, cursor.Name, cursor.Key, cursor.Name, cursor.Revision, q.Limit+1)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return page, err
	}
	defer tx.Rollback()
	if err = tx.QueryRowContext(ctx, `SELECT revision FROM df_skill_catalog WHERE id=1`).Scan(&page.CatalogVersion); err != nil {
		return page, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT source_key,name,revision,payload FROM df_skill_publications WHERE `+where+` ORDER BY source_key,name,revision LIMIT ?`, args...)
	if err != nil {
		return page, err
	}
	bytesUsed := 0
	for rows.Next() {
		if len(page.Publications) == q.Limit {
			page.Next = encodeCursor(cursor)
			break
		}
		var source, name string
		var revision int64
		var data []byte
		if err = rows.Scan(&source, &name, &revision, &data); err != nil {
			break
		}
		target, ok := allowed[source]
		if !ok {
			err = harness.ErrPermissionDenied
			break
		}
		target.Name = name
		var p Publication
		p, err = decodePublication(data, target, revision)
		if err != nil {
			break
		}
		if bytesUsed+len(data) > 2<<20 && len(page.Publications) > 0 {
			page.Next = encodeCursor(cursor)
			break
		}
		bytesUsed += len(data)
		page.Publications = append(page.Publications, p)
		cursor.Key, cursor.Name, cursor.Revision = source, name, revision
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return page, err
	}
	for i := range page.Publications {
		p := &page.Publications[i]
		p.FileStatus, err = skills.FilePublicationStatus(ctx, tx, p.Target.SourceIdentity, p.Name, p.Head.StateVersion)
		if err != nil {
			return page, err
		}
	}
	return page, tx.Commit()
}

// Head also returns the current catalog epoch for coherent detail responses.
func (s *Service) Head(ctx context.Context, source, name string) (harness.SkillTarget, harness.SkillHead, int64, error) {
	var target harness.SkillTarget
	var head harness.SkillHead
	var revision int64
	err := s.registry.WithPublication(ctx, source, name, func(tx *skills.PublicationTx) error {
		target = tx.Target()
		var err error
		if head, err = tx.Current(); err != nil {
			return err
		}
		revision, err = tx.CatalogVersion()
		return err
	})
	return target, head, revision, err
}

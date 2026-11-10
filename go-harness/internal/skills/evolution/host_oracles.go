package evolution

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/learning"
	"github.com/omengye/deerflow-acp/go-harness/internal/session"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillbehavior"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillmaintenance"
	"io"
	"os"
	"strings"
	"time"
)

// captureTaskOracleTx runs at the native outcome boundary, including tasks
// without a skill. The real input, answer and authorization epoch are pinned.
func (s *Service) CaptureTaskOracleTx(ctx context.Context, tx *sql.Tx, run string) error {
	return captureTaskOracleAtCompletionTx(ctx, tx, run, true)
}
func captureTaskOracleTx(ctx context.Context, tx *sql.Tx, run string) error {
	return captureTaskOracleAtCompletionTx(ctx, tx, run, false)
}
func captureTaskOracleAtCompletionTx(ctx context.Context, tx *sql.Tx, run string, terminal bool) error {
	var root, session, workspace, status string
	var input []byte
	var epoch int64
	err := tx.QueryRowContext(ctx, `SELECT i.root_id,i.session_id,s.cwd,r.status,h.content,i.epoch FROM harness_learning_inputs i JOIN harness_runs r ON r.id=i.run_id JOIN harness_inputs h ON h.run_id=r.id JOIN harness_sessions s ON s.id=i.session_id JOIN harness_learning_controls c ON c.session_id=i.session_id AND c.epoch=i.epoch AND c.contribute=1 WHERE i.run_id=?`, run).Scan(&root, &session, &workspace, &status, &input, &epoch)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if status != "completed" {
		return nil
	}
	if historical, e := skillmaintenance.HistoricalRootTx(ctx, tx, root); e != nil {
		return e
	} else if historical {
		return nil
	}
	var parts []harness.Content
	if len(input) > 2<<20 || json.Unmarshal(input, &parts) != nil {
		return errCollectionEvidence
	}
	var text strings.Builder
	for _, p := range parts {
		if p.Type == "text" {
			text.WriteString(p.Text)
			text.WriteByte('\n')
		}
	}
	prompt, expected, ok := skillbehavior.ExactOutputRequirement(text.String())
	kind, path := "output_equals", ""
	if !ok && terminal {
		prompt, path, expected, ok = skillbehavior.ExactArtifactRequirement(text.String())
		kind = "file_equals"
	}
	if !ok || learning.ValidatePromotionInstructions(prompt+"\n"+expected) != nil {
		return nil
	}
	var saved bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM df_skill_task_oracles WHERE root_id=?)`, root).Scan(&saved); err != nil || saved {
		return err
	}

	rows, err := tx.QueryContext(ctx, `SELECT event FROM harness_events WHERE run_id=? ORDER BY sequence`, run)
	if err != nil {
		return err
	}
	var answer strings.Builder
	for rows.Next() {
		var raw []byte
		if err = rows.Scan(&raw); err != nil {
			break
		}
		var e harness.RunEvent
		if len(raw) > 2<<20 || json.Unmarshal(raw, &e) != nil {
			err = errCollectionEvidence
			break
		}
		if e.Kind == "tool_start" {
			answer.Reset()
		}
		if e.Kind == "text_delta" && e.AgentName == "deerflow" {
			answer.WriteString(e.Text)
		}
		if answer.Len() > 128<<10 {
			err = errCollectionEvidence
			break
		}
	}
	if err = errors.Join(err, rows.Err(), rows.Close()); err != nil {
		return err
	}
	actual := answer.String()
	artifactExists := true
	if kind == "file_equals" {
		// Capture now, after joined task I/O; a later collector must not read a
		// workspace that may have been changed by another task or by the user.
		directory, e := os.OpenRoot(workspace)
		if e != nil {
			return nil
		}
		defer directory.Close()
		file, e := directory.Open(path)
		if errors.Is(e, os.ErrNotExist) {
			actual = "file:absent"
			artifactExists = false
		} else {
			if e != nil {
				return nil
			}
			defer file.Close()
			info, e := file.Stat()
			if e != nil || !info.Mode().IsRegular() || info.Size() > 65536 {
				return nil
			}
			content, e := io.ReadAll(io.LimitReader(file, 65537))
			if e != nil || len(content) > 65536 {
				return nil
			}
			actual = string(content)
		}
	}
	passed := artifactExists && actual == expected
	_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO df_skill_task_oracles(root_id,run_id,session_id,workspace,epoch,prompt,expected,input_sha,output_sha,passed) VALUES(?,?,?,?,?,?,?,?,?,?)`, root, run, session, workspace, epoch, prompt, expected, hash(input), hash([]byte(actual)), passed)
	if err == nil {
		_, err = tx.ExecContext(ctx, `INSERT INTO df_skill_task_oracle_kinds VALUES(?,?,?)`, root, kind, path)
	}
	return err
}

// A newly captured independent task can supply a held-out case. Candidate
// generation receives only the discovery's selected roots, never this holdout.
func (s *Service) deriveBehaviorCasesTx(ctx context.Context, tx *sql.Tx, p Proposal, in *behaviorSnapshot) error {
	var job string
	if err := tx.QueryRowContext(ctx, `SELECT job_id FROM df_skill_generation_inputs WHERE proposal_id=?`, p.ID).Scan(&job); err != nil {
		if err == sql.ErrNoRows {
			return nil
		}
		return err
	}
	r, err := readGeneration(ctx, tx, job)
	if err != nil {
		return err
	}
	pattern, suiteKey := "", ""
	var origin *submittedTaskOrigin
	used := map[string]bool{}
	if r.input.DiscoveryID == "" {
		origin, err = s.submittedOriginTx(ctx, tx, p, r)
		if err != nil {
			return err
		}
		if origin == nil || !p.Base.Exists {
			return nil
		}
		if in.Submission != nil && *in.Submission != *origin {
			return errDiscoveryEvidence
		}
		in.Submission = origin
		suiteKey = "submitted/" + p.ID
	} else {
		d, _, _, e := readDiscovery(ctx, tx, r.input.DiscoveryID)
		if e != nil {
			return e
		}
		if len(d.Evidence) < 2 || d.Evidence[0].Experience == nil {
			return nil
		}
		for _, e := range d.Evidence {
			used[e.RootID] = true
		}
		pattern = d.Evidence[0].Experience.PatternID
		suiteKey = pattern
	}
	header := harness.SkillBehaviorSuite{Version: skillbehavior.TriggerVersion, ID: "auto_" + hash([]byte(suiteKey))[:20], SourceID: p.Target.SourceID, Name: p.Target.Name}
	staging := "derive_" + rand.Text()
	chain := ""
	count := 0
	add := func(c harness.SkillBehaviorCase) error {
		if err := skillbehavior.ValidateCase(c); err != nil {
			return err
		}
		raw, _ := json.Marshal(c)
		sha := hash(raw)
		chain = encodedHash([]string{chain, sha})
		if _, err := tx.ExecContext(ctx, `INSERT INTO df_skill_behavior_cases VALUES(?,?,?,?,?)`, staging, count, c.ID, raw, sha); err != nil {
			return err
		}
		count++
		return nil
	}
	heldout := false
	cursor, firstPrompt := "", ""
	if origin != nil {
		counts, e := walkSubmittedOraclesTx(ctx, tx, p, r, origin, func(o taskOracle, holdout bool) error {
			kind := "targeted"
			if holdout {
				kind = "regression"
			}
			if firstPrompt == "" {
				firstPrompt = o.prompt
			}
			return add(harness.SkillBehaviorCase{ID: "case_" + hash([]byte(o.root))[:20], Kind: kind, Prompt: o.prompt,
				Files: map[string]string{}, Fixtures: []harness.SkillBehaviorFixture{},
				Assertions:  []harness.SkillBehaviorAssertion{{ID: "exact", Kind: o.kind, Path: o.path, Expected: o.expected}},
				Repetitions: 2, Holdout: holdout, EvidenceRoot: o.root, EvidenceDigest: o.digest()})
		})
		if e != nil {
			return e
		}
		heldout = counts.Supporting >= 2 && counts.Holdout >= 1
	}
	for origin == nil {
		rows, err := tx.QueryContext(ctx, `SELECT DISTINCT o.root_id,o.prompt,o.expected,o.passed,o.input_sha,o.output_sha,o.epoch,o.workspace,r.created_at,COALESCE(k.kind,'output_equals'),COALESCE(k.path,'') FROM df_skill_task_oracles o LEFT JOIN df_skill_task_oracle_kinds k ON k.root_id=o.root_id JOIN harness_runs r ON r.id=o.run_id
 JOIN harness_learning_inputs i ON i.root_id=o.root_id JOIN df_learning_experiences e ON e.episode_id=i.episode_id
 JOIN df_learning_pattern_members m ON m.experience_id=e.id JOIN harness_learning_controls c ON c.session_id=o.session_id AND c.epoch=o.epoch AND c.contribute=1
 WHERE m.pattern_id=? AND o.root_id>? ORDER BY o.root_id LIMIT 32`, pattern, cursor)
		if err != nil {
			return err
		}
		type oracle struct {
			root, prompt, expected, input, output, workspace, created, kind, path string
			epoch                                                                 int64
			passed                                                                bool
		}
		page := []oracle{}
		for rows.Next() {
			var v oracle
			if err = rows.Scan(&v.root, &v.prompt, &v.expected, &v.passed, &v.input, &v.output, &v.epoch, &v.workspace, &v.created, &v.kind, &v.path); err != nil {
				break
			}
			page = append(page, v)
		}
		if err = errors.Join(err, rows.Err(), rows.Close()); err != nil {
			return err
		}
		for _, v := range page {
			at, e := time.Parse(time.RFC3339Nano, v.created)
			holdout := !used[v.root] && e == nil && at.After(r.created)
			kind := "targeted"
			if holdout {
				kind = "regression"
				heldout = true
			}
			if firstPrompt == "" {
				firstPrompt = v.prompt
			}
			err = add(harness.SkillBehaviorCase{ID: "case_" + hash([]byte(v.root))[:20], Kind: kind, Prompt: v.prompt, Files: map[string]string{}, Fixtures: []harness.SkillBehaviorFixture{}, Assertions: []harness.SkillBehaviorAssertion{{ID: "exact", Kind: v.kind, Path: v.path, Expected: v.expected}}, Repetitions: 2, Holdout: holdout, EvidenceRoot: v.root, EvidenceDigest: encodedHash([]any{v.root, v.prompt, v.expected, v.passed, v.input, v.output, v.epoch, v.workspace, v.kind, v.path})})
			if err != nil {
				return err
			}
			cursor = v.root
		}
		if len(page) < 32 {
			break
		}
	}
	if !heldout || count < 3 {
		_, err = tx.ExecContext(ctx, `DELETE FROM df_skill_behavior_cases WHERE suite_digest=?`, staging)
		return err
	}
	page := 0
	items := []harness.SkillBehaviorTriggerSkill{}
	addTriggers := func() error {
		if err := add(harness.SkillBehaviorCase{ID: fmt.Sprintf("positive_%d", page), Kind: "trigger_positive", Prompt: firstPrompt, Repetitions: 2, TriggerSkills: items, Assertions: []harness.SkillBehaviorAssertion{{ID: "select", Kind: "skill_selected", Count: 1}}}); err != nil {
			return err
		}
		negative := []string{"Reply with only a friendly hello. No workflow, tools or specialized task is requested."}
		for _, item := range items {
			negative = append(negative, "Use the specialized workflow for this task: "+item.Description)
		}
		for n, prompt := range negative {
			if err := add(harness.SkillBehaviorCase{ID: fmt.Sprintf("negative_%d_%d", page, n), Kind: "trigger_negative", Prompt: prompt, Repetitions: 2, TriggerSkills: items, Assertions: []harness.SkillBehaviorAssertion{{ID: "no_select", Kind: "skill_selected", Count: 0}}}); err != nil {
				return err
			}
		}
		items = nil
		page++
		return nil
	}
	catalog, err := s.walkBehaviorCompetitorsTx(ctx, tx, p, func(item harness.SkillBehaviorTriggerSkill) error {
		items = append(items, item)
		if len(items) == 8 {
			return addTriggers()
		}
		return nil
	})
	if err != nil {
		return err
	}
	if len(items) > 0 || page == 0 {
		if err = addTriggers(); err != nil {
			return err
		}
	}
	digest := encodedHash([]any{"derived-suite-manifest-v2", header, chain, count})
	if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO df_skill_behavior_cases SELECT ?,ordinal,case_id,payload,payload_sha FROM df_skill_behavior_cases WHERE suite_digest=?`, digest, staging); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM df_skill_behavior_cases WHERE suite_digest=?`, staging); err != nil {
		return err
	}
	in.Suite, in.SuiteDigest, in.CaseRoot, in.CaseCount, in.Derived, in.CompetitorsDigest = &header, digest, chain, count, true, catalog
	return nil
}

func (s *Service) walkBehaviorCompetitorsTx(ctx context.Context, tx *sql.Tx, p Proposal, visit func(harness.SkillBehaviorTriggerSkill) error) (string, error) {
	workspace, err := s.automaticWorkspace(ctx, tx, p)
	if err != nil {
		return "", err
	}
	sum := sha256.New()
	sum.Write([]byte("["))
	count := 0
	cursor := ""
	for {
		page, next, done, err := s.behavior.access.InventoryPageTx(ctx, tx, workspace, cursor)
		if err != nil {
			return "", err
		}
		for _, entry := range page.Skills {
			if entry.Target.Name == p.Target.Name {
				continue
			}
			item := harness.SkillBehaviorTriggerSkill{Name: entry.Target.Name, Description: entry.Description}
			if count > 0 {
				sum.Write([]byte(","))
			}
			raw, _ := json.Marshal(item)
			sum.Write(raw)
			count++
			if visit != nil {
				if err = visit(item); err != nil {
					return "", err
				}
			}
		}
		if done {
			break
		}
		cursor = next
	}
	sum.Write([]byte("]"))
	return hex.EncodeToString(sum.Sum(nil)), nil
}
func (s *Service) behaviorCompetitorsTx(ctx context.Context, tx *sql.Tx, p Proposal) ([]harness.SkillBehaviorTriggerSkill, string, error) {
	digest, err := s.walkBehaviorCompetitorsTx(ctx, tx, p, nil)
	return nil, digest, err
}

func (s *Service) validateDerivedSuiteTx(ctx context.Context, tx *sql.Tx, p Proposal, in behaviorSnapshot) error {
	if in.Suite == nil {
		return skillmaintenance.ErrBlockedEvidence
	}
	workspace, err := s.automaticWorkspace(ctx, tx, p)
	if err != nil {
		return err
	}
	_, catalog, err := s.behaviorCompetitorsTx(ctx, tx, p)
	if err != nil {
		return err
	}
	if catalog != in.CompetitorsDigest {
		return errGenerationConfiguration
	}
	if in.Submission != nil {
		origin, e := submissionOriginTx(ctx, tx, p, workspace)
		if e != nil {
			return e
		}
		if origin == nil || *origin != *in.Submission {
			return errDiscoveryEvidence
		}
	}
	return walkBehaviorCases(ctx, tx, in, func(c harness.SkillBehaviorCase) error {
		if c.EvidenceRoot == "" {
			return nil
		}
		var prompt, expected, inputSHA, outputSHA, scope, assertionKind, artifactPath string
		var epoch int64
		var passed bool
		err = tx.QueryRowContext(ctx, `SELECT o.prompt,o.expected,o.passed,o.input_sha,o.output_sha,o.epoch,o.workspace,COALESCE(k.kind,'output_equals'),COALESCE(k.path,'') FROM df_skill_task_oracles o LEFT JOIN df_skill_task_oracle_kinds k ON k.root_id=o.root_id JOIN harness_learning_controls c ON c.session_id=o.session_id AND c.epoch=o.epoch AND c.contribute=1 JOIN harness_sessions s ON s.id=o.session_id AND s.cwd=o.workspace WHERE o.root_id=?`, c.EvidenceRoot).Scan(&prompt, &expected, &passed, &inputSHA, &outputSHA, &epoch, &scope, &assertionKind, &artifactPath)
		if errors.Is(err, sql.ErrNoRows) {
			return errDiscoveryEvidence
		}
		if err != nil {
			return err
		}
		if !session.SameWorkspace(scope, workspace) || encodedHash([]any{c.EvidenceRoot, prompt, expected, passed, inputSHA, outputSHA, epoch, scope, assertionKind, artifactPath}) != c.EvidenceDigest {
			return errDiscoveryEvidence
		}
		if historical, e := skillmaintenance.HistoricalRootTx(ctx, tx, c.EvidenceRoot); e != nil {
			return e
		} else if historical {
			return errDiscoveryEvidence
		}
		if in.Submission != nil {
			eligible, e := submittedOracleEligibleTx(ctx, tx, p, *in.Submission, c.EvidenceRoot, c.Holdout)
			if e != nil {
				return e
			}
			if !eligible {
				return errDiscoveryEvidence
			}
		}
		return nil
	})
}

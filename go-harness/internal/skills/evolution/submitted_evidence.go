package evolution

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/session"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillmaintenance"
)

// These are host records, never assertions or source IDs supplied in tool
// arguments. Only the input digest is retained; private chat text is not copied.
type submittedTaskOrigin struct {
	SessionID string `json:"session_id"`
	RunID     string `json:"run_id"`
	RootID    string `json:"root_id"`
	Workspace string `json:"workspace"`
	InputSHA  string `json:"input_sha"`
}

func submissionOriginTx(ctx context.Context, tx *sql.Tx, p Proposal, workspace string) (*submittedTaskOrigin, error) {
	if p.Trigger.Type != "explicit_agent_proposal" {
		return nil, nil // SDK candidates can still use host-owned suites.
	}
	o := &submittedTaskOrigin{RunID: p.Trigger.RunID}
	var input []byte
	err := tx.QueryRowContext(ctx, `SELECT r.session_id,s.cwd,m.root_id,h.content FROM harness_runs r
 JOIN harness_sessions s ON s.id=r.session_id JOIN harness_inputs h ON h.run_id=r.id
 JOIN budget_members m ON m.id=r.id AND m.session_id=r.session_id WHERE r.id=?`, o.RunID).Scan(&o.SessionID, &o.Workspace, &o.RootID, &input)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, harness.ErrPermissionDenied
	}
	if err != nil {
		return nil, err
	}
	if o.SessionID != p.Trigger.ThreadID || !session.SameWorkspace(o.Workspace, workspace) || len(input) > 2<<20 {
		return nil, harness.ErrPermissionDenied
	}
	historical, err := skillmaintenance.HistoricalRootTx(ctx, tx, o.RootID)
	if err != nil {
		return nil, err
	}
	if historical {
		return nil, harness.ErrPermissionDenied
	}
	o.InputSHA = hash(input)
	return o, nil
}

func (s *Service) submittedOriginTx(ctx context.Context, tx *sql.Tx, p Proposal, r generationRecord) (*submittedTaskOrigin, error) {
	if p.Trigger.Type != "explicit_agent_proposal" && r.input.Submission == nil {
		return nil, nil
	}
	workspace, err := s.automaticWorkspace(ctx, tx, p)
	if err != nil {
		return nil, err
	}
	o, err := submissionOriginTx(ctx, tx, p, workspace)
	if err != nil {
		return nil, err
	}
	if r.input.Submission != nil && (o == nil || *o != *r.input.Submission) {
		return nil, errDiscoveryEvidence
	}
	return o, nil
}

type taskOracle struct {
	root, prompt, expected, input, output, workspace, created, kind, path string
	epoch                                                                 int64
	passed                                                                bool
}

func (o taskOracle) digest() string {
	return encodedHash([]any{o.root, o.prompt, o.expected, o.passed, o.input, o.output, o.epoch, o.workspace, o.kind, o.path})
}

type submittedEvidenceCounts struct{ Supporting, Holdout int }

const submittedOracleFrom = ` FROM df_skill_task_oracles o LEFT JOIN df_skill_task_oracle_kinds k ON k.root_id=o.root_id
 JOIN harness_runs r ON r.id=o.run_id AND r.session_id=o.session_id AND r.status='completed'
 JOIN harness_sessions s ON s.id=o.session_id AND s.cwd=o.workspace
 JOIN harness_learning_inputs i ON i.run_id=o.run_id AND i.root_id=o.root_id AND i.session_id=o.session_id AND i.epoch=o.epoch AND i.input_sha=o.input_sha
 JOIN harness_learning_controls c ON c.session_id=o.session_id AND c.epoch=o.epoch AND c.contribute=1
 WHERE o.root_id<>? AND o.session_id=?
 AND EXISTS(SELECT 1 FROM df_skill_signals sig JOIN df_skill_signal_reads link ON link.signal_id=sig.id
 JOIN harness_skill_reads rd ON rd.id=link.read_id AND rd.kind='skill_load'
 JOIN harness_skill_outcomes outcome ON outcome.sequence=link.outcome_seq AND outcome.processed=1 AND outcome.effects_uncertain=0
 WHERE sig.root_id=o.root_id AND sig.source_key=? AND sig.name=? AND sig.version=?)
 AND NOT EXISTS(SELECT 1 FROM harness_skill_outcomes other_outcome JOIN budget_members member ON member.id=other_outcome.run_id
 WHERE member.root_id=o.root_id AND (other_outcome.effects_uncertain=1 OR other_outcome.status<>'completed'))
 AND NOT EXISTS(SELECT 1 FROM harness_skill_reads other WHERE other.root_id=o.root_id AND other.source_key=? AND other.name=? AND other.version<>?)`

func submittedOracleArgs(p Proposal, origin submittedTaskOrigin) []any {
	return []any{origin.RootID, origin.SessionID, p.Target.SourceIdentity, p.Target.Name, p.Base.Version, p.Target.SourceIdentity, p.Target.Name, p.Base.Version}
}

func submittedOracleEligibleTx(ctx context.Context, tx *sql.Tx, p Proposal, origin submittedTaskOrigin, root string, holdout bool) (bool, error) {
	args := append(submittedOracleArgs(p, origin), root)
	var created, submitted string
	err := tx.QueryRowContext(ctx, `SELECT r.created_at`+submittedOracleFrom+` AND o.root_id=?`, args...).Scan(&created)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !holdout {
		return true, nil
	}
	if err = tx.QueryRowContext(ctx, `SELECT created_at FROM df_skill_generation_inputs WHERE proposal_id=?`, p.ID).Scan(&submitted); err != nil {
		return false, err
	}
	at, err := time.Parse(time.RFC3339Nano, created)
	if err != nil {
		return false, errCollectionEvidence
	}
	cutoff, err := time.Parse(time.RFC3339Nano, submitted)
	if err != nil {
		return false, errCollectionEvidence
	}
	return at.After(cutoff), nil
}

// A completed native skill read binds each oracle to the exact target and
// baseline within the submitting conversation. Merely sharing a workspace,
// naming a skill in chat or retrying the same root cannot qualify. This path
// does not require a discovery/model job or a cross-session sharing grant.
func walkSubmittedOraclesTx(ctx context.Context, tx *sql.Tx, p Proposal, r generationRecord, origin *submittedTaskOrigin, visit func(taskOracle, bool) error) (counts submittedEvidenceCounts, err error) {
	if origin == nil || !p.Base.Exists {
		return counts, nil
	}
	cursorTime, cursorRoot := "", ""
	for {
		args := append(submittedOracleArgs(p, *origin), cursorTime, cursorTime, cursorRoot)
		rows, e := tx.QueryContext(ctx, `SELECT o.root_id,o.prompt,o.expected,o.passed,o.input_sha,o.output_sha,o.epoch,o.workspace,r.created_at,COALESCE(k.kind,'output_equals'),COALESCE(k.path,'')`+submittedOracleFrom+`
 AND (r.created_at>? OR (r.created_at=? AND o.root_id>?)) ORDER BY r.created_at,o.root_id LIMIT 32`, args...)
		if e != nil {
			return counts, e
		}
		var page []taskOracle
		for rows.Next() {
			var o taskOracle
			if e = rows.Scan(&o.root, &o.prompt, &o.expected, &o.passed, &o.input, &o.output, &o.epoch, &o.workspace, &o.created, &o.kind, &o.path); e != nil {
				break
			}
			page = append(page, o)
		}
		if e = errors.Join(e, rows.Err(), rows.Close()); e != nil {
			return counts, e
		}
		for _, o := range page {
			cursorTime, cursorRoot = o.created, o.root
			if !session.SameWorkspace(o.workspace, origin.Workspace) {
				continue
			}
			historical, e := skillmaintenance.HistoricalRootTx(ctx, tx, o.root)
			if e != nil {
				return counts, e
			}
			if historical {
				continue
			}
			created, e := time.Parse(time.RFC3339Nano, o.created)
			if e != nil {
				return counts, errCollectionEvidence
			}
			// The first two independent tasks provide support, even when they
			// arrive after submission. A separate later task is held out from
			// authoring of these immutable candidate bytes.
			holdout := counts.Supporting >= 2 && created.After(r.created)
			if holdout {
				counts.Holdout++
			} else {
				counts.Supporting++
			}
			if visit != nil {
				if e = visit(o, holdout); e != nil {
					return counts, e
				}
			}
		}
		if len(page) < 32 {
			return counts, nil
		}
	}
}

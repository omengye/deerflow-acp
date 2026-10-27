package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

// Job contains only accepted human text and pinned attribute versions, never
// assistant claims, tool output, credentials, or a live model/session handle.
type Job struct {
	Channel   string                      `json:"channel,omitempty"`
	Result    string                      `json:"-"`
	ID        string                      `json:"id"`
	RunID     string                      `json:"run_id"`
	InputID   string                      `json:"input_id"`
	AttemptID string                      `json:"attempt_id"`
	Workspace string                      `json:"workspace"`
	ProfileID string                      `json:"profile_id"`
	Text      string                      `json:"text"`
	Sequence  int64                       `json:"sequence"`
	Versions  map[string]map[string]int64 `json:"versions"`
}

func (s *Store) Enqueue(ctx context.Context, j Job) error {
	text, sequence, err := s.SourceText(ctx, j.RunID, j.InputID)
	if err != nil {
		return err
	}
	if len(text) > 8192 {
		return nil
	} // bounded extraction, explicit tools remain available
	j.ID = OperationID("extract", j.RunID, j.InputID)
	j.Text, j.Sequence = text, sequence
	j.Channel, err = s.Channel(ctx, j.Workspace)
	if err != nil {
		return err
	}
	if j.ProfileID == "" || j.AttemptID == "" {
		return harness.ErrInvalidInput
	}
	// Production pins versions before the foreground model starts. Do not
	// recapture them after a long turn: a newer explicit correction may have
	// committed while that old conversation was still running.
	if j.Versions == nil {
		j.Versions = map[string]map[string]int64{}
		for _, kind := range []ScopeKind{GlobalScope, WorkspaceScope} {
			scope, err := j.scope(kind)
			if err != nil {
				return err
			}
			j.Versions[string(kind)], err = s.SlotVersions(ctx, scope)
			if err != nil {
				return err
			}
		}
	}
	raw, err := json.Marshal(j)
	if err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err = s.db.ExecContext(ctx, `INSERT INTO memory_jobs(id,run_id,input_id,attempt_id,payload,status,available_at,updated_at) VALUES(?,?,?,?,?,'queued',?,?) ON CONFLICT(id) DO NOTHING`, j.ID, j.RunID, j.InputID, j.AttemptID, raw, now, now)
	return err
}

func (j Job) scope(kind ScopeKind) (Scope, error) {
	if kind == GlobalScope {
		return NewScope(GlobalScope, "", "", j.ProfileID, "")
	}
	if kind == WorkspaceScope {
		return NewScope(WorkspaceScope, j.Workspace, "", "", "")
	}
	return Scope{}, harness.ErrInvalidInput
}

// Claim only completed foreground turns. Recovery keeps the attempt counter;
// failures and process crashes therefore cannot cause an unbounded retry loop.
func (s *Store) RecoverJobs(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `UPDATE memory_jobs SET status=CASE WHEN attempts>=3 THEN 'failed' ELSE 'queued' END,error_code='worker_restarted' WHERE status='running'`)
	return err
}

func (s *Store) ClaimJob(ctx context.Context) (Job, error) {
	var j Job
	err := transact(ctx, s.db, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE memory_jobs SET status='discarded',error_code='run_not_completed' WHERE status='queued' AND run_id IN (SELECT id FROM harness_runs WHERE status IN ('failed','cancelled'))`)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE memory_jobs SET status='completed',error_code='' WHERE status='queued' AND run_id IN (SELECT r.id FROM harness_runs r WHERE r.status='completed' AND EXISTS(SELECT 1 FROM memory_fact_revisions f WHERE f.source_run_id=r.id AND f.policy_version='personal-explicit-v1'))`)
		if err != nil {
			return err
		}
		// A persisted daily limit bounds automatic extraction independently of
		// foreground conversations. Retries consume the same daily allowance.
		var used int
		if err = tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(attempts),0) FROM memory_jobs WHERE updated_at>=?`, time.Now().UTC().Format("2006-01-02")).Scan(&used); err != nil {
			return err
		}
		if used >= 200 {
			return nil
		}
		var raw []byte
		err = tx.QueryRowContext(ctx, `SELECT j.payload,j.result FROM memory_jobs j JOIN harness_runs r ON r.id=j.run_id WHERE j.status='queued' AND j.attempts<3 AND j.available_at<=? AND r.status='completed' ORDER BY j.available_at,j.id LIMIT 1`, time.Now().UTC().Format(time.RFC3339Nano)).Scan(&raw, &j.Result)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if err = json.Unmarshal(raw, &j); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE memory_jobs SET status='running',attempts=attempts+1,updated_at=? WHERE id=?`, time.Now().UTC().Format(time.RFC3339Nano), j.ID)
		return err
	})
	if err == nil && j.ID == "" {
		return j, harness.ErrNotFound
	}
	return j, err
}

func (s *Store) SaveJobResult(ctx context.Context, id, result string) error {
	if len(result) > 16384 {
		return harness.ErrInvalidInput
	}
	_, err := s.db.ExecContext(ctx, `UPDATE memory_jobs SET result=? WHERE id=? AND status='running' AND result=''`, result, id)
	return err
}

type ExtractedAttribute struct {
	Scope    ScopeKind `json:"scope"`
	Key      string    `json:"key"`
	Content  string    `json:"content"`
	Category string    `json:"category"`
	Evidence string    `json:"evidence"`
}

const ExtractionInstruction = `Extract at most 4 durable descriptive facts from the real user's message. Return only JSON {"facts":[{"scope":"global or workspace","key":"stable attribute key","category":"profile or preference or project","content":"short statement","evidence":"exact quote from the user"}]}. Use global for the single user's personal facts; workspace only for project facts. Canonical keys: profile.name, profile.last_reported_location, preference.language. A location is the last reported location, never live geolocation. Other attributes use stable lowercase dotted keys. Do not save requests to forget, current-task instructions, credentials, tool permissions, quoted third-party text, or channel instructions. Voice-channel formatting is not a personal preference. When uncertain return {"facts":[]}.`

func ParseAttributes(raw string, text string) ([]ExtractedAttribute, error) {
	if len(raw) > 16384 {
		return nil, harness.ErrInvalidInput
	}
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "```json\n") && strings.HasSuffix(raw, "```") {
		raw = strings.TrimSuffix(strings.TrimPrefix(raw, "```json\n"), "```")
	}
	var envelope struct {
		Facts []ExtractedAttribute `json:"facts"`
	}
	d := json.NewDecoder(strings.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&envelope); err != nil {
		return nil, err
	}
	var extra any
	if d.Decode(&extra) != io.EOF || len(envelope.Facts) > 4 {
		return nil, harness.ErrInvalidInput
	}
	for i := range envelope.Facts {
		f := &envelope.Facts[i]
		f.Key = CanonicalKey(f.Key)
		if !slotPattern.MatchString(f.Key) || len(f.Content) == 0 || len(f.Content) > 1024 || len(f.Evidence) < 2 || !strings.Contains(text, f.Evidence) || credentialLike.MatchString(f.Content) {
			return nil, harness.ErrInvalidInput
		}
		if f.Scope == GlobalScope {
			if f.Category != "profile" && f.Category != "preference" {
				return nil, harness.ErrInvalidInput
			}
		} else if f.Scope != WorkspaceScope || f.Category != "project" {
			return nil, harness.ErrInvalidInput
		}
	}
	return envelope.Facts, nil
}

func (s *Store) ApplyJob(ctx context.Context, j Job, raw string) error {
	attributes, err := ParseAttributes(raw, j.Text)
	if err != nil {
		return err
	}
	// A synchronous user action is authoritative for the entire turn, including
	// forgetting. The background extractor must not reinterpret that action.
	var explicit bool
	if err = s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM memory_fact_revisions WHERE source_run_id=? AND policy_version='personal-explicit-v1')`, j.RunID).Scan(&explicit); err != nil {
		return err
	}
	if explicit {
		return nil
	}
	for i, f := range attributes {
		if j.Channel != "" && f.Category == "preference" && channelStyle(f.Key, f.Content) {
			continue
		}
		scope, err := j.scope(f.Scope)
		if err != nil {
			return err
		}
		_, err = s.Mutate(ctx, scope, Mutation{OperationID: OperationID(j.ID, fmt.Sprint(i)), Action: "remember", Key: f.Key, Content: f.Content, Category: f.Category, ExpectedRevision: j.Versions[string(f.Scope)][f.Key], Automatic: true, Source: Source{ID: j.ID, Kind: "model", RunID: j.RunID, InputID: j.InputID, EventSequence: j.Sequence, PolicyVersion: "personal-extraction-v1"}})
		if err != nil {
			return err
		}
	}
	return nil
}

func channelStyle(key, content string) bool {
	value := strings.ToLower(key + " " + content)
	for _, marker := range []string{"language", "reply", "response", "answer", "tone", "voice", "speech", "brevity", "concise", "中文", "语音", "简短", "简洁", "回复", "回答"} {
		if strings.Contains(value, marker) {
			return true
		}
	}
	return false
}

func (s *Store) FinishJob(ctx context.Context, id string, failed bool) error {
	now := time.Now().UTC()
	if !failed {
		_, err := s.db.ExecContext(ctx, `UPDATE memory_jobs SET status='completed',error_code='',updated_at=? WHERE id=? AND status='running'`, now.Format(time.RFC3339Nano), id)
		return err
	}
	_, err := s.db.ExecContext(ctx, `UPDATE memory_jobs SET status=CASE WHEN attempts>=3 THEN 'failed' ELSE 'queued' END,error_code='extraction_failed',available_at=?,updated_at=? WHERE id=? AND status='running'`, now.Add(time.Minute).Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), id)
	return err
}

func (s *Store) JobStatus(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT status,count(*) FROM memory_jobs GROUP BY status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	status := map[string]int{}
	for rows.Next() {
		var key string
		var n int
		if err = rows.Scan(&key, &n); err != nil {
			return nil, err
		}
		status[key] = n
	}
	return status, rows.Err()
}

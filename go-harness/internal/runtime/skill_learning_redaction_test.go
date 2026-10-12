package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestSkillLearningRedactionRehashedLegacyEnvelopeCannotAuthorizeExtraction(t *testing.T) {
	s, x, extractor := newLearningTest(t, true)
	ctx := context.Background()
	request := learningTestRun(t, s, x, "Inspect complete local JSON configuration")
	envelope, err := json.Marshal(map[string]any{"content": `{"verified":true,"token":"LEGACY_PRIVATE_TOKEN"}`, "truncated": false, "next_offset": nil})
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"tool_start", "tool_execute", "tool_end"} {
		event := receiptEvent(request, kind, "configured-read", "read_file")
		if kind == "tool_start" {
			event.Arguments = []byte(`{"path":"configuration.json"}`)
		}
		if kind == "tool_end" {
			event.Content = []harness.Content{{Type: "text", Text: string(envelope)}}
		}
		if _, err = s.Store.Append(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	learningTestFinish(t, s, request, "end_turn")
	learningTestQueue(t, s)
	var original []byte
	var extraction, job string
	if err = s.Store.db.QueryRow(`SELECT id,job_id,payload FROM df_learning_extractions`).Scan(&extraction, &job, &original); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(original), "LEGACY_PRIVATE_TOKEN") || !strings.Contains(string(original), "[redacted]") {
		t.Fatal("queued extraction persisted the raw configured-tool secret")
	}
	var snapshot struct {
		View harness.SkillLearningInput `json:"view"`
	}
	if err = json.Unmarshal(original, &snapshot); err != nil {
		t.Fatal(err)
	}
	found := false
	for index, reference := range snapshot.View.References {
		if reference.Kind == "tool_observation" && reference.ToolName == "read_file" {
			snapshot.View.References[index].Text = "Arguments: {\"path\":\"configuration.json\"}\nObserved result: " + string(envelope)
			found = true
		}
	}
	if !found {
		t.Fatal("missing configured-tool observation in the actual capture")
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(original, &fields); err != nil {
		t.Fatal(err)
	}
	fields["view"], err = json.Marshal(snapshot.View)
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	// An old view cannot regain authority by updating the snapshot and job
	// hashes. Admission reconstructs the redacted view from accepted sources.
	tx, err := s.Store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	sha := fmt.Sprintf("%x", sha256.Sum256(legacy))
	if _, err = tx.ExecContext(ctx, `UPDATE df_learning_extractions SET payload=?,payload_sha=? WHERE id=?`, legacy, sha, extraction); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(ctx, `UPDATE df_skill_maintenance_jobs SET input_digest=? WHERE id=?`, sha, job); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if worked, err := s.Store.Learning.RunOne(ctx); err != nil || !worked || len(extractor.seen) != 0 {
		t.Fatal("rehashing the legacy unsafe capture authorized model I/O", worked, err, len(extractor.seen))
	}
	var state, reason string
	var calls int
	if err = s.Store.db.QueryRow(`SELECT state,error_code FROM df_skill_maintenance_jobs WHERE id=?`, job).Scan(&state, &reason); err != nil || state != "failed" || reason != "evidence_changed" {
		t.Fatal("unsafe capture did not fail closed", state, reason, err)
	}
	if err = s.Store.db.QueryRow(`SELECT count(*) FROM df_skill_maintenance_calls WHERE job_id=?`, job).Scan(&calls); err != nil || calls != 0 {
		t.Fatal("rejected legacy capture consumed model quota", calls, err)
	}
	if learningCount(t, s.Store, "df_learning_experiences") != 0 {
		t.Fatal("unsafe legacy capture produced experience")
	}
}

func TestSkillLearningRedactionHiddenTextCaptureAndLegacyView(t *testing.T) {
	for _, tamper := range []bool{false, true} {
		name := "current-capture"
		if tamper {
			name = "rehashed-legacy"
		}
		t.Run(name, func(t *testing.T) {
			s, x, extractor := newLearningTest(t, true)
			ctx := context.Background()
			request := learningTestRun(t, s, x, "Inspect complete local input; to\u200bken=HIDDEN_CAPTURE_USER")
			if _, err := s.Store.Append(ctx, harness.RunEvent{SessionID: x.ID, RunID: request.RunID, Kind: "text_delta", AgentName: "deerflow", Text: "Inspect complete local input; to\u200bken=HIDDEN_CAPTURE_ASSISTANT"}); err != nil {
				t.Fatal(err)
			}
			result := "verified=true; api_\u2060key=HIDDEN_CAPTURE_RESULT"
			for _, kind := range []string{"tool_start", "tool_execute", "tool_end"} {
				event := receiptEvent(request, kind, "hidden-text-read", "read_file")
				if kind == "tool_start" {
					event.Arguments = []byte(`{"path":"configuration.json"}`)
				}
				if kind == "tool_end" {
					event.Content = []harness.Content{{Type: "text", Text: result}}
				}
				if _, err := s.Store.Append(ctx, event); err != nil {
					t.Fatal(err)
				}
			}
			learningTestFinish(t, s, request, "end_turn")
			learningTestQueue(t, s)
			var payload []byte
			var extraction, job string
			if err := s.Store.db.QueryRow(`SELECT id,job_id,payload FROM df_learning_extractions`).Scan(&extraction, &job, &payload); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(payload), "HIDDEN_CAPTURE") || !strings.Contains(string(payload), "verified=true") || !strings.Contains(string(payload), "[redacted]") {
				t.Fatal("queued learning view leaked obfuscated source text or lost local evidence")
			}
			var snapshot struct {
				View harness.SkillLearningInput `json:"view"`
			}
			if err := json.Unmarshal(payload, &snapshot); err != nil {
				t.Fatal(err)
			}
			if tamper {
				found := false
				for index, ref := range snapshot.View.References {
					if ref.Kind == "tool_observation" && ref.ToolName == "read_file" {
						// This is the old filter-then-normalize output. Rehashing it
						// cannot authorize a call under the reconstructed current view.
						snapshot.View.References[index].Text = "Arguments: {\"path\":\"configuration.json\"}\nObserved result: verified=true; api_key=HIDDEN_CAPTURE_RESULT"
						found = true
					}
				}
				if !found {
					t.Fatal("missing local tool observation")
				}
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(payload, &fields); err != nil {
					t.Fatal(err)
				}
				view, err := json.Marshal(snapshot.View)
				if err != nil {
					t.Fatal(err)
				}
				fields["view"] = view
				legacy, err := json.Marshal(fields)
				if err != nil {
					t.Fatal(err)
				}
				tx, err := s.Store.db.BeginTx(ctx, nil)
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback()
				sha := fmt.Sprintf("%x", sha256.Sum256(legacy))
				if _, err = tx.ExecContext(ctx, `UPDATE df_learning_extractions SET payload=?,payload_sha=? WHERE id=?`, legacy, sha, extraction); err != nil {
					t.Fatal(err)
				}
				if _, err = tx.ExecContext(ctx, `UPDATE df_skill_maintenance_jobs SET input_digest=? WHERE id=?`, sha, job); err != nil {
					t.Fatal(err)
				}
				if err = tx.Commit(); err != nil {
					t.Fatal(err)
				}
			}
			if worked, err := s.Store.Learning.RunOne(ctx); err != nil || !worked {
				t.Fatal("captured view did not reach a terminal decision", worked, err)
			}
			var state, reason string
			var calls int
			if err := s.Store.db.QueryRow(`SELECT state,error_code FROM df_skill_maintenance_jobs WHERE id=?`, job).Scan(&state, &reason); err != nil {
				t.Fatal(err)
			}
			if err := s.Store.db.QueryRow(`SELECT count(*) FROM df_skill_maintenance_calls WHERE job_id=?`, job).Scan(&calls); err != nil {
				t.Fatal(err)
			}
			if tamper {
				if state != "failed" || reason != "evidence_changed" || calls != 0 || len(extractor.seen) != 0 || learningCount(t, s.Store, "df_learning_experiences") != 0 {
					t.Fatal("legacy obfuscated-secret capture authorized a call or result", state, reason, calls)
				}
				return
			}
			if state != "completed" || reason != "" || calls != 1 || len(extractor.seen) != 1 {
				t.Fatal("current sanitized capture could not be learned", state, reason, calls)
			}
			view, err := json.Marshal(extractor.seen[0])
			if err != nil || strings.Contains(string(view), "HIDDEN_CAPTURE") {
				t.Fatal("extractor received an obfuscated source secret", err)
			}
			experience := sourceExperience(t, s, x)
			source, err := s.Store.Learning.Source(ctx, x.ID, sourceExperienceRequest(experience))
			encoded, encodeErr := json.Marshal(source)
			if err != nil || encodeErr != nil || len(source.References) != 1 || !strings.Contains(string(encoded), "[redacted]") || strings.Contains(string(encoded), "HIDDEN_CAPTURE") {
				t.Fatal("cited source inspection retained unsafe text", err, encodeErr)
			}
		})
	}
}

package agent

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/acp/protocol"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillmaintenance"
)

const (
	getLearningControlMethod     = "_deerflow/learning/control/get"
	setLearningControlMethod     = "_deerflow/learning/control/set"
	listExperiencesMethod        = "_deerflow/learning/experiences"
	listExperiencePatternsMethod = "_deerflow/learning/patterns"
	listExperienceMatchesMethod  = "_deerflow/learning/matches"
	listLearningJobsMethod       = "_deerflow/learning/jobs"
	startLearningEpisodeMethod   = "_deerflow/learning/episode/start"
	cancelLearningMethod         = "_deerflow/learning/cancel"
)

func learningCapabilities() map[string]any {
	return map[string]any{"version": 1, "mode": "shadow", "getControlMethod": getLearningControlMethod, "setControlMethod": setLearningControlMethod, "workspaceSharingControl": true, "listMethod": listExperiencesMethod, "patternsMethod": listExperiencePatternsMethod, "matchesMethod": listExperienceMatchesMethod, "jobsMethod": listLearningJobsMethod, "startEpisodeMethod": startLearningEpisodeMethod, "cancelMethod": cancelLearningMethod}
}

func (a *Agent) learningRequest(ctx context.Context, method string, raw json.RawMessage) (any, error) {
	if a.service.Store.Learning == nil {
		return nil, rpcError(protocol.MethodNotFound, "Skill learning is not available")
	}
	invalid := rpcError(protocol.InvalidParams, "Invalid skill learning parameters")
	if len(raw) > 4096 || rejectDuplicateKeys(raw) != nil {
		return nil, invalid
	}
	keys := []string{"sessionId"}
	switch method {
	case getLearningControlMethod, startLearningEpisodeMethod:
	case setLearningControlMethod:
		keys = append(keys, "contribute", "expectedEpoch", "workspaceShared")
	case listExperiencesMethod, listLearningJobsMethod, listExperiencePatternsMethod, listExperienceMatchesMethod:
		keys = append(keys, "cursor", "limit")
	case cancelLearningMethod:
		keys = append(keys, "jobId", "expectedStateVersion")
	default:
		return nil, rpcError(protocol.MethodNotFound, "Method not found")
	}
	fields, err := strictObject(raw, keys...)
	var req struct {
		Session    string `json:"sessionId"`
		Contribute bool   `json:"contribute"`
		Epoch      int64  `json:"expectedEpoch"`
		Shared     *bool  `json:"workspaceShared"`
		Cursor     string `json:"cursor"`
		Limit      int    `json:"limit"`
		Job        string `json:"jobId"`
		Version    int64  `json:"expectedStateVersion"`
	}
	if err != nil || json.Unmarshal(raw, &req) != nil || !validID(req.Session, 256) {
		return nil, invalid
	}
	var result any
	switch method {
	case getLearningControlMethod:
		result, err = a.service.SkillLearningControl(ctx, a.owner, req.Session)
	case setLearningControlMethod:
		// strictObject rejects nulls, including whitespace-padded null values.
		if fields["contribute"] == nil || fields["expectedEpoch"] == nil || req.Epoch < 0 {
			return nil, invalid
		}
		result, err = a.service.SetSkillLearningControl(ctx, a.owner, req.Session, harness.SkillLearningControlRequest{Contribute: req.Contribute, ExpectedEpoch: req.Epoch, WorkspaceShared: req.Shared})
	case listExperiencesMethod, listLearningJobsMethod, listExperiencePatternsMethod, listExperienceMatchesMethod:
		if len(req.Cursor) > 128 || req.Limit < 0 || req.Limit > 100 {
			return nil, invalid
		}
		if req.Limit == 0 {
			req.Limit = 50
		}
		if method == listLearningJobsMethod {
			result, err = a.service.SkillLearningJobs(ctx, a.owner, req.Session, req.Cursor, req.Limit)
		} else if method == listExperiencePatternsMethod {
			result, err = a.service.SkillExperiencePatterns(ctx, a.owner, req.Session, req.Cursor, req.Limit)
		} else if method == listExperienceMatchesMethod {
			result, err = a.service.SkillExperienceMatches(ctx, a.owner, req.Session, req.Cursor, req.Limit)
		} else {
			result, err = a.service.SkillExperiences(ctx, a.owner, req.Session, req.Cursor, req.Limit)
		}
	case startLearningEpisodeMethod:
		var id string
		id, err = a.service.StartSkillLearningEpisode(ctx, a.owner, req.Session)
		result = map[string]any{"episode_id": id}
	case cancelLearningMethod:
		if !validID(req.Job, 128) || req.Version < 1 {
			return nil, invalid
		}
		err = a.service.CancelSkillLearningExtraction(ctx, a.owner, req.Session, req.Job, req.Version)
		result = map[string]any{"cancelled": err == nil}
	}
	if errors.Is(err, harness.ErrSkillConflict) {
		return nil, rpcError(-32019, "Learning consent changed; refresh before retrying")
	}
	if errors.Is(err, skillmaintenance.ErrConflict) {
		return nil, rpcError(-32019, "Learning job changed; refresh before retrying")
	}
	return result, err
}

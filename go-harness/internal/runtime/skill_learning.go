package runtime

import (
	"context"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func (s *Service) learningSession(ctx context.Context, owner, sessionID string) (context.Context, func(), error) {
	if s.Store.Learning == nil {
		return ctx, nil, harness.ErrInvalidInput
	}
	if err := s.Store.requireForegroundSession(ctx, sessionID); err != nil {
		return ctx, nil, err
	}
	return s.Coordinator.Begin(ctx, sessionID, owner)
}

func (s *Service) SkillLearningControl(ctx context.Context, owner, sessionID string) (harness.SkillLearningControl, error) {
	ctx, done, err := s.learningSession(ctx, owner, sessionID)
	if err != nil {
		return harness.SkillLearningControl{}, err
	}
	defer done()
	return s.Store.Learning.Control(ctx, sessionID)
}

func (s *Service) SetSkillLearningControl(ctx context.Context, owner, sessionID string, request harness.SkillLearningControlRequest) (harness.SkillLearningControl, error) {
	ctx, done, err := s.learningSession(ctx, owner, sessionID)
	if err != nil {
		return harness.SkillLearningControl{}, err
	}
	defer done()
	return s.Store.Learning.SetControl(ctx, sessionID, request)
}

func (s *Service) SkillExperiences(ctx context.Context, owner, sessionID, after string, limit int) (harness.SkillExperiencePage, error) {
	ctx, done, err := s.learningSession(ctx, owner, sessionID)
	if err != nil {
		return harness.SkillExperiencePage{}, err
	}
	defer done()
	return s.Store.Learning.Experiences(ctx, sessionID, after, limit)
}

func (s *Service) SkillExperiencePatterns(ctx context.Context, owner, sessionID, after string, limit int) (harness.SkillExperiencePatternPage, error) {
	ctx, done, err := s.learningSession(ctx, owner, sessionID)
	if err != nil {
		return harness.SkillExperiencePatternPage{}, err
	}
	defer done()
	return s.Store.Learning.Patterns(ctx, sessionID, after, limit)
}

func (s *Service) SkillExperienceMatches(ctx context.Context, owner, sessionID, after string, limit int) (harness.SkillExperienceMatchPage, error) {
	ctx, done, err := s.learningSession(ctx, owner, sessionID)
	if err != nil {
		return harness.SkillExperienceMatchPage{}, err
	}
	defer done()
	return s.Store.Learning.Matches(ctx, sessionID, after, limit)
}

func (s *Service) SkillLearningJobs(ctx context.Context, owner, sessionID, after string, limit int) (harness.SkillLearningJobPage, error) {
	ctx, done, err := s.learningSession(ctx, owner, sessionID)
	if err != nil {
		return harness.SkillLearningJobPage{}, err
	}
	defer done()
	return s.Store.Learning.Jobs(ctx, sessionID, after, limit)
}

func (s *Service) StartSkillLearningEpisode(ctx context.Context, owner, sessionID string) (string, error) {
	ctx, done, err := s.learningSession(ctx, owner, sessionID)
	if err != nil {
		return "", err
	}
	defer done()
	return s.Store.Learning.StartEpisode(ctx, sessionID)
}

func (s *Service) CancelSkillLearningExtraction(ctx context.Context, owner, sessionID, jobID string, version int64) error {
	ctx, done, err := s.learningSession(ctx, owner, sessionID)
	if err != nil {
		return err
	}
	defer done()
	return s.Store.Learning.CancelExtraction(ctx, sessionID, jobID, version)
}

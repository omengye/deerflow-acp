package evolution

import (
	"context"
	"database/sql"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/internal/skillmaintenance"
)

type maintenanceProducer struct {
	hooks   skillmaintenance.Hooks
	handler skillmaintenance.Handler
}

func (s *Service) MaintenanceQueue() *skillmaintenance.Store {
	if s.generation == nil {
		return nil
	}
	return s.generation.store
}

// RegisterMaintenanceProducer binds a fixed, host-owned handler before workers
// start. No model, task, queue record or unknown legacy kind may register one.
func (s *Service) RegisterMaintenanceProducer(kind skillmaintenance.JobKind, hooks skillmaintenance.Hooks, handler skillmaintenance.Handler) error {
	if s.generation == nil || s.generation.lifecycle.done != nil || handler == nil || hooks.Admission == nil || hooks.CallAdmission == nil || hooks.Terminal == nil {
		return invalid("maintenance producer requires startup-only hooks")
	}
	switch kind {
	case skillmaintenance.ExperienceExtract, skillmaintenance.ExperienceConsolidate, skillmaintenance.SkillMatch:
	default:
		return invalid("unsupported maintenance producer")
	}
	if s.maintenanceProducers == nil {
		s.maintenanceProducers = map[skillmaintenance.JobKind]maintenanceProducer{}
	}
	if _, exists := s.maintenanceProducers[kind]; exists {
		return invalid("duplicate maintenance producer")
	}
	s.maintenanceProducers[kind] = maintenanceProducer{hooks: hooks, handler: handler}
	return nil
}

func (s *Service) admitMaintenanceCall(ctx context.Context, tx *sql.Tx, job skillmaintenance.Job, call skillmaintenance.ModelCall, now time.Time) error {
	if producer, exists := s.maintenanceProducers[job.Request.Kind]; exists {
		return producer.hooks.CallAdmission(ctx, tx, job, call, now)
	}
	return nil
}

func (s *Service) readyMaintenance(ctx context.Context, tx *sql.Tx, job skillmaintenance.Job) (bool, error) {
	if producer, exists := s.maintenanceProducers[job.Request.Kind]; exists && producer.hooks.Ready != nil {
		return producer.hooks.Ready(ctx, tx, job)
	}
	if job.Request.Kind == skillmaintenance.SkillGeneration && s.experienceSource != nil {
		return s.experienceSource.ReadyTx(ctx, tx, job)
	}
	return true, nil
}

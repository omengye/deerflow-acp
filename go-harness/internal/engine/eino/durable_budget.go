package eino

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	durablebudget "github.com/omengye/deerflow-acp/go-harness/internal/budget"
)

func (b *runBudget) classify(err error) error {
	var limit *durablebudget.LimitError
	if errors.As(err, &limit) && !limit.Temporary {
		b.mu.Lock()
		defer b.mu.Unlock()
		return b.fail(limit.Resource)
	}
	return err
}

func (b *runBudget) reserveDurable(ctx context.Context, estimate int64, encoded, tools []byte, common *model.Options, opts []model.Option) (*modelReservation, []model.Option, error) {
	output := b.limits.MaxOutputTokens
	if common.MaxTokens != nil && *common.MaxTokens > 0 && (output == 0 || *common.MaxTokens < output) {
		output = *common.MaxTokens
	}
	digest := sha256.New()
	digest.Write(encoded)
	digest.Write(tools)
	fmt.Fprintf(digest, "/%d/%d", estimate, output)
	grant, err := b.ledger.ReserveModel(ctx, b.scope, durablebudget.ModelRequest{OperationID: rand.Text(), Digest: fmt.Sprintf("%x", digest.Sum(nil)), InputTokens: estimate, MaxOutputTokens: output})
	if err != nil {
		return nil, nil, b.classify(err)
	}
	r := &modelReservation{budget: b, input: estimate, reserved: grant.HeldTokens, grant: &grant}
	first, err := b.ledger.MarkDispatched(ctx, grant)
	if err != nil || !first {
		if err == nil {
			err = durablebudget.ErrConflict
		}
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_, settleErr := r.settleContext(cleanup, true)
		return nil, nil, errors.Join(b.classify(err), settleErr)
	}
	if grant.MaxOutputTokens > 0 {
		opts = append(append([]model.Option(nil), opts...), model.WithMaxTokens(grant.MaxOutputTokens))
	}
	return r, opts, nil
}

func (b *runBudget) durableIdentity(ctx context.Context) (*durablebudget.Identity, error) {
	if b.ledger == nil {
		return nil, nil
	}
	s, err := b.ledger.Snapshot(ctx, b.scope.RootBudgetID)
	if err != nil {
		return nil, err
	}
	return &durablebudget.Identity{RootBudgetID: s.RootBudgetID, PolicyHash: s.PolicyHash, Revision: s.Revision}, nil
}

func (e *Engine) newBudget(ctx context.Context, req harness.RunRequest) (*runBudget, error) {
	b := &runBudget{limits: e.config.Budget, startedAt: time.Now()}
	if e.config.BudgetLedger == nil {
		return b, nil
	}
	scope, ok := durablebudget.ScopeFromContext(ctx)
	if !ok || scope.RootBudgetID != req.RootBudgetID || scope.MemberID != req.RunID || scope.SessionID != req.Session.ID {
		return nil, durablebudget.ErrScope
	}
	if err := e.config.BudgetLedger.Validate(ctx, scope); err != nil {
		return nil, err
	}
	s, err := e.config.BudgetLedger.Snapshot(ctx, scope.RootBudgetID)
	if err != nil {
		return nil, err
	}
	if s.Limits != e.config.Budget {
		return nil, fmt.Errorf("%w: engine budget policy differs from immutable root", durablebudget.ErrConflict)
	}
	b.ledger, b.scope, b.elapsed = e.config.BudgetLedger, scope, s.Elapsed
	return b, nil
}

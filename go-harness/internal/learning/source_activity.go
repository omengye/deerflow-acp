package learning

import (
	"context"
	"database/sql"
	"errors"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillmaintenance"
)

type sourceActivityKey struct{}

type sourceActivity struct {
	tx     *sql.Tx
	active bool
}

// Only pure evidence readers use this scope. They validate every pinned member
// before returning temporary activity, so activity cannot mask damaged evidence
// or lost authority. No model call or mutation may run inside the scope.
func withSourceActivity(ctx context.Context, tx *sql.Tx) (context.Context, func(*error)) {
	if scope, ok := ctx.Value(sourceActivityKey{}).(*sourceActivity); ok && scope.tx == tx {
		return ctx, func(*error) {}
	}
	scope := &sourceActivity{tx: tx}
	return context.WithValue(ctx, sourceActivityKey{}, scope), func(err *error) {
		if *err == nil && scope.active {
			*err = errors.Join(skillmaintenance.ErrSourceActive, harness.ErrPermissionDenied)
		}
	}
}

func sourceActiveTx(ctx context.Context, tx *sql.Tx) error {
	if scope, ok := ctx.Value(sourceActivityKey{}).(*sourceActivity); ok && scope.tx == tx {
		scope.active = true
		return nil
	}
	return errors.Join(skillmaintenance.ErrSourceActive, harness.ErrPermissionDenied)
}

func sourceActivityRecorded(ctx context.Context, tx *sql.Tx) bool {
	scope, ok := ctx.Value(sourceActivityKey{}).(*sourceActivity)
	return ok && scope.tx == tx && scope.active
}

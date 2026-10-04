package agents

import (
	"context"
	"database/sql"
	"encoding/json"
)

type ancestryQuery interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func ReadInvocation(ctx context.Context, q ancestryQuery, id, root string) (Invocation, error) {
	var in Invocation
	var data []byte
	err := q.QueryRowContext(ctx, `SELECT id,root_id,parent_id,depth,spec_json FROM harness_agent_invocations WHERE id=? AND root_id=?`, id, root).Scan(&in.ID, &in.RootID, &in.ParentID, &in.Depth, &data)
	if err == nil {
		err = json.Unmarshal(data, &in.Spec)
	}
	if err == nil {
		err = in.Spec.Validate()
	}
	return in, err
}

// IsDescendant includes the supplied ancestor itself. Each edge is constrained
// to the same durable root; UNION bounds reads even for a corrupt cyclic graph.
func IsDescendant(ctx context.Context, q ancestryQuery, root, ancestor, id string) (bool, error) {
	var result bool
	err := q.QueryRowContext(ctx, `WITH RECURSIVE roles(id) AS (
 SELECT id FROM harness_agent_invocations WHERE id=? AND root_id=?
 UNION SELECT i.id FROM harness_agent_invocations i JOIN roles r ON i.parent_id=r.id WHERE i.root_id=?
) SELECT EXISTS(SELECT 1 FROM roles WHERE id=?)`, ancestor, root, root, id).Scan(&result)
	return result, err
}

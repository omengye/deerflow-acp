package interaction

import "context"

// PermissionProvenance is set by the trusted engine at tool admission, never
// by the model or transport. It connects a paused permission to logical work.
type PermissionProvenance struct{ RootID, RoleID, OperationID string }
type permissionProvenanceKey struct{}

func WithPermissionProvenance(ctx context.Context, p PermissionProvenance) context.Context {
	return context.WithValue(ctx, permissionProvenanceKey{}, p)
}
func PermissionProvenanceFromContext(ctx context.Context) (PermissionProvenance, bool) {
	p, ok := ctx.Value(permissionProvenanceKey{}).(PermissionProvenance)
	return p, ok
}

// This token only selects a stopped native branch. It is never a tool grant.
type RoleCancellationResume struct {
	IntentID      string
	IntentVersion int64
	RoleID        string
}
type RoleCancellationBroker interface {
	CancelStoppedRole(context.Context, string) error
}

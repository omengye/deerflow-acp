package skillmaintenance

import "context"

type migrationIdentityKey struct{}

func WithMigrationID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, migrationIdentityKey{}, id)
}

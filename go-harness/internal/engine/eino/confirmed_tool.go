package eino

import (
	"context"
	"errors"
	"github.com/omengye/deerflow-acp/go-harness/harness"
)

type confirmedToolKey struct{}
type confirmedTool struct{ name, arguments string }

// ConfirmedToolArguments is host evidence set only after permission/admission
// for these exact arguments. A model-provided JSON field cannot create it.
func ConfirmedToolArguments(ctx context.Context, name, arguments string) bool {
	value, ok := ctx.Value(confirmedToolKey{}).(confirmedTool)
	return ok && value.name == name && value.arguments == arguments
}

func safePreparationError(err error) string {
	switch {
	case errors.Is(err, harness.ErrPermissionDenied):
		return "Target or operation is not authorized."
	case errors.Is(err, harness.ErrSkillConflict):
		return "The installed baseline changed; start a new ordinary turn."
	case errors.Is(err, harness.ErrInvalidInput):
		return "Invalid edit arguments, file format, match count or credential-bearing content."
	case errors.Is(err, harness.ErrNotFound):
		return "Installed skill or edit ID not found."
	default:
		return "Host could not verify the request; no application was attempted."
	}
}

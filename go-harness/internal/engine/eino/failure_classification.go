package eino

import (
	"context"
	"errors"
	"net"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/budget"
)

func toolFailureCode(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, harness.ErrCommandUncertain) {
		return "uncertain"
	}
	if errors.Is(err, context.Canceled) {
		return "cancelled"
	}
	if errors.Is(err, harness.ErrPermissionDenied) {
		return "permission"
	}
	var quota *budget.LimitError
	if errors.As(err, &quota) {
		return "quota"
	}
	var network net.Error
	if errors.As(err, &network) {
		return "network"
	}
	return "unknown"
}

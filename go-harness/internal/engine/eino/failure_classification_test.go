package eino

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"testing"
)

func TestFilesystemFailureClassification(t *testing.T) {
	permission := &fs.PathError{Op: "removeat", Path: "server_key", Err: fs.ErrPermission}
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"permission", permission, "permission"},
		{"wrapped_permission", fmt.Errorf("tool failed: %w", permission), "permission"},
		{"cancelled_permission", errors.Join(permission, context.Canceled), "cancelled"},
		{"missing_file", &fs.PathError{Op: "open", Path: "missing", Err: fs.ErrNotExist}, "unknown"},
		{"rename", &os.LinkError{Op: "rename", Old: "a", New: "b", Err: fs.ErrNotExist}, "unknown"},
		{"network", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}, "network"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := toolFailureCode(tc.err); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
			if certifiedToolOutcome(tc.err) {
				t.Fatal("classification must not certify execution effects")
			}
		})
	}
}

//go:build !windows

package channels

// Unix deployments must keep the harness data directory private (0700), just
// like other host credentials. Windows uses user-bound DPAPI instead.
func sealCredentials(raw []byte) ([]byte, error) { return raw, nil }
func openCredentials(raw []byte) ([]byte, error) { return raw, nil }

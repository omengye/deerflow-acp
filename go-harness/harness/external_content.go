package harness

import "strings"

// ExternalContentTool retains the existing network-content classification used
// by skill-learning admission. It is not a general trust boundary and must not
// authorize memory writes. Changing that admission policy is a separate change.
func ExternalContentTool(name string) bool {
	switch name {
	case "web_fetch", "web_search", "host_opencli", "read_attachment":
		return true
	}
	return strings.HasPrefix(name, "mcp_")
}

// UntrustedMemoryContentTool is the memory provenance boundary. Unknown tools
// and local outputs are untrusted by default, independent of the legacy helper.
func UntrustedMemoryContentTool(name string) bool {
	// There is no host-certified free-text-free exception today. Local files,
	// memory, notes, failures, and unknown tools may all carry external data.
	return true
}

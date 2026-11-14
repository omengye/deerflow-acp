package harness

import "strings"

// ExternalContentTool reports whether a tool returns third-party content: web
// pages, search results, browser sessions, MCP servers or attached documents.
// Such output is untrusted data that may carry prompt injection. Durable state
// derived from a session that consumed it (memory, learned skills) records
// that provenance instead of being treated as purely user-authored.
func ExternalContentTool(name string) bool {
	switch name {
	case "web_fetch", "web_search", "host_opencli", "read_attachment":
		return true
	}
	return strings.HasPrefix(name, "mcp_")
}

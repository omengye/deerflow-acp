package deerflow

import (
	"testing"

	einoengine "github.com/omengye/deerflow-acp/go-harness/internal/engine/eino"
)

// Produce a real pre-catalog checkpoint/task in compatibility fixtures. New
// SDK configurations with an omitted subagents section now use default roles;
// omitting config alone must not silently turn this into a v3-only test.
func useLegacyRoleGraph(t *testing.T, c *Client) {
	t.Helper()
	legacy, err := c.service.Engine.(*einoengine.Engine).WithRoleCatalog(nil)
	if err != nil {
		t.Fatal(err)
	}
	c.service.Engine = legacy
	c.background.engine = legacy
}

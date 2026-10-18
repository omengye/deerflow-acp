package deerflow

import (
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/internal/testutil"
)

func TestChannelNewPinsCanonicalWorkspaceFromShortAlias(t *testing.T) {
	workspace := testutil.CanonicalTempDir(t)
	checkChannelNewSession(t, testutil.ShortPath(t, workspace), workspace)
}

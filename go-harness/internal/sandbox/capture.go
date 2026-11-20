package sandbox

import (
	"strings"
	"sync"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/secrets"
)

type capture struct {
	mu         sync.Mutex
	credential *secrets.CredentialOutput
	limit      int
	kept       []byte
	total      int64
}

func newCapture(limit int) *capture { return &capture{limit: limit} }
func (c *capture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := len(p)
	c.total += int64(n)
	keep := c.limit - len(c.kept)
	if keep > n {
		keep = n
	}
	if keep > 0 {
		c.kept = append(c.kept, p[:keep]...)
	}
	return n, nil
}

// snapshot masks known secret values. An open tail (a running command) may
// end inside a value that is still being written, so it is masked like a
// truncated capture.
func (c *capture) snapshot(openTail bool) harness.CommandOutput {
	c.mu.Lock()
	defer c.mu.Unlock()
	truncated := c.total > int64(len(c.kept))
	text := secrets.MaskTruncated(string(c.kept), truncated || openTail)
	text = strings.ToValidUTF8(c.credential.Mask(text), "\uFFFD")
	return harness.CommandOutput{Text: text, TotalBytes: c.total, Truncated: truncated}
}

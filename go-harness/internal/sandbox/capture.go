package sandbox

import (
	"strings"
	"sync"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

type capture struct {
	mu    sync.Mutex
	limit int
	kept  []byte
	total int64
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
func (c *capture) snapshot() harness.CommandOutput {
	c.mu.Lock()
	defer c.mu.Unlock()
	return harness.CommandOutput{Text: strings.ToValidUTF8(string(c.kept), "\uFFFD"), TotalBytes: c.total, Truncated: c.total > int64(len(c.kept))}
}

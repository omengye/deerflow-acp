package sandbox

import (
	"strings"
	"sync"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

type capture struct {
	mu         sync.Mutex
	limit      int
	kept, tail []byte
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
	if len(p) > 8192 {
		p = p[len(p)-8192:]
	}
	c.tail = append(c.tail, p...)
	if len(c.tail) > 8192 {
		c.tail = append(c.tail[:0], c.tail[len(c.tail)-8192:]...)
	}
	return n, nil
}
func (c *capture) snapshot() harness.CommandOutput {
	c.mu.Lock()
	defer c.mu.Unlock()
	return harness.CommandOutput{Text: strings.ToValidUTF8(string(c.kept), "\uFFFD"), TotalBytes: c.total, Truncated: c.total > int64(len(c.kept))}
}
func (c *capture) tailText() string { c.mu.Lock(); defer c.mu.Unlock(); return string(c.tail) }
func (c *capture) stripSuffix(marker string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if i := strings.LastIndex(string(c.tail), marker); i >= 0 {
		if i > 0 && c.tail[i-1] == '\n' {
			i--
		}
		c.total -= int64(len(c.tail) - i)
		c.tail = c.tail[:i]
		if int64(len(c.kept)) > c.total {
			c.kept = c.kept[:int(c.total)]
		}
	}
}

package secrets

import (
	"strings"
	"sync"
)

// Open vaults register their maskers process-wide so passive data pipelines
// (tool receipts, compaction, learning and memory) can mask known values
// without each owning a vault. Masking with another vault's values only
// over-masks; resolving and restoring values always use one explicit vault.
var registry = struct {
	sync.RWMutex
	vaults map[*Vault]struct{}
}{vaults: map[*Vault]struct{}{}}

func register(v *Vault) {
	registry.Lock()
	registry.vaults[v] = struct{}{}
	registry.Unlock()
}

func unregister(v *Vault) {
	registry.Lock()
	delete(registry.vaults, v)
	registry.Unlock()
}

func maskers() []*Masker {
	registry.RLock()
	defer registry.RUnlock()
	out := make([]*Masker, 0, len(registry.vaults))
	for v := range registry.vaults {
		if m := v.Masker(); !m.empty() {
			out = append(out, m)
		}
	}
	return out
}

// Active reports whether any open vault holds a maskable value.
func Active() bool { return len(maskers()) > 0 }

// Mask replaces every known value of every open vault with its marker.
func Mask(text string) string {
	for _, m := range maskers() {
		text = m.Mask(text)
	}
	return text
}

// MaskTruncated additionally masks a value prefix cut by an output limit.
func MaskTruncated(text string, truncated bool) string {
	for _, m := range maskers() {
		if truncated {
			text = m.MaskTruncated(text)
		} else {
			text = m.Mask(text)
		}
	}
	return text
}

// MaskRange masks text[start:end] while scanning surrounding bytes in text so
// a known secret split across start or end is masked as :partial instead of
// leaking short plaintext slices (such as 4-byte paginated reads).
func MaskRange(text string, start, end int) string {
	if start < 0 {
		start = 0
	}
	if end > len(text) {
		end = len(text)
	}
	if start >= end {
		return ""
	}
	ms := maskers()
	if len(ms) == 0 {
		return text[start:end]
	}
	maxL := 0
	for _, m := range ms {
		maxL = max(maxL, m.maxLen)
	}
	if maxL == 0 {
		return text[start:end]
	}
	winStart := max(0, start-(maxL-1))
	winEnd := min(len(text), end+(maxL-1))
	window := text[winStart:winEnd]
	relStart := start - winStart
	relEnd := end - winStart
	var b strings.Builder
	cursor := relStart
	for i := 0; i < relEnd; {
		var best *maskPattern
		for _, m := range ms {
			if idx := m.match(window, i); idx >= 0 {
				cand := &m.list[idx]
				if best == nil || cand.size > best.size {
					best = cand
				}
			}
		}
		if best == nil {
			i++
			continue
		}
		matchEnd := i + best.size
		if matchEnd <= relStart {
			i = matchEnd
			continue
		}
		if i > cursor {
			b.WriteString(window[cursor:i])
		}
		if i >= relStart && matchEnd <= relEnd {
			b.WriteString(best.marker)
		} else {
			b.WriteString(best.base + ":partial]")
		}
		cursor = min(matchEnd, relEnd)
		i = matchEnd
	}
	if cursor == relStart && b.Len() == 0 {
		return text[start:end]
	}
	if cursor < relEnd {
		b.WriteString(window[cursor:relEnd])
	}
	return b.String()
}

// ContainsKnown reports whether text contains any maskable known value.
func ContainsKnown(text string) bool {
	for _, m := range maskers() {
		if m.Contains(text) {
			return true
		}
	}
	return false
}

// Stream masks a chunked stream against the vaults open at creation time.
type Stream struct{ stages []*StreamMasker }

func NewStream() *Stream {
	var s Stream
	for _, m := range maskers() {
		s.stages = append(s.stages, m.Stream())
	}
	return &s
}

func (s *Stream) Push(chunk string) string {
	for _, stage := range s.stages {
		chunk = stage.Push(chunk)
	}
	return chunk
}

// Flush drains every stage in order; text released by an earlier stage still
// passes through the later stages.
func (s *Stream) Flush() string {
	var out string
	for i, stage := range s.stages {
		text := stage.Flush()
		for _, later := range s.stages[i+1:] {
			text = later.Push(text)
		}
		out += text
	}
	return out
}

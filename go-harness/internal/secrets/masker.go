package secrets

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Marker grammar: [secret:NAME], [secret:NAME@vN] for a retained older or
// deleted version, optionally suffixed by the encoding the value appeared in
// (:json, :url, :urlpath, :b64, :b64raw, :b64url, :b64rawurl, :trim). Markers are
// reversible references: workspace writes restore them to the exact text.
// :partial marks a value cut by an output limit and is never restored.
var markerPattern = regexp.MustCompile(`\[secret:([A-Z_][A-Z0-9_]*)(?:@v([0-9]{1,18}))?(?::([a-z0-9]{1,12}))?\]`)

// ReplaceMarkers replaces every marker in text, so that text filters can
// treat a marker as one opaque token. A marker names a secret and never
// carries its value.
func ReplaceMarkers(text string, replace func(marker string) string) string {
	if !strings.Contains(text, "[secret:") {
		return text
	}
	return markerPattern.ReplaceAllStringFunc(text, replace)
}

const (
	// Keep long encodings as block digests. Only short reversible values stay in memory.
	maxMaskPatternBytes = MaxStringBytes
	maskBlockBytes      = 4096
)

type maskEntry struct {
	name    string
	version int64
	current bool
	value   []byte
}

type maskPattern struct {
	tail   string // last bytes of a long value, used to reject full near-matches cheaply
	text   string // entire short value or a cloned prefix for a long value
	size   int
	blocks [][sha256.Size]byte
	marker string
	// jsonMarker replaces a raw value found in a string of a JSON document
	// nested inside tool output, so that restoring it in that document's text
	// yields text that is valid inside the string.
	jsonMarker string
	base       string
}

// Masker replaces known secret values with markers. It is immutable once
// built; the vault swaps in a new Masker after each mutation.
type Masker struct {
	list        []maskPattern
	full        map[string][]int // MinMaskBytes-byte prefix -> patterns, longest first
	longHeads   map[byte][]int   // large values whose first bytes may be cut by an output boundary
	maxLen      int
	restore     map[string]string
	longRestore map[string]int
}

func markerBase(name string, version int64, current bool) string {
	if current {
		return "[secret:" + name
	}
	return "[secret:" + name + "@v" + strconv.FormatInt(version, 10)
}

func jsonEscaped(value string, html bool) string {
	var b strings.Builder
	encoder := json.NewEncoder(&b)
	encoder.SetEscapeHTML(html)
	_ = encoder.Encode(value)
	out := strings.TrimSuffix(b.String(), "\n")
	return strings.TrimSuffix(strings.TrimPrefix(out, `"`), `"`)
}

type variant struct {
	suffix string
	text   string
}

func variants(value []byte) []variant {
	raw := string(value)
	out := []variant{{"", raw}}
	add := func(suffix, text string) {
		for _, existing := range out {
			if existing.text == text {
				return
			}
		}
		out = append(out, variant{suffix, text})
	}
	if utf8.Valid(value) {
		plain, html := jsonEscaped(raw, false), jsonEscaped(raw, true)
		add(":json", plain)
		if len(html) > maxMaskPatternBytes && html != plain {
			add(":jsonhtml", html)
		} else {
			add(":json", html)
		}
		add(":url", url.QueryEscape(raw))
		add(":urlpath", url.PathEscape(raw))
		if trimmed := strings.TrimSpace(raw); trimmed != raw && trimmed != "" {
			add(":trim", trimmed)
		}
	}
	add(":b64", base64.StdEncoding.EncodeToString(value))
	add(":b64raw", base64.RawStdEncoding.EncodeToString(value))
	add(":b64url", base64.URLEncoding.EncodeToString(value))
	add(":b64rawurl", base64.RawURLEncoding.EncodeToString(value))
	return out
}

// restoreText maps an encoding suffix back to the exact text it replaced.
func restoreText(suffix string, value []byte) (string, bool) {
	raw := string(value)
	switch suffix {
	case "":
		return raw, true
	case ":json":
		return jsonEscaped(raw, false), utf8.Valid(value)
	case ":jsonhtml":
		return jsonEscaped(raw, true), utf8.Valid(value)
	case ":url":
		return url.QueryEscape(raw), utf8.Valid(value)
	case ":urlpath":
		return url.PathEscape(raw), utf8.Valid(value)
	case ":trim":
		return strings.TrimSpace(raw), utf8.Valid(value)
	case ":b64":
		return base64.StdEncoding.EncodeToString(value), true
	case ":b64raw":
		return base64.RawStdEncoding.EncodeToString(value), true
	case ":b64url":
		return base64.URLEncoding.EncodeToString(value), true
	case ":b64rawurl":
		return base64.RawURLEncoding.EncodeToString(value), true
	}
	return "", false
}

var restoreSuffixes = []string{"", ":json", ":jsonhtml", ":url", ":urlpath", ":trim", ":b64", ":b64raw", ":b64url", ":b64rawurl"}

// Entries must be ordered by preference: the first marker that claims a text
// wins when two secrets or versions share a value.
type maskBuilder struct {
	m    *Masker
	seen map[[sha256.Size]byte]int
}

func newMaskBuilder() *maskBuilder {
	return &maskBuilder{&Masker{full: map[string][]int{}, longHeads: map[byte][]int{}, restore: map[string]string{}, longRestore: map[string]int{}}, map[[sha256.Size]byte]int{}}
}
func (b *maskBuilder) add(entry maskEntry) {
	m := b.m
	base := markerBase(entry.name, entry.version, entry.current)
	// Aliases still restore when several encodings produce identical bytes.
	for _, suffix := range restoreSuffixes {
		if text, ok := restoreText(suffix, entry.value); ok && len(text) <= maxMaskPatternBytes {
			m.restore[base+suffix+"]"] = text
		}
	}
	for _, v := range variants(entry.value) {
		if len(v.text) < MinMaskBytes {
			continue
		}
		digest := sha256.Sum256([]byte(v.text))
		marker := base + v.suffix + "]"
		if i, ok := b.seen[digest]; ok {
			if len(v.text) > maxMaskPatternBytes {
				m.longRestore[marker] = i
			}
			continue
		}
		p := maskPattern{text: v.text, size: len(v.text), marker: marker, jsonMarker: marker, base: base}
		if v.suffix == "" && utf8.Valid(entry.value) && jsonEscaped(v.text, false) != v.text {
			p.jsonMarker = base + ":json]"
		}
		i := len(m.list)
		if p.size > maxMaskPatternBytes {
			p.text = strings.Clone(v.text[:32])
			p.tail = strings.Clone(v.text[len(v.text)-32:])
			for start := 0; start < p.size; start += maskBlockBytes {
				p.blocks = append(p.blocks, sha256.Sum256([]byte(v.text[start:min(start+maskBlockBytes, p.size)])))
			}
			m.longRestore[marker] = i
		}
		b.seen[digest] = i
		m.list = append(m.list, p)
		m.full[p.text[:MinMaskBytes]] = append(m.full[p.text[:MinMaskBytes]], i)
		if len(p.blocks) > 0 {
			m.longHeads[p.text[0]] = append(m.longHeads[p.text[0]], i)
		}
		m.maxLen = max(m.maxLen, p.size)
	}
}
func (b *maskBuilder) finish() *Masker {
	for _, ids := range b.m.full {
		sort.SliceStable(ids, func(i, j int) bool { return b.m.list[ids[i]].size > b.m.list[ids[j]].size })
	}
	return b.m
}
func buildMasker(entries []maskEntry) *Masker {
	b := newMaskBuilder()
	for _, e := range entries {
		b.add(e)
	}
	return b.finish()
}

// matchesPrefix checks complete blocks. An incomplete last block is held back
// until more bytes arrive, or conservatively redacted at an output boundary.
func (p *maskPattern) matchesPrefix(part string) bool {
	if len(part) > p.size || len(part) == p.size && p.tail != "" && !strings.HasSuffix(part, p.tail) {
		return false
	}
	n := min(len(part), len(p.text))
	if part[:n] != p.text[:n] {
		return false
	}
	for i, digest := range p.blocks {
		start := i * maskBlockBytes
		end := min(start+maskBlockBytes, p.size)
		if end > len(part) {
			break
		}
		if sha256.Sum256([]byte(part[start:end])) != digest {
			return false
		}
	}
	return true
}

func (m *Masker) empty() bool { return m == nil || len(m.list) == 0 }

// match returns the longest pattern starting at s[i:], or -1.
func (m *Masker) match(s string, i int) int {
	if len(s)-i < MinMaskBytes {
		return -1
	}
	for _, index := range m.full[s[i:i+MinMaskBytes]] {
		p := &m.list[index]
		if len(s)-i >= p.size && p.matchesPrefix(s[i:i+p.size]) {
			return index
		}
	}
	return -1
}

// scan replaces complete matches that start before limit and returns the
// masked text of s[:consumed] together with consumed. A match starting before
// limit is always complete because limit leaves maxLen-1 bytes of lookahead.
func (m *Masker) scan(s string, limit int, inJSON bool) (string, int) {
	var b strings.Builder
	last, i := 0, 0
	for i < limit {
		index := m.match(s, i)
		if index < 0 {
			i++
			continue
		}
		if b.Len() == 0 && last == 0 {
			b.Grow(len(s))
		}
		b.WriteString(s[last:i])
		if inJSON {
			b.WriteString(m.list[index].jsonMarker)
		} else {
			b.WriteString(m.list[index].marker)
		}
		i += m.list[index].size
		last = i
	}
	consumed := max(limit, last)
	if last == 0 {
		return s[:consumed], consumed
	}
	b.WriteString(s[last:consumed])
	return b.String(), consumed
}

// Mask replaces every known value occurrence in complete text.
func (m *Masker) Mask(s string) string {
	if m.empty() || len(s) < MinMaskBytes {
		return s
	}
	out, _ := m.scan(s, len(s), false)
	return out
}

// MaskOutput masks complete tool output. Models read JSON output decoded, so
// each escaped string literal of valid JSON is masked at its decoded level:
// there a marker stands for exactly the text the model sees, which is the
// text a workspace write restores. This also catches values escaped by
// encoders other than Go's (\u00XX or \/ escapes). A JSON document inside
// such a string, for example a JSON file read as text, keeps its own
// escaping, so a raw value found in it uses a :json marker. Remaining raw
// text, such as JSON numbers, is masked last.
func (m *Masker) MaskOutput(s string) string {
	if m.empty() {
		return s
	}
	if strings.Contains(s, `\`) && json.Valid([]byte(s)) {
		s = m.maskJSONStrings(s, false)
	}
	return m.Mask(s)
}

func (m *Masker) maskJSONStrings(s string, nested bool) string {
	var b strings.Builder
	last := 0
	for i := 0; i < len(s); i++ {
		if s[i] != '"' {
			continue
		}
		end, escaped := i+1, false
		for ; end < len(s) && s[end] != '"'; end++ {
			if s[end] == '\\' {
				escaped = true
				end++
			}
		}
		if escaped {
			var decoded string
			if json.Unmarshal([]byte(s[i:end+1]), &decoded) == nil {
				masked := decoded
				if len(decoded) >= MinMaskBytes {
					masked, _ = m.scan(decoded, len(decoded), nested)
				}
				if strings.Contains(masked, `\`) && strings.Contains(masked, `"`) && json.Valid([]byte(masked)) {
					masked = m.maskJSONStrings(masked, true)
				}
				if masked != decoded {
					b.WriteString(s[last:i])
					b.WriteByte('"')
					b.WriteString(jsonEscaped(masked, false))
					b.WriteByte('"')
					last = end + 1
				}
			}
		}
		i = end
	}
	if last == 0 {
		return s
	}
	b.WriteString(s[last:])
	return b.String()
}

// MaskTruncated also masks a trailing prefix of a value that an output limit
// cut off, which a plain substring match cannot recognize.
func (m *Masker) MaskTruncated(s string) string {
	if m.empty() {
		return s
	}
	out, _, pending := m.scanBoundary(s, false)
	if pending >= 0 {
		return out + m.list[pending].base + ":partial]"
	}
	return out
}

// A prefix inside a complete match must never cause part of that match to be emitted.
func (m *Masker) scanBoundary(s string, stream bool) (string, int, int) {
	var out strings.Builder
	last, i := 0, 0
	for i+MinMaskBytes <= len(s) {
		next := i + 1
		for _, idx := range m.full[s[i:i+MinMaskBytes]] {
			p := &m.list[idx]
			n := min(p.size, len(s)-i)
			if !p.matchesPrefix(s[i : i+n]) {
				continue
			}
			if n < p.size {
				if !stream {
					if full := m.match(s, i); full >= 0 && m.list[full].size == n {
						out.WriteString(s[last:i])
						out.WriteString(m.list[full].marker)
						return out.String(), len(s), -1
					}
				}
				out.WriteString(s[last:i])
				return out.String(), i, idx
			}
			out.WriteString(s[last:i])
			out.WriteString(p.marker)
			next = i + p.size
			last = next
			break
		}
		i = next
	}
	if !stream {
		for ; i < len(s); i++ {
			for _, idx := range m.longHeads[s[i]] {
				if m.list[idx].matchesPrefix(s[i:]) {
					out.WriteString(s[last:i])
					return out.String(), i, idx
				}
			}
		}
	} else {
		for back := 0; back < utf8.UTFMax-1 && i > last && i < len(s) && !utf8.RuneStart(s[i]); back++ {
			i--
		}
	}
	out.WriteString(s[last:i])
	return out.String(), i, -1
}

// Contains reports whether text contains any maskable known value.
func (m *Masker) Contains(s string) bool {
	if m.empty() {
		return false
	}
	for i := 0; i+MinMaskBytes <= len(s); i++ {
		if m.match(s, i) >= 0 {
			return true
		}
	}
	return false
}

// Unmask restores reversible markers to the exact text they replaced.
// Unknown or partial markers stay literal.
func (m *Masker) Unmask(s string) (string, int) {
	if m == nil || len(m.restore) == 0 || !strings.Contains(s, "[secret:") {
		return s, 0
	}
	count := 0
	out := markerPattern.ReplaceAllStringFunc(s, func(marker string) string {
		if value, ok := m.restore[marker]; ok {
			count++
			return value
		}
		return marker
	})
	return out, count
}

// StreamMasker masks a chunked stream. It withholds a tail short enough that
// a value split across chunks is still recognized when the next chunk arrives.
type StreamMasker struct {
	masker    *Masker
	carry     []byte
	nextCheck int
}

func (m *Masker) Stream() *StreamMasker { return &StreamMasker{masker: m} }

func (s *StreamMasker) Push(chunk string) string {
	if s == nil || s.masker.empty() {
		return chunk
	}
	if chunk != "" {
		s.carry = append(s.carry, chunk...)
	}
	if len(s.carry) < s.nextCheck {
		return ""
	}
	s.nextCheck = 0
	data := string(s.carry)
	out, consumed, pending := s.masker.scanBoundary(data, true)
	if pending >= 0 {
		s.nextCheck = min(s.masker.list[pending].size, 2*(len(data)-consumed))
	}
	rem := copy(s.carry, s.carry[consumed:])
	clear(s.carry[rem:])
	s.carry = s.carry[:rem]
	return out
}

func (s *StreamMasker) Flush() string {
	if s == nil || len(s.carry) == 0 {
		return ""
	}
	rest := string(s.carry)
	clear(s.carry)
	s.carry = s.carry[:0]
	s.nextCheck = 0
	return s.masker.MaskTruncated(rest)
}

package secrets

import (
	"encoding/base64"
	"encoding/json"
	"net/url"
	"strings"
	"testing"
)

func testMasker(values ...string) *Masker {
	var entries []maskEntry
	for i, value := range values {
		entries = append(entries, maskEntry{name: "S" + string(rune('A'+i)), version: 1, current: true, value: []byte(value)})
	}
	return buildMasker(entries)
}

func TestMaskReplacesValueAndEncodings(t *testing.T) {
	value := `tok/en+"x"&<y>=z 中`
	m := testMasker(value)
	jsonText, _ := json.Marshal(map[string]string{"v": value})
	if got := m.Mask("plain " + value + " end"); got != "plain [secret:SA] end" {
		t.Fatalf("plain = %q", got)
	}
	if got := m.Mask(string(jsonText)); got != `{"v":"[secret:SA:json]"}` {
		t.Fatalf("json = %q", got)
	}
	if got := m.Mask("q=" + url.QueryEscape(value)); got != "q=[secret:SA:url]" {
		t.Fatalf("query = %q", got)
	}
	if got := m.Mask("p=" + url.PathEscape(value)); got != "p=[secret:SA:urlpath]" {
		t.Fatalf("path = %q", got)
	}
	for _, form := range []string{
		base64.StdEncoding.EncodeToString([]byte(value)), base64.RawStdEncoding.EncodeToString([]byte(value)),
		base64.URLEncoding.EncodeToString([]byte(value)), base64.RawURLEncoding.EncodeToString([]byte(value)),
		url.PathEscape(value), url.QueryEscape(value), value,
	} {
		masked := m.Mask(form)
		if !strings.HasPrefix(masked, "[secret:SA") {
			t.Fatalf("encoded form %q not masked: %q", form, masked)
		}
		if restored, _ := m.Unmask(masked); restored != form {
			t.Fatalf("Unmask(%q) = %q, want %q", masked, restored, form)
		}
	}
}

func TestMaskPrefersLongestAndSkipsShortValues(t *testing.T) {
	m := testMasker("abcdefgh", "abcdefghijkl", "short")
	if got := m.Mask("x abcdefghijkl abcdefgh short"); got != "x [secret:SB] [secret:SA] short" {
		t.Fatalf("Mask = %q", got)
	}
	if m.Contains("short only") || !m.Contains("..abcdefgh..") {
		t.Fatal("Contains mismatch")
	}
}

func TestUnmaskRestoresExactText(t *testing.T) {
	value := "pa ss\"word&1 "
	entries := []maskEntry{
		{name: "KEY", version: 3, current: true, value: []byte(value)},
		{name: "KEY", version: 2, value: []byte("older-value-2")},
		{name: "PIN", version: 1, current: true, value: []byte("1234")},
	}
	m := buildMasker(entries)
	text := "a=" + value + " b=" + url.QueryEscape(value) + " c=older-value-2 d=" + strings.TrimSpace(value)
	masked := m.Mask(text)
	if strings.Contains(masked, "pa ss") || strings.Contains(masked, "older-value") {
		t.Fatalf("value leaked: %q", masked)
	}
	restored, count := m.Unmask(masked)
	if restored != text || count != 4 {
		t.Fatalf("Unmask = %q (%d), want %q", restored, count, text)
	}
	if got, n := m.Unmask("[secret:PIN] [secret:NOPE] [secret:KEY:partial]"); got != "1234 [secret:NOPE] [secret:KEY:partial]" || n != 1 {
		t.Fatalf("Unmask short/unknown = %q (%d)", got, n)
	}
}

func TestStreamMaskerMatchesWholeTextAtEverySplit(t *testing.T) {
	m := testMasker("first-secret-value", "第二个密钥值-second")
	text := "start first-secret-value mid 第二个密钥值-second first-secret-valu end"
	want := m.Mask(text)
	for split := 0; split <= len(text); split++ {
		for second := split; second <= len(text); second += 7 {
			s := m.Stream()
			got := s.Push(text[:split]) + s.Push(text[split:second]) + s.Push(text[second:]) + s.Flush()
			if got != want {
				t.Fatalf("split %d/%d: %q, want %q", split, second, got, want)
			}
		}
	}
	whole := m.Stream()
	var b strings.Builder
	for _, r := range text {
		b.WriteString(whole.Push(string(r)))
	}
	if b.String()+whole.Flush() != want {
		t.Fatal("rune-by-rune stream mismatch")
	}
}

func TestMaskTruncatedMarksCutValue(t *testing.T) {
	m := testMasker("supersecretvalue")
	if got := m.MaskTruncated("output: supersec"); got != "output: [secret:SA:partial]" {
		t.Fatalf("MaskTruncated = %q", got)
	}
	if got := m.MaskTruncated("output: sup"); got != "output: sup" {
		t.Fatalf("too-short tail masked: %q", got)
	}
	if got := m.MaskTruncated("supersecretvalue done"); got != "[secret:SA] done" {
		t.Fatalf("complete value = %q", got)
	}
}

func TestMaskOutputMasksEscapedJSON(t *testing.T) {
	m := testMasker("line1\nline2-secret", "secret/path/value", "café-secret-1")
	payload, _ := json.Marshal(map[string]any{"nested": []string{"x line1\nline2-secret y"}})
	got := m.MaskOutput(string(payload))
	var decoded map[string][]string
	if err := json.Unmarshal([]byte(got), &decoded); err != nil || decoded["nested"][0] != "x [secret:SA] y" {
		t.Fatalf("MaskOutput = %s (%v)", got, err)
	}
	foreign := `{"a":"secret\/path\/value","b":"caf\u00e9-secret-1","inner":"{\"k\":\"secret\\/path\\/value\"}"}`
	got = m.MaskOutput(foreign)
	if strings.Contains(got, "path") || strings.Contains(got, "secret-1") {
		t.Fatalf("foreign escaping leaked: %s", got)
	}
	var fields map[string]string
	if err := json.Unmarshal([]byte(got), &fields); err != nil || fields["a"] != "[secret:SB]" || fields["b"] != "[secret:SC]" || fields["inner"] != `{"k":"[secret:SB]"}` {
		t.Fatalf("MaskOutput foreign = %s (%v)", got, err)
	}
	if got := m.MaskOutput(`not json "secret\/path\/value"`); got != `not json "secret\/path\/value"` {
		t.Fatalf("non-JSON text changed: %s", got)
	}
}

func TestMaskOutputRestoresAtDecodedLevel(t *testing.T) {
	value := `abc"def-secret`
	m := testMasker(value)
	// A .env file read through a JSON tool result: the model reads the
	// decoded content, and writing it back must restore the raw value.
	envOutput, _ := json.Marshal(map[string]string{"content": "TOKEN=" + value + "\n"})
	var env map[string]string
	if err := json.Unmarshal([]byte(m.MaskOutput(string(envOutput))), &env); err != nil {
		t.Fatal(err)
	}
	if restored, _ := m.Restore(env["content"]); restored != "TOKEN="+value+"\n" {
		t.Fatalf(".env restore = %q", restored)
	}
	// A JSON file read the same way keeps its own escaping.
	file, _ := json.Marshal(map[string]string{"token": value})
	fileOutput, _ := json.Marshal(map[string]string{"content": string(file)})
	var config map[string]string
	if err := json.Unmarshal([]byte(m.MaskOutput(string(fileOutput))), &config); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(config["content"], "def-secret") {
		t.Fatalf("value leaked: %q", config["content"])
	}
	if restored, _ := m.Restore(config["content"]); restored != string(file) {
		t.Fatalf("json file restore = %q, want %q", restored, file)
	}
	if _, err := m.Restore("x [secret:SA:partial]"); err == nil {
		t.Fatal("partial marker of a known secret restored")
	}
	if got, err := m.Restore("x [secret:OTHER:partial]"); err != nil || got != "x [secret:OTHER:partial]" {
		t.Fatalf("unknown partial = %q, %v", got, err)
	}
}

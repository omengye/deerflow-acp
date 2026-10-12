package learning

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSkillLearningRedactionNestedJSONToolEnvelope(t *testing.T) {
	inner := `{"verified":true,"token":"NESTED_PRIVATE_TOKEN","API_KEY":"NESTED_PRIVATE_KEY","path":"C:\\Users\\private\\config.json","count":12345678901234567890}`
	envelope := func(content string) string {
		t.Helper()
		data, err := json.Marshal(map[string]any{"content": content, "truncated": false, "next_offset": nil})
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	for name, text := range map[string]string{
		"configured-read-file": envelope(inner),
		"double-encoding":      envelope(envelope(inner)),
		"escaped-unicode-key":  envelope(`{"to\u006ben":"NESTED_PRIVATE_UNICODE"}`),
		"array":                `["token=NESTED_PRIVATE_ARRAY",{"secret":{"value":"NESTED_PRIVATE_OBJECT"}},123]`,
		"typed-secret":         `{"password":["NESTED_PRIVATE_PASSWORD"],"authorization":123456}`,
		"private-key-text":     `{"C:\\Users\\private\\config.json":"keep","person@private.invalid":true}`,
		"obfuscated-key":       `{"to\u200bken":"NESTED_PRIVATE_HIDDEN_KEY"}`,
	} {
		t.Run(name, func(t *testing.T) {
			result := redact(text, 8192)
			if strings.Contains(result, "NESTED_PRIVATE") || strings.Contains(strings.ToLower(result), "private") || !strings.Contains(result, "[redacted]") {
				t.Fatal("nested tool envelope retained sensitive content")
			}
		})
	}
	result := redact(envelope(inner), 8192)
	if !strings.Contains(result, `\"verified\":true`) || !strings.Contains(result, "12345678901234567890") {
		t.Fatal("redaction corrupted nonsensitive verification or numeric evidence", result)
	}
	for _, secret := range []string{"authorization", "cookie", "set-cookie", "proxy_authorization"} {
		inner := `{"` + secret + `":"NESTED_PRIVATE_AUTH","verified":true}`
		result := redact(envelope(inner), 8192)
		var outer struct {
			Content string `json:"content"`
		}
		var clean struct {
			Verified bool `json:"verified"`
		}
		if json.Unmarshal([]byte(result), &outer) != nil || json.Unmarshal([]byte(outer.Content), &clean) != nil || !clean.Verified || strings.Contains(result, "NESTED_PRIVATE_AUTH") {
			t.Fatal("structured redaction lost nonsensitive neighboring fields", secret)
		}
	}
	plain := "  {\n  \"value\": [12345678901234567890, true], \"text\": \"keep fixture\"\n}  "
	if result := redact(plain, 8192); result != strings.TrimSpace(plain) {
		t.Fatal("unchanged JSON evidence was reformatted", result)
	}
}

func TestSkillLearningRedactionStructuredBoundsFailClosed(t *testing.T) {
	for name, text := range map[string]string{
		"depth":             strings.Repeat("[", 32) + `"token=PRIVATE_BOUND_TOKEN"` + strings.Repeat("]", 32),
		"values":            "[" + strings.Repeat(`0,`, 4100) + `"PRIVATE_BOUND_TOKEN"]`,
		"bytes":             `{"content":"` + strings.Repeat("x", 2<<20) + `PRIVATE_BOUND_TOKEN"}`,
		"key-collision":     `{"first@private.invalid":"a","second@private.invalid":"b"}`,
		"duplicate-key":     `{"content":"PRIVATE_BOUND_TOKEN","content":"safe"}`,
		"decoded-duplicate": `{"content":"PRIVATE_BOUND_TOKEN","cont\u0065nt":"safe"}`,
	} {
		t.Run(name, func(t *testing.T) {
			if result := redact(text, 8192); result != "[redacted]" {
				t.Fatal("structured redaction limit did not fail closed")
			}
		})
	}
	if result := redact(`{"content":"token=PRIVATE_BOUND_TOKEN"}`, 5); result != "{\"con" {
		t.Fatal("redaction no longer applies its final byte limit", result)
	}
}

func TestSkillLearningRedactionEscapedTextAndStableReplay(t *testing.T) {
	for _, text := range []string{
		`Observed file: {\"token\":\"ESCAPED_PRIVATE_TOKEN\",\"verified\":true}`,
		`Truncated wrapper {"content":"{\"token\":\"ESCAPED_PRIVATE_TOKEN\"`,
		`Steps include {\\\"api_key\\\":\\\"ESCAPED_PRIVATE_KEY\\\"}`,
		`{"content":"{\"token\":\"ESCAPED_PRIVATE_TOKEN\",\"verified\":true}"}`,
		`{"content":"{\"authorization\":\"ESCAPED_PRIVATE_AUTH\",\"verified\":true}"}`,
		`{"cookie":"ESCAPED_PRIVATE_COOKIE","verified":true}`,
	} {
		clean := redact(text, 8192)
		if strings.Contains(clean, "ESCAPED_PRIVATE") || !strings.Contains(clean, "[redacted]") {
			t.Fatal("escaped text retained a sensitive field")
		}
		if replay := redact(clean, 8192); replay != clean {
			t.Fatal("saved sanitized evidence changed on replay", clean, replay)
		}
	}
}

func TestSkillLearningRedactionNormalizesHiddenCharactersBeforeFiltering(t *testing.T) {
	for name, text := range map[string]string{
		"assignment":      "Inspect whole file; to\u200bken=HIDDEN_PRIVATE_ASSIGNMENT",
		"control-key":     "Inspect whole file; api_\x01key=HIDDEN_PRIVATE_CONTROL",
		"bearer":          "Inspect whole file; Bea\u2060rer HIDDEN_PRIVATE_BEARER",
		"provider-prefix": "Inspect whole file; sk\u200b-HIDDEN_PRIVATE_PROVIDER",
		"private-key":     "Inspect whole file; -----BE\u200bGIN PRIVATE KEY-----\nHIDDEN_PRIVATE_PEM\n-----END PRIVATE KEY-----",
		"machine-path":    "Inspect whole file; /ho\u200bme/HIDDEN_PRIVATE_PATH/config.json",
		"truncated-json":  "Inspect whole file; {\"to\u202eken\":\"HIDDEN_PRIVATE_TRUNCATED\"",
		"json-string":     `{"content":"Inspect whole file; to\u200bken=HIDDEN_PRIVATE_JSON"}`,
		"double-encoding": `{"content":"{\"content\":\"Inspect whole file; to\\u200bken=HIDDEN_PRIVATE_DOUBLE\"}"}`,
	} {
		t.Run(name, func(t *testing.T) {
			clean := redact(text, 8192)
			if strings.Contains(clean, "HIDDEN_PRIVATE") || !strings.Contains(clean, "[redacted]") || !strings.Contains(clean, "Inspect whole file") {
				t.Fatal("hidden characters bypassed redaction or erased nonsensitive procedure")
			}
			if replay := redact(clean, 8192); replay != clean {
				t.Fatal("normalized source changed on replay", clean, replay)
			}
		})
	}
	for name, text := range map[string]string{
		"hidden-prefix": "\u200b \t Inspect whole file \t \u2060",
		"control-edge":  "\x01 Inspect whole file \x7f",
	} {
		t.Run(name, func(t *testing.T) {
			if clean := redact(text, 8192); clean != "Inspect whole file" || redact(clean, 8192) != clean {
				t.Fatal("normalization left unstable surrounding whitespace")
			}
		})
	}
}

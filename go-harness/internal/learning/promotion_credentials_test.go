package learning

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestSkillLearningPromotionInstructionsRejectCredentialLiterals(t *testing.T) {
	for name, text := range map[string]string{
		"unquoted":             "Read full input; token=INSTRUCTION_PRIVATE_TOKEN",
		"quoted":               `password="INSTRUCTION_PRIVATE_PASSWORD"`,
		"prefixed-legacy-key":  `service_api_key="INSTRUCTION_PRIVATE_PREFIXED"`,
		"short-literal":        "secret=abc",
		"header":               "Authorization: Bearer INSTRUCTION_PRIVATE_AUTH",
		"bearer":               "Use Bearer INSTRUCTION_PRIVATE_BEARER",
		"cookie":               "Cookie: session=INSTRUCTION_PRIVATE_COOKIE",
		"json-field":           `Example: {"api_key":"INSTRUCTION_PRIVATE_JSON","verified":true}`,
		"unicode-key":          `Example: {"to\u006ben":"INSTRUCTION_PRIVATE_UNICODE"}`,
		"hidden-key":           "Use to\u200bken=INSTRUCTION_PRIVATE_HIDDEN",
		"control-key":          "Use api_\x01key=INSTRUCTION_PRIVATE_CONTROL",
		"escaped-hidden-key":   `Example: {"to\u200bken":"INSTRUCTION_PRIVATE_ESCAPED_HIDDEN"}`,
		"provider-key":         "Use sk-INSTRUCTION_PRIVATE_PROVIDER",
		"hidden-provider-key":  "Use sk\u2060-INSTRUCTION_PRIVATE_PROVIDER",
		"github-key":           "Use github_pat_INSTRUCTION_PRIVATE_GITHUB",
		"aws-key":              "Use AKIA1234567890123456",
		"private-key":          "-----BEGIN PRIVATE KEY-----\nINSTRUCTION_PRIVATE_PEM\n-----END PRIVATE KEY-----",
		"partial-private-key":  "-----BEGIN RSA PRIVATE KEY-----\nINSTRUCTION_PRIVATE_PEM",
		"hidden-private-key":   "-----BE\u200bGIN EC PRIVATE KEY-----\nINSTRUCTION_PRIVATE_PEM",
		"url-basic-auth":       "See https://user:INSTRUCTION_PRIVATE_AUTH@docs.example.invalid/guide",
		"url-query":            "See https://docs.example.invalid/guide?token=INSTRUCTION_PRIVATE_QUERY",
		"url-encoded-key":      "See https://docs.example.invalid/guide?api%5fkey=INSTRUCTION_PRIVATE_ENCODED",
		"template-plus-secret": `token="${TOKEN}INSTRUCTION_PRIVATE_SUFFIX"`,
	} {
		t.Run(name, func(t *testing.T) {
			if err := ValidatePromotionInstructions(text); !errors.Is(err, harness.ErrInvalidInput) || strings.Contains(err.Error(), "INSTRUCTION_PRIVATE") {
				t.Fatal("concrete credential survived or error disclosed it", err)
			}
		})
	}
	text := `{"to\u006ben":"INSTRUCTION_PRIVATE_NESTED"}`
	for depth := range 4 {
		encoded, err := json.Marshal(text)
		if err != nil {
			t.Fatal(err)
		}
		text = string(encoded)
		if err := ValidatePromotionInstructions("Embedded JSON: " + text); !errors.Is(err, harness.ErrInvalidInput) {
			t.Fatal("encoded credential survived", depth, err)
		}
	}
}

func TestSkillLearningPromotionInstructionsAllowGenericGuidance(t *testing.T) {
	for name, text := range map[string]string{
		"procedure":         "Read complete JSON input before validating fields.\n",
		"public-reference":  "Read https://docs.example.invalid/guide?version=1.\n",
		"temporary-path":    "Use /tmp/scratch or C:\\Temp\\scratch for temporary output.",
		"unicode-procedure": "\u68c0\u67e5\u5b8c\u6574\u8f93\u5165\u5e76\u9a8c\u8bc1\u5fc5\u9700\u5b57\u6bb5\u3002",
		"posix-env":         `token=${SERVICE_TOKEN}; api_key="$API_KEY"`,
		"windows-env":       "password=%PASSWORD%; access_token=$env:ACCESS_TOKEN",
		"json-env":          `Example: {"api_key":"${API_KEY}","token":"<TOKEN>"}`,
		"redacted":          "Use token=[redacted] and access_token=YOUR_ACCESS_TOKEN.",
		"example":           "Use api_key=EXAMPLE_API_KEY and password=REPLACE_ME.",
		"bearer-env":        "Authorization: Bearer ${SERVICE_TOKEN}",
		"bearer-prose":      "Do not log Bearer tokens. Read the authentication scheme.",
		"placeholder-mix":   "Keep `token=${TOKEN}` and `password='<PASSWORD>'` in examples.",
	} {
		t.Run(name, func(t *testing.T) {
			if err := ValidatePromotionInstructions(text); err != nil {
				t.Fatal("generic reusable guidance rejected", err)
			}
		})
	}
	for _, text := range []string{"", string([]byte{0xff}), "Read\x00secret", strings.Repeat("x", (128<<10)+1)} {
		if err := ValidatePromotionInstructions(text); !errors.Is(err, harness.ErrInvalidInput) {
			t.Fatal("instruction bounds bypassed", err)
		}
	}
}

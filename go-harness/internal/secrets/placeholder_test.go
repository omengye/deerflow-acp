package secrets

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// bs is a backslash, so that JSON escapes are built at run time.
const bs = "\x5c"

func TestPlaceholderNames(t *testing.T) {
	for _, tc := range []struct {
		name, args string
		want       []string
	}{
		{"none", `{"a":"plain","n":1}`, nil},
		{"values and arrays", `{"a":"x {{secret:B}} y {{secret:A}}","b":["{{secret:B:url}}",{"c":"{{secret:C_2:b64}}"}]}`, []string{"A", "B", "C_2"}},
		{"keys", `{"{{secret:K}}":1}`, []string{"K"}},
		{"json escapes", `{"a":"` + bs + `u007b{secret:API_` + bs + `u004bEY}}"}`, []string{"API_KEY"}},
		{"escaped quotes around", `{"a":"` + bs + `"{{secret:Q}}` + bs + `""}`, []string{"Q"}},
		{"braces only", `{"a":"{{other}} {secret:A}} {{ secret:A }}"}`, nil},
		{"triple braces", `{"a":"{{{secret:T}}}"}`, []string{"T"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := PlaceholderNames(tc.args)
			if err != nil || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("PlaceholderNames(%s) = %v, %v; want %v", tc.args, got, err, tc.want)
			}
		})
	}
}

func TestPlaceholderNamesRejectsMalformedPlaceholders(t *testing.T) {
	for _, args := range []string{
		`{"a":"{{secret:lower}}"}`,
		`{"a":"{{secret:A:bogus}}"}`,
		`{"a":"{{secret:A:partial}}"}`,
		`{"a":"{{secret:A@v2}}"}`,
		`{"a":"{{secret:A}"}`,
		`{"a":"{{secret:"}`,
		`{"a":"{{secret:1A}}"}`,
		`{"a":"{{secret:` + strings.Repeat("A", MaxNameBytes+1) + `}}"}`,
		`{"a":"ok {{secret:A}} then {{secret:b}}"}`,
		`{"a":"{{secret:A}}"`,
	} {
		if names, err := PlaceholderNames(args); !errors.Is(err, ErrInvalid) {
			t.Errorf("PlaceholderNames(%.80s) = %v, %v; want ErrInvalid", args, names, err)
		}
	}
	if _, err := PlaceholderNames(`{"a":"{{secret:` + strings.Repeat("A", MaxNameBytes) + `}}"}`); err != nil {
		t.Fatalf("longest name rejected: %v", err)
	}
}

func stringSecrets(values map[string]string) map[string]Secret {
	out := make(map[string]Secret, len(values))
	for name, value := range values {
		out[name] = Secret{Name: name, Kind: KindString, Version: 1, Value: []byte(value)}
	}
	return out
}

func decodeArguments(t *testing.T, args string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(args), &out); err != nil {
		t.Fatalf("substituted arguments are not JSON: %v: %s", err, args)
	}
	return out
}

func TestSubstitutePlaceholdersEncodesValues(t *testing.T) {
	values := stringSecrets(map[string]string{
		"TOKEN":  "tok\"en\\with\nnewline and </script>",
		"PLUS":   "a+b/c=d e&f",
		"PADDED": "  padded-value\n",
		"NESTED": "x{{secret:TOKEN}}y",
	})
	args := `{ "keep" : 1.50 , "nested":{"t":"Bearer {{secret:TOKEN}}"},` + "\n" +
		`"url":"https://h/p?k={{secret:PLUS:url}}&p={{secret:PLUS:urlpath}}","b64":"{{secret:PLUS:b64}}","trim":"[{{secret:PADDED:trim}}]",` +
		`"adjacent":"{{secret:PLUS}}{{secret:PADDED}}","copy":"{{secret:NESTED}}","esc":"` + bs + `u007b{secret:TOKEN}}"}`
	out, err := SubstitutePlaceholders(args, values)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, `{ "keep" : 1.50 , "nested":{"t":`) {
		t.Fatalf("bytes outside changed literals were rewritten: %s", out)
	}
	got := decodeArguments(t, out)
	want := map[string]any{
		"keep":     1.5,
		"nested":   map[string]any{"t": "Bearer tok\"en\\with\nnewline and </script>"},
		"url":      "https://h/p?k=a%2Bb%2Fc%3Dd+e%26f&p=a+b%2Fc=d%20e&f",
		"b64":      "YStiL2M9ZCBlJmY=",
		"trim":     "[padded-value]",
		"adjacent": "a+b/c=d e&f  padded-value\n",
		"copy":     "x{{secret:TOKEN}}y",
		"esc":      "tok\"en\\with\nnewline and </script>",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("substituted = %#v\nwant %#v", got, want)
	}
	if plain, err := SubstitutePlaceholders(`{"a":"none"}`, values); err != nil || plain != `{"a":"none"}` {
		t.Fatalf("arguments without placeholders changed: %s %v", plain, err)
	}
}

func TestSubstitutePlaceholdersRefusesUnusableValues(t *testing.T) {
	binary := map[string]Secret{"CERT": {Name: "CERT", Kind: KindFile, Version: 1, Value: []byte{0xff, 0x00, 0xfe}}}
	if _, err := SubstitutePlaceholders(`{"a":"{{secret:CERT}}"}`, binary); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "{{secret:CERT:b64}}") {
		t.Fatalf("binary value substituted raw: %v", err)
	}
	if _, err := SubstitutePlaceholders(`{"a":"{{secret:CERT:url}}"}`, binary); !errors.Is(err, ErrInvalid) {
		t.Fatalf("binary value substituted url-encoded: %v", err)
	}
	if out, err := SubstitutePlaceholders(`{"a":"{{secret:CERT:b64}}"}`, binary); err != nil || decodeArguments(t, out)["a"] != "/wD+" {
		t.Fatalf("binary b64 = %s, %v", out, err)
	}
	var missing *MissingError
	if _, err := SubstitutePlaceholders(`{"a":"{{secret:ABSENT}}"}`, binary); !errors.As(err, &missing) || !reflect.DeepEqual(missing.Names, []string{"ABSENT"}) {
		t.Fatalf("missing value = %v", err)
	}
	if _, err := SubstitutePlaceholders(`{"a":"{{secret:bad}}"}`, binary); !errors.Is(err, ErrInvalid) {
		t.Fatalf("malformed placeholder = %v", err)
	}
	if _, err := SubstitutePlaceholders(`{"a":`, binary); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid JSON = %v", err)
	}
	large := stringSecrets(map[string]string{"BIG": strings.Repeat("v", MaxSubstitutedBytes/2)})
	if _, err := SubstitutePlaceholders(`{"a":"{{secret:BIG}}"}`, large); err != nil {
		t.Fatalf("value within the limit refused: %v", err)
	}
	for _, args := range []string{`{"a":"{{secret:BIG}}{{secret:BIG}}{{secret:BIG}}"}`, `{"a":"{{secret:BIG}}","b":"{{secret:BIG}}","c":"{{secret:BIG}}"}`} {
		if _, err := SubstitutePlaceholders(args, large); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "exceed") {
			t.Fatalf("oversized substitution of %s = %v", args, err)
		}
	}
	quoted := stringSecrets(map[string]string{"Q": strings.Repeat("\x01", MaxSubstitutedBytes/2)})
	if _, err := SubstitutePlaceholders(`{"a":"{{secret:Q}}"}`, quoted); !errors.Is(err, ErrInvalid) {
		t.Fatalf("substitution that grows past the limit when escaped = %v", err)
	}
}

package configschema

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/secrets"
	"go.yaml.in/yaml/v3"
)

// MaxConfigBytes bounds the configuration documents read for credentials.
const MaxConfigBytes = 2 << 20

// NotReadReason marks a credential-like setting this runtime never reads:
// another program may read it, and it would not understand a reference.
const NotReadReason = "this runtime does not read this setting; another program may, and it would not understand a ${secret:NAME} reference"

const searchURLReason = "search base_url and https_proxy cannot refer to secrets; only api_key can"

var utf8BOM = []byte("\xef\xbb\xbf")

// PlaintextCredential is a credential written as plain text in config.yaml.
type PlaintextCredential struct {
	// Field names the setting, for example models[deepseek].api_key.
	Field string
	// Secret is the vault name suggested for the value.
	Secret string
	// Value is the plaintext.
	Value string
	// Reason says why the setting is left alone. It is empty for a setting
	// this runtime reads and ReplaceCredentials can rewrite.
	Reason string

	node *yaml.Node
	flow bool
}

// FindPlaintextCredentials lists the credentials config.yaml holds as plain
// text: the settings this runtime reads, which can refer to the secret vault
// instead, and other settings that look like credentials, with the reason
// each is left alone. Values that are empty, placeholders or references to
// the environment or the vault are not listed.
func FindPlaintextCredentials(raw []byte) ([]PlaintextCredential, error) {
	found, _, err := findCredentials(raw)
	return found, err
}

// ReplaceCredentials returns raw with the settings in names (Field to secret
// name) replaced by ${secret:NAME} references. Every other byte, including
// comments, quoting and line endings, stays as it was, and the result must
// decode to the same document apart from those values.
func ReplaceCredentials(raw []byte, names map[string]string) ([]byte, error) {
	found, root, err := findCredentials(raw)
	if err != nil {
		return nil, err
	}
	body, bom := bytes.CutPrefix(raw, utf8BOM)
	lines := lineStarts(body)
	type edit struct {
		start, end int
		text       string
	}
	var edits []edit
	remaining := maps.Clone(names)
	for _, c := range found {
		name, ok := remaining[c.Field]
		if !ok {
			continue
		}
		if c.Reason != "" {
			return nil, fmt.Errorf("%s cannot refer to a secret: %s", c.Field, c.Reason)
		}
		if !secrets.ValidName(name) {
			return nil, fmt.Errorf("%w: invalid secret name for %s", secrets.ErrInvalid, c.Field)
		}
		start, end, reason := locateScalar(body, lines, c.node)
		if reason != "" {
			return nil, fmt.Errorf("%s cannot refer to a secret: %s", c.Field, reason)
		}
		reference := secrets.ReferenceTo(name)
		text := reference
		switch {
		case c.node.Style&yaml.DoubleQuotedStyle != 0, c.node.Style&yaml.SingleQuotedStyle == 0 && c.flow:
			text = `"` + reference + `"`
		case c.node.Style&yaml.SingleQuotedStyle != 0:
			text = "'" + reference + "'"
		}
		edits = append(edits, edit{start, end, text})
		// The expected document: the same tree with the reference as value.
		c.node.Value, c.node.Tag, c.node.Style = reference, "!!str", 0
		delete(remaining, c.Field)
	}
	if len(remaining) != 0 {
		return nil, errors.New("config.yaml no longer holds some of the settings to replace as plain text")
	}
	slices.SortFunc(edits, func(a, b edit) int { return b.start - a.start })
	out := slices.Clone(body)
	for _, e := range edits {
		out = slices.Concat(out[:e.start], []byte(e.text), out[e.end:])
	}
	var expected, actual any
	if err := root.Decode(&expected); err != nil {
		return nil, err
	}
	if err := yaml.Unmarshal(out, &actual); err != nil || !reflect.DeepEqual(expected, actual) {
		return nil, errors.New("config.yaml could not be rewritten without changing other settings; replace the values by hand")
	}
	if bom {
		out = slices.Concat(utf8BOM, out)
	}
	return out, nil
}

// SecretReferenceFields returns the settings of config.yaml that refer to
// each secret, by secret name.
func SecretReferenceFields(raw []byte) (map[string][]string, error) {
	root, err := parseConfigNode(raw)
	if err != nil {
		return nil, err
	}
	out := map[string][]string{}
	walkConfig(root, "", false, false, func(field string, _ string, value *yaml.Node, _, _ bool) {
		if value.Kind != yaml.ScalarNode {
			return
		}
		if name, ok, err := secrets.Reference(value.Value); ok && err == nil {
			out[name] = append(out[name], field)
		}
	})
	return out, nil
}

func parseConfigNode(raw []byte) (*yaml.Node, error) {
	if len(raw) > MaxConfigBytes {
		return nil, errors.New("config exceeds 2 MiB")
	}
	raw, _ = bytes.CutPrefix(raw, utf8BOM)
	var doc yaml.Node
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(&doc); err != nil {
		return nil, fmt.Errorf("invalid config.yaml: %w", err)
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, errors.New("config.yaml must hold one document")
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("config.yaml must hold an object")
	}
	return doc.Content[0], nil
}

func findCredentials(raw []byte) ([]PlaintextCredential, *yaml.Node, error) {
	root, err := parseConfigNode(raw)
	if err != nil {
		return nil, nil, err
	}
	// Decoding as the runtime does rejects what it would reject, such as
	// duplicate keys, before anything is reported or rewritten.
	var decoded any
	if err := root.Decode(&decoded); err != nil {
		return nil, nil, fmt.Errorf("invalid config.yaml: %w", err)
	}
	body, _ := bytes.CutPrefix(raw, utf8BOM)
	f := &credentialFinder{body: body, lines: lineStarts(body), seen: map[*yaml.Node]bool{}, anchored: map[*yaml.Node]bool{}}
	walkConfig(root, "", false, false, func(_, _ string, value *yaml.Node, _, anchored bool) {
		if anchored {
			f.anchored[value] = true
		}
	})
	f.runtimeSettings(root)
	walkConfig(root, "", false, false, func(field, key string, value *yaml.Node, flow, _ bool) {
		value = deref(value)
		if !f.seen[value] && credentialKey(key) && plaintext(value) {
			f.seen[value] = true
			f.found = append(f.found, PlaintextCredential{Field: field, Secret: SecretName(field), Value: value.Value, Reason: NotReadReason, node: value, flow: flow})
		}
	})
	// fields selects settings by name, so two settings cannot share one.
	count := map[string]int{}
	for _, c := range f.found {
		count[c.Field]++
	}
	for i := range f.found {
		if count[f.found[i].Field] > 1 && f.found[i].Reason == "" {
			f.found[i].Reason = "another setting has the same name; replace it by hand"
		}
	}
	return f.found, root, nil
}

type credentialFinder struct {
	body     []byte
	lines    []int
	seen     map[*yaml.Node]bool
	anchored map[*yaml.Node]bool // values inside a node with an anchor
	found    []PlaintextCredential
}

// runtimeSettings lists the settings whose values the runtime resolves.
func (f *credentialFinder) runtimeSettings(root *yaml.Node) {
	for i, model := range sequenceItems(mappingValue(root, "models")) {
		name := itemName(model, "name", i)
		f.add(model, "api_key", "models["+name+"].api_key", SecretName("MODEL", name, "API_KEY"), false, "")
		f.add(model, "base_url", "models["+name+"].base_url", SecretName("MODEL", name, "BASE_URL"), true, "")
	}
	for i, item := range sequenceItems(mappingValue(root, "tools")) {
		name := itemName(item, "name", i)
		if (harness.BuiltinToolConfig{Name: name}).Validate() != nil {
			continue // not a tool of this runtime
		}
		f.add(item, "api_key", "tools["+name+"].api_key", SecretName(name, "API_KEY"), false, "")
		proxyReason := ""
		if name == "web_search" {
			// Legacy tools[web_search].https_proxy seeds search.providers.brave.https_proxy,
			// which websearch.Validate rejects as a secret reference.
			proxyReason = searchURLReason
		}
		f.add(item, "https_proxy", "tools["+name+"].https_proxy", SecretName(name, "HTTPS_PROXY"), true, proxyReason)
	}
	for _, provider := range mappingItems(mappingValue(mappingValue(root, "search"), "providers")) {
		prefix := "search.providers." + provider.key
		f.add(provider.value, "api_key", prefix+".api_key", SecretName("SEARCH", provider.key, "API_KEY"), false, "")
		f.add(provider.value, "base_url", prefix+".base_url", SecretName("SEARCH", provider.key, "BASE_URL"), true, searchURLReason)
		f.add(provider.value, "https_proxy", prefix+".https_proxy", SecretName("SEARCH", provider.key, "HTTPS_PROXY"), true, searchURLReason)
	}
	f.add(mappingValue(mappingValue(root, "network"), "proxy"), "url", "network.proxy.url", "NETWORK_PROXY_URL", true, "")
	for i, recipe := range sequenceItems(mappingValue(mappingValue(root, "mcp"), "host_recipes")) {
		recipeName := itemName(recipe, "id", i)
		for j, server := range sequenceItems(mappingValue(recipe, "servers")) {
			serverName := itemName(server, "name", j)
			prefix := "mcp.host_recipes[" + recipeName + "].servers[" + serverName + "]"
			for _, group := range []string{"env", "headers"} {
				values := mappingValue(server, group)
				for _, item := range mappingItems(values) {
					if credentialKey(item.key) {
						f.add(values, item.key, prefix+"."+group+"."+item.key, SecretName("MCP", serverName, item.key), false, "")
					}
				}
			}
			f.add(server, "url", prefix+".url", SecretName("MCP", serverName, "URL"), true, "")
		}
	}
}

// add lists mapping[key] when it holds a plaintext credential: any text for a
// credential setting, or an address with user information for a URL setting.
func (f *credentialFinder) add(mapping *yaml.Node, key, field, secret string, address bool, reason string) {
	value := mappingValue(mapping, key)
	if value == nil || f.seen[value] {
		return
	}
	f.seen[value] = true
	if !plaintext(value) || address && !hasUserInfo(value.Value) {
		return
	}
	if reason == "" && f.anchored[value] {
		reason = "the value is inside a YAML anchor that other settings can share; replace it by hand"
	}
	if reason == "" {
		if _, _, why := locateScalar(f.body, f.lines, value); why != "" {
			reason = why
		}
	}
	f.found = append(f.found, PlaintextCredential{Field: field, Secret: secret, Value: value.Value, Reason: reason, node: value, flow: inFlow(mapping)})
}

// plaintext reports whether a value is a credential written as text rather
// than empty, a placeholder or a reference to the environment or the vault.
func plaintext(value *yaml.Node) bool {
	if value == nil || value.Kind != yaml.ScalarNode || value.Tag != "!!str" {
		return false
	}
	text := strings.TrimSpace(value.Value)
	return text != "" && !strings.HasPrefix(text, "$") && text != "not-configured" && text != "__DEERFLOW_REDACTED__"
}

func hasUserInfo(value string) bool {
	u, err := url.Parse(strings.TrimSpace(value))
	return err == nil && u.User != nil
}

// credentialKey reports whether a setting name, environment variable or HTTP
// header looks like it holds a credential.
func credentialKey(key string) bool {
	key = strings.ReplaceAll(strings.ToLower(strings.TrimSpace(key)), "-", "_")
	switch key {
	case "api_key", "apikey", "access_key", "secret_key", "private_key", "app_secret", "client_secret", "secret", "password", "passwd",
		"token", "access_token", "refresh_token", "verification_token", "encrypt_key", "encryption_key", "credential", "credentials",
		"authorization", "proxy_authorization", "cookie":
		return true
	}
	for _, suffix := range []string{"_api_key", "_apikey", "_access_key", "_secret", "_secret_key", "_private_key", "_password", "_passwd", "_token", "_credential", "_credentials"} {
		if strings.HasSuffix(key, suffix) {
			return true
		}
	}
	return false
}

// SecretName derives a vault name from parts: upper case, with every run of
// other characters turned into one underscore, at most secrets.MaxNameBytes.
func SecretName(parts ...string) string {
	var b strings.Builder
	pending := false
	for _, part := range parts {
		for _, r := range part {
			switch {
			case r >= 'a' && r <= 'z':
				r -= 'a' - 'A'
			case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			default:
				pending = b.Len() > 0
				continue
			}
			if pending {
				b.WriteByte('_')
				pending = false
			}
			b.WriteRune(r)
		}
		pending = b.Len() > 0
	}
	name := b.String()
	if name == "" || name[0] >= '0' && name[0] <= '9' {
		name = "SECRET_" + name
	}
	name = strings.TrimRight(name, "_")
	if len(name) > secrets.MaxNameBytes {
		name = strings.TrimRight(name[:secrets.MaxNameBytes], "_")
	}
	return name
}

// locateScalar returns the byte range of a single-line scalar token in body,
// or why the value cannot be rewritten in place.
func locateScalar(body []byte, lines []int, node *yaml.Node) (int, int, string) {
	switch {
	case node.Kind == yaml.AliasNode:
		return 0, 0, "the value is a YAML alias; replace it by hand"
	case node.Anchor != "":
		return 0, 0, "the value has a YAML anchor; replace it by hand"
	case node.Style&yaml.TaggedStyle != 0:
		return 0, 0, "the value has an explicit YAML tag; replace it by hand"
	case node.Style&(yaml.LiteralStyle|yaml.FoldedStyle) != 0:
		return 0, 0, "the value is a block scalar; replace it by hand"
	case node.Line < 1 || node.Line > len(lines) || node.Column < 1:
		return 0, 0, "the value could not be located; replace it by hand"
	}
	lineEnd := len(body)
	if node.Line < len(lines) {
		lineEnd = lines[node.Line] - 1
	}
	line := body[lines[node.Line-1]:lineEnd]
	// The parser counts columns in characters.
	offset := 0
	for range node.Column - 1 {
		if offset >= len(line) {
			return 0, 0, "the value could not be located; replace it by hand"
		}
		_, size := utf8.DecodeRune(line[offset:])
		offset += size
	}
	start, end := lines[node.Line-1]+offset, -1
	switch {
	case node.Style&yaml.DoubleQuotedStyle != 0:
		for i := offset + 1; i < len(line); i++ {
			if line[i] == '\\' {
				i++
			} else if line[i] == '"' {
				end = lines[node.Line-1] + i + 1
				break
			}
		}
	case node.Style&yaml.SingleQuotedStyle != 0:
		for i := offset + 1; i < len(line); i++ {
			if line[i] == '\'' {
				if i+1 < len(line) && line[i+1] == '\'' {
					i++
					continue
				}
				end = lines[node.Line-1] + i + 1
				break
			}
		}
	default:
		if bytes.HasPrefix(line[offset:], []byte(node.Value)) {
			end = start + len(node.Value)
		}
	}
	if end < 0 {
		return 0, 0, "the value spans several lines; replace it by hand"
	}
	var decoded string
	if yaml.Unmarshal(body[start:end], &decoded) != nil || decoded != node.Value {
		return 0, 0, "the value spans several lines; replace it by hand"
	}
	return start, end, ""
}

func lineStarts(body []byte) []int {
	starts := []int{0}
	for i, b := range body {
		if b == '\n' {
			starts = append(starts, i+1)
		}
	}
	return starts
}

func inFlow(node *yaml.Node) bool { return node != nil && node.Style&yaml.FlowStyle != 0 }

// deref returns the node an alias stands for.
func deref(node *yaml.Node) *yaml.Node {
	if node != nil && node.Kind == yaml.AliasNode && node.Alias != nil {
		return node.Alias
	}
	return node
}

func mappingValue(node *yaml.Node, key string) *yaml.Node {
	node = deref(node)
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Kind == yaml.ScalarNode && node.Content[i].Value == key {
			return deref(node.Content[i+1])
		}
	}
	return nil
}

type mappingItem struct {
	key   string
	value *yaml.Node
}

func mappingItems(node *yaml.Node) []mappingItem {
	node = deref(node)
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	items := make([]mappingItem, 0, len(node.Content)/2)
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Kind == yaml.ScalarNode && node.Content[i].Value != "<<" {
			items = append(items, mappingItem{node.Content[i].Value, node.Content[i+1]})
		}
	}
	return items
}

func sequenceItems(node *yaml.Node) []*yaml.Node {
	node = deref(node)
	if node == nil || node.Kind != yaml.SequenceNode {
		return nil
	}
	items := make([]*yaml.Node, len(node.Content))
	for i, item := range node.Content {
		items[i] = deref(item)
	}
	return items
}

// itemName identifies a list item by its name (or id) setting, or its index.
func itemName(item *yaml.Node, key string, index int) string {
	if value := mappingValue(item, key); value != nil && value.Kind == yaml.ScalarNode && value.Value != "" {
		return value.Value
	}
	return strconv.Itoa(index)
}

// walkConfig visits every mapping value with the field path that names it.
func walkConfig(node *yaml.Node, path string, flow, anchored bool, visit func(field, key string, value *yaml.Node, flow, anchored bool)) {
	if node == nil {
		return
	}
	flow = flow || inFlow(node)
	anchored = anchored || node.Anchor != ""
	switch node.Kind {
	case yaml.MappingNode:
		for _, item := range mappingItems(node) {
			field := item.key
			if path != "" {
				field = path + "." + item.key
			}
			visit(field, item.key, item.value, flow, anchored)
			walkConfig(item.value, field, flow, anchored, visit)
		}
	case yaml.SequenceNode:
		for i, item := range node.Content {
			name := strconv.Itoa(i)
			if mappingValue(item, "name") != nil {
				name = itemName(item, "name", i)
			} else if mappingValue(item, "id") != nil {
				name = itemName(item, "id", i)
			}
			walkConfig(item, path+"["+name+"]", flow, anchored, visit)
		}
	}
}

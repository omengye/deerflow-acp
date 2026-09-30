package memory

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"
)

type ExtractedFact struct {
	Scope      ScopeKind
	Content    string
	Category   string
	Confidence float64
}

var credentialLike = regexp.MustCompile(`(?i)(?:sk-[a-z0-9]{12,}|[a-f0-9]{32,})`)

// FilterExtraction applies host policy to advisory model JSON. It intentionally
// prefers false negatives: a candidate must be durable, descriptive, bounded,
// and free of permission, secret, path, or current-task claims.
func FilterExtraction(raw []byte, userEnabled bool) ([]ExtractedFact, []string, error) {
	allowed := []ScopeKind{WorkspaceScope}
	if userEnabled {
		allowed = append(allowed, UserScope)
	}
	return FilterExtractionForScopes(raw, allowed)
}

// FilterExtractionForScopes constrains model-proposed facts to host-selected
// destinations. The desktop session policy uses this to avoid writing into a
// workspace scope after selecting session-isolated memory.
func FilterExtractionForScopes(raw []byte, allowed []ScopeKind) ([]ExtractedFact, []string, error) {
	if len(raw) == 0 || len(raw) > 16*1024 || !utf8.Valid(raw) {
		return nil, nil, errors.New("invalid extraction response size or encoding")
	}
	var envelope struct {
		Facts []struct {
			Scope      string  `json:"scope"`
			Durability string  `json:"durability"`
			Authority  string  `json:"authority"`
			Category   string  `json:"category"`
			Content    string  `json:"content"`
			Confidence float64 `json:"confidence"`
		} `json:"facts"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&envelope); err != nil {
		return nil, nil, err
	}
	var extra any
	if dec.Decode(&extra) != io.EOF || len(envelope.Facts) > 8 {
		return nil, nil, errors.New("invalid extraction envelope")
	}
	out := make([]ExtractedFact, 0, min(len(envelope.Facts), 4))
	var rejected []string
	for _, fact := range envelope.Facts {
		reason := ""
		lower := strings.ToLower(fact.Content)
		switch {
		case fact.Durability != "durable" || fact.Authority != "descriptive":
			reason = "authority_or_durability"
		case !slices.Contains(allowed, ScopeKind(fact.Scope)):
			reason = "scope"
		case fact.Category != "preference" && fact.Category != "profile" && fact.Category != "project":
			reason = "category"
		case fact.Confidence < .75 || fact.Confidence > 1:
			reason = "confidence"
		case fact.Content == "" || len(fact.Content) > 1024 || !utf8.ValidString(fact.Content) || strings.TrimSpace(fact.Content) != fact.Content:
			reason = "content_shape"
		case strings.ContainsAny(fact.Content, "\r\n<>") || strings.Contains(lower, "://") || strings.ContainsAny(fact.Content, `/\`):
			reason = "path_or_markup"
		case credentialLike.MatchString(lower):
			reason = "secret_like"
		}
		if reason == "" {
			for _, blocked := range []string{"permission", "approve", "authorize", "allow_once", "allow always", "deploy", "push", "delete", "password", "credential", "api key", "token", "secret", "this task", "current task", "this run", "本次", "当前任务", "授权", "批准", "允许执行", "部署", "推送", "删除", "密码", "密钥", "令牌"} {
				if strings.Contains(lower, blocked) {
					reason = "permission_task_or_secret"
					break
				}
			}
		}
		if reason != "" {
			rejected = append(rejected, reason)
			continue
		}
		if len(out) == 4 {
			rejected = append(rejected, "count_limit")
			continue
		}
		out = append(out, ExtractedFact{Scope: ScopeKind(fact.Scope), Content: fact.Content, Category: fact.Category, Confidence: fact.Confidence})
	}
	return out, rejected, nil
}

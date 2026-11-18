package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/network"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

type Embedder interface {
	Embed(context.Context, []string, bool) ([][]float64, error)
	Identity() string
}

type HTTPEmbedder struct {
	URL, Model string
	Client     *http.Client
}

func NewHTTPEmbedder(endpoint, model string, policies ...harness.NetworkConfig) (*HTTPEmbedder, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "http" && u.Scheme != "https") || model == "" {
		return nil, fmt.Errorf("invalid memory embedding configuration")
	}
	var policy harness.NetworkConfig
	if len(policies) > 0 {
		policy = policies[0]
	}
	client, err := network.NewClient(policy, "", true)
	if err != nil {
		return nil, err
	}
	client.Timeout = 15 * time.Second
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	return &HTTPEmbedder{URL: strings.TrimRight(endpoint, "/") + "/embeddings", Model: model, Client: client}, nil
}
func (e *HTTPEmbedder) Identity() string { return e.Model }
func (e *HTTPEmbedder) Embed(ctx context.Context, texts []string, query bool) ([][]float64, error) {
	raw, _ := json.Marshal(map[string]any{"model": e.Model, "input": texts, "query": query})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.URL, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	response, err := e.Client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("embedding unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode == 400 || response.StatusCode == 413 || response.StatusCode == 422 {
		return nil, ErrEmbeddingInput
	}
	if response.StatusCode != 200 {
		return nil, fmt.Errorf("embedding status %d", response.StatusCode)
	}
	var body struct {
		Model   string      `json:"model"`
		Vectors [][]float64 `json:"vectors"`
	}
	if err = json.NewDecoder(io.LimitReader(response.Body, 2<<20)).Decode(&body); err != nil {
		return nil, fmt.Errorf("invalid embedding response")
	}
	if body.Model != e.Model || len(body.Vectors) != len(texts) {
		return nil, fmt.Errorf("embedding identity or count mismatch")
	}
	// Indexing validates vectors individually so one malformed vector cannot
	// discard healthy results. Search validates dimensions/norm before ranking.
	return body.Vectors, nil
}

func validVector(v []float64) bool {
	if len(v) < 2 || len(v) > 4096 {
		return false
	}
	norm := 0.0
	for _, x := range v {
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return false
		}
		norm += x * x
	}
	return norm > 0
}

func cosine(a, b []float64) float64 {
	if len(a) != len(b) || !validVector(a) || !validVector(b) {
		return -1
	}
	dot, aa, bb := 0.0, 0.0, 0.0
	for i, x := range a {
		dot += x * b[i]
		aa += x * x
		bb += b[i] * b[i]
	}
	return dot / math.Sqrt(aa*bb)
}

// Cosine values are model-specific, not probabilities. This floor is calibrated
// against the pinned Chinese model's synthetic release corpus. Hits remain
// descriptive candidates, not proof that a question has a relevant answer.
func semanticFloor(model string) float64 {
	if model == "BAAI/bge-small-zh-v1.5@46fbe35" {
		return 0.40
	}
	return 0.72
}

// HybridSearch fuses lexical and semantic ranks. Every returned hit is checked
// against a single authoritative snapshot after embedding I/O; index lag cannot
// hide newly committed lexical/profile facts or expose an obsolete revision.
func (s *Store) HybridSearch(ctx context.Context, scope Scope, query string, limit int) ([]Fact, int64, error) {
	if !utf8.ValidString(query) || len(query) > 1024 || limit < 1 || limit > 100 {
		return nil, 0, harness.ErrInvalidInput
	}
	embedder, _, _ := s.embeddingSnapshot()
	lexical, revision, err := s.searchLexical(ctx, scope, searchTerms(query), max(limit, 24), scope.kind == GlobalScope)
	if err != nil || embedder == nil {
		return lexical[:min(len(lexical), limit)], revision, err
	}
	callCtx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	queryVectors, embedErr := embedder.Embed(callCtx, []string{query}, true)
	if embedErr != nil || len(queryVectors) != 1 {
		return lexical[:min(len(lexical), limit)], revision, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, 0, err
	}
	defer tx.Rollback()
	revision, err = checkScope(ctx, tx, scope, false)
	if err != nil {
		return nil, 0, err
	}
	type hit struct {
		fact              Fact
		similarity, score float64
	}
	selected := map[string]*hit{}
	for i, f := range lexical {
		current, getErr := readFact(ctx, tx, scope, f.ID)
		if getErr == nil && current.Revision == f.Revision {
			selected[f.ID] = &hit{fact: current, score: 1.0 / float64(60+i+1)}
		}
	}
	// Read vector identity first, then fetch the small number of ranked heads.
	rows, err := tx.QueryContext(ctx, `SELECT f.id,v.vector FROM memory_facts f JOIN memory_vectors v ON v.fact_id=f.id AND v.revision=f.head_revision WHERE f.scope_key=? AND f.deleted=0 AND v.model=? ORDER BY f.id LIMIT 10000`, scope.key, embedder.Identity())
	if err != nil {
		return nil, 0, err
	}
	var semantic []hit
	for rows.Next() {
		var id string
		var raw []byte
		if err = rows.Scan(&id, &raw); err != nil {
			rows.Close()
			return nil, 0, err
		}
		var v []float64
		if json.Unmarshal(raw, &v) != nil {
			continue
		}
		score := cosine(queryVectors[0], v)
		if score >= semanticFloor(embedder.Identity()) {
			semantic = append(semantic, hit{fact: Fact{ID: id}, similarity: score})
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, 0, err
	}
	sort.Slice(semantic, func(i, j int) bool {
		if semantic[i].similarity == semantic[j].similarity {
			return semantic[i].fact.ID < semantic[j].fact.ID
		}
		return semantic[i].similarity > semantic[j].similarity
	})
	for i, item := range semantic[:min(len(semantic), 24)] {
		f, err := readFact(ctx, tx, scope, item.fact.ID)
		if err != nil {
			return nil, 0, err
		}
		if selected[f.ID] == nil {
			selected[f.ID] = &hit{fact: f}
		}
		// Chinese bigram overlap is useful for identifiers but noisy for
		// paraphrases. Give the independently ranked semantic channel twice
		// the weight; neither absolute cosine nor lexical count is confidence.
		selected[f.ID].score += 2.0 / float64(60+i+1)
	}
	ordered := make([]*hit, 0, len(selected))
	for _, h := range selected {
		ordered = append(ordered, h)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].score == ordered[j].score {
			if ordered[i].fact.UpdatedAt.Equal(ordered[j].fact.UpdatedAt) {
				return ordered[i].fact.ID < ordered[j].fact.ID
			}
			return ordered[i].fact.UpdatedAt.After(ordered[j].fact.UpdatedAt)
		}
		return ordered[i].score > ordered[j].score
	})
	result := make([]Fact, 0, limit)
	for _, h := range ordered[:min(limit, len(ordered))] {
		result = append(result, h.fact)
	}
	return result, revision, nil
}

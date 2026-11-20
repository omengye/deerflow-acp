package websearch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/network"
	"github.com/omengye/deerflow-acp/go-harness/internal/secrets"
)

type Request struct {
	Query            string `json:"query"`
	MaxResults       int    `json:"max_results,omitempty"`
	TimeRange        string `json:"time_range,omitempty" jsonschema:"description=day/week/month/year; SearXNG supports day/month/year only"`
	Provider         string `json:"provider,omitempty" jsonschema:"description=Optional brave/deepseek/searxng; honor the user's explicit choice, otherwise omit to use their saved default"`
	Language         string `json:"language,omitempty"`
	OriginalQuestion string `json:"ori_question,omitempty"`
	NeedContent      bool   `json:"need_content,omitempty"`
}

type Result struct {
	Title       string   `json:"title"`
	URL         string   `json:"url"`
	Content     string   `json:"content"`
	Snippet     string   `json:"snippet,omitempty"`
	Summary     string   `json:"summary,omitempty"`
	PageContent string   `json:"page_content,omitempty"`
	Date        string   `json:"date,omitempty"`
	Score       *float64 `json:"score,omitempty"`
	RerankScore *float64 `json:"rerank_score,omitempty"`
	Authority   *int     `json:"authority,omitempty"`
	SiteName    string   `json:"site_name,omitempty"`
}

type Response struct {
	Query        string   `json:"query"`
	Provider     string   `json:"provider"`
	TotalResults int      `json:"total_results"`
	Results      []Result `json:"results"`
}

type ConfigurationError struct{ Provider string }

func (e *ConfigurationError) Error() string {
	return Label(e.Provider) + " 尚未配置，请在设置 → 工具 → web_search 中补齐配置后继续。SearXNG 实例地址必填，Key 选填。"
}
func (e *ConfigurationError) ToolResult() string {
	body, _ := json.Marshal(map[string]any{"status": "requires_configuration", "provider": e.Provider, "message": e.Error()})
	return string(body)
}

type Provider interface {
	Search(context.Context, harness.SearchProviderConfig, Request) ([]Result, error)
}

var providers = map[string]Provider{"brave": braveProvider{}, "deepseek": deepseekProvider{}, "searxng": searxngProvider{}}

func (s *Snapshot) Search(ctx context.Context, request Request) (Response, error) {
	name := request.Provider
	if name == "" {
		name = s.Ref.DefaultProvider
	}
	adapter, ok := providers[name]
	if !ok {
		return Response{}, errors.New("provider must be brave, deepseek or searxng")
	}
	if strings.TrimSpace(request.Query) == "" || len(request.Query) > 4096 {
		return Response{}, errors.New("query must contain 1..4096 bytes")
	}
	if request.TimeRange != "" && request.TimeRange != "day" && request.TimeRange != "week" && request.TimeRange != "month" && request.TimeRange != "year" {
		return Response{}, errors.New("time_range must be day, week, month or year")
	}
	if request.MaxResults == 0 {
		request.MaxResults = 5
	}
	if request.MaxResults < 1 || request.MaxResults > 20 {
		return Response{}, errors.New("max_results must be 1..20")
	}
	p := s.providers[name]
	p.Network = s.network
	if !configured(name, p) {
		return Response{}, &ConfigurationError{Provider: name}
	}
	p, err := s.credential(ctx, p)
	if err != nil {
		return Response{}, err
	}
	results, err := adapter.Search(ctx, p, request)
	if err != nil {
		return Response{}, err
	}
	out := Response{Query: request.Query, Provider: name, Results: []Result{}}
	seen := map[string]bool{}
	for _, result := range results {
		u, err := url.Parse(result.URL)
		if err != nil || !validURL(u) || len(result.URL) > 4096 || seen[result.URL] {
			continue
		}
		seen[result.URL] = true
		result.Title, result.Content = bounded(result.Title, 512), bounded(result.Content, 2000)
		result.Summary, result.Snippet, result.PageContent = bounded(result.Summary, 2000), bounded(result.Snippet, 2000), bounded(result.PageContent, 16000)
		out.Results = append(out.Results, result)
		if len(out.Results) == request.MaxResults {
			break
		}
	}
	out.TotalResults = len(out.Results)
	s.verified(ctx, name)
	return out, nil
}

// SearchInteractive allows a desktop-attached invocation to wait for its first
// configuration. SDK callers retain an immediate structured configuration error.
func (s *Snapshot) SearchInteractive(ctx context.Context, request Request) (Response, error) {
	response, err := s.Search(ctx, request)
	var missing *ConfigurationError
	if !errors.As(err, &missing) || s.manager == nil {
		return response, err
	}
	p, err := s.manager.AwaitConfiguration(ctx, missing.Provider)
	if err != nil {
		return Response{}, err
	}
	copy := &Snapshot{Ref: s.Ref, providers: map[string]harness.SearchProviderConfig{}, manager: s.manager, network: s.network}
	for name, value := range s.providers {
		copy.providers[name] = value
	}
	copy.providers[missing.Provider] = p
	return copy.Search(ctx, request)
}

type braveProvider struct{}

func (braveProvider) Search(ctx context.Context, p harness.SearchProviderConfig, in Request) ([]Result, error) {
	q := url.Values{"q": {in.Query}, "count": {fmt.Sprint(in.MaxResults)}, "country": {"us"}, "search_lang": {"en"}, "safesearch": {"moderate"}, "text_decorations": {"false"}}
	if in.Language != "" {
		q.Set("search_lang", in.Language)
	}
	if in.TimeRange != "" {
		q.Set("freshness", map[string]string{"day": "pd", "week": "pw", "month": "pm", "year": "py"}[in.TimeRange])
	}
	body, err := requestJSON(ctx, p, http.MethodGet, p.BaseURL+"/res/v1/web/search?"+q.Encode(), nil, map[string]string{"X-Subscription-Token": p.APIKey})
	if err != nil {
		return nil, err
	}
	var data struct {
		Web *struct {
			Results []struct{ Title, URL, Description string }
		}
	}
	if json.Unmarshal(body, &data) != nil || data.Web == nil {
		return nil, errors.New("invalid Brave Search response")
	}
	results := []Result{}
	for _, r := range data.Web.Results {
		results = append(results, Result{Title: r.Title, URL: r.URL, Content: r.Description})
	}
	return results, nil
}

type deepseekProvider struct{}

func (deepseekProvider) Search(ctx context.Context, p harness.SearchProviderConfig, in Request) ([]Result, error) {
	query := in.Query
	if in.Language != "" {
		query += " lang:" + in.Language
	}
	if in.TimeRange != "" {
		now := time.Now().UTC()
		var after time.Time
		switch in.TimeRange {
		case "day":
			after = now.AddDate(0, 0, -1)
		case "week":
			after = now.AddDate(0, 0, -7)
		case "month":
			after = now.AddDate(0, -1, 0)
		case "year":
			after = now.AddDate(-1, 0, 0)
		}
		query += " after:" + after.Format("2006-01-02")
	}
	if utf8.RuneCountInString(query) > 128 || utf8.RuneCountInString(in.OriginalQuestion) > 128 {
		return nil, errors.New("DeepSeek query (including language/time filters) and ori_question must each be at most 128 Unicode characters; shorten the query")
	}
	payload := map[string]any{"query": query, "count": in.MaxResults, "need_content": in.NeedContent}
	if in.OriginalQuestion != "" {
		payload["ori_question"] = in.OriginalQuestion
	}
	body, err := requestJSON(ctx, p, http.MethodPost, p.BaseURL+"/v1/tools/search", payload, map[string]string{"Authorization": "Bearer " + p.APIKey})
	if err != nil {
		return nil, err
	}
	var data struct {
		Results *[]struct {
			Title       string   `json:"title"`
			URL         string   `json:"url"`
			Snippet     string   `json:"snippet"`
			Summary     string   `json:"summary"`
			Content     string   `json:"content"`
			Date        string   `json:"date"`
			RerankScore *float64 `json:"rerank_score"`
			Authority   *int     `json:"authority"`
			SiteName    string   `json:"site_name"`
		}
	}
	if json.Unmarshal(body, &data) != nil || data.Results == nil {
		return nil, errors.New("invalid DeepSeek Search response")
	}
	results := []Result{}
	for _, r := range *data.Results {
		content := r.Summary
		if content == "" {
			content = r.Snippet
		}
		results = append(results, Result{Title: r.Title, URL: r.URL, Content: content, Snippet: r.Snippet, Summary: r.Summary, PageContent: r.Content, Date: r.Date, RerankScore: r.RerankScore, Authority: r.Authority, SiteName: r.SiteName})
	}
	return results, nil
}

type searxngProvider struct{}

func (searxngProvider) Search(ctx context.Context, p harness.SearchProviderConfig, in Request) ([]Result, error) {
	if in.TimeRange == "week" {
		return nil, errors.New("SearXNG does not support week; choose day/month/year or omit time_range")
	}
	q := url.Values{"q": {in.Query}, "format": {"json"}}
	if in.Language != "" {
		q.Set("language", in.Language)
	}
	if in.TimeRange != "" {
		q.Set("time_range", in.TimeRange)
	}
	headers := map[string]string{}
	if p.APIKey != "" {
		value := p.APIKey
		if p.AuthHeader == "Authorization" {
			value = "Bearer " + value
		}
		headers[p.AuthHeader] = value
	}
	body, err := requestJSON(ctx, p, http.MethodGet, p.BaseURL+"/search?"+q.Encode(), nil, headers)
	if err != nil {
		return nil, err
	}
	var data struct{ Results *[]Result }
	if json.Unmarshal(body, &data) != nil || data.Results == nil {
		return nil, errors.New("SearXNG returned no JSON results field; enable JSON output on the instance")
	}
	results := *data.Results
	sort.SliceStable(results, func(i, j int) bool {
		a, b := 0.0, 0.0
		if results[i].Score != nil {
			a = *results[i].Score
		}
		if results[j].Score != nil {
			b = *results[j].Score
		}
		return a > b
	})
	return results, nil
}

// Fetch uses DeepSeek only when explicitly selected. Search and fetch defaults
// are independent, so selecting a search provider cannot add remote fetch calls.
func (s *Snapshot) Fetch(ctx context.Context, target string) (string, error) {
	u, err := url.Parse(target)
	if err != nil || !validURL(u) || len(target) > 8192 {
		return "", errors.New("invalid HTTP(S) fetch URL")
	}
	p := s.providers["deepseek"]
	p.Network = s.network
	if !configured("deepseek", p) {
		return "", &ConfigurationError{Provider: "deepseek"}
	}
	if p, err = s.credential(ctx, p); err != nil {
		return "", err
	}
	body, err := requestJSON(ctx, p, http.MethodPost, p.BaseURL+"/v1/tools/fetch", map[string]string{"url": target}, map[string]string{"Authorization": "Bearer " + p.APIKey})
	if err != nil {
		return "", err
	}
	var data struct {
		Title   string `json:"title"`
		Content string `json:"content"`
	}
	if json.Unmarshal(body, &data) != nil || strings.TrimSpace(data.Content) == "" {
		return "", errors.New("DeepSeek Fetch returned no readable content")
	}
	return data.Content, nil
}

func (s *Snapshot) FetchInteractive(ctx context.Context, target string) (string, error) {
	content, err := s.Fetch(ctx, target)
	var missing *ConfigurationError
	if !errors.As(err, &missing) || s.manager == nil {
		return content, err
	}
	p, err := s.manager.AwaitConfiguration(ctx, "deepseek")
	if err != nil {
		return "", err
	}
	copy := &Snapshot{Ref: s.Ref, providers: map[string]harness.SearchProviderConfig{}, manager: s.manager, network: s.network}
	for name, value := range s.providers {
		copy.providers[name] = value
	}
	copy.providers["deepseek"] = p
	return copy.Fetch(ctx, target)
}

// credential replaces an api_key written ${secret:NAME} with the secret's
// current value for one request.
func (s *Snapshot) credential(ctx context.Context, p harness.SearchProviderConfig) (harness.SearchProviderConfig, error) {
	name, ok, err := secrets.Reference(p.APIKey)
	if err != nil || !ok {
		return p, err
	}
	var lookup SecretLookup
	if s.manager != nil {
		s.manager.mu.Lock()
		lookup = s.manager.secrets
		s.manager.mu.Unlock()
	}
	if lookup == nil {
		return p, fmt.Errorf("the search api_key refers to secret %s, but no secret vault is open", name)
	}
	value, err := lookup(ctx, name)
	if err != nil {
		return p, err
	}
	if value == "" || strings.ContainsAny(value, "\r\n\x00") || len(value) > 16384 {
		return p, fmt.Errorf("secret %s is not a usable search credential", name)
	}
	p.APIKey = value
	return p, nil
}

func requestJSON(ctx context.Context, p harness.SearchProviderConfig, method, target string, payload any, headers map[string]string) ([]byte, error) {
	client, err := network.NewClient(p.Network, p.HTTPSProxy, false)
	if err != nil {
		return nil, err
	}
	defer client.CloseIdleConnections()
	client.Timeout = time.Duration(p.Timeout) * time.Second
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 || !validURL(req.URL) {
			return errors.New("unsupported search redirect")
		}
		if len(via) > 0 && (req.URL.Scheme != via[0].URL.Scheme || req.URL.Host != via[0].URL.Host) {
			req.Header.Del("Authorization")
			req.Header.Del("X-API-Key")
			req.Header.Del("X-Subscription-Token")
		}
		return nil
	}
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return nil, errors.New("invalid search request")
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, errors.New("invalid search request")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "DeerFlow-Search/1.0")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	response, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, harness.ClassifiedToolFailure("network", "search request failed; check the provider, network, proxy or timeout")
	}
	defer response.Body.Close()
	if response.StatusCode >= 400 {
		code := "remote"
		if response.StatusCode == 401 || response.StatusCode == 403 {
			code = "permission"
		}
		if response.StatusCode == 429 {
			code = "rate_limit"
		}
		if response.StatusCode >= 500 {
			code = "network"
		}
		return nil, harness.ClassifiedToolFailure(code, fmt.Sprintf("search service returned HTTP %d; check credentials, quota and JSON availability", response.StatusCode))
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, (2<<20)+1))
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, harness.ClassifiedToolFailure("network", "search response could not be read")
	}
	if len(data) > 2<<20 {
		return nil, errors.New("search response exceeds 2 MiB")
	}
	return data, nil
}

func bounded(value string, limit int) string {
	runes := []rune(value)
	if len(runes) > limit {
		return string(runes[:limit]) + "…"
	}
	return value
}

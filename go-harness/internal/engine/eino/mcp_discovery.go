package eino

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/mcp"
	"github.com/omengye/deerflow-acp/go-harness/internal/toolidentity"
)

const mcpPinsKey = "deerflow_mcp_batch_v2"
const directMCPTools = 10
const searchMCPResults = 5

type mcpBatchKey struct{}
type mcpInvocationKey struct{}

type mcpDiscovery struct {
	adk.BaseChatModelAgentMiddleware
	lease         *mcp.Lease
	policy        harness.ToolPolicy
	full, restore bool
	origin        string
}

func withMCPDiscovery(handlers []adk.ChatModelAgentMiddleware, node *compose.ToolsNodeConfig, ext RunExtensions, policy harness.ToolPolicy) []adk.ChatModelAgentMiddleware {
	if ext.MCP == nil {
		return handlers
	}
	d := &mcpDiscovery{lease: ext.MCP, policy: policy, full: ext.MCPExposure == "full", restore: ext.MCPHostRestore, origin: ext.MCPOrigin}
	node.BatchTools = d.batch
	return append([]adk.ChatModelAgentMiddleware{d}, handlers...)
}

// Each invocation, including DeepAgent's general-purpose child, owns its
// model-step selection. Concurrent tool calls may only stage future matches.
type mcpInvocation struct {
	base     map[string]bool
	mu       sync.Mutex
	selected map[string]bool
	shown    map[string]mcp.ToolPin
	search   bool
}

func (d *mcpDiscovery) directory() []mcp.CatalogServer {
	var out []mcp.CatalogServer
	for _, s := range d.lease.Catalog() {
		var allowed []mcp.CatalogTool
		for _, t := range s.Tools {
			if d.policy.Allows(t.Name) {
				allowed = append(allowed, t)
			}
		}
		if len(allowed) == 0 && d.policy.Allowlist != nil {
			continue
		}
		s.Tools = allowed
		out = append(out, s)
	}
	return out
}

func (d *mcpDiscovery) BeforeAgent(ctx context.Context, in *adk.ChatModelAgentContext[*schema.Message]) (context.Context, *adk.ChatModelAgentContext[*schema.Message], error) {
	i := &mcpInvocation{base: map[string]bool{}, selected: map[string]bool{}}
	out := *in
	out.Tools = slices.Clone(in.Tools)
	for _, t := range out.Tools {
		info, err := t.Info(ctx)
		if err != nil {
			return ctx, nil, err
		}
		if info.Name == "tool_search" || info.Name == "mcp_discover" {
			return ctx, nil, fmt.Errorf("reserved MCP tool %q", info.Name)
		}
		i.base[info.Name] = true
	}
	servers := d.directory()
	count := 0
	for _, s := range servers {
		count += len(s.Tools)
		if !s.Ready {
			i.search = true
		}
	}
	i.search = i.search || count > directMCPTools
	for _, name := range []string{"tool_search", "mcp_discover"} {
		if len(servers) > 0 && d.policy.Allows(name) {
			out.Tools = append(out.Tools, &mcpDiscoveryTool{Native: toolidentity.Builtin(name), name: name, discovery: d})
		}
	}
	return context.WithValue(ctx, mcpInvocationKey{}, i), &out, nil
}

type mcpDiscoveryTool struct {
	toolidentity.Native
	name      string
	discovery *mcpDiscovery
}

func (t *mcpDiscoveryTool) Info(context.Context) (*schema.ToolInfo, error) {
	params := map[string]*schema.ParameterInfo{"query": {Type: schema.String, Desc: "Tool name, keywords, or a task description."}}
	desc := "Search the authorized local MCP catalog by keyword or exact tool name. Matching tools are available on your next step. For a cold service, use mcp_discover with one of the returned server names."
	if t.name == "mcp_discover" {
		params["server"] = &schema.ParameterInfo{Type: schema.String, Required: true, Desc: "Exact server name from the authorized MCP catalog."}
		desc = "Start or reuse one specified local MCP service and discover its tools. Optionally narrow by query. Call the selected tool by its original exposed name on the next step."
	}
	return &schema.ToolInfo{Name: t.name, Desc: desc, ParamsOneOf: schema.NewParamsOneOfByParams(params)}, nil
}

type mcpSearchResult struct {
	Matches    []string       `json:"matches"`
	Candidates []mcpCandidate `json:"candidates,omitempty"`
	Servers    []string       `json:"servers,omitempty"`
	Discover   []string       `json:"discover,omitempty"`
	Status     string         `json:"status,omitempty"`
}

type mcpCandidate struct {
	Name        string `json:"name"`
	Server      string `json:"server"`
	Description string `json:"description,omitempty"`
}

func (t *mcpDiscoveryTool) InvokableRun(ctx context.Context, args string, _ ...tool.Option) (string, error) {
	var in struct{ Server, Query string }
	if err := json.Unmarshal([]byte(args), &in); err != nil || len(in.Query) > 2048 {
		return "", harness.MarkToolNotExecuted(harness.ErrInvalidInput)
	}
	i, ok := ctx.Value(mcpInvocationKey{}).(*mcpInvocation)
	if !ok {
		return "", harness.MarkToolNotExecuted(harness.ErrPermissionDenied)
	}
	d := t.discovery
	servers := d.directory()
	if t.name == "mcp_discover" {
		if !slices.ContainsFunc(servers, func(s mcp.CatalogServer) bool { return s.CanDiscover && s.Name == in.Server }) {
			return "", harness.MarkToolNotExecuted(harness.ErrPermissionDenied)
		}
		if err := d.lease.Discover(ctx, in.Server); err != nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			result, _ := json.Marshal(mcpSearchResult{Status: err.Error()})
			return string(result), nil
		}
		servers = d.directory()
	}
	type match struct {
		mcpCandidate
		score int
		ready bool
	}
	var matches []match
	query := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(in.Query, "select:")))
	var result mcpSearchResult
	for _, s := range servers {
		if t.name == "mcp_discover" && s.Name != in.Server {
			continue
		}
		label := s.Name
		if s.Description != "" {
			label += " — " + s.Description
		}
		result.Servers = append(result.Servers, label)
		if !s.Ready {
			result.Discover = append(result.Discover, s.Name)
		}
		for _, c := range s.Tools {
			score := 0
			name, original := strings.ToLower(c.Name), strings.ToLower(c.Original)
			for _, word := range strings.Fields(query) {
				switch {
				case name == word || original == word:
					score += 100
				case strings.Contains(original, word) || strings.Contains(name, word):
					score += 8
				case strings.Contains(strings.ToLower(c.Description), word):
					score += 4
				case strings.Contains(strings.ToLower(label), word):
					score++
				}
			}
			if query == "" || score > 0 {
				matches = append(matches, match{mcpCandidate{c.Name, s.Name, c.Description}, score, c.HasDefinition()})
			}
		}
	}
	sort.Slice(matches, func(a, b int) bool {
		if matches[a].score != matches[b].score {
			return matches[a].score > matches[b].score
		}
		return matches[a].Name < matches[b].Name
	})
	i.mu.Lock()
	for _, candidate := range matches[:min(len(matches), searchMCPResults)] {
		result.Matches = append(result.Matches, candidate.Name)
		result.Candidates = append(result.Candidates, candidate.mcpCandidate)
		if candidate.ready {
			i.selected[candidate.Name] = true
		}
	}
	i.mu.Unlock()
	if len(result.Matches) == 0 {
		result.Status = "No matching tools. Use an exact server name for discovery, or search with different keywords."
	}
	data, _ := json.Marshal(result)
	return string(data), nil
}

func (d *mcpDiscovery) BeforeModelRewriteState(ctx context.Context, state *adk.ChatModelAgentState, _ *adk.ModelContext) (context.Context, *adk.ChatModelAgentState, error) {
	i := ctx.Value(mcpInvocationKey{}).(*mcpInvocation)
	servers := d.directory()
	i.mu.Lock()
	defer i.mu.Unlock()
	// Native Eino checkpoints retain previously published ToolInfos and
	// completed tool results even if a sibling call suspended the batch.
	infos := make([]*schema.ToolInfo, 0, len(state.ToolInfos))
	for _, info := range state.ToolInfos {
		if info.Name == "tool_search" || info.Name == "mcp_discover" {
			if strings.Contains(info.Desc, " Authorized MCP services: ") {
				i.search = true
			}
			continue
		}
		if !i.base[info.Name] && strings.HasPrefix(info.Name, "mcp_") {
			i.selected[info.Name] = true
			continue
		}
		infos = append(infos, info)
	}
	for j := len(state.Messages) - 1; j >= 0; j-- {
		msg := state.Messages[j]
		if msg.Role != schema.Tool {
			break
		}
		if msg.ToolName == "tool_search" || msg.ToolName == "mcp_discover" {
			var r mcpSearchResult
			if json.Unmarshal([]byte(msg.Content), &r) == nil {
				for _, name := range r.Matches {
					i.selected[name] = true
				}
			}
		}
	}
	i.shown = map[string]mcp.ToolPin{}
	cold := false
	var labels []string
	for _, s := range servers {
		if !s.Ready {
			cold = true
		}
		label := s.Name
		if s.Description != "" {
			label += " (" + s.Description + ")"
		}
		labels = append(labels, label)
		for _, c := range s.Tools {
			if i.base[c.Name] {
				return ctx, nil, fmt.Errorf("duplicate MCP tool %q", c.Name)
			}
			if c.HasDefinition() && (d.full || !i.search || !d.policy.Allows("tool_search") || i.selected[c.Name]) {
				info, err := c.Definition()
				if err != nil {
					return ctx, nil, err
				}
				infos = append(infos, info)
				i.shown[c.Name] = c.ToolPin
			}
		}
	}
	for _, name := range []string{"tool_search", "mcp_discover"} {
		if len(servers) == 0 || !d.policy.Allows(name) || (name == "tool_search" && (!i.search || d.full) && !cold) || (name == "mcp_discover" && !cold) {
			continue
		}
		meta, _ := (&mcpDiscoveryTool{name: name}).Info(ctx)
		meta.Desc += " Authorized MCP services: " + strings.Join(labels, ", ") + "."
		infos = append(infos, meta)
		i.shown[name] = mcp.ToolPin{Name: name}
	}
	state.ToolInfos = infos
	return ctx, state, nil
}

func (d *mcpDiscovery) AfterModelRewriteState(ctx context.Context, state *adk.ChatModelAgentState, _ *adk.ModelContext) (context.Context, *adk.ChatModelAgentState, error) {
	if len(state.Messages) == 0 {
		return ctx, state, nil
	}
	last := len(state.Messages) - 1
	msg := *state.Messages[last]
	if msg.Role != schema.Assistant {
		return ctx, state, nil
	}
	// Overwrite any provider-supplied field. Only host-visible tool identities
	// can authorize an adapter when the batch is dispatched or restored.
	msg.Extra = maps.Clone(msg.Extra)
	delete(msg.Extra, mcpPinsKey)
	i := ctx.Value(mcpInvocationKey{}).(*mcpInvocation)
	pins := map[string]mcp.ToolPin{}
	i.mu.Lock()
	for _, call := range msg.ToolCalls {
		if pin, ok := i.shown[call.Function.Name]; ok {
			pins[call.Function.Name] = pin
		}
	}
	i.mu.Unlock()
	if len(pins) > 0 {
		saveMCPPins(&msg, pins)
	}
	state.Messages[last] = &msg
	return ctx, state, nil
}

func saveMCPPins(msg *schema.Message, pins map[string]mcp.ToolPin) {
	if msg.Extra == nil {
		msg.Extra = map[string]any{}
	}
	data, _ := json.Marshal(pins)
	msg.Extra[mcpPinsKey] = string(data)
}

func (d *mcpDiscovery) batch(ctx context.Context, msg *schema.Message, calls []schema.ToolCall) (context.Context, []tool.BaseTool, error) {
	raw, _ := msg.Extra[mcpPinsKey].(string)
	if raw == "" && !slices.ContainsFunc(calls, func(c schema.ToolCall) bool {
		return c.Function.Name == "tool_search" || c.Function.Name == "mcp_discover"
	}) {
		return ctx, nil, nil
	}
	var pins map[string]mcp.ToolPin
	if raw != "" && json.Unmarshal([]byte(raw), &pins) != nil {
		return ctx, nil, harness.ErrResourcesIncompatible
	}
	base, _ := ctx.Value(availableToolsKey{}).(map[string]bool)
	available := maps.Clone(base)
	if available == nil {
		available = map[string]bool{}
	}
	batch := map[string]tool.BaseTool{}
	var extra []tool.BaseTool
	for _, call := range calls {
		name := call.Function.Name
		pin, ok := pins[name]
		if name == "tool_search" || name == "mcp_discover" {
			available[name] = ok && pin.Name == name && base[name] && d.policy.Allows(name)
			continue
		}
		if !ok || batch[name] != nil {
			continue
		}
		if pin.Name != name || !d.policy.Allows(name) || base[name] {
			return ctx, nil, harness.ErrPermissionDenied
		}
		adapter, real, err := d.lease.Pin(ctx, pin)
		if err != nil && d.restore {
			adapter, real, err = d.lease.RestorePin(ctx, d.origin, pin)
		}
		if err != nil {
			if resumed, _, _ := compose.GetInterruptState[any](ctx); resumed {
				return ctx, nil, fmt.Errorf("%w: %s", harness.ErrResourcesWaiting, name)
			}
			continue
		} // unknown-tool middleware records a no-effect refusal
		pins[name] = real
		available[name] = true
		batch[name] = adapter
		extra = append(extra, adapter)
	}
	saveMCPPins(msg, pins)
	ctx = context.WithValue(ctx, availableToolsKey{}, available)
	return context.WithValue(ctx, mcpBatchKey{}, batch), extra, nil
}

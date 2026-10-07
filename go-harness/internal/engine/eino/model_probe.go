package eino

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

type ProbeCheck struct {
	OK        bool   `json:"ok"`
	Code      string `json:"code"`
	LatencyMS int64  `json:"latency_ms"`
}

type AgentProbe struct {
	Streaming ProbeCheck `json:"streaming"`
	Tools     ProbeCheck `json:"tools"`
	Usage     bool       `json:"usage_reported"`
	Requests  int        `json:"requests"`
}

// ProbeAgentCapabilities makes at most two bounded requests and never executes
// a returned tool. Callers provide a deadline; SDK transport retries are disabled.
func ProbeAgentCapabilities(ctx context.Context, provider, name, baseURL, apiKey string) (AgentProbe, error) {
	chat, err := newModelWithRetryPolicy(ctx, Config{Provider: provider, Model: name, BaseURL: baseURL, APIKey: apiKey}, name, true)
	if err != nil {
		return AgentProbe{}, err
	}
	return probeAgent(ctx, chat), nil
}

func probeAgent(ctx context.Context, chat model.ToolCallingChatModel) AgentProbe {
	result := AgentProbe{Streaming: ProbeCheck{Code: "not_run"}, Tools: ProbeCheck{Code: "not_run"}}
	started := time.Now()
	result.Requests++
	stream, err := chat.Stream(ctx, []*schema.Message{schema.UserMessage("Reply with the word OK only.")}, model.WithMaxTokens(512))
	var bytes int
	if err == nil && stream != nil {
		for {
			var message *schema.Message
			message, err = stream.Recv()
			if errors.Is(err, io.EOF) {
				err = nil
				break
			}
			if err != nil {
				break
			}
			if message == nil {
				continue
			}
			bytes += len(message.Content)
			result.Usage = result.Usage || message.ResponseMeta != nil && message.ResponseMeta.Usage != nil
			if bytes > 8192 {
				err = errors.New("probe output limit")
				break
			}
		}
		stream.Close()
	}
	result.Streaming = probeCheck(ctx, started, err, bytes > 0)
	if ctx.Err() != nil {
		result.Tools.Code = "cancelled"
		return result
	}
	bound, err := chat.WithTools([]*schema.ToolInfo{{Name: "deerflow_probe", Desc: "Report probe marker. Call exactly once with marker=deerflow-ok.", ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{"marker": {Type: schema.String, Required: true}})}})
	if err != nil {
		result.Tools.Code = "tool_schema_rejected"
		return result
	}
	started = time.Now()
	result.Requests++
	message, err := bound.Generate(ctx, []*schema.Message{schema.UserMessage("Call deerflow_probe with marker deerflow-ok. Do not answer in text.")}, model.WithMaxTokens(512))
	valid := false
	if message != nil {
		result.Usage = result.Usage || message.ResponseMeta != nil && message.ResponseMeta.Usage != nil
		if len(message.ToolCalls) == 1 {
			call := message.ToolCalls[0]
			var args map[string]string
			valid = call.Function.Name == "deerflow_probe" && json.Unmarshal([]byte(call.Function.Arguments), &args) == nil && strings.TrimSpace(args["marker"]) == "deerflow-ok"
		}
	}
	result.Tools = probeCheck(ctx, started, err, valid)
	return result
}

func probeCheck(ctx context.Context, started time.Time, err error, valid bool) ProbeCheck {
	check := ProbeCheck{LatencyMS: time.Since(started).Milliseconds(), Code: "unexpected_response"}
	if ctx.Err() != nil {
		check.Code = "cancelled"
	} else if err != nil {
		check.Code = "request_failed"
	} else if valid {
		check.OK = true
		check.Code = "passed"
	}
	return check
}

// Package launch shares optional runtime flags between the stdio and daemon
// executables. It contains no transport or provider initialization.
package launch

import (
	"flag"
	"fmt"
	"strings"

	deerflow "github.com/omengye/deerflow-acp/go-harness"
	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func RuntimeFlags(flags *flag.FlagSet, cfg *deerflow.Config) {
	sandboxFlags(flags, &cfg.Sandbox)
	skillsFlags(flags, cfg)
	flags.Func("vision-model", "explicit model ID supporting image input (repeatable; must also be the default or an allow-model)", func(value string) error {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("vision model ID is empty")
		}
		cfg.Media.VisionModels = append(cfg.Media.VisionModels, value)
		return nil
	})
	budget := harness.DefaultBudgetLimits()
	cfg.Budget = &budget
	flags.IntVar(&budget.MaxModelCalls, "max-model-calls", budget.MaxModelCalls, "model calls across main and child agents; 0 disables")
	flags.IntVar(&budget.MaxToolCalls, "max-tool-calls", budget.MaxToolCalls, "tool calls across main and child agents; 0 disables")
	flags.Int64Var(&budget.MaxTokens, "max-tokens", budget.MaxTokens, "aggregate run token budget, estimated until provider usage arrives; 0 disables")
	flags.IntVar(&budget.MaxOutputTokens, "max-output-tokens", budget.MaxOutputTokens, "output token cap per model request; 0 disables")
	flags.DurationVar(&budget.Timeout, "run-timeout", budget.Timeout, "run execution timeout; 0 disables")
	flags.BoolVar(&cfg.DisableSubagents, "disable-subagents", false, "disable subagent delegation")
	flags.IntVar(&cfg.BackgroundWorkers, "background-workers", 4, "maximum concurrent background attempts (1..64)")
	flags.BoolVar(&cfg.MemoryExtraction, "memory-extraction", false, "extract durable descriptive facts after successful foreground turns")
	flags.StringVar(&cfg.MemoryUserID, "memory-user-id", "", "explicit host user identity for workspace-bound user memory")
	flags.Func("allow-model", "additional model ID allowed in session config (repeatable, same provider)", func(value string) error {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("model ID is empty")
		}
		cfg.Models = append(cfg.Models, harness.ConfigValue{Value: value, Name: value})
		return nil
	})
	flags.Func("mcp-allow-command", "absolute MCP server executable allowed for client configs (repeatable)", func(value string) error {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("MCP command is empty")
		}
		cfg.MCP.AllowedCommands = append(cfg.MCP.AllowedCommands, value)
		return nil
	})
	flags.BoolVar(&cfg.MCP.AllowHTTP, "mcp-allow-http", false, "allow outgoing MCP streamable HTTP connections")
	flags.BoolVar(&cfg.MCP.AllowSSE, "mcp-allow-sse", false, "allow outgoing MCP SSE connections")
}

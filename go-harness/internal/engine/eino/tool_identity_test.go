package eino

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/toolidentity"
)

type namedEffectTool struct {
	*receiptEffectTool
	name string
}

func (t *namedEffectTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	v, err := t.receiptEffectTool.Info(ctx)
	v.Name = t.name
	return v, err
}

type trustedEffectTool struct {
	*namedEffectTool
	toolidentity.Native
}

func TestDangerousPolicyRequiresHostIdentityBeforeEffect(t *testing.T) {
	for _, name := range []string{"mcp_reader_delete_record_1234", "read_then_delete", "mcp_task_search_write_record_1234", "read_file"} {
		for _, child := range []bool{false, true} {
			t.Run(name+map[bool]string{false: "/root", true: "/child"}[child], func(t *testing.T) {
				fake := &scriptedModel{stream: func(_ context.Context, call int, _ []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
					fn, args := name, `{}`
					if child && call == 0 {
						fn, args = "task", `{"subagent_type":"general-purpose","prompt":"inspect","description":"inspect"}`
					}
					if call > 0 && !child || call > 1 {
						return textStream("done"), nil
					}
					return schema.StreamReaderFromArray([]*schema.Message{{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: fn, Type: "function", Function: schema.FunctionCall{Name: fn, Arguments: args}}}}}), nil
				}}
				var effects, approvals atomic.Int32
				underlying := &namedEffectTool{&receiptEffectTool{run: func(context.Context) (string, error) { effects.Add(1); return "mutated", nil }}, name}
				s, session, _ := runtimeWithDurableInteractionMode(t, fake, underlying, child, harness.PermissionModeDangerous)
				result, err := s.Run(context.Background(), "owner", session.ID, []harness.Content{{Type: "text", Text: "inspect"}}, nil, func(_ context.Context, p harness.PermissionRequest) (harness.PermissionDecision, error) {
					if p.TrustedBuiltin {
						t.Errorf("external tool acquired native identity: %+v", p)
					}
					approvals.Add(1)
					return harness.RejectOnce, nil
				})
				if err != nil || result.StopReason != "end_turn" || approvals.Load() != 1 || effects.Load() != 0 {
					t.Fatalf("result=%+v err=%v approvals=%d effects=%d", result, err, approvals.Load(), effects.Load())
				}
			})
		}
	}
}

func TestPermissionContractBindsToolProvenance(t *testing.T) {
	e := &Engine{config: Config{PermissionMode: harness.PermissionModeDangerous}}
	a, err := e.executionContract(nil, nil, map[string]bool{"read_file": true})
	if err != nil {
		t.Fatal(err)
	}
	b, err := e.executionContract(nil, nil, map[string]bool{"read_file": false})
	if err != nil || a == b {
		t.Fatalf("contract did not bind provenance: %s %s %v", a, b, err)
	}
}

func TestTrustedBuiltinPermissionRequiresMatchingIdentity(t *testing.T) {
	for _, identity := range []string{"read_file", "other_tool"} {
		t.Run(identity, func(t *testing.T) {
			var effects, approvals atomic.Int32
			var underlying tool.BaseTool = &trustedEffectTool{&namedEffectTool{&receiptEffectTool{run: func(context.Context) (string, error) { effects.Add(1); return "read", nil }}, "read_file"}, toolidentity.Builtin(identity)}
			fake := &scriptedModel{stream: func(_ context.Context, call int, _ []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
				if call == 0 {
					return schema.StreamReaderFromArray([]*schema.Message{{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: "read", Type: "function", Function: schema.FunctionCall{Name: "read_file", Arguments: `{}`}}}}}), nil
				}
				return textStream("done"), nil
			}}
			s, session, _ := runtimeWithDurableInteractionMode(t, fake, underlying, false, harness.PermissionModeDangerous)
			_, err := s.Run(context.Background(), "owner", session.ID, []harness.Content{{Type: "text", Text: "inspect"}}, nil, func(context.Context, harness.PermissionRequest) (harness.PermissionDecision, error) {
				approvals.Add(1)
				return harness.RejectOnce, nil
			})
			wantEffects, wantApprovals := int32(1), int32(0)
			if identity != "read_file" {
				wantEffects, wantApprovals = 0, 1
			}
			if err != nil || effects.Load() != wantEffects || approvals.Load() != wantApprovals {
				t.Fatalf("effects=%d approvals=%d err=%v", effects.Load(), approvals.Load(), err)
			}
		})
	}
}

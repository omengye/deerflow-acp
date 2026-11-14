package eino

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
)

const maxInlineToolOutput = 8 << 10
const toolOutputHead = 3 << 10
const toolOutputTail = 1 << 10

// maxInlineRunToolOutput bounds inline tool text added between two model
// calls. It is replenished before each model request: compaction, not a
// whole-run allowance, governs long-run context growth. A per-run allowance
// would hide every later result behind a snapshot in multi-hour tasks.
const maxInlineRunToolOutput = 16 << 10

// Short results, including most failure messages, always stay inline so the
// model never needs an extra read_tool_output call just to see "ok" or an error.
const alwaysInlineToolOutput = 2 << 10

// Preview retained after the inline allowance is spent.
const exhaustedPreviewHead = 1 << 10
const exhaustedPreviewTail = 512

// Matches the asset store's snapshot cap. Larger output is truncated instead of
// failing the whole execution.
const maxToolOutputSnapshot = 4 << 20

func (m *toolMiddleware) reserveInlineToolOutput(n int) bool {
	m.outputMu.Lock()
	defer m.outputMu.Unlock()
	if n > maxInlineRunToolOutput-m.outputBytes {
		return false
	}
	m.outputBytes += n
	return true
}

func (m *toolMiddleware) chargeInlineToolOutput(n int) {
	m.outputMu.Lock()
	m.outputBytes += n
	m.outputMu.Unlock()
}

// BeforeModelRewriteState replenishes the inline allowance for the tool
// results produced after this model request.
func (m *toolMiddleware) BeforeModelRewriteState(ctx context.Context, state *adk.ChatModelAgentState, _ *adk.ModelContext) (context.Context, *adk.ChatModelAgentState, error) {
	m.outputMu.Lock()
	m.outputBytes = 0
	m.outputMu.Unlock()
	return ctx, state, nil
}

// modelToolOutput keeps a complete, private snapshot before replacing an
// oversized result in the native model history. Durable tool events and
// receipts retain the original result; only the next model input is bounded.
func (m *toolMiddleware) modelToolOutput(ctx context.Context, output string) (string, error) {
	if m.outputs == nil {
		return output, nil
	}
	output = strings.ToValidUTF8(output, "\uFFFD")
	if len(output) <= alwaysInlineToolOutput {
		m.chargeInlineToolOutput(len(output))
		return output, nil
	}
	if len(output) <= maxInlineToolOutput && m.reserveInlineToolOutput(len(output)) {
		return output, nil
	}
	stored, dropped := output, 0
	if len(stored) > maxToolOutputSnapshot {
		end := maxToolOutputSnapshot
		for end > 0 && !utf8.RuneStart(stored[end]) {
			end--
		}
		stored, dropped = stored[:end], len(output)-end
	}
	ref, err := m.outputs.StoreToolOutput(ctx, m.sink.request.Session, stored)
	if err != nil {
		return "", fmt.Errorf("store complete tool output: %w", err)
	}
	marker := fmt.Sprintf("[Tool output: %d UTF-8 bytes; session snapshot %q. Read with read_tool_output using id=%q and offset=0.]", len(output), ref.ID, ref.ID)
	if dropped > 0 {
		marker = fmt.Sprintf("[Tool output: %d UTF-8 bytes; session snapshot %q keeps only the first %d bytes, the last %d bytes were dropped. Read with read_tool_output using id=%q and offset=0.]", len(output), ref.ID, len(stored), dropped, ref.ID)
	}
	head, tail := toolOutputHead, toolOutputTail
	if !m.reserveInlineToolOutput(head + tail) {
		head, tail = exhaustedPreviewHead, exhaustedPreviewTail
		m.chargeInlineToolOutput(head + tail)
	}
	return marker + "\n" + previewToolOutput(output, head, tail), nil
}

func previewToolOutput(output string, head, tail int) string {
	if len(output) <= head+tail {
		return output
	}
	headEnd := head
	for headEnd > 0 && !utf8.RuneStart(output[headEnd]) {
		headEnd--
	}
	tailStart := len(output) - tail
	for tailStart < len(output) && !utf8.RuneStart(output[tailStart]) {
		tailStart++
	}
	return fmt.Sprintf("%s\n[... %d bytes omitted ...]\n%s", output[:headEnd], tailStart-headEnd, output[tailStart:])
}

// modelEnhancedToolOutput applies the same bound to text from enhanced MCP
// tools while leaving their already-staged image/file parts in place.
func (m *toolMiddleware) modelEnhancedToolOutput(ctx context.Context, output *schema.ToolResult) (*schema.ToolResult, error) {
	if output == nil || m.outputs == nil {
		return output, nil
	}
	var text strings.Builder
	firstText := -1
	for i, part := range output.Parts {
		if part.Type != schema.ToolPartTypeText {
			continue
		}
		if firstText < 0 {
			firstText = i
		} else {
			text.WriteString("\n\n")
		}
		text.WriteString(part.Text)
	}
	if firstText < 0 {
		return output, nil
	}
	preview, err := m.modelToolOutput(ctx, text.String())
	if err != nil {
		return nil, err
	}
	if preview == text.String() {
		return output, nil
	}
	copyResult := *output
	copyResult.Parts = make([]schema.ToolOutputPart, 0, len(output.Parts))
	for i, part := range output.Parts {
		if part.Type != schema.ToolPartTypeText {
			copyResult.Parts = append(copyResult.Parts, part)
		} else if i == firstText {
			part.Text = preview
			copyResult.Parts = append(copyResult.Parts, part)
		}
	}
	return &copyResult, nil
}

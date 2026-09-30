package eino

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/cloudwego/eino/schema"
)

const maxInlineToolOutput = 8 << 10
const toolOutputHead = 3 << 10
const toolOutputTail = 1 << 10
const maxInlineRunToolOutput = 16 << 10

func (m *toolMiddleware) reserveInlineToolOutput(n int) bool {
	m.outputMu.Lock()
	defer m.outputMu.Unlock()
	if n > maxInlineRunToolOutput-m.outputBytes {
		return false
	}
	m.outputBytes += n
	return true
}

// modelToolOutput keeps a complete, private snapshot before replacing an
// oversized result in the native model history. Durable tool events and
// receipts retain the original result; only the next model input is bounded.
func (m *toolMiddleware) modelToolOutput(ctx context.Context, output string) (string, error) {
	if m.outputs == nil {
		return output, nil
	}
	if len(output) <= maxInlineToolOutput && m.reserveInlineToolOutput(len(output)) {
		return output, nil
	}
	ref, err := m.outputs.StoreToolOutput(ctx, m.sink.request.Session, output)
	if err != nil {
		return "", fmt.Errorf("store complete tool output: %w", err)
	}
	marker := fmt.Sprintf("[Tool output: %d UTF-8 bytes; session snapshot %q. Read with read_tool_output using id=%q and offset=0.]", len(output), ref.ID, ref.ID)
	if len(output) <= maxInlineToolOutput || !m.reserveInlineToolOutput(toolOutputHead+toolOutputTail) {
		return marker, nil
	}
	headEnd := toolOutputHead
	for headEnd > 0 && !utf8.RuneStart(output[headEnd]) {
		headEnd--
	}
	tailStart := len(output) - toolOutputTail
	for tailStart < len(output) && !utf8.RuneStart(output[tailStart]) {
		tailStart++
	}
	return fmt.Sprintf("%s\n%s\n[... %d bytes omitted ...]\n%s", marker, output[:headEnd], tailStart-headEnd, output[tailStart:]), nil
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

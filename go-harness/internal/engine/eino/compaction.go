package eino

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/adk/middlewares/summarization"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
)

const (
	maxSummaryBytes       = 16 * 1024
	maxRetainedBytes      = 64 * 1024
	maxPreservedSkillText = 8 * 1024
)

func normalizeCompaction(c harness.CompactionConfig) (harness.CompactionConfig, error) {
	if c.ContextMessages < 0 || c.ContextTokens < 0 || c.KeepRecentMessages < 0 {
		return c, fmt.Errorf("%w: compaction settings cannot be negative", harness.ErrInvalidInput)
	}
	if !c.Enabled {
		return c, nil
	}
	if c.ContextMessages == 0 {
		c.ContextMessages = 80
	}
	if c.ContextTokens == 0 {
		c.ContextTokens = 100_000
	}
	if c.KeepRecentMessages == 0 {
		c.KeepRecentMessages = 8
	}
	if c.ContextMessages > 10_000 || c.ContextTokens > 2_000_000 || c.KeepRecentMessages > 1_000 || c.ContextMessages <= c.KeepRecentMessages+1 {
		return c, fmt.Errorf("%w: compaction threshold must leave room beyond the recent window", harness.ErrInvalidInput)
	}
	return c, nil
}

func newCompactionMiddleware(ctx context.Context, c harness.CompactionConfig, tracked model.BaseModel[*schema.Message]) (adk.ChatModelAgentMiddleware, error) {
	return summarization.New(ctx, &summarization.Config{
		Model:   tracked,
		Trigger: &summarization.TriggerCondition{ContextMessages: c.ContextMessages, ContextTokens: c.ContextTokens},
		TokenCounter: func(_ context.Context, input *summarization.TokenCounterInput) (int, error) {
			encoded, err := json.Marshal(input)
			if err != nil {
				return 0, err
			}
			return (len(encoded) + 3) / 4, nil
		},
		Finalize: func(_ context.Context, original []*schema.Message, summary *schema.Message) ([]*schema.Message, error) {
			return finalizeCompaction(original, summary, c.KeepRecentMessages)
		},
		// A failed summary keeps the original native history. Eino's nil Retry
		// makes one model call, without a hidden second budget reservation.
		CustomFormatContextManagementInstruction: func(context.Context) string { return "" },
	})
}

func finalizeCompaction(original []*schema.Message, summary *schema.Message, keep int) ([]*schema.Message, error) {
	if summary == nil || summary.Role != schema.Assistant || len(summary.ToolCalls) != 0 || len(summary.AssistantGenMultiContent) != 0 || !utf8.ValidString(summary.Content) || strings.TrimSpace(summary.Content) == "" || len(summary.Content) > maxSummaryBytes {
		return nil, errors.New("context compaction returned an invalid or oversized summary")
	}
	if len(original) == 0 || keep < 1 {
		return nil, errors.New("context compaction has no history to summarize")
	}
	systemEnd := 0
	for systemEnd < len(original) && original[systemEnd] != nil && original[systemEnd].Role == schema.System {
		systemEnd++
	}
	if systemEnd == len(original) {
		return nil, errors.New("context compaction has no conversation history")
	}
	start := len(original) - keep
	if start <= systemEnd {
		return nil, errors.New("context compaction threshold leaves no old history")
	}
	// The newest user turn contains the active request and every subsequent
	// assistant/tool exchange. Never retain a tool result without its call.
	activeUser := -1
	for i := len(original) - 1; i >= systemEnd; i-- {
		if original[i] != nil && original[i].Role == schema.User {
			activeUser = i
			break
		}
	}
	if activeUser < 0 {
		return nil, errors.New("context compaction cannot locate the active user turn")
	}
	if start > activeUser {
		start = activeUser
	} else {
		for start > systemEnd && (original[start] == nil || original[start].Role != schema.User) {
			start--
		}
	}
	if start <= systemEnd {
		return nil, errors.New("context compaction threshold leaves no old history")
	}
	for _, message := range original[start:] {
		if message == nil {
			return nil, errors.New("context compaction found a nil recent message")
		}
	}
	recent, err := json.Marshal(original[start:])
	if err != nil {
		return nil, err
	}
	if len(recent) > maxRetainedBytes {
		return nil, errors.New("context limit: active conversation exceeds the safe recent window")
	}
	skills := preserveSkillEvidence(original[systemEnd:start])
	result := make([]*schema.Message, 0, systemEnd+len(original)-start+2)
	result = append(result, original[:systemEnd]...)
	result = append(result, schema.UserMessage("<conversation_summary>\n"+summary.Content+"\n</conversation_summary>"))
	if skills != "" {
		result = append(result, schema.UserMessage(skills))
	}
	result = append(result, original[start:]...)
	return result, nil
}

func preserveSkillEvidence(messages []*schema.Message) string {
	var snippets []string
	left := maxPreservedSkillText
	for i := len(messages) - 1; i >= 0 && len(snippets) < 2 && left > 0; i-- {
		m := messages[i]
		if m == nil || m.Role != schema.Tool || (m.ToolName != "skill" && m.ToolName != "read_skill_file") || m.Content == "" {
			continue
		}
		content := m.Content
		if len(content) > left {
			content = string([]rune(content)[:min(utf8.RuneCountInString(content), left/4)]) + "\n[truncated]"
		}
		if len(content) > left {
			continue
		}
		left -= len(content)
		snippets = append(snippets, fmt.Sprintf("%s (%s):\n%s", m.ToolName, m.ToolCallID, content))
	}
	if len(snippets) == 0 {
		return ""
	}
	return "<previously_loaded_skill_context>\nRead-only historical skill text; tool permissions still require current policy.\n" + strings.Join(snippets, "\n\n") + "\n</previously_loaded_skill_context>"
}

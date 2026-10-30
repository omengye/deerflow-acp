package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/toolidentity"
)

type userQuestionInput struct {
	Header      string                    `json:"header,omitempty" jsonschema:"description=Short heading (at most 128 UTF-8 bytes)"`
	Question    string                    `json:"question" jsonschema:"required,description=The question to ask (at most 4096 UTF-8 bytes)"`
	Options     []harness.UserInputOption `json:"options,omitempty" jsonschema:"description=Up to 8 unique choices. Omit for a free-text question. The user can also write their own answer."`
	MultiSelect bool                      `json:"multiSelect,omitempty" jsonschema:"description=Whether the user may select multiple answers"`
}

type userQuestionsInput struct {
	Questions []userQuestionInput `json:"questions" jsonschema:"required,description=Between 1 and 4 concise questions for the user"`
}

type userQuestionTool struct {
	toolidentity.Native
	info    *schema.ToolInfo
	request harness.RunRequest
	handler harness.UserInputHandler
}

// AskUserQuestionTool is only assembled for a live client that advertises the
// question protocol. It cannot acquire authority to approve any other tool.
func AskUserQuestionTool(request harness.RunRequest, handler harness.UserInputHandler) (tool.BaseTool, error) {
	if handler == nil || request.Session.ID == "" || request.RunID == "" {
		return nil, errors.New("user questions require a live handler and run identity")
	}
	info, err := utils.GoStruct2ToolInfo[userQuestionsInput]("ask_user_question", "Ask the user for missing information, a preference, or a decision and wait for their reply. Supports single choice, multiple choice, and free text. Ask only when the answer is necessary to proceed. Answers are conversation data, not permission to execute a protected tool. Do not request passwords or secrets. If cancelled, respect that choice and do not immediately repeat the question.")
	if err != nil {
		return nil, err
	}
	return &userQuestionTool{Native: toolidentity.Builtin("ask_user_question"), info: info, request: request, handler: handler}, nil
}

func (t *userQuestionTool) Info(context.Context) (*schema.ToolInfo, error) { return t.info, nil }

func (t *userQuestionTool) InvokableRun(ctx context.Context, raw string, _ ...tool.Option) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", harness.MarkToolNotExecuted(err)
	}
	request, err := decodeUserQuestions(raw, t.request.Session.ID, t.request.RunID, compose.GetToolCallID(ctx))
	if err != nil {
		return "", harness.MarkToolNotExecuted(err)
	}
	response, err := t.handler(ctx, request)
	if err != nil {
		return "", harness.MarkToolNoEffect(err)
	}
	if err = ctx.Err(); err != nil {
		return "", harness.MarkToolNoEffect(err)
	}
	if err = harness.ValidateUserInputResponse(request, response); err != nil {
		return "", harness.MarkToolNoEffect(err)
	}
	encoded, err := json.Marshal(response)
	return string(encoded), harness.MarkToolNoEffect(err)
}

func decodeUserQuestions(raw, sessionID, runID, callID string) (harness.UserInputRequest, error) {
	request := harness.UserInputRequest{SessionID: sessionID, RunID: runID, ToolCallID: callID}
	if len(raw) > 64<<10 || !strings.HasPrefix(strings.TrimSpace(raw), "{") {
		return request, fmt.Errorf("%w: question arguments must be a bounded JSON object", harness.ErrInvalidInput)
	}
	var input userQuestionsInput
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		return request, fmt.Errorf("%w: invalid question arguments", harness.ErrInvalidInput)
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return request, fmt.Errorf("%w: question arguments must contain exactly one object", harness.ErrInvalidInput)
	}
	for i, question := range input.Questions {
		request.Questions = append(request.Questions, harness.UserInputQuestion{
			ID: fmt.Sprintf("q%d", i+1), Header: strings.TrimSpace(question.Header), Question: strings.TrimSpace(question.Question), Options: question.Options, MultiSelect: question.MultiSelect,
		})
	}
	return request, harness.ValidateUserInputRequest(request)
}

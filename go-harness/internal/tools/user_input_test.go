package tools

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestAskUserQuestionToolUsesHostIdentityAndReturnsAnswers(t *testing.T) {
	item, err := AskUserQuestionTool(harness.RunRequest{Session: harness.Session{ID: "session"}, RunID: "run"}, func(_ context.Context, request harness.UserInputRequest) (harness.UserInputResponse, error) {
		if request.SessionID != "session" || request.RunID != "run" || request.ToolCallID != "question-call" || len(request.Questions) != 2 || request.Questions[0].ID != "q1" || request.Questions[1].ID != "q2" || request.Questions[0].Header != "环境" {
			t.Fatalf("lost host identity or question data: %+v", request)
		}
		return harness.UserInputResponse{Outcome: "accepted", Answers: []harness.UserInputAnswer{{QuestionID: "q1", Answers: []string{"测试"}}, {QuestionID: "q2", Answers: []string{"用户填写的文本"}}}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	node, err := compose.NewToolNode(context.Background(), &compose.ToolsNodeConfig{Tools: []tool.BaseTool{item}})
	if err != nil {
		t.Fatal(err)
	}
	output, err := node.Invoke(context.Background(), schema.AssistantMessage("", []schema.ToolCall{{ID: "question-call", Type: "function", Function: schema.FunctionCall{Name: "ask_user_question", Arguments: `{"questions":[{"header":"环境","question":"部署到哪里？","options":[{"label":"测试"},{"label":"生产"}]},{"question":"备注是什么？"}]}`}}}))
	if err != nil || len(output) != 1 {
		t.Fatalf("tool exchange failed: %+v %v", output, err)
	}
	var response harness.UserInputResponse
	if err = json.Unmarshal([]byte(output[0].Content), &response); err != nil || response.Outcome != "accepted" || response.Answers[1].Answers[0] != "用户填写的文本" {
		t.Fatalf("answer lost: %+v %v", output, err)
	}
}

func TestAskUserQuestionRejectsInvalidArgumentsBeforeClient(t *testing.T) {
	called := false
	item, err := AskUserQuestionTool(harness.RunRequest{Session: harness.Session{ID: "session"}, RunID: "run"}, func(context.Context, harness.UserInputRequest) (harness.UserInputResponse, error) {
		called = true
		return harness.UserInputResponse{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{`null`, `{}`, `{"questions":[]}`, `{"questions":[{"id":"forged","question":"q"}]}`, `{"sessionId":"foreign","questions":[{"question":"q"}]}`, `{"questions":[{"question":"q"}]} {}`} {
		_, err = item.(tool.InvokableTool).InvokableRun(context.Background(), raw)
		var rejected *harness.ToolNotExecutedError
		if !errors.As(err, &rejected) || called {
			t.Fatalf("invalid request reached client: %s %v", raw, err)
		}
	}
	if _, err = decodeUserQuestions(`{"questions":[{"question":"q","options":[{"label":"same"},{"label":"same"}]}]}`, "session", "run", "call"); err == nil {
		t.Fatal("duplicate choices accepted")
	}
}

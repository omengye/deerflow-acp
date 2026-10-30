package harness

import (
	"strings"
	"testing"
)

func questionFixture() UserInputRequest {
	return UserInputRequest{SessionID: "session", RunID: "run", ToolCallID: "call", Questions: []UserInputQuestion{
		{ID: "q1", Header: "环境", Question: "使用哪个环境？", Options: []UserInputOption{{Label: "测试"}, {Label: "生产"}}},
		{ID: "q2", Question: "需要哪些检查？", MultiSelect: true},
	}}
}

func TestUserInputResponseBindsEveryQuestionAndPreservesFreeText(t *testing.T) {
	request := questionFixture()
	response := UserInputResponse{Outcome: "accepted", Answers: []UserInputAnswer{{QuestionID: "q1", Answers: []string{"使用另一个环境"}}, {QuestionID: "q2", Answers: []string{"日志", "指标"}}}}
	if err := ValidateUserInputResponse(request, response); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []UserInputResponse{
		{Outcome: "selected", Answers: response.Answers},
		{Outcome: "accepted"},
		{Outcome: "cancelled", Answers: response.Answers},
		{Outcome: "accepted", Answers: response.Answers[:1]},
		{Outcome: "accepted", Answers: []UserInputAnswer{response.Answers[0], response.Answers[0]}},
		{Outcome: "accepted", Answers: []UserInputAnswer{{QuestionID: "foreign", Answers: []string{"ok"}}, response.Answers[1]}},
		{Outcome: "accepted", Answers: []UserInputAnswer{{QuestionID: "q1", Answers: []string{"测试", "生产"}}, response.Answers[1]}},
		{Outcome: "accepted", Answers: []UserInputAnswer{response.Answers[0], {QuestionID: "q2", Answers: []string{"日志", " 日志 "}}}},
		{Outcome: "accepted", Answers: []UserInputAnswer{response.Answers[0], {QuestionID: "q2", Answers: []string{strings.Repeat("x", 4097)}}}},
		{Outcome: "accepted", Answers: []UserInputAnswer{response.Answers[0], {QuestionID: "q2", Answers: []string{"\x00"}}}},
	} {
		if err := ValidateUserInputResponse(request, invalid); err == nil {
			t.Fatalf("accepted invalid response: %+v", invalid)
		}
	}
	if err := ValidateUserInputResponse(request, UserInputResponse{Outcome: "cancelled"}); err != nil {
		t.Fatal(err)
	}
}

func TestUserInputRequestRejectsUnboundedOrAmbiguousQuestions(t *testing.T) {
	for _, mutate := range []func(*UserInputRequest){
		func(r *UserInputRequest) { r.SessionID = "" },
		func(r *UserInputRequest) { r.Questions = nil },
		func(r *UserInputRequest) { r.Questions[1].ID = "q1" },
		func(r *UserInputRequest) { r.Questions[0].Question = " " },
		func(r *UserInputRequest) { r.Questions[0].Question = strings.Repeat("问", 1366) },
		func(r *UserInputRequest) { r.Questions[0].Header = string([]byte{0xff}) },
		func(r *UserInputRequest) { r.Questions[0].Options[1].Label = " 测试 " },
		func(r *UserInputRequest) { r.Questions[0].Options[0].Description = strings.Repeat("x", 1025) },
	} {
		request := questionFixture()
		mutate(&request)
		if err := ValidateUserInputRequest(request); err == nil {
			t.Fatalf("accepted invalid request: %+v", request)
		}
	}
}

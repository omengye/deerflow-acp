package harness

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"
)

// UserInput is a live conversation exchange, never an execution permission.
// The host supplies request identities; model arguments supply only questions.
type UserInputRequest struct {
	SessionID  string              `json:"sessionId"`
	RunID      string              `json:"runId"`
	ToolCallID string              `json:"toolCallId"`
	Questions  []UserInputQuestion `json:"questions"`
}

type UserInputQuestion struct {
	ID          string            `json:"id"`
	Header      string            `json:"header,omitempty"`
	Question    string            `json:"question"`
	Options     []UserInputOption `json:"options,omitempty"`
	MultiSelect bool              `json:"multiSelect"`
}

type UserInputOption struct {
	Label       string `json:"label" jsonschema:"required,description=Short unique option label"`
	Description string `json:"description,omitempty" jsonschema:"description=Optional explanation of this choice"`
}

type UserInputResponse struct {
	Outcome string            `json:"outcome"`
	Answers []UserInputAnswer `json:"answers,omitempty"`
}

type UserInputAnswer struct {
	QuestionID string   `json:"questionId"`
	Answers    []string `json:"answers"`
}

type UserInputHandler func(context.Context, UserInputRequest) (UserInputResponse, error)

func validUserInputText(value string, maximum int, required bool) bool {
	return len(value) <= maximum && utf8.ValidString(value) && !strings.ContainsRune(value, 0) && (!required || strings.TrimSpace(value) != "")
}

// ValidateUserInputRequest bounds the desktop exchange independently of tool
// argument parsing, including requests supplied through an SDK handler.
func ValidateUserInputRequest(request UserInputRequest) error {
	for _, id := range []string{request.SessionID, request.RunID, request.ToolCallID} {
		if !validUserInputText(id, 256, true) {
			return fmt.Errorf("%w: invalid question request identity", ErrInvalidInput)
		}
	}
	if len(request.Questions) < 1 || len(request.Questions) > 4 {
		return fmt.Errorf("%w: provide between 1 and 4 questions", ErrInvalidInput)
	}
	seen := make(map[string]bool, len(request.Questions))
	for _, question := range request.Questions {
		if !validUserInputText(question.ID, 64, true) || seen[question.ID] || !validUserInputText(question.Header, 128, false) || !validUserInputText(question.Question, 4096, true) || len(question.Options) > 8 {
			return fmt.Errorf("%w: invalid question or duplicate question identity", ErrInvalidInput)
		}
		seen[question.ID] = true
		labels := make(map[string]bool, len(question.Options))
		for _, option := range question.Options {
			label := strings.TrimSpace(option.Label)
			if !validUserInputText(option.Label, 256, true) || labels[label] || !validUserInputText(option.Description, 1024, false) {
				return fmt.Errorf("%w: invalid or duplicate question option", ErrInvalidInput)
			}
			labels[label] = true
		}
	}
	return nil
}

// Answers are data from the user. Unknown IDs, partial/duplicate replies and
// invalid outcomes cannot be mistaken for a completed question exchange.
func ValidateUserInputResponse(request UserInputRequest, response UserInputResponse) error {
	if err := ValidateUserInputRequest(request); err != nil {
		return err
	}
	invalid := func() error { return fmt.Errorf("%w: invalid user input response", ErrInvalidInput) }
	if response.Outcome == "cancelled" {
		if len(response.Answers) != 0 {
			return invalid()
		}
		return nil
	}
	if response.Outcome != "accepted" || len(response.Answers) != len(request.Questions) {
		return invalid()
	}
	questions := make(map[string]UserInputQuestion, len(request.Questions))
	for _, question := range request.Questions {
		questions[question.ID] = question
	}
	seen := make(map[string]bool, len(response.Answers))
	total := 0
	for _, answer := range response.Answers {
		question, ok := questions[answer.QuestionID]
		if !ok || seen[answer.QuestionID] || len(answer.Answers) < 1 || len(answer.Answers) > 8 || (!question.MultiSelect && len(answer.Answers) != 1) {
			return invalid()
		}
		seen[answer.QuestionID] = true
		values := make(map[string]bool, len(answer.Answers))
		for _, value := range answer.Answers {
			trimmed := strings.TrimSpace(value)
			if !validUserInputText(value, 4096, true) || values[trimmed] {
				return invalid()
			}
			values[trimmed] = true
			total += len(value)
			if total > 16384 {
				return invalid()
			}
		}
	}
	return nil
}

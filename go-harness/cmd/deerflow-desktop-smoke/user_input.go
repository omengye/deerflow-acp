package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// A missing tool or a stale pending question must fail promptly, rather than
// waiting until the WebSocket deadline after an otherwise successful turn.
func (w *wakuClient) userInputEvent(runtime, expected string) ([]obj, obj) {
	deadline := time.Now().Add(90 * time.Second)
	var events []obj
	for time.Now().Before(deadline) {
		var message obj
		if len(w.events) != 0 {
			message, w.events = w.events[0], w.events[1:]
		} else {
			message = w.ws.receive(time.Until(deadline))
		}
		if message["type"] != "event" || message["runtimeId"] != runtime {
			continue
		}
		event := object(message["event"])
		events = append(events, event)
		kind := str(event["kind"])
		need(kind != "error" && kind != "processExited" && kind != "permission", "user-input fixture received unexpected %s: %v", kind, event["payload"])
		if kind == expected {
			return events, object(event["payload"])
		}
		need(kind != "turnFinished" && kind != "userInputRequested", "expected %s, received %s: %v", expected, kind, event["payload"])
	}
	panic(fmt.Errorf("missing user-input fixture event %s", expected))
}

func questionToolOffered(request obj) bool {
	for _, value := range array(request["tools"]) {
		if object(object(value)["function"])["name"] == "ask_user_question" {
			return true
		}
	}
	return false
}

func validateQuestionForm(form obj, expected []any) string {
	requestID := str(form["requestId"])
	need(requestID != "", "desktop question omitted request identity")
	questions := array(form["questions"])
	need(len(questions) == len(expected), "desktop changed question count: %v", form)
	for i, value := range questions {
		question, original := object(value), object(expected[i])
		need(question["id"] == fmt.Sprintf("q%d", i+1), "desktop question identity missing: %v", question)
		for _, key := range []string{"header", "question", "multiSelect"} {
			need(question[key] == original[key], "desktop changed question %d %s: %v", i+1, key, question)
		}
		need(string(jsonBytes(array(question["options"]))) == string(jsonBytes(array(original["options"]))), "desktop changed question %d options: %v", i+1, question)
	}
	return requestID
}

func (s *smoke) answerQuestion(session, runtime, requestID, callID string, answers []any, start int) {
	ack := s.waku.rpc(obj{"type": "respondUserInput", "requestId": requestID, "answers": answers}, session, runtime)
	need(ack["type"] == "ack", "desktop answer was not acknowledged: %v", ack)
	events, finished := s.waku.userInputEvent(runtime, "turnFinished")
	need(finished["success"] == true, "answered question turn failed: %v", finished)
	text := ""
	for _, event := range events {
		if event["kind"] == "textDelta" {
			text += str(event["payload"])
		}
	}
	need(strings.Contains(text, reply), "model did not finish after receiving answers: %q", text)
	requests := s.fake.since(start)
	need(len(requests) == 2 && questionToolOffered(requests[0]), "desktop did not offer the real question tool or replayed the model: %d requests", len(requests))
	result := toolResult(requests, callID)
	need(result != nil, "model did not receive question answers")
	var response obj
	must(json.Unmarshal([]byte(messageText(result)), &response))
	need(response["outcome"] == "accepted" && string(jsonBytes(response["answers"])) == string(jsonBytes(answers)), "model received altered question answers: %v", response)
}

func (s *smoke) runUserInput() {
	s.startWaku(uuid(), "waku-user-input")
	saved := s.configure()
	object(saved["memory"])["enabled"] = false
	s.waku.deerflow("save", saved)
	s.waku.deerflow("start")
	s.settled()

	session, runtime := uuid(), uuid()
	need(s.waku.rpc(obj{"type": "start", "options": s.options("ask", "smoke-local", nil)}, session, runtime)["type"] == "started", "question runtime failed to start")
	s.waku.event(runtime, "connected", nil, nil)
	questions := []any{
		obj{"header": "Environment", "question": "Which fixture environment?", "multiSelect": false, "options": []any{
			obj{"label": "Preview", "description": "Use the local preview fixture"},
			obj{"label": "Staging", "description": "Use the local staging fixture"},
		}},
		obj{"header": "Checks", "question": "Which fixture checks?", "multiSelect": true, "options": []any{
			obj{"label": "Docs", "description": "Review fixture documentation"},
			obj{"label": "Tests", "description": "Review fixture tests"},
		}},
		obj{"header": "备注", "question": "请输入本地测试备注。", "multiSelect": false, "options": []any{}},
	}
	marker, callID := s.fake.plan("ask_user_question", obj{"questions": questions})
	start := s.fake.count()
	s.prompt(session, runtime, marker+": Ask the fixture questions and then summarize the answers.")
	_, form := s.waku.userInputEvent(runtime, "userInputRequested")
	requestID := validateQuestionForm(form, questions)
	need(len(s.fake.since(start)) == 1, "model continued before the user answered")
	answers := []any{
		obj{"questionId": "q1", "answers": []any{"Preview"}},
		obj{"questionId": "q2", "answers": []any{"Docs", "Tests"}},
		obj{"questionId": "q3", "answers": []any{"中文自由文本 — local smoke only"}},
	}
	s.answerQuestion(session, runtime, requestID, callID, answers, start)
	s.log("Packaged desktop question form and answers round-trip through Waku and ACP", obj{"questions": 3, "single_choice": true, "multi_select": true, "free_text": true, "permission_count": 0})

	pendingQuestions := []any{obj{"header": "Pending", "question": "Wait for the fixture answer.", "multiSelect": false, "options": []any{}}}
	marker, cancelledCall := s.fake.plan("ask_user_question", obj{"questions": pendingQuestions})
	start = s.fake.count()
	s.prompt(session, runtime, marker+": Ask this question and wait.")
	_, form = s.waku.userInputEvent(runtime, "userInputRequested")
	cancelledRequest := validateQuestionForm(form, pendingQuestions)
	need(s.waku.rpc(obj{"type": "cancel"}, session, runtime)["type"] == "ack", "question cancellation not acknowledged")
	s.waku.userInputEvent(runtime, "turnFinished")
	need(toolResult(s.fake.since(start), cancelledCall) == nil, "cancelled question unexpectedly resumed the model")

	marker, callID = s.fake.plan("ask_user_question", obj{"questions": pendingQuestions})
	start = s.fake.count()
	s.prompt(session, runtime, marker+": Ask a fresh question after the stopped turn.")
	_, form = s.waku.userInputEvent(runtime, "userInputRequested")
	requestID = validateQuestionForm(form, pendingQuestions)
	need(requestID != cancelledRequest, "fresh question reused the cancelled request identity")
	s.answerQuestion(session, runtime, requestID, callID, []any{obj{"questionId": "q1", "answers": []any{"Fresh answer after stop"}}}, start)
	s.log("Stopping a pending question allows another question on the same desktop runtime", obj{"stale_pending_question": false})
	s.waku.rpc(obj{"type": "closeSession"}, session, runtime)

	// This client deliberately uses the existing initialize helper, which omits
	// the native question capability. The same packaged daemon must hide it.
	a := s.acp("acp-without-user-input")
	plainSession := str(a.rpc("session/new", obj{"cwd": s.workspace, "mcpServers": []any{}})["sessionId"])
	marker, callID = s.fake.plan("ask_user_question", obj{"questions": pendingQuestions})
	start = s.fake.count()
	finished := a.rpc("session/prompt", obj{"sessionId": plainSession, "prompt": []any{obj{"type": "text", "text": marker + ": Reply using the tools available to this client."}}})
	requests := s.fake.since(start)
	need(finished["stopReason"] == "end_turn" && len(requests) == 1 && !questionToolOffered(requests[0]) && toolResult(requests, callID) == nil, "plain ACP client was offered an unsupported question tool: %v", finished)
	a.rpc("session/close", obj{"sessionId": plainSession})
	s.log("Packaged ACP clients without question capability do not advertise ask_user_question", obj{})
}

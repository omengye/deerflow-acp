package main

import "math"

// Exercise the desktop's management route before any prompt, including the
// same Ready snapshot returned when a client reattaches to a surviving runtime.
// startChat also calls this after service restart with the original native ID.
func (s *smoke) sessionBudgetRead(session, runtime string, cursor any, restored bool) {
	requests, alternate := s.fake.count(), s.alt.count()
	events := []obj{}
	ready := object(s.waku.event(runtime, "deerflowTasks", &events, nil))
	native := str(object(cursor)["sessionId"])
	need(native != "" && ready["kind"] == "ready" && ready["sessionId"] == native, "budget Ready belongs to another native session: %v", ready)
	need(str(ready["connectionId"]) != "" && object(ready["capabilities"])["sessionBudget"] == true, "budget capability missing from runtime Ready")

	readBudget := func(connection obj) obj {
		id := uuid()
		command := obj{"requestId": id, "connectionId": connection["connectionId"], "sessionId": native, "action": obj{"kind": "budgetGet"}}
		need(s.waku.rpc(obj{"type": "deerFlowTasks", "command": command}, session, runtime)["type"] == "ack", "budget request not acknowledged")
		for {
			event := object(s.waku.event(runtime, "deerflowTasks", &events, nil))
			if event["kind"] == "ready" || event["kind"] == "invalidated" {
				continue
			}
			need(object(event["command"])["requestId"] == id, "budget response request mismatch: %v", event)
			need(event["kind"] == "reply", "budget read failed: %v", event)
			response := object(event["response"])
			need(response["kind"] == "budget", "budget read returned wrong response: %v", response)
			budget := object(response["data"])
			need(budget["sessionId"] == native && budget["version"] == float64(1), "budget identity/version mismatch: %v", budget)
			requested, host, effective := object(budget["requested"]), object(budget["host"]), object(budget["effective"])
			for _, key := range []string{"maxTokens", "maxModelCalls", "maxToolCalls"} {
				limit, ok := host[key].(float64)
				need(ok && limit >= 0 && math.Trunc(limit) == limit, "invalid host budget %s: %v", key, host[key])
				need(requested[key] == float64(0) && effective[key] == host[key], "unmodified session does not inherit host %s: %v", key, budget)
			}
			return budget
		}
	}

	before := readBudget(ready)
	attached := s.waku.rpc(obj{"type": "attachSession"}, session)
	need(attached["runtimeId"] == runtime, "budget attach replaced the runtime")
	attachedReady := object(attached["deerflowReady"])
	need(attachedReady["kind"] == "ready" && attachedReady["sessionId"] == native && attachedReady["connectionId"] == ready["connectionId"], "budget attach did not preserve Ready identity")
	after := readBudget(attachedReady)
	need(string(jsonBytes(before)) == string(jsonBytes(after)), "read-only budget refresh changed the snapshot")
	need(s.fake.count() == requests && s.alt.count() == alternate, "reading or reattaching budget invoked a model")
	for _, event := range events {
		switch event["kind"] {
		case "promptSubmitted", "textDelta", "reasoningDelta", "activity", "permission", "turnFinished":
			panic("budget management unexpectedly executed a turn")
		}
	}
	s.log("BudgetGet before prompt and after AttachSession preserves limits without model work", obj{"native_session": native, "restored_history": restored, "version": before["version"], "effective": before["effective"], "model_requests": 0})
}

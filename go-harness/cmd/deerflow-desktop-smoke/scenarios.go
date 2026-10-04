package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

func (s *smoke) options(mode, model string, cursor any) obj {
	return obj{"provider": "deerFlow", "binary": filepath.Join(s.root, "deerflow-acp.exe"), "cwd": s.workspace, "mode": mode, "model": model, "reasoningEffort": "off", "serviceTier": nil, "contextWindow": nil, "agentPreset": nil, "computerUseEnabled": false, "providerCursor": cursor}
}
func (s *smoke) prompt(session, runtime, text string) {
	s.waku.rpc(obj{"type": "prompt", "prompt": text, "turnId": uuid(), "messageId": uuid()}, session, runtime)
}
func (s *smoke) route() {
	catalog := s.waku.rpc(obj{"type": "probeProvider", "provider": "deerFlow", "binaryOverride": filepath.Join(s.root, "deerflow-acp.exe"), "discoverModels": true, "probeVersion": false})
	models := map[string]string{}
	for _, m := range array(object(catalog["probe"])["models"]) {
		v := object(m)
		models[str(v["name"])] = str(v["id"])
	}
	need(models["Local smoke model"] == "smoke-local" && strings.HasPrefix(models["Alternate local model"], "deerflow-config:"), "configured model routes missing: %v", models)
	session, runtime := uuid(), uuid()
	need(s.waku.rpc(obj{"type": "start", "options": s.options("ask", models["Alternate local model"], nil)}, session, runtime)["type"] == "started", "alternate runtime failed")
	s.waku.event(runtime, "connected", nil, nil)
	s.prompt(session, runtime, "Reply from the alternate local model.")
	need(object(s.waku.event(runtime, "turnFinished", nil, nil))["success"] == true, "alternate turn failed")
	seen := false
	for _, r := range s.alt.since(0) {
		seen = seen || r["model"] == "smoke-alt"
	}
	need(seen, "alternate endpoint and credential not used")
	s.waku.rpc(obj{"type": "closeSession"}, session, runtime)
	s.log("Waku selected alternate model endpoint and credential", obj{})
}
func (s *smoke) mcp(a *acpClient) {
	effect := filepath.Join(s.workspace, "client-mcp-effect.txt")
	secret := uuid()
	marker, callID := s.fake.plan("__desktop_mcp_record__", obj{"value": "desktop client MCP effect"})
	approvals := 0
	a.permission = func(params obj) string {
		approvals++
		need(!exists(effect), "MCP effect before approval")
		need(!strings.Contains(string(jsonBytes(params)), secret), "MCP credential in permission")
		return "allow_once"
	}
	start := s.fake.count()
	session := str(a.rpc("session/new", obj{"cwd": s.workspace, "mcpServers": []any{obj{"name": "desktop-fixture", "command": s.executable, "args": []any{"--mcp-fixture"}, "env": []any{obj{"name": "ACP_MCP_EFFECT", "value": effect}, obj{"name": "ACP_MCP_SECRET", "value": secret}}}}})["sessionId"])
	a.rpc("session/prompt", obj{"sessionId": session, "prompt": []any{obj{"type": "text", "text": marker}}})
	need(exists(effect) && read(effect) == "desktop client MCP effect" && approvals == 1, "MCP did not execute exactly once after approval")
	requests := s.fake.since(start)
	need(toolResult(requests, callID) != nil, "MCP result not sent to model")
	need(!strings.Contains(string(jsonBytes(requests)), secret) && !strings.Contains(string(jsonBytes(a.updates)), secret), "MCP credential in model or updates")
	a.rpc("session/close", obj{"sessionId": session})
	a.rpc("session/load", obj{"sessionId": session, "cwd": s.workspace, "mcpServers": []any{}})
	start = s.fake.count()
	a.rpc("session/prompt", obj{"sessionId": session, "prompt": []any{obj{"type": "text", "text": "Reply after MCP release."}}})
	for _, r := range s.fake.since(start) {
		for _, t := range array(r["tools"]) {
			need(!strings.HasPrefix(str(object(object(t)["function"])["name"]), "mcp_"), "released MCP tool still offered")
		}
	}
	a.rpc("session/close", obj{"sessionId": session})
	a.permission = nil
	s.log("Host-authorized ACP client MCP executed and released", obj{"approvals": approvals})
}
func toolResult(requests []obj, id string) obj {
	for _, r := range requests {
		for _, v := range array(r["messages"]) {
			m := object(v)
			if m["role"] == "tool" && m["tool_call_id"] == id {
				return m
			}
		}
	}
	return nil
}
func history(requests []obj, marker string, prior bool) obj {
	for _, r := range requests {
		messages := array(r["messages"])
		for i, v := range messages {
			m := object(v)
			if m["role"] == "user" && strings.Contains(messageText(m), marker) {
				if prior {
					found := false
					for _, old := range messages[:i] {
						m := object(old)
						found = found || (m["role"] == "assistant" && strings.Contains(messageText(m), reply))
					}
					need(found, "restored model request lost prior assistant history")
				}
				return r
			}
		}
	}
	panic(fmt.Errorf("local model did not receive this turn marker"))
}
func (s *smoke) startChat(session string, cursor any) (string, any) {
	runtime := uuid()
	marker := "smoke-turn-" + runtime
	need(s.waku.rpc(obj{"type": "start", "options": s.options("ask", "smoke-local", cursor)}, session, runtime)["type"] == "started", "chat runtime did not start")
	actual := s.waku.event(runtime, "connected", nil, nil)
	if cursor != nil {
		need(string(jsonBytes(actual)) == string(jsonBytes(cursor)), "reconnect changed native session")
	}
	start := s.fake.count()
	s.prompt(session, runtime, marker+": Reply with the local smoke test phrase only.")
	events := []obj{}
	finished := object(s.waku.event(runtime, "turnFinished", &events, nil))
	need(finished["success"] == true, "chat turn failed: %v", finished)
	text := ""
	for _, e := range events {
		if e["kind"] == "textDelta" {
			text += str(e["payload"])
		}
	}
	need(strings.Contains(text, reply), "missing assistant delta")
	request := history(s.fake.since(start), marker, cursor != nil)
	skill := false
	for _, v := range array(request["tools"]) {
		skill = skill || object(object(v)["function"])["name"] == "skill"
	}
	need(skill, "bundled skills not exposed")
	extraction := false
	for _, r := range s.fake.since(start) {
		if r["stream"] == true {
			continue
		}
		for _, v := range array(r["messages"]) {
			m := object(v)
			extraction = extraction || (m["role"] == "system" && strings.Contains(messageText(m), "Extract only durable facts"))
		}
	}
	need(extraction, "configured memory extraction did not run")
	s.log("Waku Start/Prompt/TextDelta/TurnFinished", obj{"runtime_id": runtime, "native_session": object(actual)["sessionId"], "restored_history": cursor != nil})
	return runtime, actual
}
func permissionOption(permission obj, decision string) string {
	for _, v := range array(permission["options"]) {
		id := str(object(v)["id"])
		if id == decision || strings.HasSuffix(id, ":"+decision) {
			return id
		}
	}
	panic(fmt.Errorf("missing permission choice %s", decision))
}
func (s *smoke) respond(session, runtime string, p obj, decision string) {
	need(s.waku.rpc(obj{"type": "respond", "requestId": p["requestId"], "optionId": permissionOption(p, decision)}, session, runtime)["type"] == "ack", "permission not acknowledged")
}
func (s *smoke) skill(session, runtime string) {
	marker, id := s.fake.plan("skill", obj{"skill": "bootstrap"})
	start := s.fake.count()
	s.prompt(session, runtime, marker+": Load the bootstrap skill then report completion.")
	count := 0
	finished := object(s.waku.event(runtime, "turnFinished", nil, func(p obj) { count++; s.respond(session, runtime, p, "allow_once") }))
	need(finished["success"] == true && count == 1, "bundled skill turn/permission failed: %v, %d", finished, count)
	result := toolResult(s.fake.since(start), id)
	need(result != nil && strings.Contains(messageText(result), "Bootstrap Soul"), "bundled skill body missing from model")
	s.log("Bundled skill loaded through Waku and Go ACP", obj{"name": "bootstrap"})
}
func (s *smoke) startApproval(session string, cursor any) (string, any) {
	runtime := uuid()
	need(s.waku.rpc(obj{"type": "start", "options": s.options("fullAccess", "smoke-local", cursor)}, session, runtime)["type"] == "started", "approval runtime failed")
	events := []obj{}
	actual := s.waku.event(runtime, "connected", &events, nil)
	if cursor != nil {
		need(string(jsonBytes(actual)) == string(jsonBytes(cursor)), "approval reconnect changed native session")
	}
	var mode any
	for _, e := range events {
		if e["kind"] == "toolApprovalChanged" {
			mode = e["payload"]
		}
	}
	if mode == nil {
		mode = s.waku.event(runtime, "toolApprovalChanged", nil, nil)
	}
	expected := "allow_always"
	if cursor != nil {
		expected = "ask"
	}
	need(mode == expected, "backend approval mode %v differs from %s", mode, expected)
	s.log("Backend approval mode confirmed", obj{"mode": mode, "restored": cursor != nil})
	return runtime, actual
}
func (s *smoke) setApproval(session, runtime, mode string) {
	r := s.waku.rpc(obj{"type": "setToolApproval", "mode": mode}, session, runtime)
	need(r["type"] == "toolApproval" && r["mode"] == mode, "approval change not acknowledged")
}
func (s *smoke) writeTurn(session, runtime, name, decision, content string, cachedReject, prior bool) string {
	need(filepath.Base(name) == name && name != "." && name != "..", "invalid write fixture name")
	path := filepath.Join(s.workspace, name)
	need(below(path, s.workspace), "write escaped workspace")
	existed := exists(path)
	if content == "" {
		need(!existed, "new fixture already exists")
		content = "Isolated approval fixture " + uuid() + "\n"
	} else {
		need(!existed || read(path) == content, "repeat fixture changed before request")
	}
	marker, callID := s.fake.plan("write_file", obj{"path": name, "content": content})
	start := s.fake.count()
	count := 0
	s.prompt(session, runtime, marker+": Perform the isolated smoke write, then report completion.")
	finished := object(s.waku.event(runtime, "turnFinished", nil, func(p obj) {
		count++
		need(decision != "" && count == 1, "unexpected/repeated approval")
		s.respond(session, runtime, p, decision)
	}))
	need(finished["success"] == true, "approval turn failed: %v", finished)
	expected := 0
	if decision != "" {
		expected = 1
	}
	need(count == expected, "wrong permission count %d", count)
	requests := s.fake.since(start)
	request := history(requests, marker, prior)
	offered := false
	for _, tool := range array(request["tools"]) {
		offered = offered || object(object(tool)["function"])["name"] == "write_file"
	}
	need(offered, "write_file was not offered for this turn")
	need(toolResult(requests, callID) != nil, "write_file result for this exact call did not reach the model")
	denied := cachedReject || decision == "reject_once" || decision == "reject_always"
	if denied {
		need(exists(path) == existed && (!existed || read(path) == content), "rejected write changed workspace")
	} else {
		need(exists(path) && read(path) == content, "allowed write did not produce exact fixture")
	}
	s.log("Tool approval enforced on real write_file", obj{"filename": name, "decision": decision, "permission_count": count, "denied": denied, "restored_history": prior})
	return content
}
func (s *smoke) run() {
	token := uuid()
	wakuProcess := s.startWaku(token, "waku-daemon")
	saved := s.configure()
	w := s.waku
	w.deerflow("start")
	state := s.settled()
	generation := state["applied_generation"]
	a := s.acp("acp-before-apply")
	s.route()
	if !s.legacy {
		s.mcp(a)
	} else {
		s.log("Legacy enabled client MCP with empty allowlist loaded", obj{})
	}
	original := str(a.rpc("session/new", obj{"cwd": s.workspace, "mcpServers": []any{}})["sessionId"])
	probe := str(a.rpc("session/new", obj{"cwd": s.workspace, "mcpServers": []any{}})["sessionId"])
	a.rpc("session/close", obj{"sessionId": probe})
	listed := a.rpc("session/list", obj{})["sessions"]
	need(containsID(listed, "sessionId", original) && containsID(listed, "sessionId", probe), "session close lost retained history")
	a.rpc("session/load", obj{"sessionId": original, "cwd": s.workspace, "mcpServers": []any{}})
	err := safe(func() { w.deerflow("manage", obj{"operation": "session.delete", "session_id": original}) })
	need(err != nil && strings.Contains(err.Error(), "session is attached"), "attached-session deletion not rejected: %v", err)
	s.log("ACP initialize/new/list/load/close", obj{"original_session": original})
	session, approvalSession := uuid(), uuid()
	oldRuntime, cursor := s.startChat(session, nil)
	s.skill(session, oldRuntime)
	approvalRuntime, approvalCursor := s.startApproval(approvalSession, nil)
	s.writeTurn(approvalSession, approvalRuntime, "full-access.txt", "", "", false, false)
	s.setApproval(approvalSession, approvalRuntime, "ask")
	s.writeTurn(approvalSession, approvalRuntime, "ask-rejected.txt", "reject_once", "", false, false)
	s.writeTurn(approvalSession, approvalRuntime, "ask-allowed-once.txt", "allow_once", "", false, false)
	allowed := s.writeTurn(approvalSession, approvalRuntime, "ask-allowed-always.txt", "allow_always", "", false, false)
	s.writeTurn(approvalSession, approvalRuntime, "ask-allowed-always.txt", "", allowed, false, false)
	s.setApproval(approvalSession, approvalRuntime, "ask")
	s.writeTurn(approvalSession, approvalRuntime, "ask-allowed-always.txt", "reject_once", allowed, false, false)
	rejected := s.writeTurn(approvalSession, approvalRuntime, "ask-rejected-always.txt", "reject_always", "", false, false)
	s.writeTurn(approvalSession, approvalRuntime, "ask-rejected-always.txt", "", rejected, true, false)
	s.setApproval(approvalSession, approvalRuntime, "ask")
	s.writeTurn(approvalSession, approvalRuntime, "ask-rejected-always.txt", "allow_once", rejected, false, false)
	s.log("Explicit Ask clears allow-always and reject-always caches", obj{})
	changed := clone(saved)
	object(array(changed["models"])[0])["display_name"] = "Applied smoke model"
	applied := w.deerflow("save-and-apply", changed)
	saved = object(applied["document"])
	need(object(array(saved["models"])[0])["display_name"] == "Applied smoke model" && str(object(applied["status"])["apply_error"]) == "", "save-and-apply failed")
	state = s.settled()
	need(state["config_revision"] == saved["config_revision"], "runtime did not apply saved revision")
	if generation != nil {
		need(state["applied_generation"].(float64) > generation.(float64), "apply generation did not advance")
	}
	select {
	case <-a.process.done:
	case <-time.After(15 * time.Second):
		panic(fmt.Errorf("apply left old ACP connection open"))
	}
	after := s.acp("acp-after-apply")
	need(containsID(after.rpc("session/list", obj{})["sessions"], "sessionId", original), "apply lost sessions")
	after.rpc("session/load", obj{"sessionId": original, "cwd": s.workspace, "mcpServers": []any{}})
	after.rpc("session/close", obj{"sessionId": original})
	s.log("ACP session restored after apply", obj{})
	deleted := w.deerflow("manage", obj{"operation": "session.delete", "session_id": original})
	found := false
	for _, id := range array(deleted["deleted"]) {
		found = found || id == original
	}
	need(found, "management deletion not confirmed")
	need(!containsID(w.deerflow("manage", obj{"operation": "session.list"})["sessions"], "session_id", original), "deleted history reappeared")
	need(w.deerflow("manage", obj{"operation": "session.delete", "session_id": original})["already_deleted"] == true, "deletion is not retryable")
	s.log("History deletion confirmed and retryable", obj{})
	desktopID, projectID := uuid(), uuid()
	now := time.Now().Unix()
	oldSave := obj{"type": "saveTaskState", "projects": []any{obj{"id": projectID, "name": "Smoke", "path": s.workspace, "created_at": now}}, "liveSessionIds": []any{desktopID}, "sessions": []any{obj{"id": desktopID, "project_id": projectID, "title": "Delete regression", "provider": "deerFlow", "runtime_mode": "ask", "status": "idle", "created_at": now, "updated_at": now, "provider_cursor": obj{"provider": "deerFlow", "sessionId": original}}}}
	w.rpc(oldSave)
	need(containsID(w.rpc(obj{"type": "loadTaskState"})["sessions"], "id", desktopID), "desktop fixture not persisted")
	need(w.rpc(obj{"type": "removeSession"}, desktopID)["type"] == "ack", "desktop deletion not acknowledged")
	w.rpc(oldSave)
	need(!containsID(w.rpc(obj{"type": "loadTaskState"})["sessions"], "id", desktopID), "stale save resurrected deleted row")
	s.log("Stale save cannot resurrect desktop deletion", obj{})
	deadline := time.Now().Add(5 * time.Second)
	for {
		attached := w.rpc(obj{"type": "attachSession"}, session)
		if attached["runtimeId"] == nil {
			break
		}
		need(time.Now().Before(deadline), "dead runtime retained after apply")
		time.Sleep(200 * time.Millisecond)
	}
	current, _ := s.startChat(session, cursor)
	w.rpc(obj{"type": "closeSession"}, session, current)
	approvalRuntime, _ = s.startApproval(approvalSession, approvalCursor)
	s.writeTurn(approvalSession, approvalRuntime, "restored-ask-rejected.txt", "reject_once", "", false, true)
	w.rpc(obj{"type": "closeSession"}, approvalSession, approvalRuntime)
	after.process.stop()
	w.deerflow("stop")
	need(w.deerflow("status")["running"] != true, "service did not stop")
	discovery := w.rpc(obj{"type": "probeProvider", "provider": "deerFlow", "binaryOverride": filepath.Join(s.root, "deerflow-acp.exe"), "discoverModels": true, "probeVersion": false})
	need(discovery["type"] == "providerProbe", "wrong discovery response")
	provider := object(discovery["probe"])
	need(provider["provider"] == "deerFlow" && provider["installed"] == true && strings.EqualFold(str(provider["path"]), filepath.Join(s.root, "deerflow-acp.exe")), "discovery escaped package")
	need(w.deerflow("status")["running"] != true, "model discovery restarted stopped service")
	time.Sleep(time.Second)
	need(w.deerflow("status")["running"] != true, "stopped service restarted")
	s.log("Stopped service stays stopped after real model discovery", obj{})
	w.ws.send(obj{"type": "shutdown"})
	w.ws.conn.Close()
	wakuProcess.stop()
	s.startWaku(token, "waku-daemon-restarted")
	need(!containsID(s.waku.rpc(obj{"type": "loadTaskState"})["sessions"], "id", desktopID), "deleted row reappeared after daemon restart")
	s.log("Desktop deletion survives daemon restart", obj{})
}

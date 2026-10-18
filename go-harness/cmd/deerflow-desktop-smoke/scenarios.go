package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// Exercise recovery with the delivered executables, without modifying the
// supplied installation or sending a model request while configuration is bad.
func (s *smoke) startupRecovery() obj {
	var raw map[string]any
	must(yaml.Unmarshal([]byte(read(s.config)), &raw))
	settings := raw["skills"].(map[string]any)
	settings["managed_sources"] = []any{obj{"id": "managed-evolution", "root": "../skills/smoke-recovery-evolved", "scope": "global"}}
	settings["export_root"] = "../skills-exports/smoke-recovery"
	write := func() []byte {
		body, err := yaml.Marshal(raw)
		must(err)
		must(os.WriteFile(s.config, body, 0600))
		return body
	}
	before := write()
	need(!exists(filepath.Join(s.root, "user-data/skills/smoke-recovery-evolved")), "managed fixture already exists")
	document := s.waku.deerflow("snapshot")
	need(bytes.Equal(before, []byte(read(s.config))), "bootstrap changed copied configuration")
	need(exists(filepath.Join(s.root, "user-data/skills/smoke-recovery-evolved")) && exists(filepath.Join(s.root, "user-data/skills-exports/smoke-recovery")), "portable managed stores not initialized")
	need(exists(filepath.Join(s.root, "user-data/skills/public/bootstrap/SKILL.md")), "store provisioning lost initial bundled skills")
	s.log("Copied configuration initializes missing portable Skills stores", obj{})

	// External custom sources must be diagnosed, never silently created.
	outside := filepath.Join(s.root, "missing-external-source")
	settings["managed_sources"] = []any{obj{"id": "managed-evolution", "root": outside, "scope": "global"}}
	write()
	document = s.waku.deerflow("snapshot")
	need(strings.Contains(string(jsonBytes(document["skill_source_diagnostics"])), "skills.managed_sources.0"), "missing external source not diagnosed")
	requests := s.fake.count()
	started := time.Now()
	state := s.waku.deerflow("start")
	if state["applying"] == true {
		need(state["starting"] == true, "automatic startup is indistinguishable from blocking apply")
		candidate := clone(document)
		candidate["skills_enabled"] = false
		need(s.waku.deerflow("validate", candidate)["valid"] == true, "startup prevents validating a repair")
	}
	deadline := time.Now().Add(8 * time.Second)
	for state["applying"] == true && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
		state = s.waku.deerflow("status")
	}
	need(state["applying"] == false && state["starting"] == false && state["running"] == false, "failed startup did not release its state: %v", state)
	need(strings.Contains(str(state["apply_error"]), "exited before becoming ready"), "startup failure hidden behind timeout: %v", state)
	need(time.Since(started) < 8*time.Second, "failed daemon was not detected promptly")
	need(s.fake.count() == requests, "bad configuration reached the model")
	need(!exists(outside), "external custom source was provisioned")
	document = s.waku.deerflow("snapshot")
	document["skills_enabled"] = false
	document = s.waku.deerflow("save", document)
	need(document["skills_enabled"] == false, "failed startup prevented saving a repair")

	// Prove an in-flight automatic start cannot block configuration writes.
	// Everything here is in the disposable copy, including the daemon fixture.
	daemon := filepath.Join(s.root, "deerflow-acpd.exe")
	backup := filepath.Join(s.root, "smoke-real-daemon.exe")
	marker := filepath.Join(s.root, "smoke-startup-delay.fixture")
	must(os.Rename(daemon, backup))
	copyFile(s.executable, daemon)
	must(os.WriteFile(marker, []byte("isolated fixture"), 0600))
	state = s.waku.deerflow("start")
	need(state["applying"] == true && state["starting"] == true, "slow startup fixture did not enter automatic startup")
	object(array(document["models"])[0])["description"] = "Saved during automatic startup"
	document = s.waku.deerflow("save", document)
	need(s.waku.deerflow("status")["applying"] == true, "save did not overlap the in-flight startup fixture")
	deadline = time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		state = s.waku.deerflow("status")
		if state["applying"] == false {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	need(state["applying"] == false && strings.Contains(str(state["apply_error"]), "17"), "slow failed startup did not settle")
	need(s.waku.deerflow("snapshot")["config_revision"] == document["config_revision"], "startup failure rolled back the saved repair")
	must(os.Remove(daemon))
	must(os.Rename(backup, daemon))
	must(os.Remove(marker))
	s.log("Configuration saves during automatic startup and survives its failure", obj{})
	object(document["skill_sources"])["managed"] = []any{obj{"id": "managed-evolution", "root": "../skills/smoke-recovery-evolved", "scope": "global", "workspace": ""}}
	document["skills_enabled"] = true
	document = s.waku.deerflow("save", document)
	s.log("Failed startup returns promptly and configuration can be repaired", obj{"elapsed_ms": time.Since(started).Milliseconds()})
	return document
}

func (s *smoke) options(mode, model string, cursor any) obj {
	return obj{"provider": "deerFlow", "binary": filepath.Join(s.root, "deerflow-acp.exe"), "cwd": s.workspace, "mode": mode, "model": model, "reasoningEffort": "off", "serviceTier": nil, "contextWindow": nil, "agentPreset": nil, "computerUseEnabled": false, "providerCursor": cursor}
}
func (s *smoke) prompt(session, runtime, text string) {
	s.waku.rpc(obj{"type": "prompt", "prompt": text, "turnId": uuid(), "messageId": uuid()}, session, runtime)
}

func (s *smoke) textAttachment(session, runtime string, first bool) {
	marker, callID := s.fake.plan("__desktop_attachment_read__", obj{})
	start := s.fake.count()
	request := obj{"type": "prompt", "prompt": marker + ": Read the attached CSV.", "turnId": uuid(), "messageId": uuid()}
	var storedPath string
	if first {
		original := filepath.Join(s.root, "attachment-original.csv")
		must(os.WriteFile(original, []byte("mvp-attachment-body-42,hello\n"), 0600))
		stored := object(s.waku.rpc(obj{"type": "importPathAttachment", "path": original})["attachment"])
		must(os.Remove(original))
		storedPath = str(stored["path"])
		need(below(storedPath, s.root), "attachment escaped isolated root")
		request["type"] = "promptWithAttachments"
		request["attachments"] = []any{obj{"path": storedPath, "mention": stored["name"], "name": stored["name"], "is_dir": false, "is_image": false, "blob_reference": stored["reference"]}}
	}
	s.waku.rpc(request, session, runtime)
	finished := object(s.waku.event(runtime, "turnFinished", nil, nil))
	need(finished["success"] == true, "text attachment turn failed: %v", finished)
	read := false
	for _, r := range s.fake.since(start) {
		for _, v := range array(r["messages"]) {
			m := object(v)
			if m["role"] == "user" {
				need(!strings.Contains(messageText(m), "mvp-attachment-body-42"), "attachment inlined into model user prompt")
			}
			if m["role"] == "tool" && m["tool_call_id"] == callID {
				read = strings.Contains(messageText(m), "mvp-attachment-body-42")
			}
		}
	}
	need(read, "model did not read attachment through scoped tool")
	if first {
		must(os.Remove(storedPath))
	}
	s.log("Waku text attachment read from immutable session snapshot", obj{"after_service_restart": !first, "original_removed": true})
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
		// The tracked Generate API now consumes a stream for memory extraction.
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
	catalog := s.waku.deerflow("manage", obj{"operation": "skill.list", "status": "current", "limit": 100})
	need(len(array(catalog["skills"])) > 0, "current skill catalog is empty")
	for _, item := range array(catalog["skills"]) {
		need(object(object(item)["head"])["exists"] == true, "current catalog exposed a deleted skill")
	}
	s.log("Current skill catalog filter accepted through Waku and Go management", obj{})
	marker, id := s.fake.plan("skill", obj{"skill": "bootstrap"})
	start := s.fake.count()
	s.prompt(session, runtime, marker+": Load the bootstrap skill then report completion.")
	count := 0
	finished := object(s.waku.event(runtime, "turnFinished", nil, func(p obj) { count++; s.respond(session, runtime, p, "allow_once") }))
	need(finished["success"] == true && count == 0, "read-only skill unexpectedly required approval: %v, %d", finished, count)
	result := toolResult(s.fake.since(start), id)
	need(result != nil && strings.Contains(messageText(result), "Bootstrap Soul"), "bundled skill body missing from model")
	s.log("Bundled skill loaded through Waku and Go ACP without approval", obj{"name": "bootstrap", "permission_count": count})
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

func (s *smoke) cancelThenChat(session, runtime string) {
	for _, phase := range []string{"stream", "permission"} {
		path := filepath.Join(s.workspace, "cancelled-permission.txt")
		if phase == "stream" {
			s.prompt(session, runtime, "smoke-cancel-stream-"+uuid())
			s.waku.event(runtime, "textDelta", nil, nil)
		} else {
			marker, _ := s.fake.plan("write_file", obj{"path": filepath.Base(path), "content": "must not execute"})
			s.prompt(session, runtime, marker+": wait for permission, then stop")
			s.waku.event(runtime, "permission", nil, nil)
		}
		need(s.waku.rpc(obj{"type": "cancel"}, session, runtime)["type"] == "ack", "cancel not acknowledged")
		s.waku.event(runtime, "turnFinished", nil, nil)
		s.prompt(session, runtime, "Reply after stopping "+phase+"; do not retry old tools.")
		var events []obj
		finished := object(s.waku.event(runtime, "turnFinished", &events, nil))
		text := ""
		for _, event := range events {
			if event["kind"] == "textDelta" {
				text += str(event["payload"])
			}
		}
		need(finished["success"] == true && strings.Contains(text, reply), "fresh prompt after %s stop failed: %v %q", phase, finished, text)
		need(!exists(path), "cancelled permission executed during a new prompt")
		s.log("Cancel then prompt on the same Waku/ACP runtime", obj{"phase": phase})
	}
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
	saved = s.startupRecovery()
	s.preparePythonSkill()
	w := s.waku
	beforeReadiness := s.fake.count()
	readiness := w.deerflow("check-readiness")
	need(readiness["network_checked"] == false && readiness["config_revision"] == saved["config_revision"] && len(array(readiness["items"])) == 4, "invalid local readiness result")
	need(s.fake.count() == beforeReadiness, "local readiness invoked model")
	s.log("Local readiness uses saved revision without model requests", obj{})
	w.deerflow("start")
	state := s.settled()
	s.schedules()
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
	s.textAttachment(session, oldRuntime, true)
	s.skill(session, oldRuntime)
	s.pythonSkill(session, oldRuntime)
	approvalRuntime, approvalCursor := s.startApproval(approvalSession, nil)
	s.writeTurn(approvalSession, approvalRuntime, "full-access.txt", "", "", false, false)
	s.setApproval(approvalSession, approvalRuntime, "ask")
	s.cancelThenChat(approvalSession, approvalRuntime)
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
	s.textAttachment(session, current, false)
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

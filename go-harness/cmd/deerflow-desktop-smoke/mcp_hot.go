package main

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

func (s *smoke) runHotMCP() {
	s.startWaku(uuid(), "waku-mcp")
	saved := s.configure(false)
	w := s.waku
	w.deerflow("start")
	state := s.settled()
	yaml := read(s.config)
	endpoint := read(filepath.Join(s.runtime, "endpoint.json"))
	session, runtime := uuid(), uuid()
	w.rpc(obj{"type": "start", "options": s.options("ask", "smoke-local", nil)}, session, runtime)
	cursor := w.event(runtime, "connected", nil, nil)
	revision := str(saved["client_mcp_servers_revision"])
	save := func(servers []any) {
		result := w.deerflow("save-mcp", obj{"client_mcp_servers_revision": revision, "client_mcp_servers": servers})
		revision = str(result["client_mcp_servers_revision"])
		need(!strings.Contains(string(jsonBytes(result)), "private-fixture-key"), "MCP save exposed its environment")
	}
	probe := func(marker string, wantFixture, wantOther bool) {
		start := s.fake.count()
		s.prompt(session, runtime, marker)
		need(object(w.event(runtime, "turnFinished", nil, nil))["success"] == true, "MCP probe failed")
		request := history(s.fake.since(start), marker, marker != "Before adding desktop MCP")
		var fixture, other bool
		for _, t := range array(request["tools"]) {
			name := str(object(object(t)["function"])["name"])
			fixture = fixture || strings.HasPrefix(name, "mcp_desktop_fixture_record")
			other = other || strings.HasPrefix(name, "mcp_desktop_other_record")
		}
		need(fixture == wantFixture && other == wantOther, "unexpected MCP tools for %s: %t, %t", marker, fixture, other)
	}
	probe("Before adding desktop MCP", false, false)
	effect := filepath.Join(s.workspace, "hot-mcp-effect.txt")
	program := filepath.Join(s.workspace, "updating-fixture"+filepath.Ext(s.executable))
	copyFile(s.executable, program)
	stamp := time.Unix(499190400, 0)
	must(os.Chtimes(program, stamp, stamp))
	fixtureState := filepath.Join(s.workspace, "fixture-state")
	otherState := filepath.Join(s.workspace, "other-state")
	first := obj{"name": "desktop-fixture", "command": program, "args": []any{"--mcp-fixture"}, "env": []any{obj{"name": "ACP_MCP_EFFECT", "value": effect}, obj{"name": "ACP_MCP_SECRET", "value": "private-fixture-key"}, obj{"name": "ACP_MCP_FIXTURE_STATE", "value": fixtureState}}}
	other := obj{"name": "desktop-other", "command": s.executable, "args": []any{"--mcp-fixture=true"}, "env": []any{obj{"name": "ACP_MCP_EFFECT", "value": effect + ".other"}, obj{"name": "ACP_MCP_SECRET", "value": "private-fixture-key"}, obj{"name": "ACP_MCP_FIXTURE_STATE", "value": otherState}}}
	save([]any{first, other})
	probe("After adding two desktop servers", true, true)
	firstPID, otherPID := read(fixtureState+".pid"), read(otherState+".pid")
	sidecar := filepath.Join(filepath.Dir(s.config), "client-mcp-servers.json")
	definitions := read(sidecar)
	// Replace a running image at its installed path. Both versions deliberately
	// have identical bytes, size and timestamps; their file IDs are different.
	must(os.Rename(program, program+".previous"))
	copyFile(s.executable, program)
	must(os.Chtimes(program, stamp, stamp))
	probe("After an in-place desktop MCP upgrade", true, true)
	need(read(fixtureState+".pid") != firstPID && read(otherState+".pid") == otherPID, "program replacement did not restart only the upgraded MCP")
	must(os.Rename(program, program+".installing"))
	probe("While the executable is absent", false, true)
	must(os.WriteFile(fixtureState+".fail", []byte("fail"), 0600))
	must(os.Rename(program+".installing", program))
	probe("While the replacement fails its handshake", false, true)
	otherMarker, otherCall := s.fake.plan("__desktop_other_record__", obj{"value": "healthy tool worked"})
	beforeOther := s.fake.count()
	s.prompt(session, runtime, otherMarker)
	result := w.event(runtime, "turnFinished", nil, func(p obj) { s.respond(session, runtime, p, "allow_once") })
	need(object(result)["success"] == true && read(effect+".other") == "healthy tool worked" && toolResult(s.fake.since(beforeOther), otherCall) != nil, "healthy MCP failed during sibling outage")
	must(os.Remove(fixtureState + ".fail"))
	probe("After the same executable recovers", true, true)
	pid := read(fixtureState + ".pid")
	must(os.WriteFile(fixtureState+".stop", []byte(pid), 0600))
	deadline := time.Now().Add(5 * time.Second)
	for !exists(fixtureState+".stopped") && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	need(read(fixtureState+".stopped") == pid, "fixture did not exit")
	probe("After the MCP process exits independently", true, true)
	need(read(fixtureState+".pid") != pid && read(otherState+".pid") == otherPID, "unexpected process identities after recovery")
	need(read(sidecar) == definitions, "upgrade or recovery changed saved authorization")
	s.log("Original chat retained history and a healthy tool through replacement, absence, failed handshake and process exit", obj{"native_session": object(cursor)["sessionId"]})
	marker, call := s.fake.plan("__desktop_mcp_record__", obj{"value": "hot MCP succeeded"})
	start := s.fake.count()
	s.prompt(session, runtime, marker)
	approvals := 0
	finished := w.event(runtime, "turnFinished", nil, func(p obj) {
		approvals++
		// Save removal while the original turn is waiting for approval.
		save([]any{other})
		s.respond(session, runtime, p, "allow_once")
	})
	need(object(finished)["success"] == true && approvals == 1 && read(effect) == "hot MCP succeeded", "in-flight MCP call was not preserved")
	need(toolResult(s.fake.since(start), call) != nil, "MCP tool result missing")
	probe("After removing only the first desktop MCP", false, true)
	save([]any{})
	probe("After removing the final desktop MCP", false, false)
	need(w.deerflow("status")["applied_generation"] == state["applied_generation"] && read(filepath.Join(s.runtime, "endpoint.json")) == endpoint, "MCP edits restarted the runtime")
	need(read(s.config) == yaml, "MCP edits changed host YAML")
	w.rpc(obj{"type": "closeSession"}, session, runtime)
	s.log("Live chat uses added, replaced and removed tools on its next turn; active call completed", obj{"native_session": object(cursor)["sessionId"], "runtime": runtime, "approvals": approvals})
}

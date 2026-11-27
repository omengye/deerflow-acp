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
	probe("After adding two desktop servers", false, false)
	need(!exists(fixtureState+".pid") && !exists(otherState+".pid"), "ordinary chat started a saved MCP program")
	execute := func(name, value, path string, want bool) {
		marker, call := s.fake.plan(name, obj{"value": value})
		start := s.fake.count()
		s.prompt(session, runtime, marker)
		finished := w.event(runtime, "turnFinished", nil, func(p obj) { s.respond(session, runtime, p, "allow_once") })
		need(object(finished)["success"] == true, "targeted MCP turn failed")
		if want {
			need(read(path) == value && toolResult(s.fake.since(start), call) != nil, "expected original MCP tool and receipt in same turn")
		} else {
			need(!exists(path) || read(path) != value, "unavailable MCP executed")
		}
	}
	execute("__desktop_mcp_record__", "first targeted service", effect, true)
	need(!exists(otherState+".pid"), "targeted discovery started its sibling")
	execute("__desktop_other_record__", "second targeted service", effect+".other", true)
	firstPID, otherPID := read(fixtureState+".pid"), read(otherState+".pid")
	sidecar := filepath.Join(filepath.Dir(s.config), "client-mcp-servers.json")
	definitions := read(sidecar)
	// Replace a running image at its installed path. Both versions deliberately
	// have identical bytes, size and timestamps; their file IDs are different.
	must(os.Rename(program, program+".previous"))
	copyFile(s.executable, program)
	must(os.Chtimes(program, stamp, stamp))
	probe("After an in-place desktop MCP upgrade", false, false)
	need(read(fixtureState+".pid") == firstPID, "ordinary chat started the replacement")
	execute("__desktop_mcp_record__", "replaced executable", effect, true)
	need(read(fixtureState+".pid") != firstPID && read(otherState+".pid") == otherPID, "program replacement did not restart only the upgraded MCP")
	must(os.Rename(program, program+".installing"))
	probe("While the executable is absent", false, false)
	must(os.WriteFile(fixtureState+".fail", []byte("fail"), 0600))
	must(os.Rename(program+".installing", program))
	execute("__desktop_mcp_record__", "failed handshake", effect, false)
	otherMarker, otherCall := s.fake.plan("__desktop_other_record__", obj{"value": "healthy tool worked"})
	beforeOther := s.fake.count()
	s.prompt(session, runtime, otherMarker)
	result := w.event(runtime, "turnFinished", nil, func(p obj) { s.respond(session, runtime, p, "allow_once") })
	need(object(result)["success"] == true && read(effect+".other") == "healthy tool worked" && toolResult(s.fake.since(beforeOther), otherCall) != nil, "healthy MCP failed during sibling outage")
	must(os.Remove(fixtureState + ".fail"))
	execute("__desktop_mcp_record__", "recovered", effect, true)
	probe("After the same executable recovers", true, true)
	pid := read(fixtureState + ".pid")
	must(os.WriteFile(fixtureState+".stop", []byte(pid), 0600))
	deadline := time.Now().Add(5 * time.Second)
	for !exists(fixtureState+".stopped") && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	need(read(fixtureState+".stopped") == pid, "fixture did not exit")
	probe("After the MCP process exits independently", true, true)
	need(read(fixtureState+".pid") == pid, "ordinary chat restarted a crashed service")
	execute("__desktop_mcp_record__", "restarted on demand", effect, true)
	need(read(fixtureState+".pid") != pid && read(otherState+".pid") == otherPID, "unexpected process identities after recovery")
	need(read(sidecar) == definitions, "upgrade or recovery changed saved authorization")
	s.log("Original chat retained history and a healthy tool through replacement, absence, failed handshake and process exit", obj{"native_session": object(cursor)["sessionId"]})
	marker, _ := s.fake.plan("__desktop_mcp_record__", obj{"value": "hot MCP succeeded"})
	s.prompt(session, runtime, marker)
	approvals := 0
	failure := w.event(runtime, "error", nil, func(p obj) {
		approvals++
		// Save removal while the original turn is waiting for approval.
		save([]any{other})
		s.respond(session, runtime, p, "allow_once")
	})
	need(approvals == 1 && read(effect) != "hot MCP succeeded", "revoked MCP executed after approval")
	need(strings.Contains(str(failure), "MCP resources are unavailable") && object(w.event(runtime, "turnFinished", nil, nil))["success"] == false, "revocation did not report a fenced call: %v", failure)
	// Rebinding owns a new per-session set. Merely reading saved catalog
	// summaries after the edit does not launch the surviving program.
	probe("After removing only the first desktop MCP", false, false)
	save([]any{})
	probe("After removing the final desktop MCP", false, false)
	need(w.deerflow("status")["applied_generation"] == state["applied_generation"] && read(filepath.Join(s.runtime, "endpoint.json")) == endpoint, "MCP edits restarted the runtime")
	need(read(s.config) == yaml, "MCP edits changed host YAML")
	w.rpc(obj{"type": "closeSession"}, session, runtime)
	s.log("Ordinary chats launched zero MCP programs, targeted discovery reached the original tool in one turn, and removal fenced a pending call", obj{"native_session": object(cursor)["sessionId"], "runtime": runtime, "approvals": approvals})
}

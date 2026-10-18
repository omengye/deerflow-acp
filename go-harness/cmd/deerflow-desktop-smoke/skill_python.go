package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

func (s *smoke) preparePythonSkill() {
	if s.python == "" {
		return
	}
	root := filepath.Join(s.root, "user-data", "skills", "custom", "python-smoke")
	must(os.MkdirAll(root, 0700))
	for name, content := range map[string]string{
		"SKILL.md":           "---\nname: python-smoke\ndescription: Run an isolated local smoke fixture\n---\nUse skill_run to check then run run.py.\n",
		"run.py":             "import pathlib,json,sys\npathlib.Path('python-skill-argv.json').write_text(json.dumps(sys.argv[1:]))\nprint('pinned-source-v1')\n",
		"skill-runtime.json": `{"version":1,"python":{"min_version":"3.8","packages":[],"entrypoints":["run.py"]}}`,
	} {
		must(os.WriteFile(filepath.Join(root, name), []byte(content), 0600))
	}
}

func (s *smoke) pythonSkill(session, runtime string) {
	name := "bootstrap"
	if s.python != "" {
		name = "python-smoke"
	}
	marker, id := s.fake.plan("skill_run", obj{"action": "check", "name": name})
	start, approvals := s.fake.count(), 0
	s.prompt(session, runtime, marker+": Check Python readiness only; do not execute skill code.")
	finish := object(s.waku.event(runtime, "turnFinished", nil, func(p obj) {
		approvals++
		s.respond(session, runtime, p, "allow_once")
	}))
	result := toolResult(s.fake.since(start), id)
	need(finish["success"] == true && approvals == 1 && result != nil, "skill check did not use native approval: %v", finish)
	if s.python == "" {
		need(strings.Contains(messageText(result), `"code":"python_unavailable_or_not_authorized"`) && strings.Contains(messageText(result), `"execution_checked":false`), "missing Python was not reported as runtime-unready: %v", result)
		s.log("Python absent: skill check is runtime-unready, Go host remains usable", obj{})
		return
	}
	need(strings.Contains(messageText(result), `"ready":true`) && strings.Contains(messageText(result), `"execution_checked":false`), "readiness falsely executed a script: %v", result)
	output := filepath.Join(s.workspace, "python-skill-argv.json")
	need(!exists(output), "readiness ran skill code")
	args := []string{"a b", "a'b", `$(whoami)`, `"`, ""}
	for _, decision := range []string{"reject_once", "allow_once"} {
		marker, id = s.fake.plan("skill_run", obj{"action": "run", "name": name, "path": "run.py", "args": args})
		start, approvals = s.fake.count(), 0
		s.prompt(session, runtime, marker+": Run the packaged local test fixture.")
		finish = object(s.waku.event(runtime, "turnFinished", nil, func(p obj) {
			approvals++
			if decision == "allow_once" {
				// Approval occurs after snapshot binding. Mutable source V2 must not run.
				must(os.WriteFile(filepath.Join(s.root, "user-data", "skills", "custom", name, "run.py"), []byte("raise Exception('mutable-source-v2')\n"), 0600))
			}
			s.respond(session, runtime, p, decision)
		}))
		need(finish["success"] == true && approvals == 1, "script approval flow failed: %v", finish)
		if decision == "reject_once" {
			need(!exists(output), "denied script produced an effect")
			continue
		}
		result = toolResult(s.fake.since(start), id)
		need(result != nil && strings.Contains(messageText(result), `"execution_checked":true`) && strings.Contains(messageText(result), "pinned-source-v1"), "pinned script receipt missing: %v", result)
		var actual []string
		must(json.Unmarshal([]byte(read(output)), &actual))
		need(string(jsonBytes(actual)) == string(jsonBytes(args)), "PowerShell corrupted skill argv: %v", actual)
	}
	s.log("Pinned Python skill: readiness, denial, approval, immutable source and exact argv through Waku/Go", obj{})
}

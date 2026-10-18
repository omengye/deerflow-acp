package main

import (
	"encoding/json"
	"path/filepath"
	"time"
)

// Exercise the same structured management transport as the native settings
// page. Only this smoke's isolated workspace/database and fake model are used.
func (s *smoke) schedules() {
	w := s.waku
	call := func(request obj) obj { return w.deerflow("manage-envelope", request) }
	snapshot := call(obj{"operation": "schedule.snapshot"})
	need(snapshot["ok"] == true, "schedule snapshot failed: %v", snapshot)
	data := object(snapshot["data"])
	need(len(array(data["items"])) == 0 && object(data["status"])["running"] == true, "isolated scheduler not ready")
	instance := object(snapshot["runtime"])["instance_id"]
	name, content := "schedule-full-access.txt", "scheduled Full Access fixture\n"
	path := filepath.Join(s.workspace, name)
	need(!exists(path), "schedule fixture already exists")
	marker, callID := s.fake.plan("write_file", obj{"path": name, "content": content})
	modelStart := s.fake.count()
	session, runtime := uuid(), uuid()
	need(w.rpc(obj{"type": "start", "options": s.options("fullAccess", "smoke-local", nil)}, session, runtime)["type"] == "started", "schedule chat failed to start")
	w.event(runtime, "connected", nil, nil)
	createMarker, createCall := s.fake.plan("schedule_manage", obj{"action": "create", "request_id": "native-schedule-smoke", "name": "Native conversation smoke", "prompt": marker + ": Perform the isolated scheduled write, then report completion.", "timing": obj{"kind": "once", "at": time.Now().Add(time.Hour).Format(time.RFC3339), "timezone": "Asia/Shanghai"}})
	s.prompt(session, runtime, createMarker+": Create a native scheduled task to perform this write in one hour.")
	finished := object(w.event(runtime, "turnFinished", nil, nil))
	need(finished["success"] == true, "schedule conversation failed: %v", finished)
	receipt := toolResult(s.fake.since(modelStart), createCall)
	need(receipt != nil, "native schedule creation did not return a model tool receipt")
	var item obj
	must(json.Unmarshal([]byte(messageText(receipt)), &item))
	need(object(object(item["spec"])["source"])["kind"] == "desktop", "creation lost host identity")
	w.rpc(obj{"type": "closeSession"}, session, runtime)
	id := object(item["spec"])["id"]
	need(str(id) != "", "native creation returned no task ID")
	visible := object(call(obj{"operation": "schedule.snapshot"})["data"])
	need(len(array(visible["items"])) == 1 && object(object(array(visible["items"])[0])["spec"])["id"] == id, "conversation-created task is not visible to settings")
	need(object(item["spec"])["approval"] == "allow_always", "new schedule did not default to Full Access")
	timing := obj{"kind": "once", "at": time.Now().Add(10 * time.Second).Format(time.RFC3339), "timezone": "Asia/Shanghai"}
	preview := call(obj{"operation": "schedule.preview", "timing": timing, "expected_instance_id": instance})
	need(preview["ok"] == true && str(object(preview["data"])["next_local"]) != "", "schedule preview failed")
	rescheduled := call(obj{"operation": "schedule.reschedule", "id": id, "expected_revision": item["revision"], "timing": timing, "expected_instance_id": instance})
	need(rescheduled["ok"] == true, "reschedule failed: %v", rescheduled)
	conflict := call(obj{"operation": "schedule.delete", "id": id, "expected_revision": item["revision"], "expected_instance_id": instance})
	need(conflict["ok"] == false && conflict["code"] == "conflict", "stale task deletion accepted")
	deadline := time.Now().Add(60 * time.Second)
	var history []any
	for time.Now().Before(deadline) {
		data = object(call(obj{"operation": "schedule.snapshot", "id": id})["data"])
		history = array(data["history"])
		if len(history) == 1 && object(object(history[0])["outcome"])["status"] == "completed" {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	need(len(history) == 1 && object(object(history[0])["outcome"])["status"] == "completed", "rescheduled task failed to execute: %v", data)
	need(exists(path) && read(path) == content && toolResult(s.fake.since(modelStart), callID) != nil, "Full Access schedule did not complete the authorized write without prompting")
	item = object(array(data["items"])[0])
	need(item["pause_reason"] == "exhausted" && item["paused"] == true && str(object(object(history[0])["outcome"])["session_id"]) != "", "ended schedule lost state or session")
	rearmed := call(obj{"operation": "schedule.reschedule", "id": id, "expected_revision": item["revision"], "rearm": true,
		"timing": obj{"kind": "once", "at": time.Now().Add(time.Hour).Format(time.RFC3339), "timezone": "Asia/Shanghai"}})
	need(rearmed["ok"] == true, "single-run rearm failed")
	item = object(rearmed["data"])
	need(item["paused"] == false && object(item["spec"])["id"] == id, "rearm changed identity")
	paused := call(obj{"operation": "schedule.pause", "id": id, "expected_revision": item["revision"], "paused": true})
	need(paused["ok"] == true, "pause schedule failed")
	data = object(call(obj{"operation": "schedule.snapshot", "id": id})["data"])
	item = object(array(data["items"])[0])
	deleted := call(obj{"operation": "schedule.delete", "id": id, "expected_revision": item["revision"]})
	need(deleted["ok"] == true, "delete schedule failed")
	data = object(call(obj{"operation": "schedule.snapshot", "id": id})["data"])
	need(len(array(data["items"])) == 0 && len(array(data["history"])) == 1, "delete lost history or retained plan")
	s.log("Native conversation schedule creation and settings preview/reschedule/conflict/execute/rearm/pause/delete through Waku and Go", obj{"default_approval": "allow_always", "history_preserved": true, "authorized_write_without_prompt": true, "model_tool_receipt": true})
}

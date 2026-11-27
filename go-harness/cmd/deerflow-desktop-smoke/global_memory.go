package main

// Use the same Waku management commands as Settings → Memory. The smoke
// package has its own disposable profile and never accesses installed memory.
func (s *smoke) globalMemory() func() {
	call := func(request obj) obj { return s.waku.deerflow("manage-envelope", request) }
	success := func(request obj) obj {
		reply := call(request)
		need(reply["ok"] == true, "global memory %v failed: %v", request["operation"], reply["code"])
		return object(reply["data"])
	}
	list := obj{"operation": "memory.personal.get", "limit": 12, "facts_only": true}
	profile := str(success(list)["profile_id"])
	need(profile != "", "missing global memory profile")
	create := obj{"operation": "memory.personal.mutate", "operation_id": uuid(), "profile_id": profile,
		"action": "remember", "key": "manual." + uuid(), "category": "preference",
		"content": "I prefer short summaries.", "expected_revision": 0}
	created := success(create)
	id := str(created["fact_id"])
	need(id != "" && created["status"] == "saved", "global memory was not created")
	need(success(create)["fact_id"] == id, "retry created a different global memory")
	edit := obj{"operation": "memory.personal.mutate", "operation_id": uuid(), "profile_id": profile,
		"action": "edit", "fact_id": id, "content": "I prefer detailed summaries.", "expected_revision": created["revision"]}
	edited := success(edit)
	need(edited["status"] == "updated" && edited["fact_id"] == id && edited["revision"] == float64(2), "edit did not advance the original memory")
	recovered := success(obj{"operation": "memory.personal.receipt", "operation_id": edit["operation_id"]})
	need(recovered["found"] == true && object(recovered["receipt"])["revision"] == edited["revision"], "edit receipt missing")
	stale := clone(edit)
	stale["operation_id"] = uuid()
	need(call(stale)["code"] == "memory_conflict", "stale edit was accepted")
	review := clone(list)
	review["review_fact_id"] = id
	review["cursor"] = id // The reviewed row is outside this cursor's page.
	current := object(success(review)["reviewed_fact"])
	need(current["id"] == id && current["content"] == edit["content"], "off-page edit conflict could not be reviewed")
	s.log("Global memory add, edit, conflict and receipt through desktop management", obj{})

	return func() {
		page := success(list)
		found := false
		for _, row := range array(page["facts"]) {
			fact := object(row)
			if fact["id"] == id {
				found = fact["content"] == edit["content"] && fact["revision"] == edited["revision"]
			}
		}
		need(page["profile_id"] == profile && found, "edited global memory did not survive restart")
		deleted := success(obj{"operation": "memory.personal.mutate", "operation_id": uuid(), "profile_id": profile,
			"action": "forget", "fact_id": id, "expected_revision": edited["revision"]})
		need(deleted["status"] == "forgotten" && !containsID(success(list)["facts"], "id", id), "deleted global memory still listed")
		reviewed, present := success(review)["reviewed_fact"]
		need(present && reviewed == nil, "review still returned a forgotten memory")
		s.log("Edited global memory survives restart and deletion removes it from Settings", obj{})
	}
}

package main

import "path/filepath"

// Exercise the delivered desktop management transport only in the disposable
// package. The fixture keeps evolution disabled, so deletion cannot need review.
func (s *smoke) directSkillDelete() func() {
	need(object(s.waku.deerflow("snapshot")["skill_evolution"])["enabled"] == false, "delete fixture unexpectedly enabled review")
	page := s.waku.deerflow("manage", obj{"operation": "skill.list", "status": "current", "name": "bootstrap", "limit": 100})
	need(len(array(page["skills"])) == 1, "missing isolated deletion fixture")
	entry := object(array(page["skills"])[0])
	source := filepath.Join(str(entry["source_directory"]), "SKILL.md")
	need(below(source, s.root) && exists(source), "delete fixture source is not isolated")
	request := obj{"operation": "skill.delete", "target": entry["target"], "expected_head": entry["head"], "request_key": uuid()}
	result := s.waku.deerflow("manage", request)
	applied := object(result["applied_head"])
	need(result["replayed"] == false && applied["exists"] == false && applied["enabled"] == false && applied["managed"] == true && applied["version"] == "", "direct deletion did not return a tombstone: %v", result)
	need(applied["state_version"] == object(entry["head"])["state_version"].(float64)+1, "deletion did not advance the observed head")
	check := func() {
		page := s.waku.deerflow("manage", obj{"operation": "skill.list", "status": "current", "name": "bootstrap", "limit": 100})
		need(len(array(page["skills"])) == 0, "deleted skill reappeared in the current catalog")
		replayed := s.waku.deerflow("manage", request)
		need(replayed["replayed"] == true && string(jsonBytes(replayed["applied_head"])) == string(jsonBytes(applied)) && object(object(replayed["skill"])["head"])["exists"] == false, "direct deletion lost its idempotent receipt")
		need(exists(source), "catalog deletion removed source files")
	}
	check()
	s.log("Direct desktop skill deletion works without evolution review and is retryable", obj{})
	return func() {
		check()
		s.log("Direct skill deletion survives service restart and startup import", obj{})
	}
}

package main

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type rateLimitPlan struct {
	afterCall  string
	rejections int
	requests   []time.Time
}

func (f *fakeModel) planRateLimit(marker, afterCall string, rejections int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rateLimits == nil {
		f.rateLimits = make(map[string]*rateLimitPlan)
	}
	f.rateLimits[marker] = &rateLimitPlan{afterCall: afterCall, rejections: rejections}
}

// The caller holds f.mu. A tool-dependent rate limit begins only after the
// actual result is present, so a replayed tool request cannot pass the fixture.
func (f *fakeModel) rateLimited(body obj) bool {
	for _, value := range array(body["messages"]) {
		message := object(value)
		if message["role"] == "system" && strings.Contains(messageText(message), "Extract only durable facts") {
			return false
		}
	}
	latest := ""
	messages := array(body["messages"])
	for i := len(messages) - 1; i >= 0; i-- {
		message := object(messages[i])
		if message["role"] == "user" {
			latest = messageText(message)
			break
		}
	}
	for marker, plan := range f.rateLimits {
		if !strings.Contains(latest, marker) || plan.afterCall != "" && toolResult([]obj{body}, plan.afterCall) == nil {
			continue
		}
		plan.requests = append(plan.requests, time.Now())
		return len(plan.requests) <= plan.rejections
	}
	return false
}

func (f *fakeModel) rateLimitTimes(marker string) []time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]time.Time(nil), f.rateLimits[marker].requests...)
}

func rateLimitBackoff(times []time.Time) []int64 {
	need(len(times) == 4, "rate-limited model boundary made %d requests, expected 4", len(times))
	gaps := make([]int64, 0, 3)
	for i := 1; i < len(times); i++ {
		delay := times[i].Sub(times[i-1])
		minimum := time.Second << (i - 1)
		need(delay >= minimum-100*time.Millisecond && delay < minimum+15*time.Second, "retry %d did not respect bounded backoff: %s", i, delay)
		gaps = append(gaps, delay.Milliseconds())
	}
	return gaps
}

// Failure is expected in one scenario. Keep its complete event sequence rather
// than using the ordinary helper, which correctly rejects every driver error.
func (w *wakuClient) rateLimitTurn(runtime string) ([]obj, obj) {
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
		need(event["kind"] != "processExited" && event["kind"] != "permission", "rate-limit fixture unexpectedly exited or requested permission: %v", event)
		if event["kind"] == "turnFinished" {
			return events, object(event["payload"])
		}
	}
	panic(fmt.Errorf("rate-limit turn did not finish"))
}

func (s *smoke) rateLimitToolEvidence(nativeSession, callID string) obj {
	path := filepath.Join(s.root, "user-data", "config", "go-harness-state", "harness.db")
	need(below(path, s.root), "rate-limit evidence escaped disposable package")
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?mode=ro")
	must(err)
	defer db.Close()
	var run, state string
	must(db.QueryRow(`SELECT run_id,state FROM harness_tool_receipts WHERE session_id=? AND tool_call_id=?`, nativeSession, callID).Scan(&run, &state))
	var receipts, executes, models, tools int
	must(db.QueryRow(`SELECT count(*) FROM harness_tool_receipts WHERE session_id=? AND tool_call_id=?`, nativeSession, callID).Scan(&receipts))
	must(db.QueryRow(`SELECT count(*) FROM harness_events WHERE run_id=? AND json_extract(event,'$.kind')='tool_execute' AND json_extract(event,'$.toolCallId')=?`, run, callID).Scan(&executes))
	must(db.QueryRow(`SELECT model_calls,tool_calls FROM budget_roots WHERE root_run_id=?`, run).Scan(&models, &tools))
	need(receipts == 1 && executes == 1 && state == "completed" && models == 5 && tools == 1, "retry replayed tool or bypassed accounting: receipts=%d executes=%d state=%s models=%d tools=%d", receipts, executes, state, models, tools)
	return obj{"run_id": run, "receipt_state": state, "tool_execution_count": executes, "model_calls": models, "tool_calls": tools}
}

func (s *smoke) runRateLimits() {
	s.startWaku(uuid(), "waku-rate-limit")
	saved := s.configure(false)
	object(saved["memory"])["enabled"] = false
	s.waku.deerflow("save", saved)
	s.waku.deerflow("start")
	s.settled()

	session := uuid()
	runtime, cursor := s.startApproval(session, nil)
	name, content := "rate-limit-single-write.txt", "completed before rate limiting\n"
	marker, callID := s.fake.plan("write_file", obj{"path": name, "content": content})
	s.fake.planRateLimit(marker, callID, 3)
	start := s.fake.count()
	s.prompt(session, runtime, marker+": write the fixture once and report completion.")
	events, finished := s.waku.rateLimitTurn(runtime)
	need(finished["success"] == true, "429 recovery turn failed: %v", finished)
	for _, event := range events {
		need(event["kind"] != "error", "recoverable 429 leaked an error into the desktop: %v", event)
	}
	requests := s.fake.since(start)
	need(len(requests) == 5, "tool plus retried model boundary made %d total requests, expected 5", len(requests))
	for _, request := range requests[1:] {
		result := toolResult([]obj{request}, callID)
		need(result != nil && !strings.Contains(messageText(result), "Tool failed:"), "retry lost completed tool result")
	}
	need(read(filepath.Join(s.workspace, name)) == content, "completed write was lost")
	evidence := s.rateLimitToolEvidence(str(object(cursor)["sessionId"]), callID)
	evidence["retry_delays_ms"] = rateLimitBackoff(s.fake.rateLimitTimes(marker))
	evidence["http_requests"] = len(requests)
	s.log("Packaged 429 retries retain completed tool and meter every attempt", evidence)
	s.waku.rpc(obj{"type": "closeSession"}, session, runtime)

	session = uuid()
	runtime, _ = s.startApproval(session, nil)
	marker = "smoke-rate-limit-" + uuid()
	s.fake.planRateLimit(marker, "", 100)
	start = s.fake.count()
	s.prompt(session, runtime, marker+": respond after rate limiting clears.")
	events, finished = s.waku.rateLimitTurn(runtime)
	need(finished["success"] == false, "exhausted 429 incorrectly completed successfully")
	var notices []string
	for _, event := range events {
		if event["kind"] == "error" {
			notices = append(notices, str(event["payload"]))
		}
	}
	need(len(notices) == 1, "expected one desktop limit notice, got %v", notices)
	notice := notices[0]
	need(strings.Contains(notice, "限流") && strings.Contains(notice, "稍后") && strings.Contains(notice, "切换"), "desktop did not explain model rate limiting in Chinese: %q", notice)
	for _, forbidden := range []string{"Internal error", "automaticReplay", "execution", "fixture-private", "{", "}"} {
		need(!strings.Contains(notice, forbidden), "desktop exposed internal rate-limit metadata: %q", notice)
	}
	need(len(s.fake.since(start)) == 4, "exhausted rate limit did not stop at four HTTP requests")
	s.log("Packaged exhausted 429 shows a concise Chinese desktop error", obj{"http_requests": 4, "retry_delays_ms": rateLimitBackoff(s.fake.rateLimitTimes(marker)), "desktop_error": notice})
	start = s.fake.count()
	s.prompt(session, runtime, "Reply on the same session after the previous limit ended.")
	events, finished = s.waku.rateLimitTurn(runtime)
	need(finished["success"] == true && len(s.fake.since(start)) == 1, "a fresh turn could not continue after exhausted 429")
	for _, event := range events {
		need(event["kind"] != "error", "fresh turn after 429 still failed: %v", event)
	}
	s.log("Same packaged session accepts a new turn after exhausted 429", obj{})
	s.waku.rpc(obj{"type": "closeSession"}, session, runtime)
}

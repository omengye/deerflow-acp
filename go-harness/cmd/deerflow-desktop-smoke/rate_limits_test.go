package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestRateLimitFixtureStartsAfterActualToolResultAndStopsAfterPlan(t *testing.T) {
	f := newFakeModel("", "")
	defer f.server.Close()
	marker, id := f.plan("write_file", obj{"path": "isolated.txt", "content": "fixture"})
	f.planRateLimit(marker, id, 3)
	body := obj{"model": "fixture", "messages": []any{obj{"role": "user", "content": marker}}, "tools": []any{obj{"type": "function", "function": obj{"name": "write_file"}}}}
	request := func() (int, obj) {
		t.Helper()
		response, err := http.Post(f.server.URL+"/v1/chat/completions", "application/json", bytes.NewReader(jsonBytes(body)))
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		var result obj
		if err = json.NewDecoder(response.Body).Decode(&result); err != nil {
			t.Fatal(err)
		}
		return response.StatusCode, result
	}
	status, result := request()
	if status != http.StatusOK || len(f.rateLimitTimes(marker)) != 0 {
		t.Fatalf("provider was limited before the tool executed: %d %+v", status, result)
	}
	message := object(object(array(result["choices"])[0])["message"])
	body["messages"] = append(array(body["messages"]), message, obj{"role": "tool", "tool_call_id": id, "content": "write complete"})
	for attempt := 0; attempt < 4; attempt++ {
		status, result = request()
		if attempt < 3 {
			if status != http.StatusTooManyRequests || !strings.Contains(str(object(result["error"])["message"]), "fixture-private-rate-limit") {
				t.Fatalf("missing rejected attempt %d: %d %+v", attempt, status, result)
			}
		} else if status != http.StatusOK || object(array(result["choices"])[0])["finish_reason"] != "stop" {
			t.Fatalf("last attempt did not finish without replaying the tool: %d %+v", status, result)
		}
	}
	if f.count() != 5 || len(f.rateLimitTimes(marker)) != 4 {
		t.Fatalf("wrong request counts: total=%d model-boundary=%d", f.count(), len(f.rateLimitTimes(marker)))
	}
}

func TestRateLimitFixtureDoesNotLeakIntoFollowingTurnOrAuxiliaryModel(t *testing.T) {
	f := newFakeModel("", "")
	defer f.server.Close()
	marker := "smoke-rate-limit-fixture"
	f.planRateLimit(marker, "", 100)
	for _, messages := range [][]any{
		{obj{"role": "user", "content": marker}, obj{"role": "user", "content": "new turn"}},
		{obj{"role": "system", "content": "Extract only durable facts"}, obj{"role": "user", "content": marker}},
	} {
		response, err := http.Post(f.server.URL+"/v1/chat/completions", "application/json", bytes.NewReader(jsonBytes(obj{"stream": true, "messages": messages})))
		if err != nil {
			t.Fatal(err)
		}
		data, readErr := io.ReadAll(response.Body)
		response.Body.Close()
		if readErr != nil || response.StatusCode != http.StatusOK || !bytes.Contains(data, []byte("data: [DONE]")) {
			t.Fatalf("unrelated request received a rate limit: status=%d data=%s err=%v", response.StatusCode, data, readErr)
		}
	}
	if len(f.rateLimitTimes(marker)) != 0 {
		t.Fatal("unrelated calls consumed rate-limit attempts")
	}
}

func TestRateLimitBackoffRejectsMissingUnboundedAndImmediateRetries(t *testing.T) {
	start := time.Now()
	for _, test := range []struct {
		name   string
		times  []time.Duration
		passes bool
	}{
		{"expected", []time.Duration{0, time.Second, 3 * time.Second, 7 * time.Second}, true},
		{"normal-overhead", []time.Duration{0, 1100 * time.Millisecond, 3200 * time.Millisecond, 7300 * time.Millisecond}, true},
		{"missing-attempt", []time.Duration{0, time.Second, 3 * time.Second}, false},
		{"extra-attempt", []time.Duration{0, time.Second, 3 * time.Second, 7 * time.Second, 15 * time.Second}, false},
		{"no-delay", []time.Duration{0, 0, 0, 0}, false},
		{"unbounded-delay", []time.Duration{0, time.Second, 3 * time.Second, time.Minute}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			var times []time.Time
			for _, elapsed := range test.times {
				times = append(times, start.Add(elapsed))
			}
			err := safe(func() { rateLimitBackoff(times) })
			if (err == nil) != test.passes {
				t.Fatalf("passes=%t err=%v", test.passes, err)
			}
		})
	}
}

func TestRateLimitTurnRetainsExpectedDesktopError(t *testing.T) {
	w := &wakuClient{events: []obj{
		{"type": "event", "runtimeId": "fixture", "event": obj{"kind": "error", "payload": "模型服务请求受到限流"}},
		{"type": "event", "runtimeId": "fixture", "event": obj{"kind": "turnFinished", "payload": obj{"success": false}}},
	}}
	events, finished := w.rateLimitTurn("fixture")
	if len(events) != 2 || finished["success"] != false || events[0]["payload"] != "模型服务请求受到限流" {
		t.Fatalf("error was lost or treated as success: %+v %+v", events, finished)
	}
}

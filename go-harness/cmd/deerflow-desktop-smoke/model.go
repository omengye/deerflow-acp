package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"time"
)

type fakeModel struct {
	mu           sync.Mutex
	requests     []obj
	plans        map[string]obj
	server       *httptest.Server
	logPath, key string
}

func newFakeModel(path, key string) *fakeModel {
	f := &fakeModel{plans: map[string]obj{}, logPath: path, key: key}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	return f
}
func (f *fakeModel) since(index int) []obj {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]obj(nil), f.requests[index:]...)
}
func (f *fakeModel) count() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.requests) }
func (f *fakeModel) plan(name string, args obj) (string, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	marker, id := "smoke-tool-"+uuid(), "call_"+uuid()
	f.plans[marker] = obj{"id": id, "type": "function", "function": obj{"name": name, "arguments": string(jsonBytes(args))}}
	return marker, id
}
func (f *fakeModel) planned(body obj) obj {
	messages := array(body["messages"])
	latest := -1
	for i := len(messages) - 1; i >= 0; i-- {
		if object(messages[i])["role"] == "user" {
			latest = i
			break
		}
	}
	if latest < 0 {
		return nil
	}
	var call obj
	text := messageText(object(messages[latest]))
	for marker, plan := range f.plans {
		if strings.Contains(text, marker) {
			call = clone(plan)
			break
		}
	}
	if call == nil {
		return nil
	}
	fn := object(call["function"])
	if fn["name"] == "__desktop_mcp_record__" {
		for _, v := range array(body["tools"]) {
			name := str(object(object(v)["function"])["name"])
			if strings.HasPrefix(name, "mcp_desktop_fixture_record") {
				fn["name"] = name
				break
			}
		}
	}
	for _, v := range messages[latest+1:] {
		m := object(v)
		if m["role"] == "tool" && m["tool_call_id"] == call["id"] {
			return nil
		}
	}
	for _, v := range array(body["tools"]) {
		if object(object(v)["function"])["name"] == fn["name"] {
			return call
		}
	}
	return nil
}
func (f *fakeModel) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" || (r.URL.Path != "/v1/chat/completions" && r.URL.Path != "/chat/completions") {
		http.Error(w, "unexpected endpoint", 400)
		return
	}
	if f.key != "" && r.Header.Get("Authorization") != "Bearer "+f.key {
		http.Error(w, "unauthorized", 401)
		return
	}
	var body obj
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20))
	if d.Decode(&body) != nil {
		http.Error(w, "invalid body", 400)
		return
	}
	f.mu.Lock()
	f.requests = append(f.requests, clone(body))
	if f.logPath != "" {
		file, e := os.OpenFile(f.logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if e == nil {
			file.Write(append(jsonBytes(body), '\n'))
			file.Close()
		}
	}
	call := f.planned(body)
	f.mu.Unlock()
	content := reply
	for _, v := range array(body["messages"]) {
		m := object(v)
		if m["role"] == "system" && strings.Contains(messageText(m), "Extract only durable facts") {
			content = `{"facts":[]}`
		}
	}
	msg := obj{"role": "assistant", "content": content}
	finish := "stop"
	if call != nil {
		msg["content"] = nil
		msg["tool_calls"] = []any{call}
		finish = "tool_calls"
	}
	common := obj{"id": "chatcmpl-smoke", "created": time.Now().Unix(), "model": body["model"]}
	usage := obj{"prompt_tokens": 8, "completion_tokens": 8, "total_tokens": 16}
	if body["stream"] == true {
		w.Header().Set("Content-Type", "text/event-stream")
		if call != nil {
			call["index"] = 0
		}
		for _, choice := range []obj{{"index": 0, "delta": msg, "finish_reason": nil}, {"index": 0, "delta": obj{}, "finish_reason": finish}} {
			piece := clone(common)
			piece["object"] = "chat.completion.chunk"
			piece["choices"] = []any{choice}
			if choice["finish_reason"] != nil {
				piece["usage"] = usage
			}
			fmt.Fprintf(w, "data: %s\n\n", jsonBytes(piece))
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	} else {
		w.Header().Set("Content-Type", "application/json")
		common["object"] = "chat.completion"
		common["choices"] = []any{obj{"index": 0, "message": msg, "finish_reason": finish}}
		common["usage"] = usage
		json.NewEncoder(w).Encode(common)
	}
}

// The same native executable supplies the isolated stdio MCP fixture. Its
// environment comes only from the smoke harness; credentials never enter logs.
func serveMCP(input io.Reader, output io.Writer) error {
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	writer := json.NewEncoder(output)
	for scanner.Scan() {
		var req obj
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			return err
		}
		if req["id"] == nil {
			continue
		}
		params, _ := req["params"].(map[string]any)
		result := obj{}
		switch req["method"] {
		case "initialize":
			result = obj{"protocolVersion": params["protocolVersion"], "capabilities": obj{"tools": obj{}}, "serverInfo": obj{"name": "desktop-mcp-smoke", "version": "2"}}
		case "tools/list":
			result = obj{"tools": []any{obj{"name": "record", "description": "Record the isolated desktop smoke value", "inputSchema": obj{"type": "object", "properties": obj{"value": obj{"type": "string"}}, "required": []any{"value"}}}}}
		case "tools/call":
			args, _ := params["arguments"].(map[string]any)
			value, valid := args["value"].(string)
			if params["name"] != "record" || os.Getenv("ACP_MCP_SECRET") == "" || os.Getenv("ACP_MCP_EFFECT") == "" || !valid {
				result = obj{"isError": true, "content": []any{obj{"type": "text", "text": "fixture rejected call"}}}
			} else {
				if err := os.WriteFile(os.Getenv("ACP_MCP_EFFECT"), []byte(value), 0600); err != nil {
					return err
				}
				result = obj{"content": []any{obj{"type": "text", "text": "recorded successfully"}}}
			}
		}
		if err := writer.Encode(obj{"jsonrpc": "2.0", "id": req["id"], "result": result}); err != nil {
			return err
		}
	}
	return scanner.Err()
}

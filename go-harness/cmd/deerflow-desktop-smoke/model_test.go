package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFakeModelUsesActualToolResultAndCredentialRoute(t *testing.T) {
	f := newFakeModel("", "fixture-only")
	defer f.server.Close()
	marker, id := f.plan("write_file", obj{"path": "test.txt", "content": "synthetic"})
	body := obj{"model": "fixture", "messages": []any{obj{"role": "user", "content": marker}}, "tools": []any{obj{"type": "function", "function": obj{"name": "write_file"}}}}
	request := func(key string) (int, obj) {
		req, e := http.NewRequest("POST", f.server.URL+"/v1/chat/completions", bytes.NewReader(jsonBytes(body)))
		if e != nil {
			t.Fatal(e)
		}
		req.Header.Set("Authorization", "Bearer "+key)
		res, e := http.DefaultClient.Do(req)
		if e != nil {
			t.Fatal(e)
		}
		defer res.Body.Close()
		var result obj
		if res.StatusCode == 200 {
			if e = json.NewDecoder(res.Body).Decode(&result); e != nil {
				t.Fatal(e)
			}
		}
		return res.StatusCode, result
	}
	if status, _ := request("wrong"); status != 401 {
		t.Fatalf("wrong credential: %d", status)
	}
	_, result := request("fixture-only")
	msg := object(object(array(result["choices"])[0])["message"])
	if object(array(msg["tool_calls"])[0])["id"] != id {
		t.Fatal("planned call missing")
	}
	body["messages"] = append(array(body["messages"]), msg, obj{"role": "tool", "tool_call_id": id, "content": "Denied by user"})
	_, result = request("fixture-only")
	choice := object(array(result["choices"])[0])
	if choice["finish_reason"] != "stop" || object(choice["message"])["content"] != reply {
		t.Fatal("denied tool result reissued")
	}
	if f.count() != 2 {
		t.Fatal("unauthorized request should not be recorded")
	}
}

func TestMCPFixtureRejectsMissingCredentialAndWritesAfterAuthorization(t *testing.T) {
	effect := filepath.Join(t.TempDir(), "effect.txt")
	t.Setenv("ACP_MCP_EFFECT", effect)
	t.Setenv("ACP_MCP_SECRET", "")
	input := obj{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": obj{"name": "record", "arguments": obj{"value": "isolated fixture"}}}
	var out bytes.Buffer
	if err := serveMCP(bytes.NewReader(append(jsonBytes(input), '\n')), &out); err != nil {
		t.Fatal(err)
	}
	if exists(effect) || !strings.Contains(out.String(), `"isError":true`) {
		t.Fatal("unauthorized fixture produced an effect")
	}
	t.Setenv("ACP_MCP_SECRET", "fixture-only-secret")
	out.Reset()
	if err := serveMCP(bytes.NewReader(append(jsonBytes(input), '\n')), &out); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(effect)
	if err != nil || string(data) != "isolated fixture" || strings.Contains(out.String(), "fixture-only-secret") {
		t.Fatalf("authorized fixture: %q %v", data, err)
	}
}

func TestFakeModelStreamAndMemoryExtraction(t *testing.T) {
	f := newFakeModel("", "")
	defer f.server.Close()
	body := obj{"stream": true, "messages": []any{obj{"role": "system", "content": "Extract only durable facts"}}}
	r, e := http.Post(f.server.URL+"/v1/chat/completions", "application/json", bytes.NewReader(jsonBytes(body)))
	if e != nil {
		t.Fatal(e)
	}
	defer r.Body.Close()
	b, e := io.ReadAll(r.Body)
	if e != nil {
		t.Fatal(e)
	}
	if !bytes.Contains(b, []byte(`\"facts\":[]`)) || !bytes.Contains(b, []byte("data: [DONE]")) {
		t.Fatalf("invalid extraction stream %s", b)
	}
}

func TestImmutableCopyRejectsLinkedInputs(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source")
	if err := os.WriteFile(source, []byte("input"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(source, link); err != nil {
		t.Skipf("symlink privilege unavailable: %v", err)
	}
	if err := safe(func() { copyFile(link, filepath.Join(dir, "copied")) }); err == nil {
		t.Fatal("linked input accepted")
	}
}

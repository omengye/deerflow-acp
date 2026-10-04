package harness

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestWSLHelperOptionsPreserveUnchangedCheckpointPolicyEncoding(t *testing.T) {
	// Sandbox policy hashes are stored in extension checkpoints. Adding an
	// unset helper option must not invalidate every existing non-WSL checkpoint.
	config := WSLSandboxConfig{Executable: "wsl.exe", Distribution: "fixture", User: "worker"}
	data, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"Executable":"wsl.exe","Distribution":"fixture","User":"worker"}` {
		t.Fatalf("legacy policy representation changed: %s", data)
	}
	config.HelperPath, config.HelperSHA256 = "/fixture/helper", strings.Repeat("a", 64)
	data, err = json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"HelperPath":"/fixture/helper"`) || !strings.Contains(string(data), `"HelperSHA256":"`+config.HelperSHA256+`"`) {
		t.Fatal("explicit helper identity omitted from policy")
	}
}

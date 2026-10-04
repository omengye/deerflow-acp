package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestDaemonStartupFailurePersistsLifecycleLog(t *testing.T) {
	dir := t.TempDir()
	logs := filepath.Join(dir, "logs")
	t.Setenv("DEERFLOW_GO_LOG_DIR", logs)
	config := filepath.Join(dir, "invalid.yaml")
	if err := os.WriteFile(config, []byte("models: [never-record-raw-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	err := run([]string{"--config", config, "--data-dir", filepath.Join(dir, "data"), "--runtime-dir", filepath.Join(dir, "runtime"), "--model", "fixture"})
	if err == nil {
		t.Fatal("invalid config started")
	}
	files, err := filepath.Glob(filepath.Join(logs, "*.jsonl"))
	if err != nil || len(files) != 1 {
		t.Fatalf("missing startup log: %v %v", files, err)
	}
	data, err := os.ReadFile(files[0])
	if err != nil || !bytes.Contains(data, []byte("startup.configuration")) || !bytes.Contains(data, []byte(`"code":"failed"`)) || bytes.Contains(data, []byte("never-record-raw-secret")) {
		t.Fatalf("invalid startup diagnostic: %s %v", data, err)
	}
}

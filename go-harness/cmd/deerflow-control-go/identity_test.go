package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestControlRequiresExplicitRuntimeInsteadOfGuessingProfile(t *testing.T) {
	t.Setenv("DEERFLOW_GO_RUNTIME_DIR", "")
	var out bytes.Buffer
	err := run(nil, strings.NewReader(`{"operation":"daemon.status"}`), &out)
	if err == nil || !strings.Contains(err.Error(), "--runtime-dir") || out.Len() != 0 {
		t.Fatal(err, out.String())
	}
}

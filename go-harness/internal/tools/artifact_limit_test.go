package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestPresentFilesDescriptionUsesConfiguredLimit(t *testing.T) {
	tool, err := PresentFilesTool(func(context.Context, string, []string) ([]harness.Content, error) { return nil, nil }, 750*1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	info, err := tool.Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(info.Desc, "750.00 MiB") || !strings.Contains(info.Desc, "786432000 bytes") {
		t.Fatal(info.Desc)
	}
}

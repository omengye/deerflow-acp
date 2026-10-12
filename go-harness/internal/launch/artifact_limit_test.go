package launch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	deerflow "github.com/omengye/deerflow-acp/go-harness"
)

func TestApplyConfigArtifactLimit(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  int64
	}{
		{"", 500}, {"null", 500}, {"1", 1}, {"750", 750},
		{"0", 0}, {"-1", 0}, {"1.5", 0}, {"\"500\"", 0}, {"true", 0},
		{"8796093022208", 0},
	} {
		t.Run("value="+tc.value, func(t *testing.T) {
			raw := "models:\n  - name: test\n    provider: openai\n    model: test\n"
			if tc.value != "" {
				raw += "local_acp:\n  max_artifact_size_mib: " + tc.value + "\n"
			}
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
				t.Fatal(err)
			}
			var cfg deerflow.Config
			connections := 16
			source, err := ApplyConfig(path, &cfg, &connections, nil)
			if tc.want == 0 {
				if err == nil || !strings.Contains(err.Error(), "max_artifact_size_mib") {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err != nil || cfg.MaxArtifactBytes != tc.want*1024*1024 {
				t.Fatalf("bytes=%d err=%v", cfg.MaxArtifactBytes, err)
			}
			if got := source.Settings.Fields["/runtime/max_artifact_size_mib"].Value; got != tc.want {
				t.Fatalf("applied=%v want=%d", got, tc.want)
			}
		})
	}
}

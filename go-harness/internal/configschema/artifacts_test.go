package configschema

import "testing"

func TestArtifactLimitIsHostOnly(t *testing.T) {
	for _, field := range Describe().Fields {
		if field.ConfigPath == "local_acp.max_artifact_size_mib" {
			t.Fatal("artifact limit must not be a desktop editable field")
		}
	}
	for _, value := range []any{0, -1, 1.5, "500", true, int64(1 << 43)} {
		if err := ValidateNative(map[string]any{"local_acp": map[string]any{"max_artifact_size_mib": value}}); err == nil {
			t.Fatalf("accepted invalid limit %v", value)
		}
	}
}

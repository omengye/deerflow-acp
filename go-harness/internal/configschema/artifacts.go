package configschema

import (
	"fmt"
	"math"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

// ArtifactSizeLimitMiB is YAML-only; it deliberately adds no editable UI field.
func ArtifactSizeLimitMiB(document map[string]any) (int64, error) {
	local, _ := document["local_acp"].(map[string]any)
	value := local["max_artifact_size_mib"]
	if value == nil {
		return harness.DefaultMaxArtifactSizeMiB, nil
	}
	var n int64
	switch v := value.(type) {
	case int:
		n = int64(v)
	case int64:
		n = v
	default:
		return 0, fmt.Errorf("local_acp.max_artifact_size_mib must be a positive integer in MiB")
	}
	if n <= 0 || n > (math.MaxInt64-1)/(1024*1024) {
		return 0, fmt.Errorf("local_acp.max_artifact_size_mib must be between 1 and %d", (math.MaxInt64-1)/(1024*1024))
	}
	return n, nil
}

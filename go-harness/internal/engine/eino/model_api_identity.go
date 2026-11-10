package eino

import (
	"encoding/json"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func contractModelAPI(api harness.ModelAPI) harness.ModelAPI {
	if api == harness.ModelAPIAuto || api == harness.ModelAPIChatCompletions {
		return ""
	}
	return api
}

// Keep existing maintenance identities for the legacy protocol while binding
// Responses jobs to their distinct request and history format.
func appendModelAPIIdentity(identity []byte, apis ...harness.ModelAPI) []byte {
	protocols := make([]string, len(apis))
	changed := false
	for i, api := range apis {
		if api == harness.ModelAPIResponses {
			protocols[i] = string(api)
			changed = true
		}
	}
	if !changed {
		return identity
	}
	encoded, _ := json.Marshal(protocols)
	return append(append(identity, []byte("\x00model-api:")...), encoded...)
}

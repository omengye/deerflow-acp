package eino

import (
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestModelAPIExecutionContractPinsEffectiveRootAndRouteProtocol(t *testing.T) {
	contract := func(root, route harness.ModelAPI) string {
		t.Helper()
		e := newTestEngine(t, Config{
			Provider: "openai", Model: "root", API: root, ChatModel: &scriptedModel{},
			ModelRoutes: map[string]harness.ModelRoute{"alternate": {Provider: "openai", Model: "alternate", API: route}},
		})
		value, err := e.executionContract(nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	legacy := contract("", "")
	for _, root := range []harness.ModelAPI{"", harness.ModelAPIAuto, harness.ModelAPIChatCompletions} {
		for _, route := range []harness.ModelAPI{"", harness.ModelAPIAuto, harness.ModelAPIChatCompletions} {
			if got := contract(root, route); got != legacy {
				t.Fatalf("equivalent legacy protocols changed the execution contract: root=%q route=%q", root, route)
			}
		}
	}
	rootResponses := contract(harness.ModelAPIResponses, "")
	routeResponses := contract("", harness.ModelAPIResponses)
	if rootResponses == legacy || routeResponses == legacy || rootResponses == routeResponses {
		t.Fatal("execution contract did not distinguish root and selected-route protocols")
	}
}

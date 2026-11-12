package desktopconfig

import (
	"bytes"
	"os"
	"testing"

	deerflow "github.com/omengye/deerflow-acp/go-harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/launch"
)

func TestImageGenerationSettingsRoundTripAndValidation(t *testing.T) {
	options := setup(t)
	doc := run(t, options, "snapshot", nil)
	first := modelByName(t, doc, "first")
	if first["supports_image_generation"] != false {
		t.Fatal("legacy model unexpectedly enables image generation")
	}
	first["api"], first["supports_image_generation"] = "responses", true
	doc = run(t, options, "save", doc)
	if modelByName(t, doc, "first")["supports_image_generation"] != true {
		t.Fatal("image generation setting was lost on save")
	}
	var cfg deerflow.Config
	connections := 0
	if _, err := launch.ApplyConfig(options.Config, &cfg, &connections, nil); err != nil {
		t.Fatal(err)
	}
	if !cfg.Media.SupportsImageGeneration(cfg.Model) {
		t.Fatal("image generation setting did not reach the runtime")
	}
	delete(modelByName(t, doc, "first"), "supports_image_generation")
	delete(modelByName(t, doc, "first"), "api")
	doc = run(t, options, "save", doc)
	if modelByName(t, doc, "first")["supports_image_generation"] != true {
		t.Fatal("legacy client erased the image generation setting")
	}
	before, err := os.ReadFile(options.Config)
	if err != nil {
		t.Fatal(err)
	}
	for _, api := range []string{"auto", "chat_completions"} {
		candidate := run(t, options, "snapshot", nil)
		model := modelByName(t, candidate, "first")
		model["api"] = api
		delete(model, "supports_image_generation") // Validate retained settings, too.
		if _, err := Run(options, "save", mustJSON(t, candidate)); err == nil {
			t.Fatalf("accepted image generation with %s", api)
		}
	}
	after, err := os.ReadFile(options.Config)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("rejected update modified the configuration")
	}
	first = modelByName(t, doc, "first")
	first["api"], first["supports_image_generation"] = "auto", false
	run(t, options, "save", doc)
}

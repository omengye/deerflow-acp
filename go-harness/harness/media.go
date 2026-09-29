package harness

import "context"

const (
	MaxInputImageBytes      int64 = 20 * 1024 * 1024
	MaxInputImagesPerTurn         = 8
	MaxInputImageTotalBytes int64 = 40 * 1024 * 1024
	MaxInputFileBytes       int64 = 25 * 1024 * 1024
	AssetImage                    = "image"
	AssetFile                     = "file"
	AssetArtifact                 = "artifact"
)

// AssetRef is durable metadata, never image bytes or a provider download URL.
// Resolving it requires the currently authorized session, not its embedded ID.
type AssetRef struct {
	ID        string `json:"id"`
	SessionID string `json:"sessionId"`
	SHA256    string `json:"sha256"`
	Size      int64  `json:"size"`
	MimeType  string `json:"mimeType"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
}

type AssetResolver interface {
	Resolve(context.Context, string, AssetRef) ([]byte, error)
}

// ToolImageImporter stages inline tool images for the terminal tool receipt.
// The returned references become resolvable only after that receipt commits.
type ToolImageImporter interface {
	StageToolImages(context.Context, Session, string, string, []Content) ([]Content, error)
}

// ModelImageImporter stages one generated image. Its asset is published with
// the corresponding image_delta event, before it can be used in model history.
type ModelImageImporter interface {
	StageModelImage(context.Context, Session, string, Content) (Content, error)
}

// MediaConfig is host policy. Vision support is an explicit per-model allowlist
// and remains disabled when empty; provider names do not imply capabilities.
type MediaConfig struct {
	VisionModels []string `json:"visionModels,omitempty"`
}

func (c MediaConfig) SupportsVision(model string) bool {
	if model == "" {
		return false
	}
	for _, allowed := range c.VisionModels {
		if model == allowed {
			return true
		}
	}
	return false
}

func AssetURI(ref AssetRef) string { return "deerflow-asset://" + ref.SessionID + "/" + ref.ID }

package eino

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
)

const assetRefExtraKey = "deerflow_asset_ref"

func invalidMedia(reason string) error { return fmt.Errorf("%w: %s", harness.ErrInvalidInput, reason) }

func validateAsset(ref harness.AssetRef, sessionID string, image bool) error {
	if ref.ID == "" || ref.SessionID == "" || strings.ContainsAny(ref.ID+ref.SessionID, "/\\?#%") {
		return invalidMedia("invalid asset identity")
	}
	if sessionID != "" && ref.SessionID != sessionID {
		return fmt.Errorf("%w: asset belongs to another session", harness.ErrPermissionDenied)
	}
	hash, err := hex.DecodeString(ref.SHA256)
	if err != nil || len(hash) != sha256.Size || ref.Size < 0 || len(ref.Name) > 1024 {
		return invalidMedia("invalid asset size or digest")
	}
	if image {
		if ref.Kind != harness.AssetImage || ref.Size == 0 || ref.Size > harness.MaxInputImageBytes || !imageMIME(ref.MimeType) {
			return invalidMedia("unsupported image asset")
		}
	}
	return nil
}

func imageMIME(mime string) bool {
	return mime == "image/png" || mime == "image/jpeg" || mime == "image/gif" || mime == "image/webp"
}

func imageReference(ref harness.AssetRef) (schema.MessageInputPart, error) {
	if err := validateAsset(ref, "", true); err != nil {
		return schema.MessageInputPart{}, err
	}
	encoded, err := json.Marshal(ref)
	if err != nil {
		return schema.MessageInputPart{}, err
	}
	uri := harness.AssetURI(ref)
	return schema.MessageInputPart{Type: schema.ChatMessagePartTypeImageURL,
		Image: &schema.MessageInputImage{MessagePartCommon: schema.MessagePartCommon{URL: &uri, MIMEType: ref.MimeType}},
		// A string survives both native gob checkpoints and JSON session stores
		// without registering an interface payload type in Eino's serializer.
		Extra: map[string]any{assetRefExtraKey: string(encoded)}}, nil
}

func assetFromPart(extra map[string]any) (harness.AssetRef, error) {
	encoded, ok := extra[assetRefExtraKey].(string)
	if !ok {
		return harness.AssetRef{}, invalidMedia("image requires a normalized asset reference")
	}
	var ref harness.AssetRef
	if err := json.Unmarshal([]byte(encoded), &ref); err != nil {
		return ref, invalidMedia("invalid asset reference metadata")
	}
	return ref, nil
}

func validateImagePart(part schema.MessageInputPart, sessionID string) (harness.AssetRef, error) {
	if part.Image == nil || part.Image.Base64Data != nil || part.Image.URL == nil {
		return harness.AssetRef{}, invalidMedia("inline or unnormalized images cannot enter durable history")
	}
	ref, err := assetFromPart(part.Extra)
	if err != nil {
		return ref, err
	}
	if err = validateAsset(ref, sessionID, true); err != nil {
		return ref, err
	}
	if *part.Image.URL != harness.AssetURI(ref) || part.Image.MIMEType != ref.MimeType {
		return ref, invalidMedia("asset URI or MIME type does not match its metadata")
	}
	return ref, nil
}

func attachmentText(content harness.Content) (string, error) {
	if content.Data != "" || content.URI == "" || len(content.URI) > 8192 || len(content.Name) > 1024 || len(content.Description) > 2048 {
		return "", invalidMedia("attachment must be a bounded reference without inline data")
	}
	uri, err := url.Parse(content.URI)
	if err != nil || uri.User != nil {
		return "", invalidMedia("invalid attachment URI")
	}
	if content.Asset != nil {
		if err := validateAsset(*content.Asset, "", false); err != nil {
			return "", err
		}
		if content.URI != harness.AssetURI(*content.Asset) {
			return "", invalidMedia("attachment URI does not match its asset")
		}
	} else {
		if uri.Scheme != "http" && uri.Scheme != "https" {
			return "", invalidMedia("local attachment requires a normalized asset reference")
		}
		if uri.Host == "" {
			return "", invalidMedia("invalid remote attachment URI")
		}
		ext := strings.ToLower(path.Ext(uri.Path))
		if strings.HasPrefix(strings.ToLower(content.MimeType), "image/") || ext == ".png" || ext == ".jpg" || ext == ".jpeg" || ext == ".gif" || ext == ".webp" {
			return "", invalidMedia("remote images must be imported as local image assets")
		}
	}
	meta, err := json.Marshal(struct {
		Name, URI, MIMEType, Description string
		Size                             *int64
	}{content.Name, content.URI, content.MimeType, content.Description, content.Size})
	if err != nil {
		return "", err
	}
	return "\n[Attachment reference; contents have not been read: " + string(meta) + "]\n", nil
}

func convertContent(role schema.RoleType, content []harness.Content) (*schema.Message, error) {
	message := &schema.Message{Role: role}
	var text strings.Builder
	var parts []schema.MessageInputPart
	multimodal := false
	for _, part := range content {
		switch part.Type {
		case "", "text":
			text.WriteString(part.Text)
			parts = append(parts, schema.MessageInputPart{Type: schema.ChatMessagePartTypeText, Text: part.Text})
		case "image":
			if role != schema.User || part.Asset == nil || part.Data != "" {
				return nil, invalidMedia("user images must be normalized into session assets before execution")
			}
			if part.URI != harness.AssetURI(*part.Asset) || part.MimeType != part.Asset.MimeType {
				return nil, invalidMedia("image metadata does not match its asset")
			}
			image, err := imageReference(*part.Asset)
			if err != nil {
				return nil, err
			}
			parts = append(parts, image)
			multimodal = true
		case "resource_link", "file":
			attachment, err := attachmentText(part)
			if err != nil {
				return nil, err
			}
			text.WriteString(attachment)
			parts = append(parts, schema.MessageInputPart{Type: schema.ChatMessagePartTypeText, Text: attachment})
		default:
			return nil, invalidMedia("unsupported input content type " + part.Type)
		}
	}
	if multimodal {
		message.UserInputMultiContent = parts
	} else {
		message.Content = text.String()
	}
	return message, nil
}

// ProjectToolContent creates durable Eino tool content from already-normalized
// harness assets. Tool middleware still verifies the current session before
// returning the result to native history. It never imports paths or downloads.
func ProjectToolContent(content []harness.Content) (*schema.ToolResult, error) {
	result := &schema.ToolResult{}
	for _, part := range content {
		switch part.Type {
		case "", "text":
			result.Parts = append(result.Parts, schema.ToolOutputPart{Type: schema.ToolPartTypeText, Text: part.Text})
		case "image":
			if part.Asset == nil || part.Data != "" || part.URI != harness.AssetURI(*part.Asset) || part.MimeType != part.Asset.MimeType {
				return nil, invalidMedia("tool image must be a normalized asset")
			}
			image, err := imageReference(*part.Asset)
			if err != nil {
				return nil, err
			}
			result.Parts = append(result.Parts, schema.ToolOutputPart{Type: schema.ToolPartTypeImage, Image: &schema.ToolOutputImage{MessagePartCommon: image.Image.MessagePartCommon}, Extra: image.Extra})
		case "resource_link", "file":
			if _, err := attachmentText(part); err != nil {
				return nil, err
			}
			uri := part.URI
			var extra map[string]any
			if part.Asset != nil {
				encoded, _ := json.Marshal(part.Asset)
				extra = map[string]any{assetRefExtraKey: string(encoded)}
			}
			result.Parts = append(result.Parts, schema.ToolOutputPart{Type: schema.ToolPartTypeFile, File: &schema.ToolOutputFile{MessagePartCommon: schema.MessagePartCommon{URL: &uri, MIMEType: part.MimeType}}, Extra: extra})
		default:
			return nil, invalidMedia("unsupported tool media type")
		}
	}
	return result, nil
}

type mediaProjection struct {
	resolver         harness.AssetResolver
	policy           harness.MediaConfig
	sessionID, model string
}

func (p *mediaProjection) hydrate(ctx context.Context, input []*schema.Message, opts []model.Option) ([]*schema.Message, error) {
	// Serialize a private copy using the same value boundary as durable native
	// sessions; provider adapters cannot mutate the stored message or its parts.
	encoded, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("clone model messages: %w", err)
	}
	var output []*schema.Message
	if err := json.Unmarshal(encoded, &output); err != nil {
		return nil, err
	}
	selectedModel := p.model
	if override := model.GetCommonOptions(&model.Options{}, opts...).Model; override != nil {
		selectedModel = *override
	}
	type cachedImage struct {
		ref  harness.AssetRef
		data string
	}
	cache := make(map[string]cachedImage)
	var resolvedBytes int64
	for _, message := range output {
		if message == nil {
			continue
		}
		for _, part := range message.MultiContent {
			if part.Type != schema.ChatMessagePartTypeText {
				return nil, invalidMedia("legacy multimedia history must be reimported as asset references")
			}
		}
		if err := validateModelOutput(message); err != nil {
			return nil, err
		}
		for i, part := range message.UserInputMultiContent {
			switch part.Type {
			case schema.ChatMessagePartTypeText, schema.ChatMessagePartTypeToolSearchResult:
			case schema.ChatMessagePartTypeImageURL:
				ref, err := validateImagePart(part, p.sessionID)
				if err != nil {
					return nil, err
				}
				if !p.policy.SupportsVision(selectedModel) {
					return nil, invalidMedia("selected model does not have configured vision capability")
				}
				if p.resolver == nil {
					return nil, invalidMedia("image asset resolver is unavailable")
				}
				key := *part.Image.URL
				cached, exists := cache[key]
				if exists && cached.ref != ref {
					return nil, invalidMedia("conflicting metadata for repeated image asset")
				}
				data := cached.data
				if !exists {
					// Bound hydration of reconstructed history as well as current
					// input. Repeated references share one immutable encoded value.
					if len(cache) >= 32 || resolvedBytes+ref.Size > harness.MaxInputImageTotalBytes {
						return nil, invalidMedia("model request exceeds image hydration limits")
					}
					bytes, err := p.resolver.Resolve(ctx, p.sessionID, ref)
					if err != nil {
						return nil, fmt.Errorf("resolve image asset: %w", err)
					}
					if int64(len(bytes)) != ref.Size || fmt.Sprintf("%x", sha256.Sum256(bytes)) != strings.ToLower(ref.SHA256) || http.DetectContentType(bytes) != ref.MimeType {
						return nil, invalidMedia("resolved image does not match asset metadata")
					}
					data = base64.StdEncoding.EncodeToString(bytes)
					cache[key] = cachedImage{ref: ref, data: data}
					resolvedBytes += ref.Size
				}
				part.Image.URL, part.Image.Base64Data = nil, &data
				delete(part.Extra, assetRefExtraKey)
				message.UserInputMultiContent[i] = part
			case schema.ChatMessagePartTypeFileURL:
				content, err := fileInputContent(part, p.sessionID)
				if err != nil {
					return nil, err
				}
				text, err := attachmentText(content)
				if err != nil {
					return nil, err
				}
				message.UserInputMultiContent[i] = schema.MessageInputPart{Type: schema.ChatMessagePartTypeText, Text: text}
			default:
				return nil, invalidMedia("unsupported model input media")
			}
		}
	}
	return output, nil
}

func fileInputContent(part schema.MessageInputPart, sessionID string) (harness.Content, error) {
	if part.File == nil || part.File.URL == nil || part.File.Base64Data != nil {
		return harness.Content{}, invalidMedia("file contents must be normalized before execution")
	}
	content := harness.Content{Type: "resource_link", URI: *part.File.URL, MimeType: part.File.MIMEType, Name: part.File.Name}
	if _, found := part.Extra[assetRefExtraKey]; found {
		ref, err := assetFromPart(part.Extra)
		if err != nil {
			return content, err
		}
		if err := validateAsset(ref, sessionID, false); err != nil {
			return content, err
		}
		content.Asset, content.Name, content.Size = &ref, ref.Name, &ref.Size
	}
	return content, nil
}

func validateModelOutput(message *schema.Message) error {
	if message == nil {
		return nil
	}
	for _, part := range message.AssistantGenMultiContent {
		if part.Image != nil || part.Audio != nil || part.Video != nil || (part.Type != schema.ChatMessagePartTypeText && part.Type != schema.ChatMessagePartTypeReasoning) {
			return invalidMedia("generated multimedia must be imported before entering durable history")
		}
	}
	return nil
}

func validateProviderOutput(message *schema.Message) error {
	if err := validateModelOutput(message); err != nil {
		return err
	}
	if message == nil {
		return nil
	}
	for _, part := range message.UserInputMultiContent {
		if part.Type != schema.ChatMessagePartTypeText || part.Image != nil || part.Audio != nil || part.Video != nil || part.File != nil {
			return invalidMedia("generated multimedia requires normalization")
		}
	}
	for _, part := range message.MultiContent {
		if part.Type != schema.ChatMessagePartTypeText {
			return invalidMedia("generated legacy multimedia requires normalization")
		}
	}
	return nil
}

func validateToolMedia(result *schema.ToolResult, sessionID string) (*schema.ToolResult, error) {
	if result == nil {
		return nil, nil
	}
	for _, part := range result.Parts {
		if part.Audio != nil || part.Video != nil || (part.Image != nil && part.Type != schema.ToolPartTypeImage) || (part.File != nil && part.Type != schema.ToolPartTypeFile) {
			return nil, invalidMedia("unexpected media fields in tool result")
		}
	}
	parts, err := result.ToMessageInputParts()
	if err != nil {
		return nil, err
	}
	safe := &schema.ToolResult{}
	for _, part := range parts {
		switch part.Type {
		case schema.ChatMessagePartTypeText:
			safe.Parts = append(safe.Parts, schema.ToolOutputPart{Type: schema.ToolPartTypeText, Text: part.Text})
		case schema.ChatMessagePartTypeToolSearchResult:
			safe.Parts = append(safe.Parts, schema.ToolOutputPart{Type: schema.ToolPartTypeToolSearchResult, ToolSearchResult: part.ToolSearchResult})
		case schema.ChatMessagePartTypeImageURL:
			ref, err := validateImagePart(part, sessionID)
			if err != nil {
				return nil, err
			}
			image, err := imageReference(ref)
			if err != nil {
				return nil, err
			}
			safe.Parts = append(safe.Parts, schema.ToolOutputPart{Type: schema.ToolPartTypeImage, Image: &schema.ToolOutputImage{MessagePartCommon: image.Image.MessagePartCommon}, Extra: image.Extra})
		case schema.ChatMessagePartTypeFileURL:
			content, err := fileInputContent(part, sessionID)
			if err != nil {
				return nil, err
			}
			projected, err := ProjectToolContent([]harness.Content{content})
			if err != nil {
				return nil, err
			}
			safe.Parts = append(safe.Parts, projected.Parts...)
		default:
			return nil, invalidMedia("tool media requires normalization before entering durable history")
		}
	}
	return safe, nil
}

// normalizeToolImages imports raw enhanced-tool images before any event,
// receipt, or native checkpoint can observe their Base64 payload. The store
// publishes the staged snapshots with the successful tool_end transaction.
func normalizeToolImages(ctx context.Context, result *schema.ToolResult, req harness.RunRequest, callID string, importer harness.ToolImageImporter) (*schema.ToolResult, error) {
	if result == nil {
		return nil, nil
	}
	var raw []harness.Content
	for _, part := range result.Parts {
		if part.Image == nil || part.Image.Base64Data == nil {
			continue
		}
		if part.Type != schema.ToolPartTypeImage || part.Image.URL != nil || part.Image.MIMEType == "" || len(part.Extra) > 0 || part.Audio != nil || part.Video != nil || part.File != nil {
			return nil, invalidMedia("ambiguous inline tool image")
		}
		raw = append(raw, harness.Content{Type: "image", Data: *part.Image.Base64Data, MimeType: part.Image.MIMEType})
	}
	if len(raw) == 0 {
		return validateToolMedia(result, req.Session.ID)
	}
	if importer == nil {
		return nil, invalidMedia("tool image importer is unavailable")
	}
	imported, err := importer.StageToolImages(ctx, req.Session, req.RunID, callID, raw)
	if err != nil {
		return nil, err
	}
	if len(imported) != len(raw) {
		return nil, invalidMedia("tool image importer returned the wrong number of assets")
	}
	safe := &schema.ToolResult{Parts: make([]schema.ToolOutputPart, 0, len(result.Parts))}
	index := 0
	for _, part := range result.Parts {
		if part.Image != nil && part.Image.Base64Data != nil {
			projected, err := ProjectToolContent([]harness.Content{imported[index]})
			if err != nil {
				return nil, err
			}
			safe.Parts = append(safe.Parts, projected.Parts...)
			index++
		} else {
			safe.Parts = append(safe.Parts, part)
		}
	}
	return validateToolMedia(safe, req.Session.ID)
}

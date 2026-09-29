package assets

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/session"
)

func (s *Store) Prepare(ctx context.Context, x harness.Session, input []harness.Content) (prepared *Prepared, returnErr error) {
	p, err := s.begin(x)
	if err != nil {
		return nil, err
	}
	defer func() {
		if returnErr != nil {
			returnErr = errors.Join(returnErr, p.Finish(false))
		}
	}()
	var imageCount int
	var imageBytes int64
	hasContent := false
	for _, content := range input {
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		if len(content.Name) > 1024 || len(content.Description) > 16384 {
			return nil, invalid("attachment metadata is too large")
		}
		switch content.Type {
		case "text":
			if content.Data != "" || content.Asset != nil || content.URI != "" {
				return nil, invalid("text content cannot carry attachment data")
			}
			p.Input = append(p.Input, harness.Content{Type: "text", Text: content.Text})
			hasContent = hasContent || strings.TrimSpace(content.Text) != ""
		case "image", "resource_link":
			var normalized harness.Content
			if content.Asset != nil {
				if content.Data != "" || content.URI != harness.AssetURI(*content.Asset) {
					return nil, invalid("asset references must use their stable URI")
				}
				if _, err = s.resolve(ctx, x.ID, *content.Asset); err != nil {
					return nil, err
				}
				if content.Type == "image" && content.Asset.Kind != harness.AssetImage {
					return nil, invalid("asset is not an input image")
				}
				if content.Type == "resource_link" && content.Asset.Kind == harness.AssetImage {
					return nil, invalid("image assets require image content")
				}
				p.refs = append(p.refs, *content.Asset)
				normalized = assetContent(*content.Asset, content.Description)
			} else if content.Type == "image" {
				if content.URI != "" || content.Text != "" {
					return nil, invalid("remote images are not downloaded; send inline image data or a workspace file link")
				}
				data, mediaType, decodeErr := decodeImage(content.Data, content.MimeType)
				if decodeErr != nil {
					return nil, decodeErr
				}
				ref, snapshotErr := p.snapshot(ctx, bytes.NewReader(data), safeName(content.Name, "image"+imageExtension(mediaType)), mediaType, harness.AssetImage, harness.MaxInputImageBytes)
				if snapshotErr != nil {
					return nil, snapshotErr
				}
				normalized = assetContent(ref, content.Description)
			} else {
				if content.Data != "" || content.Text != "" || content.URI == "" {
					return nil, invalid("resource links require a URI")
				}
				u, parseErr := url.Parse(content.URI)
				if parseErr != nil {
					return nil, invalid("invalid resource URI")
				}
				if content.Size != nil && (*content.Size < 0 || *content.Size > harness.MaxInputFileBytes) {
					return nil, invalid("file resource exceeds its size limit")
				}
				switch u.Scheme {
				case "file":
					ref, fileErr := p.snapshotResource(ctx, content, u)
					if fileErr != nil {
						return nil, fileErr
					}
					normalized = assetContent(ref, content.Description)
				case "http", "https":
					if u.Host == "" || len(content.URI) > 8192 || imageHint(content.MimeType, u.Path) || imageHint("", content.Name) {
						return nil, invalid("remote images are not downloaded; send inline image data or a workspace file link")
					}
					normalized = harness.Content{Type: "resource_link", URI: content.URI, Name: safeName(content.Name, filepath.Base(u.Path)), MimeType: normalizeMIME(content.MimeType), Size: content.Size, Description: content.Description}
				default:
					return nil, invalid("unsupported resource URI")
				}
			}
			if normalized.Type == "image" {
				imageCount++
				imageBytes += normalized.Asset.Size
				if imageCount > harness.MaxInputImagesPerTurn || imageBytes > harness.MaxInputImageTotalBytes {
					return nil, invalid("a prompt supports at most 8 images and 40 MiB of image bytes")
				}
			}
			p.Input = append(p.Input, normalized)
			hasContent = true
		default:
			return nil, invalid("only text, image and resource_link prompt content is supported")
		}
	}
	if !hasContent {
		return nil, invalid("prompt must contain text or an attachment")
	}
	return p, nil
}

func assetContent(ref harness.AssetRef, description string) harness.Content {
	typeName := "resource_link"
	if ref.Kind == harness.AssetImage {
		typeName = "image"
	}
	size := ref.Size
	return harness.Content{Type: typeName, URI: harness.AssetURI(ref), Name: ref.Name, MimeType: ref.MimeType, Size: &size, Description: description, Asset: &ref}
}

func normalizeMIME(value string) string {
	value = strings.ToLower(strings.TrimSpace(strings.Split(value, ";")[0]))
	if value == "image/jpg" || value == "image/pjpeg" {
		return "image/jpeg"
	}
	return value
}
func imageHint(mimeType, path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	if strings.HasPrefix(normalizeMIME(mimeType), "image/") || imageExtensionMIME(ext) != "" || strings.HasPrefix(mime.TypeByExtension(ext), "image/") {
		return true
	}
	// Keep unsupported image hints deterministic across host MIME databases.
	switch ext {
	case ".svg", ".svgz", ".bmp", ".ico", ".tif", ".tiff", ".avif", ".heic", ".heif":
		return true
	}
	return false
}
func imageExtension(mimeType string) string {
	switch mimeType {
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	}
	return ""
}
func imageExtensionMIME(ext string) string {
	switch strings.ToLower(ext) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg", ".jpe":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	}
	return ""
}
func safeName(value, fallback string) string {
	value = filepath.Base(strings.ReplaceAll(value, "\\", "/"))
	if value == "." || value == "" || value == "/" {
		value = fallback
	}
	if value == "." || value == "" {
		value = "attachment"
	}
	return value
}

func decodeImage(encoded, declared string) ([]byte, string, error) {
	encoded = strings.TrimSpace(encoded)
	if encoded == "" || len(encoded) > base64.StdEncoding.EncodedLen(int(harness.MaxInputImageBytes)) {
		return nil, "", invalid("image must be nonempty and at most 20 MiB")
	}
	if strings.ContainsAny(encoded, "\r\n\t ") {
		return nil, "", invalid("image data is not valid base64")
	}
	data, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil {
		return nil, "", invalid("image data is not valid base64")
	}
	mimeType, err := validateImage(data, declared)
	return data, mimeType, err
}
func validateImage(data []byte, declared string) (string, error) {
	if len(data) == 0 || int64(len(data)) > harness.MaxInputImageBytes {
		return "", invalid("image must be nonempty and at most 20 MiB")
	}
	detected := http.DetectContentType(data)
	if imageExtension(detected) == "" {
		return "", invalid("supported image formats are PNG, JPEG, GIF and WebP")
	}
	if normalized := normalizeMIME(declared); normalized != "" && normalized != detected {
		return "", invalid("image content does not match the declared MIME type")
	}
	return detected, nil
}

func (p *Prepared) snapshotResource(ctx context.Context, content harness.Content, u *url.URL) (harness.AssetRef, error) {
	if u.RawQuery != "" || u.Fragment != "" {
		return harness.AssetRef{}, invalid("local file references cannot contain query or fragment")
	}
	path := filepath.FromSlash(u.Path)
	if u.Host != "" && u.Host != "localhost" {
		if runtime.GOOS != "windows" {
			return harness.AssetRef{}, invalid("remote file hosts are not supported")
		}
		path = filepath.FromSlash("//" + u.Host + u.Path)
	}
	if runtime.GOOS == "windows" && len(path) > 3 && (path[0] == '/' || path[0] == '\\') && path[2] == ':' {
		path = path[1:]
	}
	f, err := openWorkspaceFile(p.session.CWD, path)
	if err != nil {
		return harness.AssetRef{}, err
	}
	defer f.Close()
	before, err := f.Stat()
	if err != nil {
		return harness.AssetRef{}, err
	}
	if !before.Mode().IsRegular() || before.Size() > harness.MaxInputFileBytes {
		return harness.AssetRef{}, invalid("resource must be a regular workspace file at most 25 MiB")
	}
	name := safeName(content.Name, filepath.Base(path))
	mediaType := normalizeMIME(content.MimeType)
	if mediaType == "" {
		mediaType = mime.TypeByExtension(filepath.Ext(path))
	}
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	kind := harness.AssetFile
	var source io.Reader = f
	limit := harness.MaxInputFileBytes
	if imageHint(content.MimeType, path) || imageHint("", content.Name) {
		data, readErr := io.ReadAll(&contextReader{ctx: ctx, reader: io.LimitReader(f, harness.MaxInputImageBytes+1)})
		if readErr != nil {
			return harness.AssetRef{}, readErr
		}
		mediaType, err = validateImage(data, content.MimeType)
		if err != nil {
			return harness.AssetRef{}, err
		}
		source = bytes.NewReader(data)
		kind = harness.AssetImage
		limit = harness.MaxInputImageBytes
	}
	ref, err := p.snapshot(ctx, source, name, mediaType, kind, limit)
	if err != nil {
		return harness.AssetRef{}, err
	}
	after, err := f.Stat()
	if err != nil {
		return harness.AssetRef{}, err
	}
	if before.Size() != after.Size() || before.ModTime() != after.ModTime() || ref.Size != before.Size() {
		return harness.AssetRef{}, invalid("resource changed while being snapshotted")
	}
	return ref, nil
}

func openWorkspaceFile(cwd, path string) (*os.File, error) {
	actual, err := session.NormalizeWorkspace(cwd)
	if err != nil {
		return nil, err
	}
	if !session.SameWorkspace(actual, cwd) {
		return nil, invalid("workspace directory was replaced")
	}
	if !filepath.IsAbs(path) {
		return nil, invalid("local resource path must be absolute")
	}
	relative, err := filepath.Rel(cwd, path)
	if err != nil || !filepath.IsLocal(relative) {
		return nil, invalid("resource must stay inside the session workspace")
	}
	expected, err := os.Lstat(cwd)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(cwd)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(expected, opened) {
		return nil, invalid("workspace changed while opening")
	}
	f, err := openRead(root, relative)
	if err != nil {
		return nil, invalid("resource cannot be opened inside the workspace")
	}
	return f, nil
}

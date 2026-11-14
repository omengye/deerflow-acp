package deerflow

import (
	"context"
	"strings"
	"sync"

	"github.com/omengye/deerflow-acp/go-harness/channelproto"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/automation"
	"github.com/omengye/deerflow-acp/go-harness/internal/session"
)

// Only committed output events from this invocation are considered. Never scan
// old session artifacts, parse arbitrary Markdown paths, or forward view_image.
type channelImages struct {
	mu      sync.Mutex
	session string
	refs    []harness.AssetRef
	omitted bool
}

func (c *channelImages) observe(e harness.RunEvent) {
	if e.SessionID != c.session || (e.Kind != "image_delta" && (e.Kind != "tool_end" || e.Status != "completed" || e.ToolName == "view_image")) {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, content := range e.Content {
		ref := content.Asset
		if ref == nil || ref.SessionID != c.session || ref.ID == "" || ref.SHA256 == "" || !strings.HasPrefix(ref.MimeType, "image/") {
			continue
		}
		if !((content.Type == "image" && ref.Kind == harness.AssetImage) || (content.Type == "resource_link" && ref.Kind == harness.AssetArtifact && e.ToolName == "present_files")) {
			continue
		}
		duplicate := false
		for _, saved := range c.refs {
			duplicate = duplicate || saved.SHA256 == ref.SHA256
		}
		if duplicate {
			continue
		}
		if len(c.refs) == channelproto.MaxReplyImages {
			c.omitted = true
			continue
		}
		c.refs = append(c.refs, *ref)
	}
}

func (c *channelImages) finish(out *automation.Outcome) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out.Images = append([]harness.AssetRef(nil), c.refs...)
	if c.omitted {
		out.Text += "\n本次图片数量超过渠道上限，其余图片请在桌面会话查看。"
	}
}

func (c *Client) channelImage(ctx context.Context, sessionID, workspace string, ref harness.AssetRef) (channelproto.Image, error) {
	if ref.SessionID != sessionID || (ref.Kind != harness.AssetImage && ref.Kind != harness.AssetArtifact) || ref.Size <= 0 || ref.Size > channelproto.MaxImageBytes || !channelproto.ImageMIME(ref.MimeType) {
		return channelproto.Image{}, harness.ErrInvalidInput
	}
	x, err := c.service.Store.Session(ctx, sessionID)
	if err != nil {
		return channelproto.Image{}, err
	}
	if !session.SameWorkspace(x.CWD, workspace) {
		return channelproto.Image{}, harness.ErrPermissionDenied
	}
	path, err := c.assets.SnapshotPath(ctx, sessionID, ref)
	if err != nil {
		return channelproto.Image{}, err
	}
	return channelproto.Image{Path: path, SHA256: ref.SHA256, Size: ref.Size, MimeType: ref.MimeType}, nil
}

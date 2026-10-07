package adapter

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

type Attachment struct {
	URL    string
	Name   string
	MIME   string
	SHA256 string
	Size   int64
}
type AttachmentError struct{ Detail string }

func (e *AttachmentError) Error() string  { return e.Detail }
func attachmentError(detail string) error { return &AttachmentError{detail} }
func (a Attachment) image() bool {
	switch a.MIME {
	case "image/jpeg", "image/png", "image/webp", "image/gif":
		return true
	}
	return false
}

var mediaPath = regexp.MustCompile(`^/media/([0-9a-f]{64})(?:\.[a-z0-9]{1,8})?$`)

func ParseAttachments(tags [][]string, relay string) ([]Attachment, error) {
	origin, _ := url.Parse(relay)
	if origin == nil {
		return nil, attachmentError("invalid relay origin")
	}
	expectedScheme := strings.Replace(strings.Replace(origin.Scheme, "wss", "https", 1), "ws", "http", 1)
	result := []Attachment{}
	seen := map[string]Attachment{}
	var total int64
	for _, tag := range tags {
		if len(tag) == 0 || tag[0] != "imeta" {
			continue
		}
		fields := map[string]string{}
		for _, item := range tag[1:] {
			k, v, ok := strings.Cut(item, " ")
			if !ok {
				continue
			}
			if old, exists := fields[k]; exists && old != v {
				return nil, attachmentError("conflicting attachment metadata")
			}
			fields[k] = v
		}
		a := Attachment{URL: fields["url"], Name: fields["filename"], MIME: strings.ToLower(strings.TrimSpace(strings.Split(fields["m"], ";")[0])), SHA256: strings.ToLower(fields["x"])}
		if a.MIME == "" {
			a.MIME = "application/octet-stream"
		}
		var err error
		a.Size, err = strconv.ParseInt(fields["size"], 10, 64)
		if err != nil || !hexID(a.SHA256) || a.Size < 1 {
			return nil, attachmentError("attachments require valid SHA-256 and byte size")
		}
		u, err := url.Parse(a.URL)
		if err != nil || u.User != nil || u.Fragment != "" || u.RawQuery != "" || !strings.EqualFold(u.Host, origin.Host) || u.Scheme != expectedScheme {
			return nil, attachmentError("attachment must use the configured relay media origin")
		}
		match := mediaPath.FindStringSubmatch(u.Path)
		if len(match) != 2 || match[1] != a.SHA256 {
			return nil, attachmentError("media URL does not identify the declared SHA-256")
		}
		if strings.HasPrefix(a.MIME, "audio/") || strings.HasPrefix(a.MIME, "video/") || (strings.HasPrefix(a.MIME, "image/") && !a.image()) {
			return nil, attachmentError("unsupported image, audio or video attachment")
		}
		limit := int64(25 << 20)
		if a.image() {
			limit = 20 << 20
		}
		if a.Size > limit {
			return nil, attachmentError("attachment exceeds its size limit")
		}
		if a.Name == "" {
			a.Name = filepath.Base(u.Path)
		}
		a.Name = safeName(a.Name)
		if prior, exists := seen[a.URL]; exists {
			if prior.MIME != a.MIME || prior.Size != a.Size || prior.SHA256 != a.SHA256 {
				return nil, attachmentError("conflicting duplicate attachment")
			}
			continue
		}
		seen[a.URL] = a
		result = append(result, a)
		total += a.Size
	}
	if len(result) > 8 || total > 40<<20 {
		return nil, attachmentError("at most 8 attachments totaling 40 MiB are supported")
	}
	return result, nil
}
func safeName(value string) string {
	out := []rune{}
	for _, r := range value {
		if r < 32 || r == 127 || strings.ContainsRune(`<>:"/\|?*`, r) {
			r = '_'
		}
		out = append(out, r)
		if len(out) >= 120 {
			break
		}
	}
	name := strings.Trim(string(out), " .")
	if name == "" {
		return "attachment"
	}
	return name
}
func digestString(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
func imageMIME(data []byte) string {
	switch {
	case bytes.HasPrefix(data, []byte{0xff, 0xd8, 0xff}):
		return "image/jpeg"
	case bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")):
		return "image/png"
	case bytes.HasPrefix(data, []byte("GIF87a")) || bytes.HasPrefix(data, []byte("GIF89a")):
		return "image/gif"
	case len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP":
		return "image/webp"
	}
	return ""
}
func verifiedBytes(root *os.Root, name string, a Attachment) ([]byte, error) {
	info, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() != a.Size {
		return nil, attachmentError("attachment cache is not an ordinary file of the declared size")
	}
	file, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	actual, err := file.Stat()
	if err != nil || !os.SameFile(info, actual) {
		return nil, attachmentError("attachment changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(file, a.Size+1))
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	if int64(len(data)) != a.Size || hex.EncodeToString(sum[:]) != a.SHA256 {
		return nil, attachmentError("downloaded attachment size or SHA-256 does not match")
	}
	detected := imageMIME(data)
	if (a.image() || detected != "") && detected != a.MIME {
		return nil, attachmentError("downloaded image format does not match its MIME type")
	}
	return data, nil
}
func openCache(workspace, session, event string) (*os.Root, string, error) {
	root, err := os.OpenRoot(workspace)
	if err != nil {
		return nil, "", err
	}
	current := root
	parts := []string{".buzz-attachments", digestString(session), digestString(event)}
	for _, part := range parts {
		info, e := current.Lstat(part)
		if errors.Is(e, os.ErrNotExist) {
			e = current.Mkdir(part, 0700)
			if e != nil && !errors.Is(e, os.ErrExist) {
				current.Close()
				return nil, "", e
			}
			info, e = current.Lstat(part)
		}
		if e != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			current.Close()
			return nil, "", attachmentError("attachment cache contains a link or non-directory")
		}
		next, e := current.OpenRoot(part)
		if e != nil {
			current.Close()
			return nil, "", e
		}
		actual, e := next.Stat(".")
		if e != nil || !os.SameFile(info, actual) {
			next.Close()
			current.Close()
			return nil, "", attachmentError("attachment cache changed while opening")
		}
		current.Close()
		current = next
	}
	return current, filepath.Join(append([]string{workspace}, parts...)...), nil
}
func PrepareAttachments(ctx context.Context, buzz *BuzzCLI, workspace, session, event string, attachments []Attachment) ([]map[string]any, error) {
	if len(attachments) == 0 {
		return nil, nil
	}
	root, directory, err := openCache(workspace, session, event)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	blocks := []map[string]any{}
	for _, a := range attachments {
		name := a.SHA256 + "-" + a.Name
		data, e := verifiedBytes(root, name, a)
		if e != nil {
			if info, statErr := root.Lstat(name); statErr == nil && (!info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0) {
				return nil, attachmentError("attachment cache file is a link or non-file")
			}
			token := make([]byte, 12)
			if _, err = rand.Read(token); err != nil {
				return nil, err
			}
			temporary := ".download-" + hex.EncodeToString(token)
			file, e := root.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
			if e != nil {
				return nil, e
			}
			_, e = buzz.run(ctx, []string{"media", "get", a.URL}, "", false, file, a.Size)
			closeErr := file.Close()
			if e == nil {
				e = closeErr
			}
			if e == nil {
				data, e = verifiedBytes(root, temporary, a)
			}
			if e == nil {
				e = root.Rename(temporary, name)
			}
			_ = root.Remove(temporary)
			if e != nil {
				if errorKind(e, "transport") {
					return nil, e
				}
				return nil, attachmentError("attachment download or verification failed")
			}
		}
		if a.image() {
			blocks = append(blocks, map[string]any{"type": "image", "data": base64.StdEncoding.EncodeToString(data), "mimeType": a.MIME, "_meta": map[string]string{"name": a.Name}})
		} else {
			full := filepath.ToSlash(filepath.Join(directory, name))
			if !strings.HasPrefix(full, "/") {
				full = "/" + full
			}
			uri := (&url.URL{Scheme: "file", Path: full}).String()
			blocks = append(blocks, map[string]any{"type": "resource_link", "uri": uri, "name": a.Name, "mimeType": a.MIME, "size": a.Size})
		}
	}
	return blocks, nil
}

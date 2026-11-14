package wechat

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/omengye/deerflow-acp/go-harness/channelproto"
)

const wechatCDN = "https://novac2c.cdn.weixin.qq.com/c2c"

// SendImage never retries sendmessage: a lost acknowledgement is ambiguous.
// Failures before that call are distinguishable so the host can send a notice.
func (c *Client) SendImage(ctx context.Context, cred Credentials, to, token, id string, image channelproto.Image) error {
	if to == "" || token == "" || id == "" {
		return channelproto.ErrImageNotSent
	}
	data, err := readImage(ctx, image)
	if err != nil {
		return errors.Join(channelproto.ErrImageNotSent, err)
	}
	item, err := c.uploadImage(ctx, cred, to, data)
	if err != nil {
		return errors.Join(channelproto.ErrImageNotSent, err)
	}
	body := map[string]any{"base_info": map[string]string{"channel_version": "1.0"}, "msg": map[string]any{
		"from_user_id": "", "to_user_id": to, "context_token": token, "client_id": id,
		"message_type": 2, "message_state": 2, "item_list": []any{item},
	}}
	var out map[string]any
	return c.request(ctx, cred, "/ilink/bot/sendmessage", nil, body, &out)
}

func readImage(ctx context.Context, image channelproto.Image) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !filepath.IsAbs(image.Path) || image.Size <= 0 || image.Size > channelproto.MaxImageBytes || !channelproto.ImageMIME(image.MimeType) || len(image.SHA256) != 64 {
		return nil, errors.New("invalid outbound image snapshot")
	}
	f, err := os.Open(image.Path)
	if err != nil {
		return nil, errors.New("image snapshot unavailable")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != image.Size {
		return nil, errors.New("image snapshot size changed")
	}
	data, err := io.ReadAll(io.LimitReader(f, image.Size+1))
	if err != nil {
		return nil, errors.New("image snapshot read failed")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if int64(len(data)) != image.Size || fmt.Sprintf("%x", sha256.Sum256(data)) != image.SHA256 || http.DetectContentType(data) != image.MimeType {
		return nil, errors.New("image snapshot content changed")
	}
	return data, nil
}

func (c *Client) uploadImage(ctx context.Context, cred Credentials, to string, data []byte) (map[string]any, error) {
	var key, fileKey [16]byte
	if _, err := rand.Read(key[:]); err != nil {
		return nil, err
	}
	if _, err := rand.Read(fileKey[:]); err != nil {
		return nil, err
	}
	// iLink's CDN protocol requires AES-128-ECB with PKCS#7 padding.
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	pad := aes.BlockSize - len(data)%aes.BlockSize
	ciphertext := make([]byte, len(data)+pad)
	copy(ciphertext, data)
	for n := len(data); n < len(ciphertext); n++ {
		ciphertext[n] = byte(pad)
	}
	for n := 0; n < len(ciphertext); n += aes.BlockSize {
		block.Encrypt(ciphertext[n:n+aes.BlockSize], ciphertext[n:n+aes.BlockSize])
	}
	keyHex, fileHex := hex.EncodeToString(key[:]), hex.EncodeToString(fileKey[:])
	var upload struct {
		URL   string `json:"upload_full_url"`
		Param string `json:"upload_param"`
	}
	err = c.request(ctx, cred, "/ilink/bot/getuploadurl", nil, map[string]any{
		"base_info": map[string]string{"channel_version": "1.0"},
		"filekey":   fileHex, "media_type": 1, "to_user_id": to, "rawsize": len(data),
		"rawfilemd5": fmt.Sprintf("%x", md5.Sum(data)), "filesize": len(ciphertext),
		"no_need_thumb": true, "aeskey": keyHex,
	}, &upload)
	if err != nil {
		return nil, err
	}
	target := strings.TrimSpace(upload.URL)
	if target == "" && upload.Param != "" && len(upload.Param) <= 16384 {
		target = wechatCDN + "/upload?" + url.Values{"encrypted_query_param": {upload.Param}, "filekey": {fileHex}}.Encode()
	}
	u, err := url.Parse(target)
	if err != nil || len(target) > 32768 || u.Scheme != "https" || u.User != nil || u.Port() != "" || u.Fragment != "" || !strings.HasSuffix(strings.ToLower(u.Hostname()), ".weixin.qq.com") {
		return nil, errors.New("invalid WeChat CDN upload URL")
	}
	param, err := c.uploadCiphertext(ctx, target, ciphertext)
	if err != nil {
		return nil, err
	}
	return map[string]any{"type": 2, "image_item": map[string]any{
		"media": map[string]any{
			"encrypt_query_param": param,
			// The provider's sender encodes the hexadecimal key string as base64.
			"aes_key": base64.StdEncoding.EncodeToString([]byte(keyHex)), "encrypt_type": 1,
		},
		"mid_size": len(ciphertext),
	}}, nil
}

func (c *Client) uploadCiphertext(ctx context.Context, target string, data []byte) (string, error) {
	for attempt := 0; attempt < 3; attempt++ {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		r, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(data))
		if err != nil {
			return "", errors.New("cannot construct WeChat CDN request")
		}
		// CDN requests carry encrypted bytes, never the bot's authorization.
		r.Header.Set("Content-Type", "application/octet-stream")
		resp, err := c.HTTP.Do(r)
		if err != nil {
			continue
		}
		param := resp.Header.Get("x-encrypted-param")
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK && param != "" && len(param) <= 16384 {
			return param, nil
		}
		if resp.StatusCode >= 300 && resp.StatusCode < 500 {
			return "", fmt.Errorf("WeChat CDN HTTP status %d", resp.StatusCode)
		}
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return "", errors.New("WeChat CDN upload failed")
}

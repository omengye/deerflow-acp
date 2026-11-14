package wechat

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/channelproto"
)

func imageSnapshot(t *testing.T, large bool) (channelproto.Image, []byte) {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	if large {
		// A valid PNG with trailing bytes exercises files larger than stdio's cap.
		buf.Write(bytes.Repeat([]byte{42}, channelproto.MaxFrame+100))
	}
	data := buf.Bytes()
	path := filepath.Join(t.TempDir(), "snapshot.blob")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return channelproto.Image{Path: path, SHA256: fmt.Sprintf("%x", sha256.Sum256(data)), Size: int64(len(data)), MimeType: "image/png"}, data
}

func TestImagePluginUploadsEncryptedSnapshotAndSendsNativeImage(t *testing.T) {
	for _, fullURL := range []bool{false, true} {
		t.Run(fmt.Sprintf("full_url=%v", fullURL), func(t *testing.T) {
			snapshot, original := imageSnapshot(t, true)
			var key []byte
			var fileKey string
			var uploaded []byte
			uploads, sends := 0, 0
			p := NewPlugin()
			p.client = fakeClient(func(r *http.Request) (*http.Response, error) {
				switch r.URL.Path {
				case "/ilink/bot/getuploadurl":
					if r.Header.Get("Authorization") != "Bearer secret" {
						t.Fatal("missing API authorization")
					}
					var req struct {
						FileKey string `json:"filekey"`
						AESKey  string `json:"aeskey"`
						MD5     string `json:"rawfilemd5"`
						To      string `json:"to_user_id"`
						RawSize int    `json:"rawsize"`
						Size    int    `json:"filesize"`
						Type    int    `json:"media_type"`
						NoThumb bool   `json:"no_need_thumb"`
					}
					if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
						t.Fatal(err)
					}
					key, _ = hex.DecodeString(req.AESKey)
					fileKey = req.FileKey
					if len(key) != 16 || len(fileKey) != 32 || req.MD5 != fmt.Sprintf("%x", md5.Sum(original)) || req.RawSize != len(original) || req.Size != (len(original)/16+1)*16 || req.Type != 1 || req.To != "alice" || !req.NoThumb {
						t.Fatal("wrong upload metadata", req)
					}
					if fullURL {
						return response(`{"upload_full_url":"https://novac2c.cdn.weixin.qq.com/custom?signature=private","upload_param":"ignored"}`), nil
					}
					return response(`{"upload_param":"opaque +/&?"}`), nil
				case "/c2c/upload", "/custom":
					uploads++
					if r.Method != "POST" || r.Header.Get("Content-Type") != "application/octet-stream" || r.Header.Get("Authorization") != "" || r.Header.Get("AuthorizationType") != "" || r.Header.Get("X-WECHAT-UIN") != "" {
						t.Fatal("wrong CDN headers")
					}
					if !fullURL && (r.URL.Query().Get("filekey") != fileKey || r.URL.Query().Get("encrypted_query_param") != "opaque +/&?") {
						t.Fatal("incorrect upload URL")
					}
					body, _ := io.ReadAll(r.Body)
					if uploads == 1 {
						uploaded = body
						res := response("")
						res.StatusCode = 503
						return res, nil
					}
					if !bytes.Equal(uploaded, body) {
						t.Fatal("retry changed ciphertext")
					}
					res := response("")
					res.Header.Set("x-encrypted-param", "download-param")
					return res, nil
				case "/ilink/bot/sendmessage":
					sends++
					var req struct {
						Msg struct {
							To      string `json:"to_user_id"`
							Key     string `json:"client_id"`
							Context string `json:"context_token"`
							Type    int    `json:"message_type"`
							State   int    `json:"message_state"`
							Items   []struct {
								Type  int `json:"type"`
								Image struct {
									Size  int `json:"mid_size"`
									Media struct {
										Param string `json:"encrypt_query_param"`
										Key   string `json:"aes_key"`
										Type  int    `json:"encrypt_type"`
									} `json:"media"`
								} `json:"image_item"`
							} `json:"item_list"`
						} `json:"msg"`
					}
					if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
						t.Fatal(err)
					}
					if req.Msg.To != "alice" || req.Msg.Key != "stable-image-0" || req.Msg.Context != "reply-token" || req.Msg.Type != 2 || req.Msg.State != 2 || len(req.Msg.Items) != 1 {
						t.Fatal("wrong native message", req)
					}
					item := req.Msg.Items[0]
					encodedKey, _ := base64.StdEncoding.DecodeString(item.Image.Media.Key)
					if item.Type != 2 || item.Image.Size != len(uploaded) || item.Image.Media.Param != "download-param" || string(encodedKey) != hex.EncodeToString(key) || item.Image.Media.Type != 1 {
						t.Fatal("wrong image reference", item)
					}
					block, _ := aes.NewCipher(key)
					plain := make([]byte, len(uploaded))
					for n := 0; n < len(plain); n += 16 {
						block.Decrypt(plain[n:n+16], uploaded[n:n+16])
					}
					pad := int(plain[len(plain)-1])
					if pad < 1 || pad > 16 || !bytes.Equal(plain[len(plain)-pad:], bytes.Repeat([]byte{byte(pad)}, pad)) || !bytes.Equal(plain[:len(plain)-pad], original) {
						t.Fatal("encryption did not preserve image bytes")
					}
					return response(`{"ret":0}`), nil
				default:
					t.Fatal("unexpected request", r.URL.Path)
					return nil, nil
				}
			})
			secret, _ := json.Marshal(Credentials{Token: "secret", BotID: "bot"})
			raw, _ := json.Marshal(channelproto.Send{Auth: channelproto.Auth{Secret: secret}, Message: channelproto.Message{Sender: "alice", Context: "reply-token"}, Key: "stable-image-0", Image: &snapshot})
			if len(raw) > channelproto.MaxFrame {
				t.Fatal("image bytes entered stdio")
			}
			out, err := p.Handle(t.Context(), "send", raw)
			if err != nil || !out.(map[string]bool)["sent"] || uploads != 2 || sends != 1 || !Descriptor().OutboundImages {
				t.Fatal(out, err, uploads, sends)
			}
		})
	}
}

func TestImageSnapshotValidationBeforeNetwork(t *testing.T) {
	image, _ := imageSnapshot(t, false)
	for _, tc := range []struct {
		name   string
		change func(*channelproto.Image)
	}{
		{"relative", func(i *channelproto.Image) { i.Path = "photo.png" }},
		{"missing", func(i *channelproto.Image) { i.Path += "missing" }},
		{"digest", func(i *channelproto.Image) { i.SHA256 = strings.Repeat("0", 64) }},
		{"size", func(i *channelproto.Image) { i.Size++ }},
		{"too_large", func(i *channelproto.Image) { i.Size = channelproto.MaxImageBytes + 1 }},
		{"empty", func(i *channelproto.Image) { i.Size = 0 }},
		{"mime", func(i *channelproto.Image) { i.MimeType = "image/jpeg" }},
		{"svg", func(i *channelproto.Image) { i.MimeType = "image/svg+xml" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := image
			tc.change(&bad)
			c := fakeClient(func(*http.Request) (*http.Response, error) { t.Fatal("invalid image reached network"); return nil, nil })
			if err := c.SendImage(t.Context(), Credentials{}, "alice", "context", "id", bad); !errors.Is(err, channelproto.ErrImageNotSent) {
				t.Fatal(err)
			}
		})
	}
}

func TestImageFailuresDistinguishUploadFromAmbiguousSend(t *testing.T) {
	image, _ := imageSnapshot(t, false)
	for _, mode := range []string{"bad_url", "redirect", "missing_url", "upload_4xx", "upload_5xx", "upload_no_ack", "upload_network", "send_network", "send_no_ack", "expired"} {
		t.Run(mode, func(t *testing.T) {
			uploads, sends := 0, 0
			c := fakeClient(func(r *http.Request) (*http.Response, error) {
				switch r.URL.Path {
				case "/ilink/bot/getuploadurl":
					if mode == "expired" {
						return response(`{"errcode":-14}`), nil
					}
					if mode == "bad_url" {
						return response(`{"upload_full_url":"https://evil.test/private"}`), nil
					}
					if mode == "missing_url" {
						return response(`{}`), nil
					}
					return response(`{"upload_param":"private-param"}`), nil
				case "/c2c/upload":
					uploads++
					res := response("PRIVATE-CDN-BODY")
					switch mode {
					case "upload_4xx":
						res.StatusCode = 403
					case "upload_5xx":
						res.StatusCode = 503
					case "redirect":
						res.StatusCode = 302
						res.Header.Set("Location", "https://evil.test")
					case "upload_network":
						return nil, errors.New("PRIVATE-URL")
					case "upload_no_ack":
					default:
						res.Header.Set("x-encrypted-param", "download")
					}
					return res, nil
				case "/ilink/bot/sendmessage":
					sends++
					if mode == "send_network" {
						return nil, errors.New("PRIVATE-URL")
					}
					return response(""), nil
				default:
					t.Fatal("redirect followed")
					return nil, nil
				}
			})
			err := c.SendImage(t.Context(), Credentials{Token: "secret"}, "alice", "context", "id", image)
			ambiguous := strings.HasPrefix(mode, "send_")
			if err == nil || errors.Is(err, channelproto.ErrImageNotSent) == ambiguous || strings.Contains(err.Error(), "PRIVATE") {
				t.Fatal(err)
			}
			if (ambiguous && sends != 1) || (!ambiguous && sends != 0) {
				t.Fatal("message was retried or sent before upload", sends)
			}
			wantUploads := 1
			switch mode {
			case "expired", "bad_url", "missing_url":
				wantUploads = 0
			case "upload_5xx", "upload_no_ack", "upload_network":
				wantUploads = 3
			}
			if uploads != wantUploads {
				t.Fatal("wrong upload retry count", uploads)
			}
			if mode == "expired" && !errors.Is(err, ErrExpired) {
				t.Fatal("lost auth expiry")
			}
		})
	}
}

func TestImageCancellationDoesNotUpload(t *testing.T) {
	image, _ := imageSnapshot(t, false)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	c := fakeClient(func(*http.Request) (*http.Response, error) { t.Fatal("cancelled upload started"); return nil, nil })
	if err := c.SendImage(ctx, Credentials{}, "alice", "context", "id", image); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

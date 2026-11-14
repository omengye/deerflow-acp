package channels

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/channelproto"
	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func saveImagesTx(ctx context.Context, tx *sql.Tx, instance, account, id string, images []harness.AssetRef) error {
	if len(images) > channelproto.MaxReplyImages {
		return userError("too many channel images")
	}
	for position, ref := range images {
		raw, err := json.Marshal(ref)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO harness_channel_images(instance,account,id,position,asset) VALUES(?,?,?,?,?)`, instance, account, id, position, raw); err != nil {
			return err
		}
	}
	return nil
}

func sendAcknowledged(ctx context.Context, p transport, send channelproto.Send) error {
	var ack struct {
		Sent bool `json:"sent"`
	}
	if err := p.Call(ctx, "send", send, &ack); err != nil {
		return err
	}
	if !ack.Sent {
		return userError("plugin did not acknowledge delivery")
	}
	return nil
}

func (m *Manager) sendReply(ctx context.Context, i *instance, p transport, auth channelproto.Auth, cfg Config, msg channelproto.Message, session, key, reply string) error {
	send := channelproto.Send{Auth: auth, Message: msg, Key: key, Text: reply, Options: cfg.Options}
	if reply != "" {
		if err := sendAcknowledged(ctx, p, send); err != nil {
			return err
		}
	}
	rows, err := m.db.QueryContext(ctx, `SELECT position,asset,status FROM harness_channel_images WHERE instance=? AND account=? AND id=? ORDER BY position`, i.id, auth.Account, msg.ID)
	if err != nil {
		return err
	}
	type pendingImage struct {
		position int
		ref      harness.AssetRef
		status   string
	}
	var images []pendingImage
	for rows.Next() {
		var image pendingImage
		var raw []byte
		if err = rows.Scan(&image.position, &raw, &image.status); err == nil {
			err = json.Unmarshal(raw, &image.ref)
		}
		if err != nil {
			rows.Close()
			return err
		}
		images = append(images, image)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	i.mu.Lock()
	supported := i.manifest.Descriptor.OutboundImages
	i.mu.Unlock()
	failed := 0
	for _, image := range images {
		if err := ctx.Err(); err != nil {
			return err
		}
		if image.status == "sent" {
			continue
		}
		if image.status != "pending" {
			return userError("image delivery requires review")
		}
		var attachment channelproto.Image
		if !supported || m.ResolveImage == nil || image.ref.SessionID != session {
			err = channelproto.ErrImageNotSent
		} else {
			attachment, err = m.ResolveImage(ctx, session, cfg.Workspace, image.ref)
		}
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err = m.imageStatus(ctx, i.id, auth.Account, msg.ID, image.position, "failed"); err != nil {
				return err
			}
			failed++
			continue
		}
		if err = m.imageStatus(ctx, i.id, auth.Account, msg.ID, image.position, "sending"); err != nil {
			return err
		}
		send.Key, send.Text, send.Image = key+"-image-"+strconv.Itoa(image.position), "", &attachment
		err = sendAcknowledged(ctx, p, send)
		state := "sent"
		if err != nil {
			state = "unknown"
			if errors.Is(err, channelproto.ErrImageNotSent) && !errors.Is(err, channelproto.ErrAuthExpired) && ctx.Err() == nil {
				state = "failed"
			}
		}
		if saveErr := m.imageStatus(ctx, i.id, auth.Account, msg.ID, image.position, state); saveErr != nil {
			return errors.Join(err, saveErr)
		}
		if state == "unknown" {
			return err
		}
		if state == "failed" {
			failed++
		}
	}
	if failed > 0 {
		send.Key, send.Image = key+"-image-notice", nil
		send.Text = fmt.Sprintf("有 %d 张图片未发送成功（渠道不支持、图片不可用或上传失败）。请在桌面会话查看；不会自动重发。会话：%s", failed, session)
		return sendAcknowledged(ctx, p, send)
	}
	return nil
}

func (m *Manager) imageStatus(ctx context.Context, instance, account, id string, position int, state string) error {
	persist, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	_, err := m.db.ExecContext(persist, `UPDATE harness_channel_images SET status=? WHERE instance=? AND account=? AND id=? AND position=?`, state, instance, account, id, position)
	return err
}

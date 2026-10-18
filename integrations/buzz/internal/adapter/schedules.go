package adapter

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

type buzzScheduleMessageKey struct{}

func (a *ACPClient) scheduleAccount() string {
	identity, _ := json.Marshal([]string{strings.TrimRight(a.Config.Buzz.RelayURL, "/"), a.Config.Buzz.AgentPubkey})
	return fmt.Sprintf("%x", sha256.Sum256(identity))
}

func (a *ACPClient) scheduleManage(ctx context.Context, request any, output any) error {
	args := []string{"--manage", "--no-auto-start"}
	// Only connection options are inherited; never copy protocol/prompt args.
	for _, option := range []struct{ flag, value string }{{"--runtime-dir", a.Config.Deerflow.RuntimeDir}, {"--config", a.Config.Deerflow.ConfigPath}} {
		value := option.value
		if value == "" {
			for i := 0; i+1 < len(a.Config.Deerflow.Args); i++ {
				if a.Config.Deerflow.Args[i] == option.flag {
					value = a.Config.Deerflow.Args[i+1]
				}
			}
		}
		if value != "" {
			args = append(args, option.flag, value)
		}
	}
	raw, err := json.Marshal(request)
	if err != nil {
		return err
	}
	bounded, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(bounded, a.Config.Deerflow.Command, args...)
	hideCommand(cmd)
	cmd.Env = mergeEnv(os.Environ(), a.Config.Deerflow.Env)
	cmd.Stdin = bytes.NewReader(raw)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err = cmd.Run(); err != nil {
		return errors.New("local schedule management transport failed")
	}
	var envelope struct {
		OK   bool            `json:"ok"`
		Data json.RawMessage `json:"data"`
		Code string          `json:"code"`
	}
	if json.Unmarshal(stdout.Bytes(), &envelope) != nil || !envelope.OK {
		return errors.New("local schedule management rejected request")
	}
	if output != nil {
		return json.Unmarshal(envelope.Data, output)
	}
	return nil
}

func (a *ACPClient) refreshSchedules(ctx context.Context) error {
	var result struct {
		Version string `json:"version"`
	}
	return a.registerSchedules(ctx, &result)
}
func (a *ACPClient) registerSchedules(ctx context.Context, output any) error {
	return a.scheduleManage(ctx, map[string]any{"operation": "schedule.buzz.register", "account": a.scheduleAccount(), "workspace": a.Config.Deerflow.Workspace, "allowed_senders": a.Config.Buzz.AllowedPubkeys, "full_access": a.Config.Deerflow.ScheduleFullAccess && !a.Config.Buzz.AllowAnySender}, output)
}
func (a *ACPClient) bindSchedules(ctx context.Context, session string, m Pending) error {
	var result struct {
		Version string `json:"version"`
	}
	if err := a.registerSchedules(ctx, &result); err != nil {
		return err
	}
	if !m.ScheduleAllowed {
		result.Version = ""
	}
	return a.scheduleManage(ctx, map[string]any{"operation": "schedule.buzz.bind", "session_id": session, "source": map[string]any{
		"account": a.scheduleAccount(), "sender": m.Author, "workspace": a.Config.Deerflow.Workspace, "conversation": m.Channel, "message_id": m.ID, "grant": result.Version,
	}}, nil)
}

type scheduleDelivery struct {
	ID     string `json:"id"`
	Text   string `json:"text"`
	Target struct {
		Account      string `json:"account"`
		Sender       string `json:"sender"`
		Conversation string `json:"conversation"`
		ReplyTo      string `json:"reply_to"`
	} `json:"target"`
}

func (a *App) deliverSchedules(ctx context.Context, client *ACPClient) error {
	request := func(op, id, status string, output any) error {
		return client.scheduleManage(ctx, map[string]any{"operation": op, "account": client.scheduleAccount(), "workspace": a.Config.Deerflow.Workspace, "id": id, "status": status}, output)
	}
	var pending []scheduleDelivery
	if err := request("schedule.buzz.poll", "", "", &pending); err != nil {
		return err
	}
	for _, item := range pending {
		var claimed scheduleDelivery
		if err := request("schedule.buzz.claim", item.ID, "", &claimed); err != nil {
			return err
		}
		_, err := a.Buzz.Send(ctx, claimed.Target.Conversation, claimed.Target.ReplyTo, claimed.Text)
		status := "delivered"
		if err != nil {
			status = "unknown"
			if errorKind(err, "transport") {
				status = "ready"
			}
		}
		if ackErr := request("schedule.buzz.ack", item.ID, status, nil); ackErr != nil {
			return ackErr
		}
		if err != nil {
			return err
		}
	}
	return nil
}

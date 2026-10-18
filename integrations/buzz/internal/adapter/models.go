package adapter

import (
	"encoding/hex"
	"encoding/json"
	"strings"
)

type Channel struct {
	ID   string
	Name string
	DM   bool
}
type Message struct {
	ID              string     `json:"id"`
	Author          string     `json:"pubkey"`
	Created         int64      `json:"created_at"`
	Kind            int        `json:"kind"`
	Content         string     `json:"content"`
	Tags            [][]string `json:"tags"`
	Channel         string     `json:"-"`
	DM              bool       `json:"-"`
	ScheduleAllowed bool       `json:"-"`
}
type Pending struct {
	Message
	Key          string
	Conversation string
	Attempts     int
	Response     *string
}

func hexID(v string) bool { b, e := hex.DecodeString(v); return e == nil && len(b) == 32 }
func parseMessage(channel Channel, raw json.RawMessage) (Message, bool) {
	var m Message
	if json.Unmarshal(raw, &m) != nil || !hexID(m.ID) || !hexID(m.Author) || m.Created < 0 {
		return m, false
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	for _, key := range []string{"content", "created_at", "kind"} {
		if len(fields[key]) == 0 || string(fields[key]) == "null" {
			return m, false
		}
	}
	m.ID = strings.ToLower(m.ID)
	m.Author = strings.ToLower(m.Author)
	m.Channel = channel.ID
	m.DM = channel.DM
	return m, true
}
func (m Message) mentions(pub string) bool {
	for _, tag := range m.Tags {
		if len(tag) >= 2 && tag[0] == "p" && strings.EqualFold(tag[1], pub) {
			return true
		}
	}
	return false
}
func (m Message) conversation(scope string) string {
	if m.DM || scope == "channel" {
		return "sender:" + m.Author + ":channel:" + m.Channel
	}
	root := ""
	reply := false
	var unmarked []string
	for _, tag := range m.Tags {
		if len(tag) < 2 || tag[0] != "e" || !hexID(tag[1]) {
			continue
		}
		marker := ""
		if len(tag) > 3 {
			marker = tag[3]
		}
		switch marker {
		case "root":
			root = strings.ToLower(tag[1])
		case "reply":
			reply = true
		case "":
			unmarked = append(unmarked, strings.ToLower(tag[1]))
		}
	}
	if !reply && len(unmarked) < 2 {
		root = ""
	}
	if root == "" && ((reply && len(unmarked) > 0) || len(unmarked) >= 2) {
		root = unmarked[0]
	}
	if root == "" {
		root = m.ID
	}
	return "sender:" + m.Author + ":thread:" + m.Channel + ":" + root
}
func (c Config) eligible(m Message) bool {
	if m.Author == c.Buzz.AgentPubkey {
		return false
	}
	hasAttachment := false
	for _, tag := range m.Tags {
		if len(tag) > 0 && tag[0] == "imeta" {
			hasAttachment = true
		}
	}
	if strings.TrimSpace(m.Content) == "" && !hasAttachment {
		return false
	}
	kindAllowed := false
	for _, kind := range c.Buzz.MessageKinds {
		if kind == m.Kind {
			kindAllowed = true
		}
	}
	if !kindAllowed {
		return false
	}
	senderAllowed := c.Buzz.AllowAnySender
	for _, pub := range c.Buzz.AllowedPubkeys {
		if m.Author == pub {
			senderAllowed = true
		}
	}
	if !senderAllowed {
		return false
	}
	return m.DM || !c.Buzz.RequireMention || m.mentions(c.Buzz.AgentPubkey)
}

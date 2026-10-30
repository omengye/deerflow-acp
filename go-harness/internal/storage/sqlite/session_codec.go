package sqlite

import (
	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
)

// DecodeSessionEvent uses the same registered-type codec as the native store.
func DecodeSessionEvent(data []byte) (*adk.SessionEvent[*schema.Message], error) {
	var event adk.SessionEvent[*schema.Message]
	err := (&canonicalEventSerializer{}).Unmarshal(data, &event)
	return &event, err
}

func EncodeSessionEvent(event *adk.SessionEvent[*schema.Message]) ([]byte, error) {
	return (&canonicalEventSerializer{}).Marshal(event)
}

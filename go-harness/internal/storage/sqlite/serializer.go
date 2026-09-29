package sqlite

import (
	"bytes"
	"encoding/json"

	"github.com/cloudwego/eino/schema"
)

// Eino's serializer retains registered concrete types but emits map keys in
// unspecified order. Canonical JSON makes identical durable events byte-stable
// for append retry detection. UseNumber preserves integers above 2^53 and avoids
// converting application numbers through float64 while sorting object keys.
type canonicalEventSerializer struct {
	schema.HumanReadableSerializer
}

func (s *canonicalEventSerializer) Marshal(value any) ([]byte, error) {
	data, err := s.HumanReadableSerializer.Marshal(value)
	if err != nil {
		return nil, err
	}
	var tree any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err = decoder.Decode(&tree); err != nil {
		return nil, err
	}
	return json.Marshal(tree)
}

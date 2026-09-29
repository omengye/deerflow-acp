package protocol

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"strings"
	"unicode/utf8"
)

type envelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

func readFrame(reader *bufio.Reader, limit int) ([]byte, error) {
	var frame []byte
	for {
		part, err := reader.ReadSlice('\n')
		if err == nil {
			part = part[:len(part)-1]
		}
		if len(frame)+len(part) > limit {
			return nil, ErrFrameTooLarge
		}
		frame = append(frame, part...)
		if err == nil {
			return frame, nil
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if err == io.EOF && len(frame) > 0 {
			return nil, ErrTruncatedFrame
		}
		return nil, err
	}
}

func parseEnvelope(frame []byte) (envelope, *Error) {
	var msg envelope
	if !utf8.Valid(frame) || !json.Valid(frame) {
		return msg, &Error{Code: ParseError, Message: "Parse error"}
	}
	trimmed := bytes.TrimSpace(frame)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return msg, &Error{Code: InvalidRequest, Message: "Invalid Request"}
	}
	if err := json.Unmarshal(frame, &msg); err != nil {
		return envelope{}, &Error{Code: InvalidRequest, Message: "Invalid Request"}
	}
	if msg.JSONRPC != "2.0" {
		return envelope{}, &Error{Code: InvalidRequest, Message: "Invalid Request"}
	}
	if len(msg.ID) > 0 {
		if _, err := idKey(msg.ID); err != nil {
			return envelope{}, &Error{Code: InvalidRequest, Message: "Invalid request ID"}
		}
	}
	if msg.Method != "" {
		if msg.Result != nil || msg.Error != nil {
			return envelope{}, &Error{Code: InvalidRequest, Message: "Invalid Request"}
		}
		if msg.Params != nil {
			params := bytes.TrimSpace(msg.Params)
			if len(params) == 0 || (params[0] != '{' && params[0] != '[') {
				return msg, &Error{Code: InvalidParams, Message: "Invalid params"}
			}
		}
		return msg, nil
	}
	if msg.ID == nil || (msg.Result == nil) == (msg.Error == nil) || msg.Params != nil {
		return envelope{}, &Error{Code: InvalidRequest, Message: "Invalid Request"}
	}
	return msg, nil
}

// IDs retain their JSON type. We emit string IDs, never floating-point counters.
// Numeric inbound IDs are preserved byte-for-byte in their responses. ACP clients
// should not use fractional IDs (JSON-RPC recommends integer numbers).
func idKey(id json.RawMessage) (string, error) {
	var value any
	dec := json.NewDecoder(bytes.NewReader(id))
	dec.UseNumber()
	if err := dec.Decode(&value); err != nil {
		return "", err
	}
	switch v := value.(type) {
	case string:
		return "s:" + v, nil
	case json.Number:
		return numericIDKey(string(v)), nil
	case nil:
		return "null", nil
	default:
		return "", fmt.Errorf("invalid JSON-RPC ID type")
	}
}

// Normalize equal numeric spellings without converting to float64 (which loses
// integer precision) or expanding exponents (which can allocate enormous ints).
func numericIDKey(number string) string {
	sign := ""
	if strings.HasPrefix(number, "-") {
		sign, number = "-", number[1:]
	}
	var exponent big.Int
	if index := strings.IndexAny(number, "eE"); index >= 0 {
		exponent.SetString(number[index+1:], 10)
		number = number[:index]
	}
	fractional := 0
	if index := strings.IndexByte(number, '.'); index >= 0 {
		fractional = len(number) - index - 1
		number = number[:index] + number[index+1:]
	}
	number = strings.TrimLeft(number, "0")
	if number == "" {
		return "n:0"
	}
	trimmed := strings.TrimRight(number, "0")
	exponent.Add(&exponent, big.NewInt(int64(len(number)-len(trimmed)-fractional)))
	return "n:" + sign + trimmed + "e" + exponent.String()
}

func marshalFrame(value any, limit int) ([]byte, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	if len(encoded) > limit {
		return nil, ErrFrameTooLarge
	}
	return append(encoded, '\n'), nil
}

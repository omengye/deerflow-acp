// Package wslhelper defines the bounded host/guest command protocol. It has no
// model, daemon or interpreter dependencies and can be cross-compiled on Windows.
package wslhelper

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"path"
	"strings"
)

const Version = "deerflow-wsl-v1"
const MaxRequest = 4 << 20
const MaxFrame = 64 << 10
const ChunkSize = 32 << 10

type Probe struct {
	Version   string `json:"version"`
	SHA256    string `json:"sha256"`
	Arch      string `json:"arch"`
	Kernel    string `json:"kernel"`
	CWD       string `json:"cwd"`
	Pidfd     bool   `json:"pidfd"`
	Subreaper bool   `json:"subreaper"`
}

type Request struct {
	Version      string            `json:"version"`
	ID           string            `json:"id"`
	SHA256       string            `json:"sha256"`
	CWD          string            `json:"cwd"`
	TimeoutNanos int64             `json:"timeoutNanos"`
	Argv         []string          `json:"argv"`
	Environment  map[string]string `json:"environment"`
}

type Receipt struct {
	State     string `json:"state"`
	ExitCode  *int   `json:"exitCode"`
	Confirmed bool   `json:"confirmed"`
}

type Frame struct {
	Version string   `json:"version"`
	ID      string   `json:"id"`
	Kind    string   `json:"kind"`
	Data    []byte   `json:"data,omitempty"`
	Receipt *Receipt `json:"receipt,omitempty"`
}

func Decode(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return errors.New("invalid WSL protocol data")
	}
	if decoder.Decode(new(any)) != io.EOF {
		return errors.New("extra WSL protocol data")
	}
	return nil
}

func ValidDigest(value string, size int) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == size && value == strings.ToLower(value)
}

func (r Request) Validate() error {
	if r.Version != Version || !ValidDigest(r.ID, 16) || !ValidDigest(r.SHA256, 32) || r.TimeoutNanos <= 0 || !path.IsAbs(r.CWD) || strings.ContainsAny(r.CWD, "\x00\r\n") || len(r.Argv) == 0 || len(r.Argv) > 1026 || !path.IsAbs(r.Argv[0]) {
		return errors.New("invalid WSL command request")
	}
	for _, arg := range r.Argv {
		if len(arg) > 1<<20 || strings.ContainsRune(arg, 0) {
			return errors.New("invalid WSL argument")
		}
	}
	for key, value := range r.Environment {
		if key == "" || strings.ContainsRune(value, 0) {
			return errors.New("invalid WSL environment")
		}
		for i, ch := range key {
			if !(ch == '_' || ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || i > 0 && ch >= '0' && ch <= '9') {
				return errors.New("invalid WSL environment")
			}
		}
	}
	return nil
}

func (f Frame) Validate(id string) error {
	if f.Version != Version || f.ID != id {
		return errors.New("WSL frame identity mismatch")
	}
	switch f.Kind {
	case "stdout", "stderr":
		if len(f.Data) == 0 || len(f.Data) > ChunkSize || f.Receipt != nil {
			return errors.New("invalid WSL output frame")
		}
	case "receipt":
		if len(f.Data) != 0 || f.Receipt == nil {
			return errors.New("invalid WSL receipt frame")
		}
		r := f.Receipt
		switch r.State {
		case "completed":
			if r.ExitCode == nil || *r.ExitCode != 0 || !r.Confirmed {
				return errors.New("invalid completed receipt")
			}
		case "failed":
			if r.ExitCode == nil || *r.ExitCode == 0 || !r.Confirmed {
				return errors.New("invalid failed receipt")
			}
		case "cancelled", "timed_out":
			if !r.Confirmed {
				return errors.New("unconfirmed termination")
			}
		case "uncertain":
			if r.Confirmed {
				return errors.New("invalid uncertain receipt")
			}
		default:
			return errors.New("unknown WSL receipt state")
		}
	default:
		return errors.New("unknown WSL frame kind")
	}
	return nil
}

func EncodeFrame(frame Frame) ([]byte, error) {
	if err := frame.Validate(frame.ID); err != nil {
		return nil, err
	}
	data, err := json.Marshal(frame)
	if err != nil || len(data) > MaxFrame {
		return nil, errors.New("WSL frame exceeds limit")
	}
	packet := make([]byte, 4+len(data))
	binary.BigEndian.PutUint32(packet, uint32(len(data)))
	copy(packet[4:], data)
	return packet, nil
}

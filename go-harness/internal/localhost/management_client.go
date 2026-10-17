package localhost

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ManagementCall reads the protected endpoint without exposing its bearer token
// to arguments or output. Only literal loopback endpoints are accepted.
func ManagementCall(ctx context.Context, dir string, request json.RawMessage) (json.RawMessage, error) {
	if dir == "" {
		var err error
		dir, err = DefaultRuntimeDir()
		if err != nil {
			return nil, err
		}
	}
	f, err := os.Open(filepath.Join(dir, EndpointFilename))
	if err != nil {
		return nil, errors.New("cannot read local daemon endpoint; start deerflow-acpd first")
	}
	raw, err := io.ReadAll(io.LimitReader(f, 16385))
	_ = f.Close()
	if err != nil || len(raw) > 16384 {
		return nil, errors.New("invalid daemon endpoint")
	}
	var ep Endpoint
	if json.Unmarshal(raw, &ep) != nil || ep.Host != "127.0.0.1" && ep.Host != "::1" || ep.Port < 1 || ep.Port > 65535 || len(ep.Token) < 32 || len(ep.Token) > 256 || strings.ContainsAny(ep.Token, " \t\r\n") {
		return nil, errors.New("invalid local daemon endpoint")
	}
	if len(request) > 64<<10 || !json.Valid(request) {
		return nil, errors.New("request must be JSON of at most 64 KiB")
	}
	var compact map[string]json.RawMessage
	if json.Unmarshal(request, &compact) != nil || compact == nil {
		return nil, errors.New("request must be a JSON object")
	}
	if ep.InstanceID != "" {
		if supplied, ok := compact["expected_instance_id"]; ok {
			var expected string
			if json.Unmarshal(supplied, &expected) != nil || expected != ep.InstanceID {
				return nil, errors.New("daemon instance does not match expected_instance_id; no operation sent")
			}
		}
		compact["expected_instance_id"], _ = json.Marshal(ep.InstanceID)
	}
	request, err = json.Marshal(compact)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", net.JoinHostPort(ep.Host, strconv.Itoa(ep.Port)))
	if err != nil {
		return nil, errors.New("cannot connect to local daemon")
	}
	defer conn.Close()
	deadline, _ := ctx.Deadline()
	_ = conn.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	if _, err = fmt.Fprintf(conn, "DFACP/1 %s MANAGE\n", ep.Token); err != nil {
		return nil, errors.New("local handshake failed")
	}
	reader := bufio.NewReader(conn)
	line, err := readLine(reader, 4096)
	if err != nil || line != "OK" {
		return nil, errors.New("daemon rejected local management handshake")
	}
	if _, err = conn.Write(append(request, '\n')); err != nil {
		return nil, err
	}
	line, err = readLine(reader, 4<<20)
	if err != nil {
		return nil, err
	}
	if !json.Valid([]byte(line)) {
		return nil, errors.New("invalid management response")
	}
	if ep.InstanceID != "" {
		var envelope struct {
			Runtime struct {
				InstanceID string `json:"instance_id"`
			} `json:"runtime"`
		}
		if json.Unmarshal([]byte(line), &envelope) != nil || envelope.Runtime.InstanceID != ep.InstanceID {
			return nil, errors.New("management response came from a different daemon instance")
		}
	}
	return json.RawMessage(line), nil
}

package channels

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/channelproto"
)

type Manifest struct {
	ID         string                  `json:"id"`
	Command    string                  `json:"command"`
	Args       []string                `json:"args,omitempty"`
	SHA256     string                  `json:"sha256"`
	Descriptor channelproto.Descriptor `json:"descriptor"`
	Previous   *Manifest               `json:"previous,omitempty"`
}

func (m Manifest) Validate() error {
	if !validID(m.ID) || !filepath.IsAbs(m.Command) || len(m.Args) > 16 {
		return userError("invalid plugin identity or executable path")
	}
	for _, a := range m.Args {
		if len(a) > 4096 || strings.ContainsRune(a, 0) {
			return userError("invalid plugin argument")
		}
	}
	f, err := os.Open(m.Command)
	if err != nil {
		return userError("channel plugin executable is unavailable")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return userError("plugin must be a regular executable")
	}
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return err
	}
	if len(m.SHA256) != 64 || !strings.EqualFold(hex.EncodeToString(h.Sum(nil)), m.SHA256) {
		return userError("channel plugin checksum mismatch; explicitly register the new version")
	}
	return nil
}
func validID(s string) bool {
	if len(s) < 1 || len(s) > 64 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

type transport interface {
	Call(context.Context, string, any, any) error
	Close()
}
type process struct {
	cmd  *exec.Cmd
	in   io.WriteCloser
	out  *bufio.Scanner
	gate chan struct{}
	once sync.Once
	next uint64
}

func startProcess(ctx context.Context, m Manifest) (transport, channelproto.Descriptor, error) {
	if err := m.Validate(); err != nil {
		return nil, channelproto.Descriptor{}, err
	}
	cmd := exec.Command(m.Command, m.Args...)
	cmd.WaitDelay = 2 * time.Second
	hideProcess(cmd)
	cmd.Dir = filepath.Dir(m.Command)
	// Never inherit model keys, endpoint tokens or arbitrary host environment.
	for _, key := range []string{"SystemRoot", "WINDIR", "PATH", "TEMP", "TMP", "HOME", "USERPROFILE", "LOCALAPPDATA"} {
		if v, ok := os.LookupEnv(key); ok {
			cmd.Env = append(cmd.Env, key+"="+v)
		}
	}
	cmd.Stderr = io.Discard
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, channelproto.Descriptor{}, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		in.Close()
		return nil, channelproto.Descriptor{}, err
	}
	if err = cmd.Start(); err != nil {
		in.Close()
		out.Close()
		return nil, channelproto.Descriptor{}, userError("cannot start channel plugin")
	}
	p := &process{cmd: cmd, in: in, out: bufio.NewScanner(out), gate: make(chan struct{}, 1)}
	p.out.Buffer(make([]byte, 4096), channelproto.MaxFrame)
	var d channelproto.Descriptor
	if err = p.Call(ctx, "initialize", map[string]int{"protocol": channelproto.Version}, &d); err != nil {
		p.Close()
		return nil, d, err
	}
	if d.ID != m.ID || d.Protocol != channelproto.Version || len(d.Name) > 100 || d.Name == "" || len(d.Version) > 64 || len(d.Fields) > 16 {
		p.Close()
		return nil, d, userError("incompatible channel descriptor")
	}
	seen := map[string]bool{}
	for _, f := range d.Fields {
		if !validID(f.Key) || len(f.Label) > 100 || seen[f.Key] || (f.Secret && !f.Login) {
			p.Close()
			return nil, d, userError("invalid channel field")
		}
		seen[f.Key] = true
	}
	return p, d, nil
}
func (p *process) Close() {
	p.once.Do(func() { p.in.Close(); _ = p.cmd.Process.Kill(); _ = p.cmd.Wait() })
}
func (p *process) Call(ctx context.Context, method string, input, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	select {
	case p.gate <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-p.gate }()
	p.next++
	id := p.next
	raw, err := json.Marshal(input)
	if err != nil {
		return err
	}
	if len(raw) > channelproto.MaxFrame-256 {
		return userError("channel request too large")
	}
	request := channelproto.Request{JSONRPC: "2.0", ID: id, Method: method, Params: raw}
	type result struct {
		r   channelproto.Response
		err error
	}
	done := make(chan result, 1)
	go func() {
		if err := json.NewEncoder(p.in).Encode(request); err != nil {
			done <- result{err: userError("channel transport closed")}
			return
		}
		if !p.out.Scan() {
			done <- result{err: userError("channel plugin disconnected or sent an oversized frame")}
			return
		}
		var r channelproto.Response
		if json.Unmarshal(p.out.Bytes(), &r) != nil || r.JSONRPC != "2.0" || r.ID != id {
			done <- result{err: userError("invalid channel response")}
			return
		}
		done <- result{r: r}
	}()
	select {
	case <-ctx.Done():
		p.Close()
		<-done
		return ctx.Err()
	case v := <-done:
		if v.err != nil {
			p.Close()
			return v.err
		}
		if v.r.Error != nil {
			if v.r.Error.Code == -32001 {
				return channelproto.ErrAuthExpired
			}
			return fmt.Errorf("channel %s failed (code %d)", method, v.r.Error.Code)
		}
		if out != nil && json.Unmarshal(v.r.Result, out) != nil {
			return userError("invalid channel response data")
		}
		return nil
	}
}

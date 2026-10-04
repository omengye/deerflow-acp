package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

type obj = map[string]any

const nilID = "00000000-0000-0000-0000-000000000000"
const reply = "DeerFlow Desktop local smoke reply."
const maxMessage = 48 << 20

func must(err error) {
	if err != nil {
		panic(err)
	}
}
func need(ok bool, format string, args ...any) {
	if !ok {
		panic(fmt.Errorf(format, args...))
	}
}
func object(v any) obj {
	m, ok := v.(map[string]any)
	need(ok, "expected object, got %T: %v", v, v)
	return m
}
func array(v any) []any {
	if v == nil {
		return nil
	}
	a, ok := v.([]any)
	need(ok, "expected array, got %T", v)
	return a
}
func str(v any) string       { s, _ := v.(string); return s }
func jsonBytes(v any) []byte { b, e := json.Marshal(v); must(e); return b }
func clone(v obj) obj        { var m obj; must(json.Unmarshal(jsonBytes(v), &m)); return m }
func uuid() string {
	b := make([]byte, 16)
	_, e := rand.Read(b)
	must(e)
	b[6] = b[6]&15 | 64
	b[8] = b[8]&63 | 128
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// jsonProcess owns only the child it starts, with isolated environment and logs.
type jsonProcess struct {
	cmd            *exec.Cmd
	input          io.WriteCloser
	lines          chan []byte
	done           chan struct{}
	stdout, stderr *os.File
	waitErr        error
	stopRead       chan struct{}
	stopOnce       sync.Once
}

func startProcess(args, env []string, dir, logs, name string) *jsonProcess {
	p := &jsonProcess{cmd: exec.Command(args[0], args[1:]...), lines: make(chan []byte, 256), done: make(chan struct{}), stopRead: make(chan struct{})}
	p.cmd.Env = env
	p.cmd.Dir = dir
	hideWindow(p.cmd)
	var e error
	p.stderr, e = os.Create(logs + "/" + name + ".stderr.log")
	must(e)
	p.stdout, e = os.Create(logs + "/" + name + ".stdout.log")
	must(e)
	p.cmd.Stderr = p.stderr
	p.input, e = p.cmd.StdinPipe()
	must(e)
	output, e := p.cmd.StdoutPipe()
	must(e)
	if e = p.cmd.Start(); e != nil {
		p.stderr.Close()
		p.stdout.Close()
		must(e)
	}
	go func() {
		defer func() { output.Close(); close(p.lines); p.waitErr = p.cmd.Wait(); close(p.done) }()
		r := bufio.NewScanner(output)
		r.Buffer(make([]byte, 4096), maxMessage)
		for r.Scan() {
			b := append([]byte(nil), r.Bytes()...)
			p.stdout.Write(append(b, '\n'))
			select {
			case p.lines <- b:
			case <-p.stopRead:
				return
			}
		}
		if r.Err() != nil {
			p.stderr.WriteString(r.Err().Error())
		}
	}()
	return p
}
func (p *jsonProcess) send(v any) { _, e := p.input.Write(append(jsonBytes(v), '\n')); must(e) }
func (p *jsonProcess) receive(timeout time.Duration) obj {
	select {
	case b, ok := <-p.lines:
		need(ok, "%s closed stdout", p.cmd.Path)
		var m obj
		must(json.Unmarshal(bytes.TrimPrefix(b, []byte{0xef, 0xbb, 0xbf}), &m))
		return m
	case <-time.After(timeout):
		panic(fmt.Errorf("%s timed out", p.cmd.Path))
	}
}
func (p *jsonProcess) stop() {
	p.stopOnce.Do(func() { close(p.stopRead) })
	p.input.Close()
	select {
	case <-p.done:
	case <-time.After(4 * time.Second):
		p.cmd.Process.Kill()
		<-p.done
	}
	p.stderr.Close()
	p.stdout.Close()
}

type webSocket struct {
	conn   net.Conn
	reader *bufio.Reader
}

func connectWS(address string) *webSocket {
	c, e := net.DialTimeout("tcp", address, 30*time.Second)
	must(e)
	w := &webSocket{conn: c, reader: bufio.NewReader(c)}
	key := base64.StdEncoding.EncodeToString([]byte(uuid())[:16])
	must(c.SetDeadline(time.Now().Add(30 * time.Second)))
	_, e = fmt.Fprintf(c, "GET /v1 HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n\r\n", address, key)
	must(e)
	response, e := http.ReadResponse(w.reader, nil)
	must(e)
	digest := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	need(response.StatusCode == 101 && response.Header.Get("Sec-WebSocket-Accept") == base64.StdEncoding.EncodeToString(digest[:]), "invalid WebSocket upgrade")
	return w
}
func (w *webSocket) frame(op byte, b []byte) {
	need(len(b) <= maxMessage, "oversized outgoing frame")
	mask := make([]byte, 4)
	_, e := rand.Read(mask)
	must(e)
	header := []byte{0x80 | op}
	n := len(b)
	if n < 126 {
		header = append(header, 0x80|byte(n))
	} else if n < 65536 {
		header = append(header, 0xfe, byte(n>>8), byte(n))
	} else {
		header = append(header, 0xff)
		header = binary.BigEndian.AppendUint64(header, uint64(n))
	}
	header = append(header, mask...)
	for i, v := range b {
		header = append(header, v^mask[i%4])
	}
	must(w.conn.SetWriteDeadline(time.Now().Add(30 * time.Second)))
	_, e = w.conn.Write(header)
	must(e)
}
func (w *webSocket) send(v any) { w.frame(1, jsonBytes(v)) }
func (w *webSocket) receive(timeout time.Duration) obj {
	must(w.conn.SetReadDeadline(time.Now().Add(timeout)))
	var data []byte
	for {
		header := make([]byte, 2)
		_, e := io.ReadFull(w.reader, header)
		must(e)
		opcode := header[0] & 15
		need(header[0]&0x70 == 0 && header[1]&0x80 == 0, "unsupported or masked server WebSocket frame")
		n := uint64(header[1] & 127)
		if n == 126 {
			b := make([]byte, 2)
			_, e = io.ReadFull(w.reader, b)
			must(e)
			n = uint64(binary.BigEndian.Uint16(b))
		} else if n == 127 {
			b := make([]byte, 8)
			_, e = io.ReadFull(w.reader, b)
			must(e)
			n = binary.BigEndian.Uint64(b)
		}
		need(n <= maxMessage && uint64(len(data))+n <= maxMessage, "oversized incoming frame")
		payload := make([]byte, int(n))
		_, e = io.ReadFull(w.reader, payload)
		must(e)
		if opcode == 8 {
			panic(fmt.Errorf("Waku closed WebSocket"))
		}
		if opcode == 9 {
			w.frame(10, payload)
			continue
		}
		if opcode == 10 {
			continue
		}
		need(opcode == 1 || opcode == 0, "unexpected WebSocket opcode %d", opcode)
		data = append(data, payload...)
		if header[0]&0x80 != 0 {
			var m obj
			must(json.Unmarshal(data, &m))
			return m
		}
	}
}

type wakuClient struct {
	ws     *webSocket
	events []obj
	log    func(string, obj)
}

func connectWaku(ready obj, token string, log func(string, obj)) *wakuClient {
	w := &wakuClient{ws: connectWS(str(ready["address"])), log: log}
	w.ws.send(obj{"type": "hello", "protocolVersion": ready["protocolVersion"], "token": token, "clientId": uuid(), "resumeFrom": []any{}})
	hello := w.ws.receive(30 * time.Second)
	need(hello["type"] == "hello" && hello["protocolVersion"] == ready["protocolVersion"], "Waku handshake failed: %v", hello)
	log("Waku WebSocket Hello", obj{"protocol": ready["protocolVersion"]})
	return w
}
func (w *wakuClient) rpc(command obj, ids ...string) obj {
	session, runtime := nilID, nilID
	if len(ids) > 0 {
		session = ids[0]
	}
	if len(ids) > 1 {
		runtime = ids[1]
	}
	id := uuid()
	w.ws.send(obj{"type": "request", "requestId": id, "sessionId": session, "runtimeId": runtime, "command": command})
	deadline := time.Now().Add(160 * time.Second)
	for time.Now().Before(deadline) {
		m := w.ws.receive(time.Until(deadline))
		if m["type"] == "event" {
			w.events = append(w.events, m)
		}
		if m["type"] == "response" && m["requestId"] == id {
			o := object(m["outcome"])
			need(o["status"] == "ok", "Waku %s failed: %v", command["type"], o["error"])
			return object(o["payload"])
		}
	}
	panic(fmt.Errorf("Waku request timed out"))
}
func (w *wakuClient) deerflow(operation string, input ...obj) obj {
	data := obj{}
	if len(input) > 0 {
		data = input[0]
	}
	start := time.Now()
	r := w.rpc(obj{"type": "deerFlow", "operation": operation, "input": data})
	need(r["type"] == "deerFlow", "wrong DeerFlow response: %v", r)
	w.log("DeerFlow "+operation, obj{"elapsed_ms": time.Since(start).Milliseconds()})
	return object(r["data"])
}
func (w *wakuClient) event(runtime, kind string, collect *[]obj, permission func(obj)) any {
	deadline := time.Now().Add(120 * time.Second)
	for time.Now().Before(deadline) {
		var m obj
		if len(w.events) > 0 {
			m = w.events[0]
			w.events = w.events[1:]
		} else {
			m = w.ws.receive(time.Until(deadline))
		}
		if m["type"] != "event" || m["runtimeId"] != runtime {
			continue
		}
		e := object(m["event"])
		if collect != nil {
			*collect = append(*collect, e)
		}
		need(e["kind"] != "error", "ACP driver error: %v", e["payload"])
		if e["kind"] == kind {
			return e["payload"]
		}
		if e["kind"] == "permission" && permission != nil {
			permission(object(e["payload"]))
		}
		need(e["kind"] != "processExited", "ACP process exited before %s", kind)
	}
	panic(fmt.Errorf("missing Waku event %s", kind))
}

type acpClient struct {
	process    *jsonProcess
	sequence   int
	permission func(obj) string
	updates    []any
}

func (a *acpClient) rpc(method string, params obj) obj {
	a.sequence++
	id := a.sequence
	a.process.send(obj{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	for {
		m := a.process.receive(150 * time.Second)
		if m["method"] != nil && m["id"] != nil {
			if m["method"] == "session/request_permission" && a.permission != nil {
				choice := a.permission(object(m["params"]))
				a.process.send(obj{"jsonrpc": "2.0", "id": m["id"], "result": obj{"outcome": obj{"outcome": "selected", "optionId": choice}}})
			} else {
				a.process.send(obj{"jsonrpc": "2.0", "id": m["id"], "error": obj{"code": -32601, "message": "Smoke client has no tools"}})
			}
		} else if m["method"] == "session/update" {
			a.updates = append(a.updates, m["params"])
		}
		if m["id"] == float64(id) && (m["result"] != nil || m["error"] != nil) {
			need(m["error"] == nil, "ACP %s failed: %v", method, m["error"])
			return object(m["result"])
		}
	}
}
func (a *acpClient) initialize() {
	r := a.rpc("initialize", obj{"protocolVersion": 1, "clientCapabilities": obj{"terminal": false}, "clientInfo": obj{"name": "deerflow-desktop-smoke", "version": "2"}})
	need(r["protocolVersion"] == float64(1), "ACP v1 not negotiated")
}
func safe(action func()) (err error) {
	defer func() {
		if r := recover(); r != nil {
			if e, ok := r.(error); ok {
				err = e
			} else {
				err = fmt.Errorf("%v", r)
			}
		}
	}()
	action()
	return
}
func containsID(rows any, key, id string) bool {
	for _, r := range array(rows) {
		if object(r)[key] == id {
			return true
		}
	}
	return false
}
func messageText(m obj) string {
	if s, ok := m["content"].(string); ok {
		return s
	}
	var b strings.Builder
	for _, v := range array(m["content"]) {
		b.WriteString(str(object(v)["text"]))
	}
	return b.String()
}

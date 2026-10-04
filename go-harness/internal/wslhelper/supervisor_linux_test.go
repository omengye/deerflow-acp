package wslhelper

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if os.Getenv("DEERFLOW_TEST_HELPER_PROCESS") == "1" {
		os.Exit(Main(os.Args[1:]))
	}
	os.Exit(m.Run())
}

func TestLinuxHelperCleansDescendantsOnEOFAndBrokenOutput(t *testing.T) {
	for _, mode := range []string{"eof", "broken_output", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			digest, err := selfDigest()
			if err != nil {
				t.Fatal(err)
			}
			cwd := t.TempDir()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, executable, "run")
			cmd.Env = []string{"DEERFLOW_TEST_HELPER_PROCESS=1"}
			cmd.Dir = cwd
			stdin, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			defer stdin.Close()
			outR, outW, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer outR.Close()
			defer outW.Close()
			cmd.Stdout = outW
			var diagnostics bytes.Buffer
			cmd.Stderr = &diagnostics
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			outW.Close()
			script := `/bin/sleep 30 & printf '%s' "$!" > child.pid; wait`
			if mode == "broken_output" {
				script = `/bin/sleep 30 & printf '%s' "$!" > child.pid; /usr/bin/head -c 65536 /dev/zero; wait`
			}
			limit := 20 * time.Second
			if mode == "deadline" {
				limit = 500 * time.Millisecond
			}
			request := Request{Version: Version, ID: strings.Repeat("b", 32), SHA256: digest, CWD: cwd, Argv: []string{"/bin/sh", "-c", script}, Environment: map[string]string{"PATH": "/no-interpreters"}, TimeoutNanos: int64(limit)}
			if err := json.NewEncoder(stdin).Encode(request); err != nil {
				t.Fatal(err)
			}
			until := time.Now().Add(3 * time.Second)
			pid := 0
			for time.Now().Before(until) {
				data, _ := os.ReadFile(filepath.Join(cwd, "child.pid"))
				pid, _ = strconv.Atoi(string(data))
				if pid > 0 {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if pid == 0 {
				stdin.Close()
				_ = cmd.Wait()
				t.Fatalf("helper did not create fixture child: %s", diagnostics.String())
			}
			switch mode {
			case "eof":
				stdin.Close()
			case "broken_output":
				outR.Close()
			}
			err = cmd.Wait()
			if ctx.Err() != nil {
				t.Fatal("helper did not independently clean up before the watchdog")
			}
			if _, err := os.Stat(filepath.Join("/proc", strconv.Itoa(pid))); !os.IsNotExist(err) {
				t.Fatalf("helper left fixture child %d alive", pid)
			}
			if mode == "broken_output" {
				if err == nil {
					t.Fatal("broken transport claimed success")
				}
				return
			}
			if err != nil {
				t.Fatalf("helper failed: %v %s", err, diagnostics.String())
			}
			data, err := io.ReadAll(outR)
			if err != nil {
				t.Fatal(err)
			}
			if len(data) < 4 || int(binary.BigEndian.Uint32(data)) != len(data)-4 {
				t.Fatal("missing bounded terminal frame")
			}
			var frame Frame
			if Decode(data[4:], &frame) != nil || frame.Validate(request.ID) != nil || frame.Receipt == nil || !frame.Receipt.Confirmed {
				t.Fatal("invalid terminal receipt")
			}
			want := "cancelled"
			if mode == "deadline" {
				want = "timed_out"
			}
			if frame.Receipt.State != want {
				t.Fatalf("expected %s, got %+v", want, frame.Receipt)
			}
		})
	}
}

func TestLinuxHelperRejectsVersionAndDigestBeforeStartingCommand(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	digest, err := selfDigest()
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"version", "digest"} {
		t.Run(mode, func(t *testing.T) {
			cwd := t.TempDir()
			request := Request{Version: Version, ID: strings.Repeat("c", 32), SHA256: digest, CWD: cwd, TimeoutNanos: int64(time.Second), Argv: []string{"/bin/sh", "-c", "printf executed > marker"}}
			if mode == "version" {
				request.Version = "future"
			} else {
				request.SHA256 = strings.Repeat("0", 64)
			}
			data, _ := json.Marshal(request)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, executable, "run")
			cmd.Env = []string{"DEERFLOW_TEST_HELPER_PROCESS=1"}
			cmd.Stdin = bytes.NewReader(append(data, '\n'))
			if err := cmd.Run(); err == nil {
				t.Fatal("incompatible request accepted")
			}
			if _, err := os.Stat(filepath.Join(cwd, "marker")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("rejected request started a command")
			}
		})
	}
}

func TestLinuxHelperNeverSignalsNonOwnedIdentity(t *testing.T) {
	// The helper process is not its own child. A pidfd alone grants no ownership.
	if err := killOwned(os.Getpid()); err != nil {
		t.Fatal(err)
	}
}

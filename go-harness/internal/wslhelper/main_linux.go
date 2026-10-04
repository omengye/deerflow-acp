package wslhelper

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

func selfDigest() (string, error) {
	file, err := os.Open("/proc/self/exe")
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, io.LimitReader(file, 64<<20)); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func probe(cwd string) (Probe, error) {
	result := Probe{Version: Version, Arch: runtime.GOARCH}
	var err error
	result.SHA256, err = selfDigest()
	if err != nil {
		return result, err
	}
	var name unix.Utsname
	if err := unix.Uname(&name); err != nil {
		return result, err
	}
	result.Kernel = string(name.Release[:])
	result.Kernel, _, _ = strings.Cut(result.Kernel, "\x00")
	fd, err := unix.PidfdOpen(os.Getpid(), 0)
	if err != nil {
		return result, err
	}
	defer unix.Close(fd)
	if err := unix.PidfdSendSignal(fd, 0, nil, 0); err != nil {
		return result, err
	}
	result.Pidfd = true
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		return result, err
	}
	result.Subreaper = true
	if _, err := ownedChildren(); err != nil {
		return result, err
	}
	result.CWD, err = filepath.EvalSymlinks(cwd)
	if err != nil {
		return result, err
	}
	info, err := os.Stat(result.CWD)
	if err != nil || !info.IsDir() || !filepath.IsAbs(result.CWD) {
		return result, errors.New("workspace unavailable")
	}
	return result, nil
}

// Main operates as a standalone subreaper; never call it inside the daemon.
func Main(args []string) int {
	if len(args) == 2 && args[0] == "probe" {
		result, err := probe(args[1])
		if err != nil {
			fmt.Fprintln(os.Stderr, "WSL helper probe failed")
			return 1
		}
		if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
			return 1
		}
		return 0
	}
	if len(args) != 1 || args[0] != "run" {
		fmt.Fprintln(os.Stderr, "usage: deerflow-wsl-helper probe <workspace> | run")
		return 2
	}
	reader := bufio.NewReaderSize(os.Stdin, MaxRequest+1)
	line, err := reader.ReadSlice('\n')
	if err != nil || len(line) > MaxRequest {
		fmt.Fprintln(os.Stderr, "invalid WSL request size")
		return 2
	}
	var request Request
	if Decode(line, &request) != nil || request.Validate() != nil {
		fmt.Fprintln(os.Stderr, "invalid WSL request")
		return 2
	}
	status, err := probe(request.CWD)
	if err != nil || status.SHA256 != request.SHA256 || status.CWD != request.CWD {
		fmt.Fprintln(os.Stderr, "WSL helper identity or workspace changed")
		return 2
	}
	if err := unix.SetNonblock(int(os.Stdout.Fd()), true); err != nil {
		return 2
	}
	// A broken host transport must return EPIPE and allow owned-child cleanup,
	// rather than terminating this subreaper on SIGPIPE.
	sigpipe := make(chan os.Signal, 1)
	signal.Notify(sigpipe, syscall.SIGPIPE)
	defer signal.Stop(sigpipe)
	output := newFrameWriter(request.ID)
	stopped := make(chan string, 1)
	go func() {
		// EOF, malformed control, and explicit cancellation all stop the child.
		line, _ := reader.ReadSlice('\n')
		reason := "cancelled"
		if string(line) == "timed_out\n" {
			reason = "timed_out"
		}
		stopped <- reason
	}()
	receipt := supervise(request, stopped, output)
	if output.send(Frame{Kind: "receipt", Receipt: &receipt}) != nil {
		return 1
	}
	return 0
}

func commandEnvironment(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, key+"="+values[key])
	}
	return result
}

// writeTransport bounds a stalled/disconnected host without abandoning cleanup
// of guest children. Tool bytes are sent only inside typed protocol frames.
func writeTransport(packet []byte) error {
	deadline := time.Now().Add(time.Second)
	for len(packet) > 0 {
		n, err := unix.Write(int(os.Stdout.Fd()), packet)
		if n > 0 {
			packet = packet[n:]
		}
		if err == nil {
			continue
		}
		if err != unix.EAGAIN && err != unix.EINTR {
			return err
		}
		if time.Now().After(deadline) {
			return errors.New("WSL host output stalled")
		}
		_, err = unix.Poll([]unix.PollFd{{Fd: int32(os.Stdout.Fd()), Events: unix.POLLOUT}}, 20)
		if err != nil && err != unix.EINTR {
			return err
		}
	}
	return nil
}

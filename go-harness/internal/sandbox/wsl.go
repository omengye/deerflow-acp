package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/wslhelper"
)

func helperDigest(file *os.File) (string, error) {
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 || info.Size() > 64<<20 {
		return "", errors.New("invalid WSL helper file")
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, io.NewSectionReader(file, 0, info.Size())); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func (b *Backend) checkHelper() error {
	if b.helper == nil {
		return fmt.Errorf("%w: Go WSL helper is missing", harness.ErrSandboxUnavailable)
	}
	info, err := b.helper.Stat()
	current, pathErr := os.Stat(b.cfg.WSL.HelperPath)
	digest, hashErr := helperDigest(b.helper)
	if err != nil || pathErr != nil || hashErr != nil || !os.SameFile(info, current) || digest != b.cfg.WSL.HelperSHA256 {
		return fmt.Errorf("%w: Go WSL helper identity or checksum changed", harness.ErrSandboxUnavailable)
	}
	return nil
}

func (b *Backend) translateWSLPath(ctx context.Context, host string) (string, error) {
	out, err := b.control(ctx, b.wslArgs("/usr/bin/env", "-i", "PATH=/usr/bin:/bin", "wslpath", "-a", "-u", host))
	guest := strings.TrimSpace(out)
	if err != nil || !strings.HasPrefix(guest, "/") || strings.ContainsAny(guest, "\r\n\x00") {
		return "", fmt.Errorf("%w: WSL path translation failed", harness.ErrSandboxUnavailable)
	}
	return guest, nil
}

func (b *Backend) probeWSL(ctx context.Context) error {
	if strings.HasPrefix(b.cwd, `\\`) {
		return fmt.Errorf("WSL2 backend does not support UNC workspaces")
	}
	if b.cfg.WSL.HelperPath == "" {
		executable, err := os.Executable()
		if err != nil {
			return err
		}
		b.cfg.WSL.HelperPath = filepath.Join(filepath.Dir(executable), "deerflow-wsl-helper-linux-"+runtime.GOARCH)
	}
	if !filepath.IsAbs(b.cfg.WSL.HelperPath) || strings.ContainsAny(b.cfg.WSL.HelperPath, "\r\n\x00") {
		return errors.New("Go WSL helper must use an absolute host path")
	}
	if b.cfg.WSL.HelperSHA256 == "" {
		file, err := os.Open(b.cfg.WSL.HelperPath + ".sha256")
		if err != nil {
			return fmt.Errorf("%w: Go WSL helper checksum file is missing", harness.ErrSandboxUnavailable)
		}
		data, err := io.ReadAll(io.LimitReader(file, 256))
		file.Close()
		if err != nil {
			return err
		}
		b.cfg.WSL.HelperSHA256 = strings.TrimSpace(string(data))
	}
	if !wslhelper.ValidDigest(b.cfg.WSL.HelperSHA256, 32) {
		return errors.New("invalid Go WSL helper SHA-256")
	}
	file, err := os.Open(b.cfg.WSL.HelperPath)
	if err != nil {
		return fmt.Errorf("%w: Go WSL helper is missing", harness.ErrSandboxUnavailable)
	}
	b.helper = file
	ready := false
	defer func() {
		if !ready {
			file.Close()
			b.helper = nil
		}
	}()
	if err := b.checkHelper(); err != nil {
		return err
	}
	b.guestHelper, err = b.translateWSLPath(ctx, b.cfg.WSL.HelperPath)
	if err != nil {
		return err
	}
	guest, err := b.translateWSLPath(ctx, b.cwd)
	if err != nil {
		return err
	}
	out, err := b.control(ctx, b.wslArgs("/usr/bin/env", "-i", "PATH=/usr/bin:/bin", b.guestHelper, "probe", guest))
	var status wslhelper.Probe
	if err != nil || wslhelper.Decode([]byte(out), &status) != nil || status.Version != wslhelper.Version || status.SHA256 != b.cfg.WSL.HelperSHA256 || status.Arch != runtime.GOARCH || !status.Pidfd || !status.Subreaper {
		return fmt.Errorf("%w: matching Go WSL helper with Linux pidfd, subreaper and /proc support is required", harness.ErrSandboxUnavailable)
	}
	kernel := strings.ToLower(status.Kernel)
	if !strings.Contains(kernel, "microsoft-standard") && !strings.Contains(kernel, "wsl2") {
		return fmt.Errorf("%w: explicit WSL2 distribution is required", harness.ErrSandboxUnavailable)
	}
	if !strings.HasPrefix(status.CWD, "/") || strings.ContainsAny(status.CWD, "\r\n\x00") {
		return fmt.Errorf("%w: invalid Go WSL workspace", harness.ErrSandboxUnavailable)
	}
	b.guestCWD = status.CWD
	ready = true
	return nil
}

func (b *Backend) runWSL(ctx context.Context, t *task, request harness.CommandRequest) execution {
	if err := b.checkHelper(); err != nil {
		return execution{confirmed: true, notExecuted: true, state: harness.CommandFailed, err: err}
	}
	env := map[string]string{"PATH": "/usr/bin:/bin"}
	for key, value := range b.cfg.Environment {
		env[key] = value
	}
	data, err := json.Marshal(wslhelper.Request{Version: wslhelper.Version, ID: t.snapshot.ID, SHA256: b.cfg.WSL.HelperSHA256, CWD: b.guestCWD, TimeoutNanos: int64(request.Timeout), Argv: remoteCommand(b.cfg, request), Environment: env})
	if err != nil || len(data)+1 > wslhelper.MaxRequest {
		return execution{confirmed: true, notExecuted: true, state: harness.CommandFailed, err: errors.New("WSL command exceeds protocol limit")}
	}
	spec := b.processSpec(t, b.wslArgs("/usr/bin/env", "-i", "PATH=/usr/bin:/bin", b.guestHelper, "run"), true)
	spec.env, spec.initialInput = environment(nil), append(data, '\n')
	stream := &wslStream{id: t.snapshot.ID, stdout: t.stdout, stderr: t.stderr}
	transportErrors := newCapture(4096)
	result := runProcess(ctx, spec, stream, transportErrors)
	if t.view().PID == 0 {
		return result
	}
	return stream.finish(result)
}

// wslStream parses length-framed control independently of untrusted tool bytes.
// It keeps at most one bounded packet while captures retain their own limits.
type wslStream struct {
	id             string
	stdout, stderr *capture
	buffer         []byte
	failed         bool
	receipt        *wslhelper.Receipt
}

func (s *wslStream) Write(data []byte) (int, error) {
	n := len(data)
	for len(data) > 0 && !s.failed {
		if s.receipt != nil {
			s.failed = true
			break
		}
		target := 4
		if len(s.buffer) >= 4 {
			size := int(binary.BigEndian.Uint32(s.buffer[:4]))
			if size == 0 || size > wslhelper.MaxFrame {
				s.failed = true
				break
			}
			target += size
		}
		count := min(target-len(s.buffer), len(data))
		s.buffer = append(s.buffer, data[:count]...)
		data = data[count:]
		if len(s.buffer) < target || target == 4 {
			continue
		}
		var frame wslhelper.Frame
		if wslhelper.Decode(s.buffer[4:], &frame) != nil || frame.Validate(s.id) != nil {
			s.failed = true
			break
		}
		s.buffer = s.buffer[:0]
		switch frame.Kind {
		case "stdout":
			_, _ = s.stdout.Write(frame.Data)
		case "stderr":
			_, _ = s.stderr.Write(frame.Data)
		case "receipt":
			s.receipt = frame.Receipt
		}
	}
	return n, nil // Always drain the transport, including invalid output.
}

func (s *wslStream) finish(host execution) execution {
	if s.failed || len(s.buffer) != 0 || s.receipt == nil || !s.receipt.Confirmed || !host.confirmed || host.exitCode == nil || *host.exitCode != 0 {
		return execution{confirmed: false, state: harness.CommandUncertain, err: harness.ErrCommandUncertain}
	}
	result := execution{confirmed: true, state: harness.CommandState(s.receipt.State), exitCode: s.receipt.ExitCode}
	if result.state == harness.CommandCancelled {
		result.err = context.Canceled
	}
	if result.state == harness.CommandTimedOut {
		result.err = context.DeadlineExceeded
	}
	return result
}

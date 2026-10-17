package documents

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

type limitedOutput struct{ bytes.Buffer }

func (b *limitedOutput) Write(p []byte) (int, error) {
	if b.Len()+len(p) > MaxText {
		return 0, errors.New("PDF text exceeds 4 MiB")
	}
	return b.Buffer.Write(p)
}

// Poppler is optional, host-installed, and never downloaded by the agent. A
// private temporary file and argv invocation avoid shell/path interpolation.
func pdfText(ctx context.Context, data []byte) (string, error) {
	if !bytes.HasPrefix(data, []byte("%PDF-")) {
		return "", errors.New("invalid PDF header")
	}
	executable := os.Getenv("DEERFLOW_PDFTOTEXT")
	if executable == "" {
		executable = "pdftotext"
	} else if !filepath.IsAbs(executable) {
		return "", errors.New("DEERFLOW_PDFTOTEXT must be an absolute executable path")
	}
	resolved, err := exec.LookPath(executable)
	if err != nil {
		return "", errors.New("PDF requires Poppler pdftotext on PATH or DEERFLOW_PDFTOTEXT (absolute executable path)")
	}
	dir, err := os.MkdirTemp("", "deerflow-pdf-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	input := filepath.Join(dir, "input.pdf")
	if err = os.WriteFile(input, data, 0600); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, resolved, "-enc", "UTF-8", "-layout", "-f", "1", "-l", "200", input, "-")
	hideProcess(cmd)
	cmd.WaitDelay = time.Second
	var output limitedOutput
	cmd.Stdout = &output
	if err = cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", errors.New("PDF extraction failed (encrypted, malformed, or output limit exceeded)")
	}
	text := strings.ReplaceAll(output.String(), "\f", "\n")
	if !utf8.ValidString(text) {
		return "", errors.New("PDF extraction returned invalid UTF-8")
	}
	if strings.TrimSpace(text) == "" {
		return "", errors.New("PDF has no extractable text; OCR is not enabled")
	}
	return "PDF text extraction (first 200 pages maximum; no OCR):\n" + text, nil
}

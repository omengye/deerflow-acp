// deerflow-control-go invokes authenticated local host management. Requests are
// read from stdin or a JSON file; no credentials are accepted on the command line.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"unicode/utf8"

	"github.com/omengye/deerflow-acp/go-harness/internal/documents"
	"github.com/omengye/deerflow-acp/go-harness/internal/localhost"
	qrcode "github.com/skip2/go-qrcode"
)

func run(args []string, stdin io.Reader, stdout io.Writer) error {
	f := flag.NewFlagSet("deerflow-control-go", flag.ContinueOnError)
	dir := f.String("runtime-dir", "", "daemon runtime directory")
	expectedInstance := f.String("expect-instance-id", "", "require a previously observed daemon instance; fail before sending on mismatch")
	file := f.String("request", "", "JSON request file; default stdin")
	qrOutput := f.String("qr-output", "", "write login QR as a new local PNG (does not overwrite)")
	preview := f.String("preview-file", "", "extract passive preview from an explicitly selected local PDF/DOCX/XLSX; no daemon call")
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return errors.New("usage: deerflow-control-go [--runtime-dir DIR] [--request FILE]; JSON request on stdin by default")
	}
	if *preview != "" {
		if *dir != "" || *file != "" || *qrOutput != "" || *expectedInstance != "" {
			return errors.New("--preview-file cannot be combined with management options")
		}
		text, truncated, err := documentPreview(context.Background(), *preview)
		if err != nil {
			return err
		}
		return json.NewEncoder(stdout).Encode(map[string]any{"text": text, "truncated": truncated})
	}
	if *dir == "" {
		*dir = os.Getenv("DEERFLOW_GO_RUNTIME_DIR")
	}
	if *dir == "" {
		return errors.New("--runtime-dir or DEERFLOW_GO_RUNTIME_DIR is required; do not guess a profile or open its database")
	}
	source := stdin
	if *file != "" {
		input, err := os.Open(*file)
		if err != nil {
			return err
		}
		defer input.Close()
		source = input
	}
	request, err := io.ReadAll(io.LimitReader(source, 64<<10+1))
	if err != nil {
		return err
	}
	if len(request) > 64<<10 {
		return errors.New("request exceeds 64 KiB")
	}
	if *expectedInstance != "" {
		var fields map[string]json.RawMessage
		if json.Unmarshal(request, &fields) != nil || fields == nil {
			return errors.New("request must be a JSON object")
		}
		if _, exists := fields["expected_instance_id"]; exists {
			return errors.New("specify expected instance only once")
		}
		fields["expected_instance_id"], _ = json.Marshal(*expectedInstance)
		request, err = json.Marshal(fields)
		if err != nil {
			return err
		}
	}
	response, err := localhost.ManagementCall(context.Background(), *dir, request)
	if err != nil {
		return err
	}
	var value struct {
		OK bool `json:"ok"`
	}
	if err = json.Unmarshal(response, &value); err != nil {
		return err
	}
	if _, err = fmt.Fprintln(stdout, string(response)); err != nil {
		return err
	}
	if !value.OK {
		return errors.New("management operation failed (see response)")
	}
	if *qrOutput != "" {
		var payload struct {
			Data struct {
				Content string `json:"qrcode_content"`
			} `json:"data"`
		}
		if json.Unmarshal(response, &payload) != nil || payload.Data.Content == "" || len(payload.Data.Content) > 4096 {
			return errors.New("response contains no login QR content")
		}
		png, err := qrcode.Encode(payload.Data.Content, qrcode.Medium, 320)
		if err != nil {
			return err
		}
		path, err := filepath.Abs(*qrOutput)
		if err != nil {
			return err
		}
		out, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		_, writeErr := out.Write(png)
		syncErr := out.Sync()
		closeErr := out.Close()
		if err = errors.Join(writeErr, syncErr, closeErr); err != nil {
			return err
		}
	}
	return nil
}

func documentPreview(ctx context.Context, path string) (string, bool, error) {
	if !filepath.IsAbs(path) || documents.MIME(path) == "" {
		return "", false, errors.New("preview requires an absolute PDF/DOCX/XLSX file path")
	}
	f, err := os.Open(path)
	if err != nil {
		return "", false, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", false, err
	}
	if !info.Mode().IsRegular() || info.Size() > documents.MaxBytes {
		return "", false, errors.New("document preview limit is 8 MiB")
	}
	data, err := io.ReadAll(io.LimitReader(f, documents.MaxBytes+1))
	if err != nil {
		return "", false, err
	}
	doc, err := documents.Extract(ctx, filepath.Base(path), data)
	if err != nil {
		return "", false, err
	}
	text := doc.Text
	truncated := len(text) > 8192
	if truncated {
		end := 8192
		for end > 0 && !utf8.RuneStart(text[end]) {
			end--
		}
		text = text[:end]
	}
	return text, truncated, nil
}
func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

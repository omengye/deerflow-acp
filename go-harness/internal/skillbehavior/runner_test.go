package skillbehavior

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func TestOCIScriptAcceptance(t *testing.T) {
	if os.Getenv("DEERFLOW_OCI_TEST") != "1" {
		t.Skip("enable actual OCI acceptance")
	}
	if !RuntimeReady(context.Background()) {
		t.Fatal("fixed OCI image unavailable")
	}
	files := map[string][]byte{"scripts/main.py": []byte("import pathlib,socket,sys\np=pathlib.Path('result.txt');p.write_text(pathlib.Path('input.txt').read_text().upper())\nassert not pathlib.Path('/var/run/docker.sock').exists()\ntry:\n pathlib.Path('/etc/deerflow-test').write_text('x')\nexcept OSError: pass\nelse: raise Exception('root is writable')\ntry:\n socket.create_connection(('1.1.1.1',443),timeout=1)\nexcept OSError: pass\nelse: raise Exception('network is available')\nprint('isolated')\n")}
	r, err := RunScript(context.Background(), files, map[string]string{"input.txt": "expected"}, "scripts/main.py", nil)
	if err != nil || r.ExitCode != 0 || r.Files["result.txt"] != "EXPECTED" || !strings.Contains(r.Stdout, "isolated") || r.Runtime != RuntimeVersion || len(r.Digest) != 64 {
		t.Fatalf("execution: %+v, %v", r, err)
	}
	files["scripts/main.py"] = []byte("raise RuntimeError('intentional')")
	r, err = RunScript(context.Background(), files, map[string]string{}, "scripts/main.py", nil)
	if err != nil || r.ExitCode == 0 {
		t.Fatalf("failure incorrectly passed: %v, %v", r.ExitCode, err)
	}
	files["scripts/main.py"] = []byte("import time; time.sleep(120)")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err = RunScript(ctx, files, map[string]string{}, "scripts/main.py", nil)
	if err == nil {
		t.Fatal("timeout passed")
	}
}

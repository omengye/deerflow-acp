package skillbehavior

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

// OCIImage is an immutable linux/amd64 manifest, installed by the desktop
// provisioner. Execution never pulls an unreviewed tag or mounts host paths.
const OCIImage = "sha256:05a994d2344eb364cc186910f4fc8786a38215f39f00d0acc25bff33a08f4177"
const RuntimeVersion = "oci-python312-v1/" + OCIImage

var ErrRuntimeUnavailable = errors.New("isolated skill runtime unavailable")

type ScriptResult struct {
	Path     string            `json:"path"`
	Runtime  string            `json:"runtime"`
	ExitCode int               `json:"exit_code"`
	Stdout   string            `json:"stdout"`
	Stderr   string            `json:"stderr"`
	Files    map[string]string `json:"files"`
	Digest   string            `json:"digest"`
}

type cappedOutput struct {
	bytes.Buffer
	overflow bool
}

func (b *cappedOutput) Write(p []byte) (int, error) {
	n := len(p)
	left := (1 << 20) - b.Len()
	if len(p) > left {
		b.overflow = true
		p = p[:max(left, 0)]
	}
	_, _ = b.Buffer.Write(p)
	return n, nil
}

func RuntimeReady(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "image", "inspect", OCIImage, "--format", "{{.Id}}")
	out, err := cmd.Output()
	return err == nil && strings.Contains(string(out), "sha256:")
}

func RunScript(parent context.Context, files map[string][]byte, fixtures map[string]string, path string, args []string) (ScriptResult, error) {
	var result ScriptResult
	if !ValidPath(path) || len(args) > 32 || files[path] == nil {
		return result, harness.ErrInvalidInput
	}
	if !strings.HasSuffix(path, ".py") && !strings.HasSuffix(path, ".sh") {
		return result, ErrRuntimeUnavailable
	}
	for _, a := range args {
		if !Text(a, 4096) {
			return result, harness.ErrInvalidInput
		}
	}
	for p, v := range files {
		if !ValidPath(p) || !Text(string(v), 512<<10) {
			return result, harness.ErrInvalidInput
		}
	}
	for p, v := range fixtures {
		if !ValidPath(p) || !Text(v, 65536) {
			return result, harness.ErrInvalidInput
		}
	}
	input, err := json.Marshal(struct {
		Files    map[string][]byte `json:"files"`
		Fixtures map[string]string `json:"fixtures"`
		Path     string            `json:"path"`
		Args     []string          `json:"args"`
	}{files, fixtures, path, args})
	if err != nil || len(input) > 4<<20 {
		return result, harness.ErrInvalidInput
	}
	ctx, cancel := context.WithTimeout(parent, 40*time.Second)
	defer cancel()
	name := "deerflow-eval-" + strings.ToLower(rand.Text())
	cmd := exec.CommandContext(ctx, "docker", "run", "--rm", "--pull=never", "--name", name, "--network=none", "--read-only", "--cap-drop=ALL", "--security-opt=no-new-privileges", "--user=65534:65534", "--pids-limit=64", "--memory=256m", "--memory-swap=256m", "--cpus=1", "--tmpfs=/work:rw,nosuid,nodev,size=32m,mode=1777", "--tmpfs=/tmp:rw,nosuid,nodev,size=8m,mode=1777", "--workdir=/work", "--env=PYTHONDONTWRITEBYTECODE=1", "-i", OCIImage, "python", "-I", "-c", scriptDriver)
	cmd.Stdin = bytes.NewReader(input)
	var stdout, stderr cappedOutput
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	// Even cancellation of the Docker client must remove its container. Verify
	// absence; a cleanup failure cannot produce successful evaluation evidence.
	cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
	defer stop()
	_ = exec.CommandContext(cleanup, "docker", "rm", "-f", name).Run()
	remaining, inspectErr := exec.CommandContext(cleanup, "docker", "container", "ls", "-a", "--filter", "name=^/"+name+"$", "--format", "{{.Names}}").Output()
	if inspectErr != nil || strings.TrimSpace(string(remaining)) != "" || cleanup.Err() != nil {
		return result, errors.New("isolated runtime cleanup failed")
	}
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	if runErr != nil || stdout.overflow || stderr.overflow {
		return result, ErrRuntimeUnavailable
	}
	if json.Unmarshal(stdout.Bytes(), &result) != nil {
		return result, errors.New("invalid isolated process result")
	}
	if result.ExitCode == -127 {
		return result, ErrRuntimeUnavailable
	}
	if result.Path != path || len(result.Stdout) > 65536 || len(result.Stderr) > 65536 || len(result.Files) > 256 {
		return result, errors.New("invalid isolated process output")
	}
	for p, v := range result.Files {
		if !ValidPath(p) || !Text(v, 65536) {
			return result, errors.New("invalid isolated artifact")
		}
	}
	result.Runtime = RuntimeVersion
	result.Digest = ""
	canonical, _ := json.Marshal(result)
	result.Digest = Hash(canonical)
	return result, nil
}

// No assertions, answers, credentials, sockets or host paths enter the guest.
// File output and process status are inspected by the host after execution.
const scriptDriver = `import os,sys,json,base64,subprocess,pathlib,signal,resource
r=json.load(sys.stdin)
root=pathlib.Path('/work'); pkg=root/'skill'; work=root/'task'
pkg.mkdir();work.mkdir()
for p,b in r['files'].items():
 f=pkg/p; f.parent.mkdir(parents=True,exist_ok=True);f.write_bytes(base64.b64decode(b));f.chmod(0o444)
for p,b in r['fixtures'].items():
 f=work/p; f.parent.mkdir(parents=True,exist_ok=True);f.write_text(b)
script=pkg/r['path']; interpreter='python' if str(script).endswith('.py') else '/bin/sh'
def limits():
 resource.setrlimit(resource.RLIMIT_FSIZE,(1048576,1048576))
 resource.setrlimit(resource.RLIMIT_NOFILE,(64,64))
 resource.setrlimit(resource.RLIMIT_CPU,(20,20))
out=open('/tmp/stdout','wb');err=open('/tmp/stderr','wb')
p=subprocess.Popen([interpreter,str(script)]+(r['args'] or []),cwd=work,stdin=subprocess.DEVNULL,stdout=out,stderr=err,env={'PATH':'/usr/local/bin:/usr/bin:/bin','PYTHONDONTWRITEBYTECODE':'1','HOME':'/tmp'},start_new_session=True,preexec_fn=limits)
try: code=p.wait(timeout=25)
except subprocess.TimeoutExpired:
 os.killpg(p.pid,signal.SIGKILL); p.wait();code=-124
out.close();err.close()
stdout=pathlib.Path('/tmp/stdout').read_bytes()[:65536].decode('utf-8','replace');stderr=pathlib.Path('/tmp/stderr').read_bytes()[:65536].decode('utf-8','replace')
if 'ModuleNotFoundError:' in stderr: code=-127
files={};total=0
for f in work.rglob('*'):
 if f.is_symlink(): code=-126;break
 if not f.is_file():continue
 if f.stat().st_size>65536:code=-125;break
 b=f.read_text(errors='replace');total+=len(b.encode())
 if total>524288 or len(files)>=256:code=-125;break
 files[f.relative_to(work).as_posix()]=b
print(json.dumps({'path':r['path'],'exit_code':code,'stdout':stdout,'stderr':stderr,'files':files}))`

var _ io.Writer = (*cappedOutput)(nil)

func HasScripts(files map[string][]byte) bool {
	for p := range files {
		if strings.HasPrefix(p, "scripts/") || strings.HasSuffix(p, ".py") || strings.HasSuffix(p, ".sh") || strings.HasSuffix(p, ".js") || strings.HasSuffix(p, ".ps1") {
			return true
		}
	}
	return false
}

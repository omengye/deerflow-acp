"""Explicit SSH operations for an existing Anthropic Sandbox Runtime host."""

import argparse
import base64
import json
import os
from pathlib import Path, PurePosixPath
import re
import shlex
import sys
import time

POLICY_PATH = "/root/.srt-settings.json"
OUTPUT_LIMIT = 1024 * 1024

# Data travels as encoded JSON, never interpolated into Python or shell syntax.
POLICY_UPDATE = r'''
import base64,fcntl,hashlib,json,os,secrets,stat,sys,tempfile
payload=json.loads(base64.b64decode(sys.argv[1])); path=payload['path']
def unique(pairs):
 data={}
 for key,value in pairs:
  if key in data: raise ValueError('duplicate policy key')
  data[key]=value
 return data
lock=os.open(path+'.lock',os.O_CREAT|os.O_RDWR|os.O_NOFOLLOW,0o600)
with os.fdopen(lock,'a') as lease:
 fcntl.flock(lease,fcntl.LOCK_EX)
 fd=os.open(path,os.O_RDONLY|os.O_NOFOLLOW)
 with os.fdopen(fd,'rb') as source:
  info=os.fstat(source.fileno())
  if not stat.S_ISREG(info.st_mode): raise ValueError('policy is not a regular file')
  raw=source.read(1048577)
 if len(raw)>1048576: raise ValueError('policy exceeds size limit')
 data=json.loads(raw,object_pairs_hook=unique)
 if not isinstance(data,dict): raise ValueError('policy must be an object')
 section,key=('network','allowedDomains') if payload['kind']=='domain' else ('filesystem','allowWrite')
 if not isinstance(data.get(section),dict): raise ValueError('missing policy section')
 values=data[section].get(key,[])
 if not isinstance(values,list) or any(not isinstance(v,str) for v in values): raise ValueError('invalid policy list')
 value=payload['value']
 if value in values:
  print(json.dumps({'changed':False})); sys.exit(0)
 data[section][key]=values+[value]
 updated=(json.dumps(data,indent=2,ensure_ascii=False)+'\n').encode('utf-8')
 backup=path+'.bak-'+secrets.token_hex(8)
 with open(backup,'xb') as out:
  os.fchmod(out.fileno(),0o600);out.write(raw);out.flush();os.fsync(out.fileno())
 fd,tmp=tempfile.mkstemp(prefix='.srt-policy-',dir=os.path.dirname(path))
 try:
  with os.fdopen(fd,'wb') as out:
   os.fchmod(out.fileno(),stat.S_IMODE(info.st_mode));os.fchown(out.fileno(),info.st_uid,info.st_gid)
   out.write(updated);out.flush();os.fsync(out.fileno())
  verify=os.open(path,os.O_RDONLY|os.O_NOFOLLOW)
  with os.fdopen(verify,'rb') as check:
   current=os.fstat(check.fileno());unchanged=check.read(1048577)==raw and current.st_ino==info.st_ino and current.st_dev==info.st_dev
  if not unchanged: raise ValueError('policy changed concurrently; refusing overwrite')
  os.replace(tmp,path)
  parent=os.open(os.path.dirname(path),os.O_DIRECTORY)
  try: os.fsync(parent)
  finally: os.close(parent)
 finally:
  if os.path.exists(tmp): os.unlink(tmp)
 print(json.dumps({'changed':True,'backup':backup,'sha256':hashlib.sha256(updated).hexdigest()}))
'''


def validate_domain(value):
    if len(value) > 253 or not re.fullmatch(r"[A-Za-z0-9](?:[A-Za-z0-9.-]*[A-Za-z0-9])?", value):
        raise ValueError("Use an exact DNS name, without a URL, wildcard or port")
    if any(not re.fullmatch(r"[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?", label) for label in value.split(".")):
        raise ValueError("Invalid DNS label")
    return value.lower()


def validate_write_path(value):
    path = PurePosixPath(value)
    if not value.startswith("/") or value == "/" or ".." in path.parts or any(ord(c) < 32 for c in value):
        raise ValueError("Use a specific absolute Linux directory without traversal")
    return value


def policy_command(kind, value):
    payload = base64.b64encode(json.dumps({"path": POLICY_PATH, "kind": kind, "value": value}).encode()).decode()
    return shlex.join(["python3", "-I", "-c", POLICY_UPDATE, payload])


def sandbox_command(command, settings=None):
    if not command or any("\x00" in arg for arg in command):
        raise ValueError("Provide a nonempty command argument list")
    argv = ["srt"]
    if settings:
        if not settings.startswith("/") or any(ord(c) < 32 for c in settings):
            raise ValueError("Settings must be an absolute Linux path")
        argv.extend(["-s", settings])
    argv.extend(["-c", shlex.join(command)])
    return shlex.join(argv)


def connect(args):
    import paramiko

    client = paramiko.SSHClient()
    try:
        # No TOFU or automatic enrollment. Verify host identity out of band.
        client.load_host_keys(str(Path(args.known_hosts).expanduser()))
        client.set_missing_host_key_policy(paramiko.RejectPolicy())
        client.connect(hostname=args.host, port=args.port, username=args.user,
                       key_filename=str(Path(args.key).expanduser()),
                       look_for_keys=False, allow_agent=False,
                       timeout=15, auth_timeout=15, banner_timeout=15)
        return client
    except BaseException:
        client.close()
        raise


def remote(args, command):
    client = connect(args)
    channel = None
    try:
        transport = client.get_transport()
        channel = transport.open_session(timeout=15)
        channel.settimeout(15)
        channel.exec_command(command)
        channel.shutdown_write()
        deadline = time.monotonic() + args.timeout
        output, errors, total = bytearray(), bytearray(), 0
        while True:
            for ready, receive, buffer in ((channel.recv_ready, channel.recv, output),
                                            (channel.recv_stderr_ready, channel.recv_stderr, errors)):
                if ready():
                    chunk = receive(32768)
                    total += len(chunk)
                    if total > OUTPUT_LIMIT:
                        raise RuntimeError("Remote output limit reached; remote termination is unconfirmed; do not retry")
                    buffer.extend(chunk)
            if channel.exit_status_ready() and not channel.recv_ready() and not channel.recv_stderr_ready():
                code = channel.recv_exit_status()
                if code < 0:
                    raise RuntimeError("Remote exit status unavailable; do not retry")
                return code, output.decode("utf-8", "replace"), errors.decode("utf-8", "replace")
            if time.monotonic() >= deadline:
                raise TimeoutError("Remote deadline reached; remote termination is unconfirmed; do not retry")
            time.sleep(0.01)
    finally:
        if channel is not None:
            channel.close()
        client.close()


def parser():
    result = argparse.ArgumentParser(description=__doc__)
    result.add_argument("--host", default="1.92.70.163")
    result.add_argument("--port", type=int, default=2233)
    result.add_argument("--user", default="root")
    result.add_argument("--key", default="~/Documents/ssh_info/huaweicloud_ed25519")
    result.add_argument("--known-hosts", default="~/.ssh/known_hosts")
    result.add_argument("--timeout", type=int, default=60)
    commands = result.add_subparsers(dest="action", required=True)
    commands.add_parser("status")
    commands.add_parser("get-config")
    run = commands.add_parser("run")
    run.add_argument("-s", "--settings")
    run.add_argument("command", nargs=argparse.REMAINDER)
    for name, key in (("allow-domain", "domain"), ("allow-write", "path")):
        item = commands.add_parser(name)
        item.add_argument(key)
        item.add_argument("--confirm-policy-change", action="store_true", required=True)
    return result


def main(argv=None):
    args = parser().parse_args(argv)
    if not 1 <= args.port <= 65535 or not 1 <= args.timeout <= 300:
        raise ValueError("Invalid port or timeout")
    if args.action == "status":
        command = "srt --version && node --version && bwrap --version && command -v socat && test -r /root/.srt-settings.json"
    elif args.action == "get-config":
        command = shlex.join(["cat", "--", POLICY_PATH])
    elif args.action == "run":
        values = args.command[1:] if args.command[:1] == ["--"] else args.command
        command = sandbox_command(values, args.settings)
    elif args.action == "allow-domain":
        command = policy_command("domain", validate_domain(args.domain))
    else:
        command = policy_command("write", validate_write_path(args.path))
    code, output, errors = remote(args, command)
    print(output, end="")
    print(errors, end="", file=sys.stderr)
    return code


if __name__ == "__main__":
    try:
        sys.exit(main())
    except (ValueError, RuntimeError, TimeoutError) as error:
        print(str(error), file=sys.stderr)
        sys.exit(2)
    except Exception as error:
        print("SSH/runtime failure (" + type(error).__name__ + "); no automatic retry. Verify dependencies, host identity and credentials locally.", file=sys.stderr)
        sys.exit(3)

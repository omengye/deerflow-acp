#!/usr/bin/env python3
# -*- coding: utf-8 -*-
r"""华为云 Linux 主机管理客户端（Paramiko 实现）

主机配置：
  - Host: 1.92.70.163
  - Port: 2233
  - User: root
  - Key:  <UserHome>/Documents/ssh_info/huaweicloud_ed25519 (保存在用户目录，严禁放入 skill 目录)
          或通过环境变量 HUAWEICLOUD_SSH_KEY 指定
"""

import argparse
import os
import sys
from pathlib import Path

try:
    import paramiko
except ImportError:
    print("错误: 缺少依赖库 paramiko，请先运行: pip install paramiko", file=sys.stderr)
    sys.exit(1)

# 连接配置
DEFAULT_HOST = os.environ.get("HUAWEICLOUD_SSH_HOST", "1.92.70.163")
DEFAULT_PORT = int(os.environ.get("HUAWEICLOUD_SSH_PORT", "2233"))
DEFAULT_USER = os.environ.get("HUAWEICLOUD_SSH_USER", "root")

def detect_user_home() -> Path:
    for env in ("USERPROFILE", "HOME"):
        val = os.environ.get(env)
        if val and Path(val).exists():
            return Path(val)
    return Path.home()

def get_default_key_path() -> Path:
    env_key = os.environ.get("HUAWEICLOUD_SSH_KEY")
    if env_key:
        return Path(env_key)
    return detect_user_home() / "Documents" / "ssh_info" / "huaweicloud_ed25519"

def get_default_known_hosts_path() -> Path:
    return Path(os.environ.get("HUAWEICLOUD_SSH_KNOWN_HOSTS", "~/.ssh/known_hosts")).expanduser()


def get_client(key_path: Path = None, host: str = DEFAULT_HOST, port: int = DEFAULT_PORT,
               user: str = DEFAULT_USER, known_hosts: Path = None) -> paramiko.SSHClient:
    if key_path is None:
        key_path = get_default_key_path()

    key_path = Path(key_path).expanduser()
    known_hosts = Path(known_hosts).expanduser() if known_hosts is not None else get_default_known_hosts_path()
    if not key_path.is_file():
        print(f"错误: 找不到 SSH 密钥文件: {key_path}", file=sys.stderr)
        print("请确认密钥文件保存在当前用户目录: Documents/ssh_info/huaweicloud_ed25519", file=sys.stderr)
        print("或设置环境变量 HUAWEICLOUD_SSH_KEY", file=sys.stderr)
        sys.exit(2)

    if not known_hosts.is_file():
        print(f"错误: 找不到已验证的 SSH known_hosts 文件: {known_hosts}", file=sys.stderr)
        print("请通过 --known-hosts 或 HUAWEICLOUD_SSH_KNOWN_HOSTS 指定，不自动信任新主机。", file=sys.stderr)
        sys.exit(2)

    ssh = paramiko.SSHClient()
    try:
        ssh.load_host_keys(str(known_hosts))
        ssh.set_missing_host_key_policy(paramiko.RejectPolicy())
    except Exception as e:
        ssh.close()
        print(f"读取 SSH known_hosts 失败: {type(e).__name__}", file=sys.stderr)
        sys.exit(2)

    try:
        key = paramiko.Ed25519Key.from_private_key_file(str(key_path))
    except Exception as e:
        try:
            key = paramiko.RSAKey.from_private_key_file(str(key_path))
        except Exception:
            ssh.close()
            print(f"解析私钥文件失败 ({key_path}): {type(e).__name__}", file=sys.stderr)
            sys.exit(2)

    try:
        ssh.connect(
            hostname=host,
            port=port,
            username=user,
            pkey=key,
            timeout=15,
            auth_timeout=15,
            banner_timeout=15,
            look_for_keys=False,
            allow_agent=False
        )
    except Exception as e:
        ssh.close()
        print(f"连接服务器失败 ({user}@{host}:{port}): {e}", file=sys.stderr)
        sys.exit(3)

    return ssh

def client_for(args):
    return get_client(key_path=args.key, host=args.host, port=args.port,
                      user=args.user, known_hosts=args.known_hosts)


def cmd_status(args):
    """查看主机概况与运行状态"""
    ssh = client_for(args)
    script = (
        "echo '=== [系统版本] ===' && cat /etc/os-release | grep PRETTY_NAME && "
        "echo '=== [运行时间与负载] ===' && uptime && "
        "echo '=== [内存状态] ===' && free -h && "
        "echo '=== [磁盘状态] ===' && df -h / && "
        "echo '=== [网络接口] ===' && ip -br a"
    )
    stdin, stdout, stderr = ssh.exec_command(script)
    exit_code = stdout.channel.recv_exit_status()
    out = stdout.read().decode('utf-8', errors='replace')
    err = stderr.read().decode('utf-8', errors='replace')
    if out:
        print(out.strip())
    if err:
        print("\nStderr:\n" + err.strip(), file=sys.stderr)
    ssh.close()
    return exit_code

def cmd_exec(args):
    """执行任意 shell 命令"""
    ssh = client_for(args)
    command = " ".join(args.command)
    stdin, stdout, stderr = ssh.exec_command(command)
    exit_code = stdout.channel.recv_exit_status()
    out = stdout.read().decode('utf-8', errors='replace')
    err = stderr.read().decode('utf-8', errors='replace')
    if out:
        print(out, end='')
    if err:
        print(err, end='', file=sys.stderr)
    ssh.close()
    return exit_code

def cmd_upload(args):
    """上传本地文件到远程"""
    local_path = Path(args.local).resolve()
    if not local_path.is_file():
        print(f"本地文件不存在: {local_path}", file=sys.stderr)
        return 1

    ssh = client_for(args)
    sftp = ssh.open_sftp()
    remote_path = args.remote
    print(f"正在上传 {local_path} -> {remote_path} ...")
    try:
        sftp.put(str(local_path), remote_path)
        print("上传成功！")
        return 0
    except Exception as e:
        print(f"上传失败: {e}", file=sys.stderr)
        return 1
    finally:
        sftp.close()
        ssh.close()

def cmd_download(args):
    """从远程下载文件到本地"""
    remote_path = args.remote
    local_path = Path(args.local).resolve()
    local_path.parent.mkdir(parents=True, exist_ok=True)

    ssh = client_for(args)
    sftp = ssh.open_sftp()
    print(f"正在下载 {remote_path} -> {local_path} ...")
    try:
        sftp.get(remote_path, str(local_path))
        print("下载成功！")
        return 0
    except Exception as e:
        print(f"下载失败: {e}", file=sys.stderr)
        return 1
    finally:
        sftp.close()
        ssh.close()

def parser():
    parser = argparse.ArgumentParser(description="华为云 Linux ECS 管理工具")
    parser.add_argument("--key", type=Path, default=get_default_key_path(), help="外部 SSH 私钥路径")
    parser.add_argument("--known-hosts", type=Path, default=get_default_known_hosts_path(), help="已单独验证的 known_hosts 文件")
    parser.add_argument("--host", default=DEFAULT_HOST)
    parser.add_argument("--port", type=int, default=DEFAULT_PORT)
    parser.add_argument("--user", default=DEFAULT_USER)
    subparsers = parser.add_subparsers(dest="action", required=True)

    # status
    subparsers.add_parser("status", help="检查主机连通性及资源状态")

    # exec
    p_exec = subparsers.add_parser("exec", help="在远程主机上执行 shell 命令")
    p_exec.add_argument("command", nargs=argparse.REMAINDER, help="Shell 命令及参数")

    # upload
    p_upload = subparsers.add_parser("upload", help="上传文件到远程服务器")
    p_upload.add_argument("local", help="本地文件路径")
    p_upload.add_argument("remote", help="远程目标路径")

    # download
    p_download = subparsers.add_parser("download", help="从远程服务器下载文件")
    p_download.add_argument("remote", help="远程文件路径")
    p_download.add_argument("local", help="本地保存路径")

    return parser


def main(argv=None):
    arguments = parser()
    args = arguments.parse_args(argv)
    if not 1 <= args.port <= 65535:
        arguments.error("SSH port must be between 1 and 65535")
    if args.action == "exec":
        args.command = args.command[1:] if args.command[:1] == ["--"] else args.command
        if not args.command:
            arguments.error("exec requires a remote command")

    actions = {
        "status": cmd_status,
        "exec": cmd_exec,
        "upload": cmd_upload,
        "download": cmd_download
    }

    exit_code = actions[args.action](args)
    return exit_code

if __name__ == "__main__":
    sys.exit(main())

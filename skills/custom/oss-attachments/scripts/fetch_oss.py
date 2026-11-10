"""Download the App's signed, bounded inputs. Never receives account secrets."""
import argparse
import datetime as dt
import hashlib
import json
import os
from pathlib import Path
import re
import stat
import sys
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request

HOST = "s3.cstcloud.cn"
BUCKET = "52e24feeab1948d3a16697f4ccb0d814"
PREFIX = "deerflow-app/attachments/"
MAX_FILE = 50 * 1024 * 1024
MAX_TOTAL = 100 * 1024 * 1024
MAX_MANIFEST = 64 * 1024


class AttachmentError(Exception):
    def __init__(self, code):
        self.code = code
        super().__init__(code)


def require(condition, code="invalid_manifest"):
    if not condition:
        raise AttachmentError(code)


def ordinary(path):
    info = path.lstat()
    require(not stat.S_ISLNK(info.st_mode) and not (getattr(info, "st_file_attributes", 0) & 0x400), "unsafe_path")


def within(workspace, relative, create_parents=False):
    require(isinstance(relative, str) and 0 < len(relative) <= 4096, "unsafe_path")
    value = Path(relative)
    require(not value.is_absolute() and ".." not in value.parts and not any(":" in p for p in value.parts), "unsafe_path")
    current = workspace
    for index, part in enumerate(value.parts):
        current = current / part
        if current.exists() or current.is_symlink():
            ordinary(current)
        elif create_parents and index < len(value.parts) - 1:
            current.mkdir(mode=0o700)
        require(current.resolve().is_relative_to(workspace), "unsafe_path")
    return current


def validate(item, now=None):
    require(isinstance(item, dict))
    name, mime, size, digest = (item.get(k) for k in ("name", "mimeType", "size", "sha256"))
    require(isinstance(name, str) and 0 < len(name) <= 1024 and not any(ord(c) < 32 for c in name))
    require(isinstance(mime, str) and re.fullmatch(r"[\w.+-]+/[\w.+-]+", mime) is not None)
    maximum = 20 * 1024 * 1024 if mime.startswith("image/") else MAX_FILE
    require(type(size) is int and 0 <= size <= maximum, "size_limit")
    require(isinstance(digest, str) and re.fullmatch(r"[a-f0-9]{64}", digest) is not None)
    url = item.get("url")
    require(isinstance(url, str) and len(url) <= 16384 and not any(ord(c) < 32 for c in url))
    parsed = urllib.parse.urlsplit(url)
    require(parsed.scheme == "https" and parsed.hostname == HOST and parsed.port in (None, 443)
            and not parsed.username and not parsed.password and not parsed.fragment, "unapproved_source")
    path = urllib.parse.unquote(parsed.path, errors="strict")
    require(path.startswith(f"/{BUCKET}/{PREFIX}") and not any(p in ("", ".", "..") for p in path[1:].split("/")))
    require("\\" not in path and not any(ord(c) < 32 for c in path), "unsafe_path")
    require(item.get("id") == path[len(BUCKET) + 2:])
    query_pairs = urllib.parse.parse_qsl(parsed.query, keep_blank_values=True, strict_parsing=True)
    query = dict(query_pairs)
    require(len(query) == len(query_pairs) and query.get("X-Amz-Algorithm") == "AWS4-HMAC-SHA256"
            and query.get("X-Amz-SignedHeaders") == "host"
            and re.fullmatch(r"[a-f0-9]{64}", query.get("X-Amz-Signature", "")) is not None)
    signed = dt.datetime.strptime(query["X-Amz-Date"], "%Y%m%dT%H%M%SZ").replace(tzinfo=dt.timezone.utc)
    expires = int(query["X-Amz-Expires"])
    require(1 <= expires <= 604800)
    now = now or dt.datetime.now(dt.timezone.utc)
    require(now < signed + dt.timedelta(seconds=expires), "link_expired")
    require(signed <= now + dt.timedelta(minutes=15), "invalid_signature_time")
    require(item.get("headers") == {"User-Agent": "S3Drive"}, "unapproved_headers")
    return item


def read_manifest(workspace, relative):
    path = within(workspace, relative)
    require(path.is_file() and path.stat().st_size <= MAX_MANIFEST)
    with path.open("rb") as stream:
        raw = stream.read(MAX_MANIFEST + 1)
    require(len(raw) <= MAX_MANIFEST)
    text = raw.decode("utf-8-sig")
    start, end = "[DEERFLOW_OSS_ATTACHMENTS_V1]", "[/DEERFLOW_OSS_ATTACHMENTS_V1]"
    if start in text:
        require(text.count(start) == 1 and text.count(end) == 1)
        text = text.split(start, 1)[1].split(end, 1)[0]
    manifest = json.loads(text)
    require(isinstance(manifest, dict) and type(manifest.get("version")) is int and manifest["version"] == 1)
    items = manifest.get("attachments")
    require(isinstance(items, list) and 1 <= len(items) <= 8)
    # Validate all URLs and budgets before making any request.
    files = [validate(item) for item in items]
    require(sum(item["size"] for item in files) <= MAX_TOTAL, "size_limit")
    require(len({item["id"] for item in files}) == len(files))
    return files


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        raise AttachmentError("redirect_blocked")


def file_hash(path):
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(64 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def fetch(item, workspace, opener=None, deadline=None):
    deadline = deadline or (time.monotonic() + 240)
    basename = item["name"].replace("\\", "/").rsplit("/", 1)[-1]
    basename = re.sub(r"[^\w. -]", "_", basename).strip(" .")[:120] or "attachment"
    while len(basename.encode("utf-8")) > 170:
        basename = basename[:-1]
    relative = ".deerflow/inputs/oss/" + item["sha256"] + "-" + basename
    target = within(workspace, relative, create_parents=True)
    if target.is_file() and target.stat().st_size == item["size"] and file_hash(target) == item["sha256"]:
        return {"ok": True, "name": item["name"], "mimeType": item["mimeType"], "size": item["size"], "path": relative, "cached": True}
    opener = opener or urllib.request.build_opener(NoRedirect())
    request = urllib.request.Request(item["url"], headers=item["headers"], method="GET")
    temporary = None
    try:
        require(time.monotonic() < deadline, "network_error")
        with opener.open(request, timeout=min(30, max(1, deadline - time.monotonic()))) as response:
            require(response.status == 200, "download_denied")
            length = response.headers.get("Content-Length")
            require(length is None or int(length) == item["size"], "size_mismatch")
            descriptor, temporary = tempfile.mkstemp(prefix="download-", suffix=".part", dir=target.parent)
            digest, count = hashlib.sha256(), 0
            with os.fdopen(descriptor, "wb") as output:
                while True:
                    require(time.monotonic() < deadline, "network_error")
                    chunk = response.read(min(64 * 1024, item["size"] - count + 1))
                    if not chunk:
                        break
                    count += len(chunk)
                    require(count <= item["size"], "size_mismatch")
                    digest.update(chunk)
                    output.write(chunk)
                require(count == item["size"] and digest.hexdigest() == item["sha256"], "integrity_failed")
                output.flush()
                os.fsync(output.fileno())
            within(workspace, relative)
            os.replace(temporary, target)
        return {"ok": True, "name": item["name"], "mimeType": item["mimeType"], "size": item["size"], "path": relative, "cached": False}
    except urllib.error.HTTPError as error:
        raise AttachmentError("download_denied" if error.code in (401, 403, 404) else "network_error") from None
    except (OSError, urllib.error.URLError, TimeoutError):
        raise AttachmentError("network_error") from None
    finally:
        if temporary and os.path.exists(temporary):
            os.unlink(temporary)


def main():
    parser = argparse.ArgumentParser(description="Download App OSS inputs into the current workspace")
    parser.add_argument("--manifest", required=True)
    args = parser.parse_args()
    workspace = Path.cwd().resolve()
    try:
        items = read_manifest(workspace, args.manifest)
        deadline = time.monotonic() + 240
        results = []
        for item in items:
            try:
                results.append(fetch(item, workspace, deadline=deadline))
            except AttachmentError as error:
                results.append({"ok": False, "name": item["name"], "code": error.code})
        result = {"ok": all(item["ok"] for item in results), "files": results}
    except AttachmentError as error:
        result = {"ok": False, "code": error.code}
    except Exception:
        # HTTP/JSON/URL errors may contain a signed URL. Never print their text.
        result = {"ok": False, "code": "invalid_manifest"}
    if hasattr(sys.stdout, "reconfigure"):
        sys.stdout.reconfigure(encoding="utf-8")
    print(json.dumps(result, ensure_ascii=False))
    return 0 if result["ok"] else 1


if __name__ == "__main__":
    sys.exit(main())

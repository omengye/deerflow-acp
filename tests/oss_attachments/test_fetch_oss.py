import datetime as dt
import hashlib
import importlib.util
import io
import json
from pathlib import Path
import tempfile
import unittest
import urllib.error
from unittest.mock import patch

SOURCE = Path(__file__).resolve().parents[2] / "skills/custom/oss-attachments/scripts/fetch_oss.py"
spec = importlib.util.spec_from_file_location("fetch_oss", SOURCE)
fetch_oss = importlib.util.module_from_spec(spec)
spec.loader.exec_module(fetch_oss)


class Response(io.BytesIO):
    status = 200
    def __init__(self, body, length=None):
        super().__init__(body)
        self.headers = {} if length is None else {"Content-Length": str(length)}


class Opener:
    def __init__(self, body):
        self.body = body
        self.requests = []
    def open(self, request, timeout):
        self.requests.append(request)
        return Response(self.body)


def item(body=b"actual input bytes", **overrides):
    stamp = dt.datetime.now(dt.timezone.utc).strftime("%Y%m%dT%H%M%SZ")
    key = "deerflow-app/attachments/test/input.txt"
    value = {
        "id": key, "name": "input.txt", "mimeType": "text/plain", "size": len(body),
        "sha256": hashlib.sha256(body).hexdigest(), "headers": {"User-Agent": "S3Drive"},
        "url": f"https://{fetch_oss.HOST}/{fetch_oss.BUCKET}/{key}?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Date={stamp}&X-Amz-Expires=86400&X-Amz-SignedHeaders=host&X-Amz-Signature=" + "a" * 64,
    }
    value.update(overrides)
    return value


class FetchTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.workspace = Path(self.directory.name).resolve()

    def assert_code(self, code, call):
        with self.assertRaises(fetch_oss.AttachmentError) as raised:
            call()
        self.assertEqual(code, raised.exception.code)

    def test_download_verifies_bytes_uses_provider_header_and_caches(self):
        source = item()
        opener = Opener(b"actual input bytes")
        result = fetch_oss.fetch(fetch_oss.validate(source), self.workspace, opener)
        self.assertEqual(b"actual input bytes", (self.workspace / result["path"]).read_bytes())
        self.assertEqual("S3Drive", opener.requests[0].get_header("User-agent"))
        self.assertTrue(fetch_oss.fetch(source, self.workspace, opener)["cached"])
        self.assertEqual(1, len(opener.requests))
        self.assertNotIn(source["url"], json.dumps(result))

    def test_changed_local_cache_is_downloaded_again(self):
        source = item()
        opener = Opener(b"actual input bytes")
        result = fetch_oss.fetch(source, self.workspace, opener)
        (self.workspace / result["path"]).write_bytes(b"tampered")
        self.assertFalse(fetch_oss.fetch(source, self.workspace, opener)["cached"])
        self.assertEqual(2, len(opener.requests))

    def test_hash_size_and_partial_download_fail_without_final_files(self):
        for value, body, code in ((item(sha256="0" * 64), b"actual input bytes", "integrity_failed"),
                                   (item(size=2), b"actual input bytes", "size_mismatch"),
                                   (item(), b"partial", "integrity_failed")):
            self.assert_code(code, lambda: fetch_oss.fetch(value, self.workspace, Opener(body)))
            self.assertFalse(list(self.workspace.rglob("*.part")))
            self.assertFalse(list(self.workspace.rglob("*.txt")))

    def test_empty_input_is_supported(self):
        result = fetch_oss.fetch(fetch_oss.validate(item(b"")), self.workspace, Opener(b""))
        self.assertEqual(0, (self.workspace / result["path"]).stat().st_size)

    def test_filename_cannot_escape_workspace_and_long_unicode_names_work(self):
        result = fetch_oss.fetch(item(name="../../" + "图片" * 90 + ".txt"), self.workspace, Opener(b"actual input bytes"))
        path = self.workspace / result["path"]
        self.assertEqual(self.workspace / ".deerflow/inputs/oss", path.parent)
        self.assertLess(len(path.name.encode("utf-8")), 255)
        self.assert_code("unsafe_path", lambda: fetch_oss.within(self.workspace, "../outside"))

    def test_unapproved_host_bucket_headers_and_traversal_are_rejected(self):
        source = item()
        for url in (source["url"].replace("https://", "http://"), source["url"].replace(fetch_oss.HOST, "127.0.0.1"),
                    source["url"].replace(fetch_oss.BUCKET, "otherbucket"), source["url"].replace("test/input.txt", "../secret")):
            with self.assertRaises(fetch_oss.AttachmentError):
                fetch_oss.validate(item(url=url))
        self.assert_code("unapproved_headers", lambda: fetch_oss.validate(item(headers={"Authorization": "do not forward"})))

    def test_expired_signed_link_has_actionable_error(self):
        value = item()
        future = dt.datetime.now(dt.timezone.utc) + dt.timedelta(days=2)
        self.assert_code("link_expired", lambda: fetch_oss.validate(value, now=future))

    def test_manifest_validates_all_items_and_total_budget(self):
        manifest = self.workspace / "manifest.json"
        manifest.write_text(json.dumps({"version": 1, "attachments": [item()]}))
        self.assertEqual(1, len(fetch_oss.read_manifest(self.workspace, "manifest.json")))
        manifest.write_text(json.dumps({"version": 1, "attachments": [item(), item()]}))
        self.assert_code("invalid_manifest", lambda: fetch_oss.read_manifest(self.workspace, "manifest.json"))
        self.assert_code("size_limit", lambda: fetch_oss.validate(item(size=fetch_oss.MAX_FILE + 1)))

    def test_redirect_and_http_errors_never_expose_signed_queries(self):
        self.assert_code("redirect_blocked", lambda: fetch_oss.NoRedirect().redirect_request(None, None, 302, "", {}, "https://other.host"))
        opener = Opener(b"")
        with patch.object(opener, "open", side_effect=urllib.error.HTTPError(item()["url"], 403, "secret query", {}, None)):
            self.assert_code("download_denied", lambda: fetch_oss.fetch(item(), self.workspace, opener))

    def test_symlink_directory_is_rejected(self):
        outside = self.workspace / "outside"
        outside.mkdir()
        try:
            (self.workspace / ".deerflow").symlink_to(outside, target_is_directory=True)
        except OSError:
            self.skipTest("symlinks unavailable")
        self.assert_code("unsafe_path", lambda: fetch_oss.fetch(item(), self.workspace, Opener(b"actual input bytes")))


if __name__ == "__main__":
    unittest.main()

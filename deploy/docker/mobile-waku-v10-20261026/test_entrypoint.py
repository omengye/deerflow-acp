"""Exercise startup/cleanup with fake binaries; never starts DeerFlow or calls models."""
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest


ENTRYPOINT = Path(__file__).with_name("entrypoint.sh")
BASH = os.environ.get("BASH_TEST_BINARY") or shutil.which("bash")


@unittest.skipUnless(BASH, "Bash is required")
class EntrypointTests(unittest.TestCase):
    def run_entrypoint(self, **overrides):
        with tempfile.TemporaryDirectory(prefix="deerflow-entrypoint-") as folder:
            root = Path(folder)
            image = root / "image"
            binaries = image / "bin"
            resources = image / "resources"
            binaries.mkdir(parents=True)
            resources.mkdir()
            (resources / "default-config.yaml").write_text("{}\n")
            fake_scripts = {
                "deerflow-config-go": 'echo init >> "$TEST_LOG"\n',
                "deerflow-acp": '''[[ -z "${WAKU_DAEMON_TOKEN+x}" ]] || exit 77
case "${!#}" in
  --start-daemon)
    echo start >> "$TEST_LOG"
    if [[ "${START_SIGNAL:-0}" == 1 ]]; then kill -TERM "$PPID"; fi
    exit "${START_STATUS:-0}"
    ;;
  --stop-daemon) echo stop >> "$TEST_LOG" ;;
  *) exit 99 ;;
esac
''',
                "waku-daemon": '''[[ "${WAKU_DAEMON_TOKEN:-}" == fake-test-token ]] || exit 78
echo waku >> "$TEST_LOG"
if [[ "${WAKU_SIGNAL:-0}" == 1 ]]; then kill -TERM "$PPID"; fi
exit "${WAKU_STATUS:-0}"
''',
                "deerflow-acpd": "exit 99\n",
                "deerflow-control-go": "exit 99\n",
                "deerflow-skill-go": "exit 99\n",
            }
            for name, body in fake_scripts.items():
                path = binaries / name
                path.write_text("#!/bin/bash\nset -euo pipefail\n" + body, newline="\n")
                path.chmod(0o755)
            log = root / "events.log"
            env = dict(os.environ)
            for name in ("START_SIGNAL", "START_STATUS", "WAKU_SIGNAL", "WAKU_STATUS"):
                env.pop(name, None)
            env.update({
                "WAKU_DAEMON_TOKEN": "fake-test-token",
                "DEER_FLOW_PORTABLE_ROOT": (root / "portable").as_posix(),
                "TEST_LOG": log.as_posix(),
                **overrides,
            })
            # Only redirect the hardcoded image directory to the local fake fixture.
            source = ENTRYPOINT.read_text().replace(
                "readonly image_root=/opt/deerflow",
                "readonly image_root=" + "'" + image.as_posix().replace("'", "'\\''") + "'",
            )
            if os.name == "nt":
                # Git Bash cannot apply POSIX install modes to Windows temp ACLs.
                # Only adapt file staging; the startup, signals and traps stay exact.
                source = '''install() {
  if [[ "$1" == -d ]]; then shift 3; mkdir -p "$@";
  else shift 2; cp "$@"; fi
}
''' + source
            result = subprocess.run(
                [BASH, "--noprofile", "--norc", "-s"],
                input=source, text=True, capture_output=True, env=env, timeout=15,
            )
            events = log.read_text().splitlines() if log.exists() else []
            return result, events

    def test_native_starts_before_waku_and_stops_on_normal_exit(self):
        result, events = self.run_entrypoint()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(events, ["init", "start", "waku", "stop"])

    def test_native_failure_cleans_up_without_starting_waku(self):
        result, events = self.run_entrypoint(START_STATUS="17")
        self.assertEqual(result.returncode, 17, result.stderr)
        self.assertEqual(events, ["init", "start", "stop"])

    def test_waku_failure_keeps_exit_status_and_stops_native(self):
        result, events = self.run_entrypoint(WAKU_STATUS="23")
        self.assertEqual(result.returncode, 23, result.stderr)
        self.assertEqual(events, ["init", "start", "waku", "stop"])

    def test_signal_during_native_start_does_not_require_waku_pid(self):
        result, events = self.run_entrypoint(START_SIGNAL="1")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(events, ["init", "start", "stop"])

    def test_signal_after_waku_start_stops_native(self):
        result, events = self.run_entrypoint(WAKU_SIGNAL="1")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(events, ["init", "start", "waku", "stop"])


if __name__ == "__main__":
    unittest.main()

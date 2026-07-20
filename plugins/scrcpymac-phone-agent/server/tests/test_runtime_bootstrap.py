import os
import shutil
import subprocess
import tempfile
import unittest
from pathlib import Path


SOURCE_PLUGIN_ROOT = Path(__file__).resolve().parents[2]


class RuntimeBootstrapTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temp_dir = tempfile.TemporaryDirectory()
        self.root = Path(self.temp_dir.name) / "plugin"
        shutil.copytree(
            SOURCE_PLUGIN_ROOT,
            self.root,
            ignore=shutil.ignore_patterns(
                ".venv",
                ".runtime-bootstrap.lock",
                "__pycache__",
                "*.pyc",
            ),
        )

        self.fake_bin = Path(self.temp_dir.name) / "fake-bin"
        self.fake_bin.mkdir()
        self.uv_log = Path(self.temp_dir.name) / "uv.log"
        self.python_log = Path(self.temp_dir.name) / "python.log"
        self.base_python = self.fake_bin / "python3"
        self.fake_uv = self.fake_bin / "uv"

        self._write_executable(
            self.base_python,
            """#!/usr/bin/env bash
set -euo pipefail
if [[ "${1:-}" == "-c" ]]; then
  exit 0
fi
echo "unexpected base python invocation: $*" >&2
exit 1
""",
        )
        self._write_executable(
            self.fake_uv,
            """#!/usr/bin/env bash
set -euo pipefail
printf '%s\\n' "$*" >> "$FAKE_UV_LOG"
if [[ -n "${FAKE_UV_SLEEP:-}" ]]; then
  sleep "$FAKE_UV_SLEEP"
fi
if [[ "${1:-}" == "venv" ]]; then
  destination=""
  for argument in "$@"; do
    destination="$argument"
  done
  mkdir -p "$destination/bin"
  cat > "$destination/bin/python" <<'PYTHON'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\\n' "$*" >> "$FAKE_VENV_PYTHON_LOG"
exit 0
PYTHON
  chmod +x "$destination/bin/python"
fi
""",
        )

        self.env = os.environ.copy()
        self.env.update(
            {
                "PATH": f"{self.fake_bin}:{self.env['PATH']}",
                "PHONE_AGENT_BOOTSTRAP_PYTHON": str(self.base_python),
                "PHONE_AGENT_AUTO_DOWNLOAD_ADB": "0",
                "FAKE_UV_LOG": str(self.uv_log),
                "FAKE_VENV_PYTHON_LOG": str(self.python_log),
            }
        )

    def tearDown(self) -> None:
        self.temp_dir.cleanup()

    @staticmethod
    def _write_executable(path: Path, content: str) -> None:
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(content, encoding="utf-8")
        path.chmod(0o755)

    def _run(self, *parts: str) -> subprocess.CompletedProcess[str]:
        return subprocess.run(
            parts,
            cwd=self.root,
            env=self.env,
            check=True,
            capture_output=True,
            text=True,
        )

    def _uv_invocations(self) -> list[str]:
        if not self.uv_log.exists():
            return []
        return self.uv_log.read_text(encoding="utf-8").splitlines()

    def test_runtime_is_created_once_and_reused(self) -> None:
        ensure_runtime = str(self.root / "scripts/ensure-runtime.sh")
        self.env["PHONE_AGENT_BOOTSTRAP_PYTHON"] = "python3"

        self._run(ensure_runtime)
        first_invocations = self._uv_invocations()
        self.assertEqual(len(first_invocations), 2)
        self.assertIn(f"venv --python {self.base_python}", first_invocations[0])
        self.assertIn("pip install --python", first_invocations[1])
        self.assertIn(f"-e {self.root / 'server'}", first_invocations[1])

        self._run(ensure_runtime)
        self.assertEqual(self._uv_invocations(), first_invocations)

    def test_dependency_change_rebuilds_runtime(self) -> None:
        ensure_runtime = str(self.root / "scripts/ensure-runtime.sh")
        self._run(ensure_runtime)

        pyproject = self.root / "server/pyproject.toml"
        pyproject.write_text(
            pyproject.read_text(encoding="utf-8") + "\n# dependency fingerprint change\n",
            encoding="utf-8",
        )
        self._run(ensure_runtime)

        self.assertEqual(len(self._uv_invocations()), 4)

    def test_concurrent_bootstrap_installs_once(self) -> None:
        ensure_runtime = str(self.root / "scripts/ensure-runtime.sh")
        self.env["FAKE_UV_SLEEP"] = "0.2"

        first = subprocess.Popen(
            [ensure_runtime],
            cwd=self.root,
            env=self.env,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
        )
        second = subprocess.Popen(
            [ensure_runtime],
            cwd=self.root,
            env=self.env,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
        )
        first_stdout, first_stderr = first.communicate(timeout=10)
        second_stdout, second_stderr = second.communicate(timeout=10)

        self.assertEqual(first.returncode, 0, first_stderr)
        self.assertEqual(second.returncode, 0, second_stderr)
        self.assertEqual(first_stdout, "")
        self.assertEqual(second_stdout, "")
        self.assertEqual(len(self._uv_invocations()), 2)

    def test_first_mcp_launch_bootstraps_without_stdout_noise(self) -> None:
        result = self._run(str(self.root / "bin/phone-agent"), "mcp")

        self.assertEqual(result.stdout, "")
        self.assertIn("Phone Agent runtime ready", result.stderr)
        python_invocations = self.python_log.read_text(encoding="utf-8").splitlines()
        self.assertIn("-m phone_agent.server", python_invocations)

    def test_linux_launcher_selects_linux_bundled_adb(self) -> None:
        fake_uname = self.fake_bin / "uname"
        managed_python = self.fake_bin / "managed-python"
        managed_log = Path(self.temp_dir.name) / "managed-python.log"
        bundled_adb = self.root / "bin/linux/x86_64/adb"

        self._write_executable(
            fake_uname,
            """#!/usr/bin/env bash
case "${1:-}" in
  -s) echo "Linux" ;;
  -m) echo "x86_64" ;;
  *) echo "Linux" ;;
esac
""",
        )
        self._write_executable(
            managed_python,
            """#!/usr/bin/env bash
set -euo pipefail
printf 'ADB_PATH=%s ARGS=%s\\n' "${ADB_PATH:-}" "$*" > "$MANAGED_PYTHON_LOG"
""",
        )
        self._write_executable(bundled_adb, "#!/usr/bin/env bash\nexit 0\n")
        self.env.update(
            {
                "PHONE_AGENT_PYTHON": str(managed_python),
                "MANAGED_PYTHON_LOG": str(managed_log),
            }
        )

        self._run(str(self.root / "bin/phone-agent"), "devices")

        invocation = managed_log.read_text(encoding="utf-8")
        self.assertIn(f"ADB_PATH={bundled_adb}", invocation)


if __name__ == "__main__":
    unittest.main()

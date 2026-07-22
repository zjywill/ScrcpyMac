import json
import re
import unittest
from pathlib import Path

import tomllib


PLUGIN_ROOT = Path(__file__).resolve().parents[2]
REPO_ROOT = Path(__file__).resolve().parents[4]


class PackagingTests(unittest.TestCase):
    def test_marketplace_policy_is_supported(self) -> None:
        marketplace = json.loads(
            (REPO_ROOT / ".agents/plugins/marketplace.json").read_text(encoding="utf-8")
        )
        plugin = next(
            item for item in marketplace["plugins"] if item["name"] == "scrcpymac-phone-agent"
        )

        self.assertEqual(plugin["policy"]["installation"], "AVAILABLE")
        self.assertEqual(plugin["policy"]["authentication"], "ON_INSTALL")

    def test_versions_match_across_package_metadata(self) -> None:
        marketplace = json.loads(
            (REPO_ROOT / ".agents/plugins/marketplace.json").read_text(encoding="utf-8")
        )
        marketplace_plugin = next(
            item for item in marketplace["plugins"] if item["name"] == "scrcpymac-phone-agent"
        )
        codex_manifest = json.loads(
            (PLUGIN_ROOT / ".codex-plugin/plugin.json").read_text(encoding="utf-8")
        )
        cursor_manifest = json.loads(
            (PLUGIN_ROOT / ".cursor-plugin/plugin.json").read_text(encoding="utf-8")
        )
        pyproject = tomllib.loads(
            (PLUGIN_ROOT / "server/pyproject.toml").read_text(encoding="utf-8")
        )
        ui_package = json.loads(
            (PLUGIN_ROOT / "ui/package.json").read_text(encoding="utf-8")
        )
        init_source = (PLUGIN_ROOT / "server/phone_agent/__init__.py").read_text(
            encoding="utf-8"
        )
        init_version = re.search(r'__version__\s*=\s*"([^"]+)"', init_source)
        go_version_source = (
            PLUGIN_ROOT / "go/internal/version/version.go"
        ).read_text(encoding="utf-8")
        go_version = re.search(r'var Version\s*=\s*"([^"]+)"', go_version_source)

        self.assertIsNotNone(init_version)
        self.assertIsNotNone(go_version)
        versions = {
            marketplace["metadata"]["version"],
            marketplace_plugin["version"],
            codex_manifest["version"],
            cursor_manifest["version"],
            pyproject["project"]["version"],
            ui_package["version"],
            init_version.group(1),
            go_version.group(1),
        }
        self.assertEqual(len(versions), 1)

    def test_mcp_entrypoint_has_portable_plugin_working_directory(self) -> None:
        mcp_config = json.loads((PLUGIN_ROOT / ".mcp.json").read_text(encoding="utf-8"))
        public_mcp_config = json.loads(
            (PLUGIN_ROOT / "mcp.json").read_text(encoding="utf-8")
        )
        server = mcp_config["mcpServers"]["scrcpymac-phone-agent"]

        self.assertEqual(mcp_config, public_mcp_config)
        self.assertEqual(server["command"], "./mcp-server.sh")
        self.assertEqual(server["cwd"], ".")
        self.assertTrue((PLUGIN_ROOT / "mcp-server.sh").is_file())
        self.assertTrue((PLUGIN_ROOT / "scripts/ensure-runtime.sh").is_file())

    def test_codex_widget_build_is_packaged(self) -> None:
        widget = PLUGIN_ROOT / "server/phone_agent/static/scrcpymac-app.html"
        build_script = PLUGIN_ROOT / "scripts/build-ui.sh"
        package_lock = PLUGIN_ROOT / "ui/package-lock.json"

        self.assertTrue(widget.is_file())
        self.assertGreater(widget.stat().st_size, 100_000)
        self.assertTrue(build_script.is_file())
        self.assertTrue(build_script.stat().st_mode & 0o111)
        self.assertTrue(package_lock.is_file())

    def test_standalone_runtime_is_packaged_without_app_agent_client(self) -> None:
        scrcpy_server = PLUGIN_ROOT / "share/scrcpy-server"
        adb = PLUGIN_ROOT / "bin/darwin/adb"
        app_agent_client = PLUGIN_ROOT / "server/phone_agent/agent_client.py"
        notices = PLUGIN_ROOT / "THIRD_PARTY_NOTICES.md"
        adb_notices = PLUGIN_ROOT / "licenses/android-platform-tools-NOTICE.txt"
        adb_properties = PLUGIN_ROOT / "licenses/android-platform-tools-source.properties"

        self.assertTrue(scrcpy_server.is_file())
        self.assertGreater(scrcpy_server.stat().st_size, 80_000)
        self.assertTrue(adb.is_file())
        self.assertTrue(adb.stat().st_mode & 0o111)
        self.assertGreater(adb.stat().st_size, 10_000_000)
        self.assertFalse((PLUGIN_ROOT / "bin/darwin/arm64/adb").exists())
        self.assertFalse((PLUGIN_ROOT / "bin/darwin/x86_64/adb").exists())
        self.assertFalse(app_agent_client.exists())
        self.assertTrue(notices.is_file())
        self.assertGreater(adb_notices.stat().st_size, 1_000_000)
        self.assertIn("Pkg.Revision=", adb_properties.read_text(encoding="utf-8"))

    def test_go_dependency_notices_are_packaged(self) -> None:
        notices = (PLUGIN_ROOT / "THIRD_PARTY_NOTICES.md").read_text(encoding="utf-8")
        dependencies = {
            "github.com/modelcontextprotocol/go-sdk v1.6.1":
                "go-modelcontextprotocol-go-sdk-LICENSE.txt",
            "github.com/google/jsonschema-go v0.4.3":
                "go-google-jsonschema-go-LICENSE.txt",
            "github.com/segmentio/asm v1.1.3":
                "go-segmentio-asm-LICENSE.txt",
            "github.com/segmentio/encoding v0.5.4":
                "go-segmentio-encoding-LICENSE.txt",
            "github.com/yosida95/uritemplate/v3 v3.0.2":
                "go-yosida95-uritemplate-LICENSE.txt",
            "golang.org/x/oauth2 v0.35.0": "go-x-oauth2-LICENSE.txt",
            "golang.org/x/sys v0.41.0": "go-x-sys-LICENSE.txt",
        }

        for module, license_name in dependencies.items():
            with self.subTest(module=module):
                self.assertIn(module, notices)
                license_path = PLUGIN_ROOT / "licenses" / license_name
                self.assertTrue(license_path.is_file())
                self.assertGreater(license_path.stat().st_size, 1_000)


if __name__ == "__main__":
    unittest.main()

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

        self.assertIsNotNone(init_version)
        versions = {
            marketplace["metadata"]["version"],
            marketplace_plugin["version"],
            codex_manifest["version"],
            cursor_manifest["version"],
            pyproject["project"]["version"],
            ui_package["version"],
            init_version.group(1),
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
        scrcpy_server = PLUGIN_ROOT / "bin/darwin/share/scrcpy-server"
        app_agent_client = PLUGIN_ROOT / "server/phone_agent/agent_client.py"
        notices = PLUGIN_ROOT / "THIRD_PARTY_NOTICES.md"

        self.assertTrue(scrcpy_server.is_file())
        self.assertGreater(scrcpy_server.stat().st_size, 80_000)
        self.assertFalse(app_agent_client.exists())
        self.assertTrue(notices.is_file())


if __name__ == "__main__":
    unittest.main()

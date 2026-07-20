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
        codex_manifest = json.loads(
            (PLUGIN_ROOT / ".codex-plugin/plugin.json").read_text(encoding="utf-8")
        )
        cursor_manifest = json.loads(
            (PLUGIN_ROOT / ".cursor-plugin/plugin.json").read_text(encoding="utf-8")
        )
        pyproject = tomllib.loads(
            (PLUGIN_ROOT / "server/pyproject.toml").read_text(encoding="utf-8")
        )
        init_source = (PLUGIN_ROOT / "server/phone_agent/__init__.py").read_text(
            encoding="utf-8"
        )
        init_version = re.search(r'__version__\s*=\s*"([^"]+)"', init_source)

        self.assertIsNotNone(init_version)
        versions = {
            codex_manifest["version"],
            cursor_manifest["version"],
            pyproject["project"]["version"],
            init_version.group(1),
        }
        self.assertEqual(len(versions), 1)

    def test_mcp_entrypoint_and_runtime_bootstrap_exist(self) -> None:
        mcp_config = json.loads((PLUGIN_ROOT / ".mcp.json").read_text(encoding="utf-8"))
        command = mcp_config["mcpServers"]["scrcpymac-phone-agent"]["command"]

        self.assertEqual(command, "./mcp-server.sh")
        self.assertTrue((PLUGIN_ROOT / "mcp-server.sh").is_file())
        self.assertTrue((PLUGIN_ROOT / "scripts/ensure-runtime.sh").is_file())


if __name__ == "__main__":
    unittest.main()

import asyncio
import base64
import io
import unittest

from mcp.server.fastmcp import FastMCP
from PIL import Image

from phone_agent.mcp_ui import SCRCPYMAC_APP_URI, _preview_frame, register_scrcpymac_app


class FakeActions:
    def __init__(self) -> None:
        self.serial = "device-1"

    def backend(self) -> str:
        return "adb"

    def selected_serial(self) -> str:
        return self.serial

    def devices(self) -> list[dict]:
        return [
            {
                "serial": "device-1",
                "state": "device",
                "model": "Test Phone",
                "product": "test",
            }
        ]

    def select_device(self, serial: str) -> dict:
        self.serial = serial
        return {"ok": True, "serial": serial}

    def screenshot(self) -> dict:
        image = Image.new("RGB", (1080, 2400), "#147a68")
        raw = io.BytesIO()
        image.save(raw, format="PNG")
        return {
            "serial": self.serial,
            "width": 1080,
            "height": 2400,
            "png_bytes": raw.getvalue(),
            "backend": "adb",
        }


class FakeRuntime:
    def __init__(self) -> None:
        self.active = False
        self.serial = ""

    def backend_name(self) -> str:
        return "plugin-h264" if self.active else "adb"

    def stream_connect_domains(self) -> list[str]:
        return [
            "http://127.0.0.1:9478",
            "ws://127.0.0.1:9478",
        ]

    def status(self) -> dict:
        return {
            "ok": True,
            "state": "streaming" if self.active else "idle",
            "backend": self.backend_name(),
            "encoding": "H.264" if self.active else "JPEG",
            "serial": self.serial,
            "streamUrl": (
                "ws://127.0.0.1:9478/stream?token=test"
                if self.active
                else None
            ),
        }

    def start(
        self,
        serial: str,
        *,
        max_fps: int,
        resolution_percent: int,
    ) -> dict:
        self.active = True
        self.serial = serial
        return {
            **self.status(),
            "maxFps": max_fps,
            "resolutionPercent": resolution_percent,
        }

    def stop(self) -> dict:
        self.active = False
        return self.status()

    def is_active(self) -> bool:
        return self.active

    def tap_relative(self, x: float, y: float) -> dict:
        return {"ok": True, "action": "tap", "point": [x, y]}

    def swipe_relative(
        self,
        x1: float,
        y1: float,
        x2: float,
        y2: float,
        *,
        duration_ms: int,
    ) -> dict:
        return {
            "ok": True,
            "action": "swipe",
            "from": [x1, y1],
            "to": [x2, y2],
            "durationMs": duration_ms,
        }

    def key(self, name: str) -> dict:
        return {"ok": True, "action": "key", "key": name}

    def paste(self, text: str) -> dict:
        return {"ok": True, "action": "paste", "text": text}


class McpUiTests(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self) -> None:
        self.actions = FakeActions()
        self.runtime = FakeRuntime()
        self.mcp = FastMCP("scrcpymac-ui-test")
        register_scrcpymac_app(self.mcp, lambda: self.actions, self.runtime)

    async def test_open_tool_points_to_native_widget(self) -> None:
        tools = await self.mcp.list_tools()
        open_tool = next(tool for tool in tools if tool.name == "open_scrcpymac")

        self.assertEqual(open_tool.meta["ui"]["resourceUri"], SCRCPYMAC_APP_URI)
        self.assertEqual(open_tool.meta["ui"]["visibility"], ["model", "app"])
        self.assertEqual(open_tool.meta["openai/outputTemplate"], SCRCPYMAC_APP_URI)

        result = await self.mcp.call_tool(
            "open_scrcpymac",
            {"display_mode": "fullscreen"},
        )
        self.assertEqual(result.structuredContent["preferredDisplayMode"], "fullscreen")
        self.assertFalse(result.isError)

    async def test_resource_is_single_file_mcp_app_html(self) -> None:
        resources = await self.mcp.list_resources()
        resource = next(item for item in resources if str(item.uri) == SCRCPYMAC_APP_URI)

        self.assertEqual(resource.mimeType, "text/html;profile=mcp-app")
        self.assertEqual(
            resource.meta["ui"]["csp"]["connectDomains"],
            [
                "http://127.0.0.1:9478",
                "ws://127.0.0.1:9478",
            ],
        )
        self.assertEqual(
            resource.meta["openai/widgetCSP"]["resource_domains"],
            ["data:", "blob:"],
        )

        contents = list(await self.mcp.read_resource(SCRCPYMAC_APP_URI))
        html = contents[0].content
        self.assertIn("<title>ScrcpyMac</title>", html)
        self.assertNotRegex(html, r"<script[^>]+src=")
        self.assertNotRegex(html, r"<link[^>]+href=")

    async def test_internal_tools_are_app_only(self) -> None:
        tools = await self.mcp.list_tools()
        internal = [tool for tool in tools if tool.name.startswith("scrcpymac_ui_")]

        self.assertGreaterEqual(len(internal), 10)
        for tool in internal:
            self.assertEqual(tool.meta["ui"]["visibility"], ["app"])
            self.assertNotIn("resourceUri", tool.meta["ui"])

    async def test_snapshot_returns_compressed_structured_frame_once(self) -> None:
        result = await self.mcp.call_tool(
            "scrcpymac_ui_snapshot",
            {"max_width": 540, "quality": 70},
        )

        payload = result.structuredContent
        self.assertEqual(payload["frameWidth"], 540)
        self.assertEqual(payload["frameHeight"], 1200)
        self.assertEqual(payload["deviceWidth"], 1080)
        self.assertEqual(payload["deviceHeight"], 2400)
        self.assertEqual(payload["mimeType"], "image/jpeg")
        self.assertGreater(len(base64.b64decode(payload["dataBase64"])), 1000)
        self.assertNotIn(payload["dataBase64"], result.content[0].text)

    async def test_start_stream_uses_standalone_runtime(self) -> None:
        result = await self.mcp.call_tool(
            "scrcpymac_ui_start_stream",
            {
                "serial": "device-1",
                "max_fps": 60,
                "resolution_percent": 50,
            },
        )

        payload = result.structuredContent
        self.assertEqual(payload["backend"], "plugin-h264")
        self.assertEqual(payload["maxFps"], 60)
        self.assertEqual(payload["resolutionPercent"], 50)
        self.assertTrue(payload["streamUrl"].startswith("ws://127.0.0.1:"))

        stopped = await self.mcp.call_tool("scrcpymac_ui_stop_stream", {})
        self.assertEqual(stopped.structuredContent["state"], "idle")


if __name__ == "__main__":
    unittest.main()

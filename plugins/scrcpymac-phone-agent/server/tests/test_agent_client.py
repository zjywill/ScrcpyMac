import io
import json
import unittest
from unittest.mock import MagicMock, patch

from phone_agent.actions import NodeCriteria, PhoneActions, _find_node
from phone_agent.agent_client import AgentClient, _header_int, _header_value


class _FakeHTTPResponse:
    def __init__(self, body: bytes, headers: dict[str, str] | None = None):
        self._body = body
        self.headers = headers or {}

    def read(self) -> bytes:
        return self._body

    def __enter__(self):
        return self

    def __exit__(self, *args):
        return False


class AgentClientTests(unittest.TestCase):
    @patch("phone_agent.agent_client.urllib.request.urlopen")
    def test_health_ttl_avoids_repeat_requests(self, mock_urlopen: MagicMock) -> None:
        mock_urlopen.return_value = _FakeHTTPResponse(
            json.dumps(
                {"ok": True, "connected": True, "serial": "device1", "width": 1080, "height": 2400}
            ).encode()
        )
        client = AgentClient(base_url="http://127.0.0.1:9", availability_ttl_s=60)

        self.assertTrue(client.is_available())
        self.assertTrue(client.is_available())
        self.assertEqual(mock_urlopen.call_count, 1)

    @patch("phone_agent.agent_client.urllib.request.urlopen")
    def test_request_failure_invalidates_cache(self, mock_urlopen: MagicMock) -> None:
        from urllib.error import HTTPError

        ok = _FakeHTTPResponse(
            json.dumps({"ok": True, "connected": True, "serial": "device1"}).encode()
        )
        mock_urlopen.side_effect = [
            ok,
            HTTPError("http://x", 503, "fail", hdrs=None, fp=io.BytesIO(b"down")),
        ]
        client = AgentClient(base_url="http://127.0.0.1:9", availability_ttl_s=60)
        self.assertTrue(client.is_available())
        with self.assertRaises(OSError):
            client.tap(1, 2)
        self.assertFalse(client._available)

    @patch("phone_agent.agent_client.urllib.request.urlopen")
    def test_screenshot_reads_response_headers(self, mock_urlopen: MagicMock) -> None:
        mock_urlopen.return_value = _FakeHTTPResponse(
            b"\x89PNG",
            {
                "X-ScrcpyMac-Width": "1080",
                "X-ScrcpyMac-Height": "2400",
                "X-ScrcpyMac-Serial": "device1",
            },
        )
        client = AgentClient(base_url="http://127.0.0.1:9")
        client._available = True
        shot = client.screenshot()
        self.assertEqual(shot["width"], 1080)
        self.assertEqual(shot["height"], 2400)
        self.assertEqual(shot["serial"], "device1")
        self.assertEqual(shot["backend"], "scrcpymac-agent")

    @patch("phone_agent.agent_client.urllib.request.urlopen")
    def test_foreground_endpoint(self, mock_urlopen: MagicMock) -> None:
        mock_urlopen.return_value = _FakeHTTPResponse(
            json.dumps(
                {
                    "ok": True,
                    "serial": "device1",
                    "foreground": {"package": "com.tencent.mm", "activity": ".ui.LauncherUI"},
                }
            ).encode()
        )
        client = AgentClient(base_url="http://127.0.0.1:9")
        app = client.foreground_app()
        self.assertEqual(app["package"], "com.tencent.mm")


class HeaderHelperTests(unittest.TestCase):
    def test_header_lookup_is_case_insensitive(self) -> None:
        headers = {"x-scrcpymac-width": "720"}
        self.assertEqual(_header_value(headers, "X-ScrcpyMac-Width"), "720")
        self.assertEqual(_header_int(headers, "X-ScrcpyMac-Width"), 720)


class PhoneActionsTests(unittest.TestCase):
    def test_find_node_matches_partial_text(self) -> None:
        nodes = [{"text": "发送消息", "content_desc": "", "center": [1, 2]}]
        found = _find_node(nodes, NodeCriteria(text="发送"))
        self.assertIsNotNone(found)

    def test_criteria_matches_any_alternative(self) -> None:
        nodes = [{"text": "Search", "content_desc": "", "resource_id": ""}]
        self.assertIsNotNone(_find_node(nodes, NodeCriteria(text=["搜索", "Search"])))
        self.assertIsNone(_find_node(nodes, NodeCriteria(text=["发送", "Send"])))

    def test_criteria_matches_resource_id_and_class(self) -> None:
        nodes = [{"text": "", "content_desc": "", "resource_id": "com.x:id/menu_search", "class": "android.widget.ImageView"}]
        self.assertIsNotNone(_find_node(nodes, NodeCriteria(resource_id="menu_search")))
        self.assertIsNotNone(_find_node(nodes, NodeCriteria(class_name="ImageView")))

    def test_criteria_requires_an_attribute(self) -> None:
        with self.assertRaises(Exception):
            NodeCriteria()

    def test_ui_tree_uses_single_agent_round_trip(self) -> None:
        agent = MagicMock()
        agent.is_available.return_value = True
        agent.ui_tree.return_value = (
            '<hierarchy><node text="发送" clickable="true" bounds="[0,0][10,10]"/></hierarchy>',
            "device1",
        )
        actions = PhoneActions(agent=agent)
        result = actions.ui_tree(compact=True)
        self.assertEqual(result["serial"], "device1")
        agent.ui_tree.assert_called_once()
        agent.device_info.assert_not_called()

    def test_ui_tree_cache_reused_until_input(self) -> None:
        agent = MagicMock()
        agent.is_available.return_value = True
        agent.ui_tree.return_value = (
            '<?xml version="1.0" encoding="UTF-8"?>'
            '<hierarchy><node text="发送" clickable="true" bounds="[0,0][10,10]"/></hierarchy>',
            "device1",
        )
        agent.tap.return_value = {"ok": True, "serial": "device1"}

        actions = PhoneActions(client=MagicMock(), agent=agent)
        first = actions.ui_tree(compact=True)
        second = actions.ui_tree(compact=True)
        self.assertEqual(agent.ui_tree.call_count, 1)
        self.assertEqual(first["count"], second["count"])

        actions.tap(5, 5)
        actions.ui_tree(compact=True)
        self.assertEqual(agent.ui_tree.call_count, 2)

    def test_agent_failure_falls_back_to_adb(self) -> None:
        agent = MagicMock()
        agent.is_available.return_value = True
        agent.tap.side_effect = OSError("agent POST /tap failed (503)")
        adb = MagicMock()
        adb.serial = "device1"

        actions = PhoneActions(client=adb, agent=agent)
        result = actions.tap(5, 7)

        adb.shell.assert_called_once_with("input tap 5 7")
        self.assertTrue(result["ok"])
        self.assertEqual(result["serial"], "device1")

    def test_type_text_quotes_shell_metacharacters(self) -> None:
        adb = MagicMock()
        adb.serial = "device1"
        actions = PhoneActions(client=adb, agent=MagicMock(is_available=lambda **_: False))

        actions.type_text("pay $20 `id` 100%")

        command = adb.shell.call_args[0][0]
        self.assertTrue(command.startswith("input text "))
        self.assertIn("'pay%s$20%s`id`%s100%25'", command)

    def test_adb_client_not_constructed_when_agent_handles_call(self) -> None:
        agent = MagicMock()
        agent.is_available.return_value = True
        agent.tap.return_value = {"ok": True, "serial": "device1"}

        actions = PhoneActions(agent=agent)
        with patch("phone_agent.actions.AdbClient") as adb_cls:
            actions.tap(1, 2)
            adb_cls.assert_not_called()


if __name__ == "__main__":
    unittest.main()

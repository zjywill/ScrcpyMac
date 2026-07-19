import io
import json
import unittest
from unittest.mock import MagicMock, patch

from phone_agent.actions import NodeCriteria, PhoneActions, _find_node, _find_nodes
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


def _tree_actions(xml: str) -> PhoneActions:
    agent = MagicMock()
    agent.is_available.return_value = True
    agent.ui_tree.return_value = (xml, "device1")
    return PhoneActions(agent=agent)


class UiTreeEnrichmentTests(unittest.TestCase):
    def test_scrollable_container_kept_with_flag(self) -> None:
        result = _tree_actions(
            "<hierarchy>"
            '<node class="androidx.recyclerview.widget.RecyclerView"'
            ' scrollable="true" bounds="[0,0][1080,2000]"/>'
            "</hierarchy>"
        ).ui_tree(compact=True)
        self.assertEqual(result["count"], 1)
        node = result["nodes"][0]
        self.assertIs(node["scrollable"], True)
        self.assertEqual(node["index"], 0)

    def test_empty_edittext_kept(self) -> None:
        result = _tree_actions(
            '<hierarchy><node class="android.widget.EditText" bounds="[0,0][10,10]"/></hierarchy>'
        ).ui_tree(compact=True)
        self.assertEqual(result["count"], 1)

    def test_disabled_button_flagged(self) -> None:
        result = _tree_actions(
            '<hierarchy><node text="发送" clickable="true" enabled="false"'
            ' bounds="[0,0][10,10]"/></hierarchy>'
        ).ui_tree(compact=True)
        self.assertIs(result["nodes"][0]["enabled"], False)

    def test_enabled_button_omits_flag(self) -> None:
        result = _tree_actions(
            '<hierarchy><node text="发送" clickable="true" enabled="true"'
            ' bounds="[0,0][10,10]"/></hierarchy>'
        ).ui_tree(compact=True)
        self.assertNotIn("enabled", result["nodes"][0])

    def test_non_clickable_switch_kept(self) -> None:
        # Real-world Switch widgets are often clickable=false (the parent row
        # takes the click) with no text; they must still appear in the tree.
        result = _tree_actions(
            '<hierarchy><node class="android.widget.Switch" clickable="false"'
            ' checkable="true" checked="false" bounds="[901,959][1038,1085]"/></hierarchy>'
        ).ui_tree(compact=True)
        self.assertEqual(result["count"], 1)
        self.assertIs(result["nodes"][0]["checked"], False)

    def test_checkable_switch_reports_checked(self) -> None:
        result = _tree_actions(
            '<hierarchy><node class="android.widget.Switch" clickable="true"'
            ' checkable="true" checked="true" bounds="[0,0][10,10]"/></hierarchy>'
        ).ui_tree(compact=True)
        node = result["nodes"][0]
        self.assertIs(node["checkable"], True)
        self.assertIs(node["checked"], True)

    def test_webview_tree_flagged_degraded(self) -> None:
        result = _tree_actions(
            "<hierarchy>"
            '<node class="android.webkit.WebView" clickable="true" bounds="[0,0][1080,2000]"/>'
            '<node text="a" bounds="[0,0][10,10]"/>'
            '<node text="b" bounds="[0,0][10,10]"/>'
            '<node text="c" bounds="[0,0][10,10]"/>'
            "</hierarchy>"
        ).ui_tree(compact=True)
        self.assertIs(result["degraded"], True)
        self.assertIn("phone_screenshot", result["hint"])

    def test_sparse_tree_flagged_degraded(self) -> None:
        result = _tree_actions("<hierarchy></hierarchy>").ui_tree(compact=True)
        self.assertIs(result["degraded"], True)

    def test_empty_dump_retried_once_then_succeeds(self) -> None:
        agent = MagicMock()
        agent.is_available.return_value = True
        agent.ui_tree.side_effect = [
            ("", "device1"),
            ('<hierarchy><node text="发送" clickable="true" bounds="[0,0][10,10]"/></hierarchy>', "device1"),
        ]
        actions = PhoneActions(agent=agent)
        with patch("phone_agent.actions.time.sleep"):
            result = actions.ui_tree(compact=True)
        self.assertEqual(result["count"], 1)
        self.assertEqual(agent.ui_tree.call_count, 2)

    def test_persistent_empty_dump_marked_degraded_and_not_cached(self) -> None:
        agent = MagicMock()
        agent.is_available.return_value = True
        agent.ui_tree.return_value = ("", "device1")
        actions = PhoneActions(agent=agent)
        with patch("phone_agent.actions.time.sleep"):
            result = actions.ui_tree(compact=True)
        self.assertIs(result["parse_error"], True)
        self.assertIs(result["degraded"], True)
        self.assertIn("phone_screenshot", result["hint"])
        # Broken dumps must not be cached: the next call re-dumps.
        calls_before = agent.ui_tree.call_count
        with patch("phone_agent.actions.time.sleep"):
            actions.ui_tree(compact=False)
        self.assertGreater(agent.ui_tree.call_count, calls_before)

    def test_normal_tree_not_degraded(self) -> None:
        result = _tree_actions(
            "<hierarchy>"
            '<node text="首页" clickable="true" bounds="[0,0][10,10]"/>'
            '<node text="通讯录" clickable="true" bounds="[0,0][10,10]"/>'
            '<node text="发现" clickable="true" bounds="[0,0][10,10]"/>'
            "</hierarchy>"
        ).ui_tree(compact=True)
        self.assertNotIn("degraded", result)


class NodeCriteriaTests(unittest.TestCase):
    def test_require_all_rejects_partial_combo(self) -> None:
        nodes = [
            {"text": "发送", "content_desc": "", "resource_id": "com.x:id/btn_send", "class": ""}
        ]
        self.assertIsNone(
            _find_node(
                nodes,
                NodeCriteria(text="发送", resource_id="btn_wrong", require_all=True),
            )
        )
        self.assertIsNotNone(
            _find_node(
                nodes,
                NodeCriteria(text="发送", resource_id="btn_send", require_all=True),
            )
        )

    def test_or_matching_remains_default(self) -> None:
        nodes = [
            {"text": "发送", "content_desc": "", "resource_id": "com.x:id/btn_send", "class": ""}
        ]
        self.assertIsNotNone(
            _find_node(nodes, NodeCriteria(text="发送", resource_id="btn_wrong"))
        )

    def test_exact_match(self) -> None:
        nodes = [{"text": "发送", "content_desc": "", "resource_id": "", "class": ""}]
        self.assertIsNone(_find_node(nodes, NodeCriteria(text="发", exact=True)))
        self.assertIsNotNone(_find_node(nodes, NodeCriteria(text="发送", exact=True)))

    def test_index_picks_nth_match(self) -> None:
        nodes = [
            {"text": "设置", "center": [1, 1]},
            {"text": "设置", "center": [2, 2]},
        ]
        criteria = NodeCriteria(text="设置")
        self.assertEqual(len(_find_nodes(nodes, criteria)), 2)
        self.assertEqual(_find_node(nodes, criteria, index=1)["center"], [2, 2])
        self.assertIsNone(_find_node(nodes, criteria, index=2))
        self.assertIsNone(_find_node(nodes, criteria, index=-1))

    def test_enabled_only_skips_disabled_nodes(self) -> None:
        nodes = [{"text": "发送", "enabled": False}]
        self.assertIsNone(_find_node(nodes, NodeCriteria(text="发送")))
        self.assertIsNotNone(
            _find_node(nodes, NodeCriteria(text="发送", enabled_only=False))
        )

    def test_clickable_only(self) -> None:
        nodes = [{"text": "发送", "clickable": False}]
        self.assertIsNone(_find_node(nodes, NodeCriteria(text="发送", clickable_only=True)))


class ScrollToFindTests(unittest.TestCase):
    def _actions(self) -> PhoneActions:
        adb = MagicMock()
        adb.serial = "device1"
        adb.screen_size.return_value = (1080, 2400)
        agent = MagicMock()
        agent.is_available.return_value = False
        return PhoneActions(client=adb, agent=agent)

    @staticmethod
    def _tree(nodes: list[dict]) -> dict:
        return {"ok": True, "nodes": nodes, "count": len(nodes), "serial": "device1"}

    def test_scroll_to_find_scrolls_until_hit(self) -> None:
        actions = self._actions()
        target = {
            "index": 0,
            "text": "目标",
            "content_desc": "",
            "resource_id": "",
            "class": "",
            "clickable": True,
            "bounds": "[0,0][10,10]",
            "center": [5, 5],
        }
        with patch.object(
            actions,
            "ui_tree",
            side_effect=[self._tree([]), self._tree([]), self._tree([target])],
        ), patch.object(actions, "swipe") as mock_swipe, patch(
            "phone_agent.actions.time.sleep"
        ):
            result = actions.find_and_tap(text="目标", scroll_to_find=2, timeout_s=5)
        self.assertTrue(result["ok"])
        self.assertEqual(mock_swipe.call_count, 2)

    def test_no_scroll_by_default(self) -> None:
        actions = self._actions()
        target = {
            "index": 0,
            "text": "目标",
            "content_desc": "",
            "resource_id": "",
            "class": "",
            "clickable": True,
            "bounds": "[0,0][10,10]",
            "center": [5, 5],
        }
        with patch.object(
            actions, "ui_tree", side_effect=[self._tree([]), self._tree([target])]
        ), patch.object(actions, "swipe") as mock_swipe, patch(
            "phone_agent.actions.time.sleep"
        ):
            result = actions.find_and_tap(text="目标", timeout_s=5)
        self.assertTrue(result["ok"])
        mock_swipe.assert_not_called()


if __name__ == "__main__":
    unittest.main()

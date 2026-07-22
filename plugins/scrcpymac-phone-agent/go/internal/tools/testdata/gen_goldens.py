#!/usr/bin/env python3
"""Regenerate the phone_ui_tree / phone_find_and_tap / phone_wait_for_text goldens.

The goldens are produced by the REAL Python implementation
(server/phone_agent/actions.py) driven over the XML fixtures in this directory,
which is the only way to prove the Go port is byte-compatible. Nothing here
re-implements the behaviour under test: the compaction goldens come from
PhoneActions.ui_tree() with _dump_ui_xml patched to return a fixture, the
selector goldens from NodeCriteria/_find_node, and the poll goldens from the real
_poll_for_node with a fake clock swapped in for the `time` module.

Usage (from anywhere):

    python3 go/internal/tools/testdata/gen_goldens.py

Requires the Python server's dependencies (PIL); no device and no adb.
"""

from __future__ import annotations

import json
import os
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
SERVER = os.path.abspath(os.path.join(HERE, "..", "..", "..", "..", "server"))
sys.path.insert(0, SERVER)

from phone_agent.actions import (  # noqa: E402
    NodeCriteria,
    PhoneActions,
    _find_node,
    _find_nodes,
    json_result,
)
import phone_agent.actions as actions  # noqa: E402

SERIAL = "2f019965"

COMPACT_FIXTURES = [
    "settings_home.xml",
    "settings_display.xml",
    "chrome_example.xml",
    "chrome_webview.xml",
    "sparse_dialog.xml",
    "flags.xml",
    "malformed.xml",
    "empty.xml",
]

# The raw (compact=false) shape is {ok, xml, serial}; two fixtures are enough to
# pin it without duplicating every dump into a second golden.
RAW_FIXTURES = ["sparse_dialog.xml", "empty.xml"]


def read_fixture(name: str) -> str:
    with open(os.path.join(HERE, name), encoding="utf-8") as handle:
        return handle.read()


def fresh_actions(xml: str) -> PhoneActions:
    """A PhoneActions that never touches adb: only _dump_ui_xml is reachable."""
    instance = PhoneActions.__new__(PhoneActions)
    instance._client = None
    instance.runtime = None
    instance._ui_tree_cache = None
    instance._dump_ui_xml = lambda: (xml, SERIAL)
    return instance


class FakeClock:
    """Stands in for the `time` module inside phone_agent.actions."""

    def __init__(self) -> None:
        self.now = 1000.0
        self.sleeps: list[float] = []

    def time(self) -> float:
        return self.now

    def sleep(self, seconds: float) -> None:
        self.sleeps.append(round(float(seconds), 6))
        self.now += float(seconds)


def gen_trees() -> None:
    for name in COMPACT_FIXTURES:
        xml = read_fixture(name)
        clock = FakeClock()
        actions.time = clock
        try:
            result = fresh_actions(xml).ui_tree(compact=True)
        finally:
            actions.time = REAL_TIME
        write(name.replace(".xml", ".compact.json"), json_result(result))

    for name in RAW_FIXTURES:
        xml = read_fixture(name)
        clock = FakeClock()
        actions.time = clock
        try:
            result = fresh_actions(xml).ui_tree(compact=False)
        finally:
            actions.time = REAL_TIME
        write(name.replace(".xml", ".raw.json"), json_result(result))


SELECTOR_CASES = [
    # (name, fixture, kwargs, index)
    # The attached OnePlus 6 runs a Chinese locale, so the real fixtures also
    # exercise the ensure_ascii=False / rune-length paths.
    ("text-substring", "settings_home.xml", {"text": "帐号"}, 0),
    ("text-substring-index1", "settings_home.xml", {"text": "帐号"}, 1),
    ("text-substring-index-oob", "settings_home.xml", {"text": "帐号"}, 999),
    ("text-negative-index", "settings_home.xml", {"text": "帐号"}, -1),
    ("text-exact-hit", "settings_home.xml", {"text": "帐号", "exact": True}, 0),
    ("text-exact-miss", "settings_home.xml", {"text": "帐", "exact": True}, 0),
    ("desc-only", "settings_home.xml", {"content_desc": "搜索"}, 0),
    ("class-substring", "settings_home.xml", {"class_name": "TextView"}, 0),
    ("clickable-container", "settings_home.xml", {"class_name": "LinearLayout"}, 2),
    ("resource-id-substring", "flags.xml", {"resource_id": "menu_search"}, 0),
    ("cjk-alternatives", "flags.xml", {"text": ["搜索", "Search"]}, 0),
    ("cjk-desc-alternatives", "flags.xml", {"content_desc": ["搜索", "Search"]}, 0),
    ("any-of-two-attributes", "flags.xml", {"text": "Wi-Fi", "resource_id": "password"}, 0),
    ("require-all-hit", "flags.xml", {"text": "Wi-Fi", "resource_id": "wifi", "require_all": True}, 0),
    ("require-all-miss", "flags.xml", {"text": "Wi-Fi", "resource_id": "password", "require_all": True}, 0),
    ("disabled-skipped", "flags.xml", {"text": "Greyed out"}, 0),
    ("disabled-skipped-all-flags", "flags.xml", {"text": "Everything"}, 0),
    ("exact-hit", "flags.xml", {"text": "Padded", "exact": True}, 0),
    ("exact-miss-substring", "flags.xml", {"text": "Padde", "exact": True}, 0),
    ("html-chars", "flags.xml", {"text": "Tom & Jerry <b>"}, 0),
    ("quote-in-needle", "flags.xml", {"content_desc": 'a "quoted" desc'}, 0),
    ("apostrophe-needle", "flags.xml", {"text": "it's"}, 0),
    ("no-bounds-node", "flags.xml", {"text": "No bounds"}, 0),
    ("trailing-junk-bounds", "flags.xml", {"text": "Trailing junk"}, 0),
    ("empty-string-dropped", "flags.xml", {"text": ["", "Wi-Fi"]}, 0),
    ("webview-class", "chrome_webview.xml", {"class_name": "WebView"}, 0),
    ("compose-desc", "sparse_dialog.xml", {"content_desc": "Close"}, 0),
    ("class-and-text-any", "settings_display.xml", {"text": "Display", "class_name": "Switch"}, 0),
    ("switch-checkable", "settings_display.xml", {"class_name": "Switch"}, 0),
]


def gen_selectors() -> None:
    cases = []
    for name, fixture, kwargs, index in SELECTOR_CASES:
        xml = read_fixture(fixture)
        tree = fresh_actions(xml).ui_tree(compact=True)
        nodes = tree.get("nodes", [])
        criteria = NodeCriteria(**kwargs)
        matches = _find_nodes(nodes, criteria)
        node = _find_node(nodes, criteria, index=index)
        cases.append(
            {
                "name": name,
                "fixture": fixture,
                "text": as_list(kwargs.get("text")),
                "content_desc": as_list(kwargs.get("content_desc")),
                "resource_id": as_list(kwargs.get("resource_id")),
                "class_name": as_list(kwargs.get("class_name")),
                "require_all": bool(kwargs.get("require_all", False)),
                "exact": bool(kwargs.get("exact", False)),
                "index": index,
                "describe": criteria.describe(),
                "match_count": len(matches),
                "match_indexes": [m["index"] for m in matches],
                "matched": json_result(node) if node is not None else None,
            }
        )
    write("selectors.json", json.dumps(cases, ensure_ascii=False, indent=2))


def as_list(value) -> list[str]:
    if value is None:
        return []
    if isinstance(value, (list, tuple)):
        return [str(v) for v in value]
    return [str(value)]


POLL_CASES = [
    # (name, fixture, kwargs, timeout_s, poll_interval_s, index, scroll_to_find)
    ("hit-first-dump", "settings_home.xml", {"text": "帐号"}, 10.0, 0.4, 0, 0),
    ("hit-second-match", "settings_home.xml", {"text": "帐号"}, 10.0, 0.4, 1, 0),
    ("miss-default-backoff", "settings_home.xml", {"text": "nope"}, 10.0, 0.4, 0, 0),
    ("miss-short-timeout", "settings_home.xml", {"text": "nope"}, 1.0, 0.4, 0, 0),
    ("miss-zero-timeout", "settings_home.xml", {"text": "nope"}, 0.0, 0.4, 0, 0),
    ("miss-negative-timeout", "settings_home.xml", {"text": "nope"}, -1.0, 0.4, 0, 0),
    ("miss-long-timeout-cap", "settings_home.xml", {"text": "nope"}, 30.0, 0.4, 0, 0),
    ("miss-with-scrolls", "settings_home.xml", {"text": "nope"}, 10.0, 0.4, 0, 3),
    ("miss-degraded-webview", "chrome_webview.xml", {"text": "nope"}, 2.0, 0.4, 0, 1),
    ("miss-index-out-of-range", "settings_home.xml", {"text": "帐号"}, 2.0, 0.4, 7, 0),
    ("miss-degraded-tree", "empty.xml", {"text": "nope"}, 2.0, 0.4, 0, 0),
    ("miss-cjk-criteria", "flags.xml", {"text": ["搜索", "Search"], "content_desc": ["搜索"]}, 3.0, 0.4, 0, 2),
    ("miss-require-all", "flags.xml", {"text": "Wi-Fi", "resource_id": "nope", "require_all": True, "exact": True}, 1.0, 0.5, 0, 0),
]

# _scroll_once returns before both the swipe and its 0.4s settle when the screen
# size is unknown, but still burns an attempt. Same shape, screen_known=False.
POLL_NO_SIZE_CASES = [
    ("miss-scroll-without-screen-size", "settings_home.xml", {"text": "nope"}, 2.0, 0.4, 0, 2),
    ("miss-scroll-without-screen-size-long", "settings_home.xml", {"text": "nope"}, 6.0, 0.4, 0, 3),
]

REAL_TIME = actions.time


def gen_poll() -> None:
    cases = []
    for screen_known, group in ((True, POLL_CASES), (False, POLL_NO_SIZE_CASES)):
        cases.extend(run_poll_cases(group, screen_known))
    write("poll.json", json.dumps(cases, ensure_ascii=False, indent=2))


def run_poll_cases(group, screen_known: bool) -> list:
    cases = []
    for name, fixture, kwargs, timeout_s, poll_interval_s, index, scroll in group:
        xml = read_fixture(fixture)
        instance = fresh_actions(xml)
        clock = FakeClock()
        dumps: list[bool] = []
        scrolls: list[str] = []

        real_ui_tree = instance.ui_tree

        def traced_ui_tree(*, compact=True, force_refresh=False):
            dumps.append(force_refresh)
            return real_ui_tree(compact=compact, force_refresh=force_refresh)

        instance.ui_tree = traced_ui_tree
        instance._screen_size = (lambda: (1080, 2280)) if screen_known else (lambda: None)
        instance.swipe = lambda *a, **kw: scrolls.append(json.dumps([a, kw])) or {}

        actions.time = clock
        error = None
        found = None
        try:
            node, tree = instance._poll_for_node(
                NodeCriteria(**kwargs),
                timeout_s=timeout_s,
                poll_interval_s=poll_interval_s,
                index=index,
                scroll_to_find=scroll,
            )
            found = json_result(node)
        except Exception as exc:  # AdbError
            error = str(exc)
        finally:
            actions.time = REAL_TIME

        cases.append(
            {
                "name": name,
                "fixture": fixture,
                "text": as_list(kwargs.get("text")),
                "content_desc": as_list(kwargs.get("content_desc")),
                "resource_id": as_list(kwargs.get("resource_id")),
                "class_name": as_list(kwargs.get("class_name")),
                "require_all": bool(kwargs.get("require_all", False)),
                "exact": bool(kwargs.get("exact", False)),
                "index": index,
                "timeout_s": timeout_s,
                "poll_interval_s": poll_interval_s,
                "scroll_to_find": scroll,
                "screen_known": screen_known,
                "dumps": dumps,
                "sleeps": clock.sleeps,
                "scrolls": len(scrolls),
                "found": found,
                "error": error,
            }
        )
    return cases


def gen_wait_for_text() -> None:
    """wait_for_text's own result shape, including the serial fallback."""
    cases = []
    for name, fixture, text, timeout_s in [
        ("hit", "settings_home.xml", "帐号", 10.0),
        ("hit-cjk", "flags.xml", "搜索", 5.0),
        ("hit-desc-never-matches", "sparse_dialog.xml", "Close", 1.0),
        ("miss", "settings_home.xml", "nope", 1.0),
        ("empty-text", "settings_home.xml", "", 1.0),
    ]:
        xml = read_fixture(fixture)
        instance = fresh_actions(xml)
        clock = FakeClock()
        actions.time = clock
        result = None
        error = None
        try:
            result = json_result(instance.wait_for_text(text, timeout_s=timeout_s))
        except Exception as exc:
            error = str(exc)
        finally:
            actions.time = REAL_TIME
        cases.append(
            {
                "name": name,
                "fixture": fixture,
                "text": text,
                "timeout_s": timeout_s,
                "result": result,
                "error": error,
            }
        )
    write("wait_for_text.json", json.dumps(cases, ensure_ascii=False, indent=2))


def gen_describe() -> None:
    """NodeCriteria.describe() over the shapes Python repr() makes interesting."""
    cases = []
    for kwargs in [
        {"text": "Settings"},
        {"text": ["搜索", "Search"]},
        {"text": "it's"},
        {"text": ['say "hi"']},
        {"text": ["it's", 'say "hi"']},
        {"text": "tab\there"},
        {"text": "line\nbreak"},
        {"text": "back\\slash"},
        {"text": "Settings", "content_desc": "More", "resource_id": "id/x", "class_name": "Button"},
        {"text": "Settings", "require_all": True},
        {"text": "Settings", "exact": True},
        {"text": "Settings", "require_all": True, "exact": True},
        {"content_desc": ["a", "b", "c"]},
        {"class_name": "android.widget.Switch"},
        {"text": "emoji😀"},
    ]:
        criteria = NodeCriteria(**kwargs)
        cases.append(
            {
                "text": as_list(kwargs.get("text")),
                "content_desc": as_list(kwargs.get("content_desc")),
                "resource_id": as_list(kwargs.get("resource_id")),
                "class_name": as_list(kwargs.get("class_name")),
                "require_all": bool(kwargs.get("require_all", False)),
                "exact": bool(kwargs.get("exact", False)),
                "describe": criteria.describe(),
            }
        )
    write("describe.json", json.dumps(cases, ensure_ascii=False, indent=2))


def write(name: str, text: str) -> None:
    path = os.path.join(HERE, name)
    with open(path, "w", encoding="utf-8") as handle:
        handle.write(text)
        handle.write("\n")
    print(f"wrote {name} ({len(text)} bytes)")


def main() -> None:
    gen_trees()
    gen_selectors()
    gen_poll()
    gen_wait_for_text()
    gen_describe()


if __name__ == "__main__":
    main()

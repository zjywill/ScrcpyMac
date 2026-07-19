# `phone_ui_tree` 优化方案（可执行）

目标：把 `phone_ui_tree` 从"能跑通线性任务"提升到"接近 Chrome DOM 的可靠命中"。
改动集中在 `server/phone_agent/actions.py` 一个文件，外加 `server.py` 的工具签名与
`server/tests/test_agent_client.py` 的用例。全程向后兼容——现有字段与 API 不删不改名，
只做**新增**与**可选参数**。

## 现状基线（改之前先读）

| 位置 | 现状 | 问题 |
|---|---|---|
| `actions.py:250-268` compact 提取 | 只留 `text / content_desc / resource_id / class / clickable / bounds / center` | 丢了 `enabled/scrollable/checked/password/focused`；无 `index` |
| `actions.py:255` 过滤 | `if not (text or desc or clickable): continue` | 空 EditText、可滚动容器被丢 |
| `actions.py:412-418` `matches` | 四类属性之间是 **OR** | 无法用"文本+id"组合消歧；无 exact；无第 N 个 |
| `actions.py:440-444` `_find_node` | 返回**第一个**匹配 | 多个同名节点无法定位 |
| `actions.py:278-303` `_poll_for_node` | 只反复 re-dump | 屏幕外元素永远找不到，不会滚动翻找 |
| `actions.py:269-276` 结果 | ParseError 才标记 | 空树 / WebView 残缺树无信号，不提示回退截图 |

以下 5 个 Phase 按收益排序，可独立合入、独立回滚。

---

## Phase 1 — 富化节点字段 + 省 token 输出

**动机**：补齐状态属性（缺口 #2、#3），让 Agent 能判断"可点/已勾/置灰/密码框/可滚动"，
并给每个节点一个稳定 `index` 用于消歧（缺口 #6）。

**文件**：`actions.py` — `ui_tree()` 的 compact 分支（当前 247-276 行）。

**替换 250-268 行的循环体为：**

```python
        nodes = []
        try:
            root = ET.fromstring(xml)
            for node in root.iter("node"):
                attrib = node.attrib
                text = attrib.get("text", "").strip()
                desc = attrib.get("content-desc", "").strip()
                cls = attrib.get("class", "")
                clickable = attrib.get("clickable") == "true"
                scrollable = attrib.get("scrollable") == "true"
                editable = "EditText" in cls
                # 只保留可交互 / 有语义的节点，但把可滚动容器和空输入框也留下
                if not (text or desc or clickable or scrollable or editable):
                    continue
                bounds = attrib.get("bounds", "")
                item = {
                    "index": len(nodes),          # 本棵树内稳定序号，供 index= 消歧
                    "text": text,
                    "content_desc": desc,
                    "resource_id": attrib.get("resource-id", ""),
                    "class": cls,
                    "clickable": clickable,
                    "bounds": bounds,
                    "center": _bounds_center(bounds),
                }
                # 布尔标志"仅在值得注意时"输出，避免每个节点都膨胀 token
                if scrollable:
                    item["scrollable"] = True
                if attrib.get("enabled") == "false":
                    item["enabled"] = False           # 缺省即 enabled，省字段
                if attrib.get("password") == "true":
                    item["password"] = True
                if attrib.get("focused") == "true":
                    item["focused"] = True
                if attrib.get("selected") == "true":
                    item["selected"] = True
                if attrib.get("checkable") == "true":
                    item["checkable"] = True
                    item["checked"] = attrib.get("checked") == "true"
                nodes.append(item)
        except ET.ParseError:
            result = {"ok": True, "xml": xml, "serial": serial, "parse_error": True}
            self._ui_tree_cache = result
            return result
```

**要点**
- 保留全部旧字段名（`content_desc / resource_id / class`），旧测试与 `NodeCriteria` 不受影响。
- 布尔标志缺省不输出 → 一棵普通树的 token 增量 <5%。
- `enabled` 仅在 `false` 时出现，下游判断 `node.get("enabled") is False` 即"置灰"。

**测试**（`test_agent_client.py` 新增）
- 构造含 `scrollable=true` 的容器 XML，断言 compact 结果里该节点在、且带 `scrollable: True`。
- 构造 `enabled=false` 按钮，断言 `node["enabled"] is False`。
- 构造 `checkable=true checked=true` 开关，断言 `checked: True`。

---

## Phase 2 — `NodeCriteria` 支持 AND / exact / index（核心可靠性）

**动机**：缺口 #1。让 `text` + `resource_id` 组合精确锁定；支持精确匹配与"第 N 个"。

**文件**：`actions.py` — `NodeCriteria`（388-430）、`_find_node`（440-444）、`_any_substring`（433-437）。

**1) 新增匹配器（替换 `_any_substring`）：**

```python
def _any_match(needles: list[str], haystack: Optional[str], *, exact: bool) -> bool:
    if not needles:
        return False
    text = haystack or ""
    if exact:
        return any(n == text for n in needles)
    return any(n and n in text for n in needles)
```

> 保留 `_any_substring` 作为薄封装（`return _any_match(needles, haystack, exact=False)`），
> 以免有其他调用点/测试引用它。

**2) `NodeCriteria.__init__` 增参、`matches` 改为可选 AND：**

```python
class NodeCriteria:
    def __init__(
        self,
        *,
        text=None,
        content_desc=None,
        resource_id=None,
        class_name=None,
        require_all: bool = False,   # True: 所有"已给出"的属性都要命中（AND）
        exact: bool = False,         # True: 精确相等而非子串
        clickable_only: bool = False,
        enabled_only: bool = True,   # 默认跳过置灰节点
    ):
        self.text = _as_list(text)
        self.content_desc = _as_list(content_desc)
        self.resource_id = _as_list(resource_id)
        self.class_name = _as_list(class_name)
        self.require_all = require_all
        self.exact = exact
        self.clickable_only = clickable_only
        self.enabled_only = enabled_only
        if not any((self.text, self.content_desc, self.resource_id, self.class_name)):
            raise AdbError("NodeCriteria requires at least one attribute")

    def _group(self, needles, value):
        # 该属性未指定 → None（不参与）；指定 → 命中与否
        return _any_match(needles, value, exact=self.exact) if needles else None

    def matches(self, node: dict) -> bool:
        if self.enabled_only and node.get("enabled") is False:
            return False
        if self.clickable_only and not node.get("clickable"):
            return False
        groups = [
            self._group(self.text, node.get("text")),
            self._group(self.content_desc, node.get("content_desc")),
            self._group(self.resource_id, node.get("resource_id")),
            self._group(self.class_name, node.get("class")),
        ]
        specified = [g for g in groups if g is not None]
        if not specified:
            return False
        return all(specified) if self.require_all else any(specified)
```

> **兼容性**：默认 `require_all=False` → 行为与今天完全一致（OR）。`enabled_only=True` 是
> 唯一默认行为变化：置灰节点不再被选中——这几乎总是我们想要的，且旧测试节点未设 `enabled`
> 故不受影响。若担心，可临时设默认 `enabled_only=False` 分两步合。

**3) `_find_node` 支持取第 N 个匹配：**

```python
def _find_nodes(nodes: list[dict], criteria: NodeCriteria) -> list[dict]:
    return [n for n in nodes if criteria.matches(n)]

def _find_node(nodes: list[dict], criteria: NodeCriteria, index: int = 0) -> Optional[dict]:
    matches = _find_nodes(nodes, criteria)
    if not matches:
        return None
    if index < 0 or index >= len(matches):
        return None
    return matches[index]
```

**测试**
- `require_all=True` 且 `text="发送"`+`resource_id="btn_wrong"`：断言**不**命中（旧 OR 会命中）。
- `require_all=True` 且两者都对：断言命中。
- `exact=True`："发"不命中"发送"，"发送"命中。
- 两个同 text 节点，`index=1` 返回第二个。
- `enabled_only=True` 跳过 `enabled=False` 节点。

---

## Phase 3 — `find_and_tap` 滚动翻找 + 透传新条件

**动机**：缺口 #4。目标在屏幕外时自动滚动查找；把 Phase 2 的能力接到工具层。

**文件**：`actions.py` — `_poll_for_node`（278-303）、`find_and_tap`（305-327）。

**1) 新增滚动辅助**（放在 `PhoneActions` 内，复用已有 `swipe` 与 `client.screen_size()`）：

```python
    def _scroll_once(self, *, direction: str = "up", fraction: float = 0.6) -> None:
        """在整屏中部做一次滚动。up=内容上移（看下方），down=看上方。"""
        try:
            w, h = self.client.screen_size()
        except (AdbError, OSError):
            return
        x = w // 2
        y1, y2 = (int(h * 0.7), int(h * 0.3)) if direction == "up" else (int(h * 0.3), int(h * 0.7))
        self.swipe(x, y1, x, y2, duration_ms=350)  # swipe 内部会失效 ui_tree 缓存
        time.sleep(0.4)
```

**2) `_poll_for_node` 增加 `scroll_to_find`、`index`：**

```python
    def _poll_for_node(
        self,
        criteria: "NodeCriteria",
        *,
        timeout_s: float,
        poll_interval_s: float = 0.4,
        index: int = 0,
        scroll_to_find: int = 0,     # 未命中时最多滚动几次
    ) -> tuple[dict, dict]:
        deadline = time.time() + timeout_s
        last_tree: dict[str, Any] = {}
        scrolls_used = 0
        attempt = 0
        while time.time() < deadline:
            last_tree = self.ui_tree(compact=True, force_refresh=attempt > 0)
            node = _find_node(last_tree.get("nodes", []), criteria, index=index)
            if node is not None:
                return node, last_tree
            if scrolls_used < scroll_to_find:
                self._scroll_once(direction="up")
                scrolls_used += 1
                attempt += 1
                continue                          # 滚动后立即重查，不退避
            attempt += 1
            time.sleep(min(poll_interval_s * (1.5 ** max(attempt - 1, 0)), 2.0))
        raise AdbError(
            f"Element not found within {timeout_s}s ({criteria.describe()}). "
            f"Last tree had {last_tree.get('count', 0)} nodes"
            + (f", after {scrolls_used} scroll(s)" if scroll_to_find else "") + "."
        )
```

**3) `find_and_tap` 透传新参数：**

```python
    def find_and_tap(
        self,
        *,
        text=None, content_desc=None, resource_id=None, class_name=None,
        require_all: bool = False,
        exact: bool = False,
        index: int = 0,
        timeout_s: float = 10,
        poll_interval_s: float = 0.4,
        scroll_to_find: int = 0,
    ) -> dict:
        criteria = NodeCriteria(
            text=text, content_desc=content_desc,
            resource_id=resource_id, class_name=class_name,
            require_all=require_all, exact=exact,
        )
        node, _ = self._poll_for_node(
            criteria, timeout_s=timeout_s, poll_interval_s=poll_interval_s,
            index=index, scroll_to_find=scroll_to_find,
        )
        if not node.get("center"):
            raise AdbError(f"Matched node has no tappable bounds ({criteria.describe()})")
        x, y = node["center"]
        return {"ok": True, "matched": node, "tap": self.tap(x, y)}
```

**测试**
- Mock `ui_tree` 前 2 次返回不含目标、第 3 次含目标；`scroll_to_find=2` 断言最终命中且
  `swipe` 被调用 2 次。
- `scroll_to_find=0`（默认）时行为与今天一致，`swipe` 不被调用。

---

## Phase 4 — 退化树检测 + 视觉回退提示

**动机**：缺口 #5。空树 / WebView 残缺时，主动告诉 Agent "该截图"。

**文件**：`actions.py` — `ui_tree()` compact 分支返回处（当前 274-276）。

**在构造 `result` 前加：**

```python
        has_webview = any("WebView" in n.get("class", "") for n in nodes)
        # 可交互节点太少通常意味着 WebView/Compose/自绘 UI，结构化信息不足
        interactive = sum(1 for n in nodes if n.get("clickable") or n.get("text"))
        sparse = interactive < 3
        result = {"ok": True, "nodes": nodes, "count": len(nodes), "serial": serial}
        if has_webview or sparse:
            result["degraded"] = True
            result["hint"] = "UI tree looks incomplete (WebView/Compose/custom-drawn). Fall back to phone_screenshot for vision."
        self._ui_tree_cache = result
        return result
```

**同步更新工具 docstring**（`server.py:180`），让模型知道遇到 `degraded: true` 应改用
`phone_screenshot`：

```python
def phone_ui_tree(compact: bool = True) -> str:
    """Dump the UI accessibility tree as JSON nodes or raw XML.
    If the result has "degraded": true, the tree is incomplete
    (WebView/Compose/game) — call phone_screenshot and use vision instead."""
```

**测试**：全 WebView / 空节点的 XML → 断言 `result["degraded"] is True`。

---

## Phase 5 — MCP 工具签名收口 + 文档

**文件**：`server.py`（178-219）、`plugins/.../README.md` 工具表。

`phone_find_and_tap` 增参并透传：

```python
@mcp.tool()
def phone_find_and_tap(
    text: str = "", content_desc: str = "", resource_id: str = "", class_name: str = "",
    require_all: bool = False,   # 组合条件都要满足（消歧）
    exact: bool = False,         # 精确匹配
    index: int = 0,              # 命中多个时取第几个
    scroll_to_find: int = 0,     # 找不到时最多滚动几次
    timeout_s: float = 10,
) -> str:
    """Find a UI element by text / content-desc / resource-id / class, then tap it.
    require_all=True 时所有给定选择器都要命中；scroll_to_find>0 时会滚动翻找。"""
    try:
        if not any((text, content_desc, resource_id, class_name)):
            raise AdbError("Provide text, content_desc, resource_id, or class_name")
        return _ok(_get_actions().find_and_tap(
            text=text or None, content_desc=content_desc or None,
            resource_id=resource_id or None, class_name=class_name or None,
            require_all=require_all, exact=exact, index=index,
            scroll_to_find=scroll_to_find, timeout_s=timeout_s,
        ))
    except (AdbError, OSError) as exc:
        return _err(exc)
```

README 的工具表补一行说明 `phone_ui_tree` 新增的状态字段与 `degraded` 提示，
`phone_find_and_tap` 的 `require_all/exact/index/scroll_to_find` 用法各给一句示例。

---

## 合入顺序与风险

| Phase | 收益 | 风险 | 依赖 |
|---|---|---|---|
| 1 富化字段 | 高 | 低（纯新增） | 无 |
| 2 AND/exact/index | 高（可靠命中） | 低-中（`enabled_only` 默认变化） | 依赖 1 的 `enabled` 字段 |
| 3 滚动翻找 | 中 | 中（滚动可能误触，仅在显式 `scroll_to_find>0` 触发） | 依赖 2 |
| 4 退化检测 | 中 | 低 | 依赖 1 |
| 5 工具收口 | 中 | 低 | 依赖 2/3 |

**建议**：1 → 2 一起合（这两个是"能不能可靠点中正确元素"的根），验证一轮真机后再上 3/4/5。

## 验收

```bash
cd plugins/scrcpymac-phone-agent/server
python -m unittest discover -s tests -v
```

真机冒烟（覆盖每个 Phase）：
1. `phone_ui_tree` 输出含 `index` 且开关类节点带 `checked`（Phase 1）。
2. 一个有两处"设置"文案的界面，用 `resource_id`+`text` 且 `require_all=true` 精确点到目标（Phase 2）。
3. 长列表底部的项，用 `scroll_to_find=3` 能点到（Phase 3）。
4. 打开一个纯 WebView 页面（如某些 H5 活动页），`phone_ui_tree` 返回 `degraded: true`（Phase 4）。

## 明确不做（本轮范围外）

- 用 AccessibilityService 事件流替代 `uiautomator dump`（需手机端常驻组件，另开工程）。
- 手机端截图 OCR 兜底的自动闭环（Agent 侧编排即可，不进 `ui_tree`）。
- 节点父子层级树输出（`index` 已足够消歧，全量层级会显著涨 token）。

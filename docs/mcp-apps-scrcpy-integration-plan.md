# MCP Apps + ScrcpyMac 镜像集成实施方案

> 版本：1.1  
> 日期：2026-07-22  
> 状态：待实施（Phase B 需先完成 §3.5 Spike 验证）  
> 前置：Phone Agent 插件 v0.5.x、ScrcpyMac Agent Service（Phase 5 已完成）

> **v1.1 变更**：基于对 `mcp` SDK 实际源码、`@modelcontextprotocol/ext-apps` API、浏览器 Local
> Network Access 限制的核实，修正了 §3.2 Python SDK 选型说明、§A.3 API 示例、并在 Phase B 前新增
> §3.5 Spike 验证步骤。详见文末「附录 D：v1.0 审查记录」。

---

## 1. 背景与目标

### 1.1 问题

当前 Phone Agent 插件通过 **MCP tools**（`phone_screenshot`、`phone_tap` 等）让 AI 控制 Android 手机，体验已经可用，但存在两个缺口：

1. **对话内无可视化**：用户和 AI 都只能通过 tool 返回的 PNG / JSON 理解屏幕，无法在聊天界面里直接「看到并点击」手机。
2. **scrcpy 不是网页**：scrcpy 是原生 H.264 解码 + TCP 控制协议，无法像普通 MCP App 那样直接塞一段 HTML；必须在 **Agent Service 桥接层** 上暴露帧与输入。

[MCP Apps](https://github.com/modelcontextprotocol/ext-apps)（规范 `io.modelcontextprotocol/ui`，2026-01-26 Stable）允许 MCP Server 在对话里嵌入 **沙箱 iframe UI**。本方案把 ScrcpyMac 的镜像能力以 **可点击预览面板** 的形式接入 MCP Apps，同时保留 ScrcpyMac.app 作为 **主镜像窗口**。

### 1.2 目标

| 目标 | 说明 |
|------|------|
| **G1** | 在支持 MCP Apps 的宿主（Claude Desktop 等）中，提供可交互的手机预览 UI |
| **G2** | 预览 UI 优先走 ScrcpyMac Agent Service（`127.0.0.1:9477`），降级 adb 截图 |
| **G3** | 向后兼容：不支持 UI 的宿主仍使用现有 24 个 text tools |
| **G4** | 不替代 ScrcpyMac.app 原生镜像；MCP App 定位为「AI 对话内的遥控器 + 预览」 |

### 1.3 非目标

- 不在 iframe 内重跑 scrcpy 协议栈或 WebCodecs 解 H.264
- 不做云端远程镜像
- 不替代 ScrcpyMac 主窗口的低延迟视频体验
- 首期不做 Claude 网页版（无法访问 `127.0.0.1`）

---

## 2. 架构总览

### 2.1 分层与职责

```
┌─────────────────────────────────────────────────────────────────┐
│  AI 宿主（Cursor / Codex / Claude Desktop）                       │
│  ┌───────────────────────────────────────────────────────────┐  │
│  │  MCP Apps iframe（ui://phone-mirror）                      │  │
│  │  - 设备状态 / 连接检查                                      │  │
│  │  - 截图预览 或 MJPEG 流                                     │  │
│  │  - 点击 → 坐标映射 → tap                                   │  │
│  │  - 敏感操作确认（微信发送等）                                │  │
│  └───────────────────────────┬───────────────────────────────┘  │
│                              │ postMessage / MCP JSON-RPC        │
│  ┌───────────────────────────▼───────────────────────────────┐  │
│  │  Phone Agent MCP Server（Python + 可选 TS UI 构建）         │  │
│  │  - 现有 24 tools（不变）                                    │  │
│  │  - 新增 ui:// 资源 + tool _meta.ui.resourceUri             │  │
│  └───────────────────────────┬───────────────────────────────┘  │
└──────────────────────────────┼──────────────────────────────────┘
                               │ stdio MCP + HTTP（UI 直连 Agent）
┌──────────────────────────────▼──────────────────────────────────┐
│  ScrcpyMac.app Agent Service（127.0.0.1:9477）                   │
│  现有：/screenshot /tap /ui-tree …                               │
│  新增：/stream.mjpeg 或 WS /stream（Phase B）                    │
└──────────────────────────────┬──────────────────────────────────┘
                               │ H.264 解码 + scrcpy 控制
┌──────────────────────────────▼──────────────────────────────────┐
│  Android 设备                                                    │
└─────────────────────────────────────────────────────────────────┘
```

### 2.2 核心原则

1. **Plugin First**：MCP Apps UI 作为插件内 `ui/` 子目录交付，与 `.cursor-plugin` / `.codex-plugin` 同包。
2. **桥接不重写**：镜像数据来自 Agent Service 或 adb，不在 iframe 内实现 scrcpy client。
3. **双通道通信**：
   - **Tool 通道**：iframe → Host → MCP tool（`phone_tap`、`phone_screenshot`）— 兼容所有宿主。
   - **直连通道**（Phase B）：iframe → `http://127.0.0.1:9477/stream.mjpeg` — 仅本地桌面宿主，需 CSP 声明。
4. **Graceful degradation**：Agent 不可用 → adb 截图轮询；宿主不支持 MCP Apps → 纯 text tools。

---

## 3. 实施阶段

分 **Phase A（MVP）**、**Phase B（流畅流）**、**Phase C（深度集成）** 三阶段，可独立合入、独立回滚。

| 阶段 | 范围 | 依赖 | 预估改动量 |
|------|------|------|------------|
| **A** | 截图轮询 + 可点击 canvas MCP App | 现有 Agent `/screenshot` + `/tap` | 插件侧 ~400 行 TS/HTML + manifest |
| **B.0** | Spike：验证宿主 LNA 放行情况 | 无仓库改动 | 0，纯验证 |
| **B** | Agent MJPEG 流 + iframe 直连（仅当 B.0 通过） | Phase A + `AgentHTTPServer` **连接模型改造** | Swift：`AgentHTTPResponse` 从单块 `Data` 改为可流式写入的类型 + streaming 生命周期接入 `ScrcpySession`，属于小型连接层重构，非"加一个 case"；加 UI ~80 行 |
| **C** | 多面板、确认 UI、App 深链 | Phase A/B | 按需迭代 |

> **v1.1 修正**：Phase B 的工作量此前被低估为"Swift ~150 行"。实际上 `AgentHTTPServer`
> 当前是一次性请求-响应模型（收完整请求 → 调 handler → 写完整响应 → `connection.cancel()`），
> 详见 §B.1 后的架构说明；MJPEG 需要长连接流式写入，是连接处理模型的改造，不是简单新增端点。

---

## Phase A — 截图轮询 MCP App（MVP）

**动机**：验证 MCP Apps 端到端流程，零 Swift 改动，最快在 Claude Desktop 看到可点击预览。

### A.1 目录结构（新增）

```
plugins/scrcpymac-phone-agent/
├── ui/
│   ├── phone-mirror/
│   │   ├── index.html          # MCP App 入口（text/html;profile=mcp-app）
│   │   ├── mirror.ts           # View 逻辑：轮询、绘制、点击映射
│   │   ├── package.json        # @modelcontextprotocol/ext-apps
│   │   └── vite.config.ts      # 构建为单文件 HTML bundle
│   └── device-status/
│       └── index.html          # 轻量：doctor 结果可视化（可选 Phase A.2）
├── server/phone_agent/
│   ├── mcp_ui.py               # 注册 ui:// 资源（FastMCP resource handler）
│   └── server.py               # tool 元数据 _meta.ui.resourceUri
└── scripts/
    └── build-ui.sh             # npm run build → 嵌入 server 或 ui/dist/
```

### A.2 MCP 资源与 Tool 关联

#### UI Resource 声明

```python
# server/phone_agent/mcp_ui.py

PHONE_MIRROR_URI = "ui://scrcpymac/phone-mirror"

def register_ui_resources(mcp: FastMCP) -> None:
    @mcp.resource(PHONE_MIRROR_URI)
    def phone_mirror_ui() -> str:
        return _load_built_html("phone-mirror/index.html")

    # Resource metadata（FastMCP / MCP SDK 支持 _meta 时）
    # mimeType: text/html;profile=mcp-app
    # _meta.ui.csp.connectDomains: ["http://127.0.0.1:9477"]  # Phase B 预留
```

#### Tool 元数据

为以下 tools 关联 UI（首期至少 `phone_screenshot`）：

```python
@mcp.tool(
    meta={
        "ui": {
            "resourceUri": "ui://scrcpymac/phone-mirror",
        }
    }
)
def phone_screenshot(include_image: bool = True):
    ...
```

可选关联：`phone_device_info`、`phone_doctor` → `ui://scrcpymac/device-status`。

> **⚠️ SDK 选型说明（v1.1 修正）**：本插件当前依赖 `mcp>=1.0.0`，`server.py` 中
> `from mcp.server.fastmcp import FastMCP` 引入的是官方 `mcp` 包内**冻结的 FastMCP 1.0**，
> 与网上文档/示例常引用的独立 `fastmcp` 包（`pip install fastmcp`，PrefectHQ/jlowin 维护，
> 现为完全不同的活跃项目，提供 `@mcp.tool(ui=ToolUI(...))` 等便捷 API）**不是同一套代码**。
> 实测 `mcp==1.28.1`：`FastMCP.tool()` / `.resource()` 均有通用 `meta: dict[str, Any]`
> 参数（`.resource()` 还有显式 `mime_type` 参数），且 `Tool.meta` 字段确实以 `alias="_meta"`
> 声明——**理论上可以手工拼 `meta={"ui": {...}}` 沿用现有依赖**，但没有自动
> `ui://` MIME 默认值、没有 `ctx.client_supports_extension()`、也没有对
> `io.modelcontextprotocol/ui` 扩展能力的自动协商声明（这些便捷能力只在独立 `fastmcp`
> 包里，对应 `PrefectHQ/fastmcp` PR #3009）。
>
> 实施前需二选一并在 Phase A 任务里显式排期：
> 1. **保留官方 `mcp` 包**：手工在 `get_capabilities()` / 低层 `Server` 上补
>    `experimental_capabilities={"io.modelcontextprotocol/ui": {}}`，并手工验证
>    `model_dump(by_alias=True)` 输出的 wire JSON 确实带 `_meta`（不同 pydantic 版本
>    行为可能有差异，需要写一个序列化单测直接断言 JSON-RPC payload）。
> 2. **迁移到独立 `fastmcp` 包**：改动集中在 import 路径（官方迁移指南称多数场景是
>    "single import change"），换来 `ui=ToolUI(...)` 等官方支持的便捷 API，但需要为现有
>    24 个 tools 跑一次完整回归（`server/tests/`），避免行为差异引入的隐性 bug。
>
> 建议：Phase A 落地时先选路线 1（改动面最小），只有确认能力协商/MIME 细节踩坑较多时再评估路线 2。

### A.3 View 实现要点（`mirror.ts`）

使用 `@modelcontextprotocol/ext-apps` 的 `App` 类：

```typescript
import { App } from "@modelcontextprotocol/ext-apps";

const app = new App();

app.oninit = async () => {
  const canvas = document.getElementById("phone") as HTMLCanvasElement;
  const ctx = canvas.getContext("2d")!;

  // 1. 启动时拉 device_info（经 host 调 tool）
  const info = await app.callServerTool({ name: "phone_device_info", arguments: {} });
  const { width, height } = info.screen;
  canvas.width = width;
  canvas.height = height;

  // 2. 轮询截图（300–500ms，约 2–3 fps）
  setInterval(async () => {
    const shot = await app.callServerTool({
      name: "phone_screenshot",
      arguments: { include_image: true },
    });
    if (shot.image) drawBase64PNG(ctx, shot.image);
  }, 400);

  // 3. 点击 → 设备像素坐标 → phone_tap
  canvas.onclick = async (e) => {
    const rect = canvas.getBoundingClientRect();
    const scaleX = width / rect.width;
    const scaleY = height / rect.height;
    const x = Math.round((e.clientX - rect.left) * scaleX);
    const y = Math.round((e.clientY - rect.top) * scaleY);
    await app.callServerTool({ name: "phone_tap", arguments: { x, y } });
  };
};
```

> **⚠️ API 修正（v1.1）**：`@modelcontextprotocol/ext-apps` 的 `App` 类没有 `callTool()`
> 方法，正确方法是 **`callServerTool({ name, arguments })`**，返回 `CallToolResult`
> （`.content` / `.structuredContent` / `.isError`）。上面 `phone_screenshot` 的调用同理
> 应写作 `app.callServerTool({ name: "phone_screenshot", arguments: { include_image: true } })`。
> 实施时以 `src/app.ts`（`modelcontextprotocol/ext-apps` 仓库）里的真实签名为准，不要照抄旧草稿。

**坐标映射**：复用现有 `phone_tap_relative` / `phone_tap_image` 的 aspect-fit 逻辑；canvas 若 letterbox 显示，需在 JS 中扣除黑边（与 `MirrorView.swift` 一致）。

### A.4 构建与打包

```bash
# scripts/build-ui.sh
cd ui/phone-mirror && npm ci && npm run build
# 输出 dist/index.html → 复制到 server/phone_agent/static/phone-mirror.html
```

CI（`.github/workflows/ci.yml`）增加：

```yaml
- name: Build MCP App UI
  run: plugins/scrcpymac-phone-agent/scripts/build-ui.sh
```

插件 manifest **无需改路径**；UI 由 MCP server 作为 resource 提供。

### A.5 Phase A 验收

- [ ] 在**已确认支持 MCP Apps 的宿主**（Claude Desktop，或 `@modelcontextprotocol/ext-apps`
      官方 sample host / MCP Inspector）安装插件后，调用 `phone_screenshot` 出现 iframe 预览
      —— Cursor/Codex 的支持现状未经验证，不作为首个验证目标
- [ ] iframe 内点击能触发 `phone_tap`，设备响应正确
- [ ] Agent 未运行时，预览仍可用（adb 截图，帧率略低）
- [ ] 不支持 MCP Apps 的宿主调用 `phone_screenshot` 行为与现版一致（仅 PNG + JSON）
- [ ] `npm run build` + 单元测试通过

---

## Phase B — Agent MJPEG 流（流畅预览）

**动机**：轮询 PNG 仅 2–3 fps；Agent 已从 H264 解码器取帧，增加 **multipart MJPEG** 即可在 iframe 内接近实时镜像。

### B.0 先做 Spike：验证宿主是否放行 iframe 访问 localhost（v1.1 新增，阻塞性前置步骤）

**这是 Phase B 立项前必须先跑通的一步，否则后面的 Swift 工作可能全部白做。**

**背景**：Chrome 147+（及基于新版 Chromium 的 Electron 应用——多数现代桌面 AI 客户端属于此列）
对 iframe 访问 loopback/私有网络地址启用了 **Local Network Access（LNA）** 限制。嵌套 iframe
（MCP App View 正是嵌套 iframe）要访问 `127.0.0.1`，**必须由父 frame 显式声明
`allow="loopback-network"`（或 `allow="local-network-access"`）权限委托** ——这与 MCP 资源里
声明的 `_meta.ui.csp.connectDomains` 是两套独立机制：CSP 决定"服务器允许 App 连什么"，
LNA 决定"浏览器允许 iframe 连什么"，**两者都要放行才能连通**。

`allow` 属性由**宿主（Claude Desktop / Cursor / Codex）渲染 iframe 时决定**，插件侧的
MCP 资源声明无法控制它。目前没有已知证据表明任何 MCP Apps 宿主会给 View iframe 委托
loopback 权限。

**Spike 步骤**（预计 0.5–1 天，不涉及任何仓库改动）：

1. 用 MCP Inspector 或 `@modelcontextprotocol/ext-apps` 官方 quickstart 起一个最小 UI resource，
   内嵌一行 `fetch("http://127.0.0.1:<临时端口>/ping")`。
2. 在目标宿主（优先 Claude Desktop，其次任何已知支持 MCP Apps 的 host）里触发该 UI。
3. 观察浏览器 / Electron devtools 是否报 `net::ERR_BLOCKED_BY_PRIVATE_NETWORK_ACCESS_CHECK`
   或类似 LNA 拦截日志。

**分支决策**：

| Spike 结果 | 后续动作 |
|------------|----------|
| 放行，能连通 localhost | 按下文 B.1–B.2 正常推进 Swift MJPEG 实现 |
| 被拦截，且宿主未提供绕过方式 | **砍掉 Phase B 的直连方案**，改为：(a) 继续用 Phase A 轮询但把间隔调短、或 (b) MCP App 内放一个"在 ScrcpyMac.app 中查看实时镜像"的按钮/深链（见 Phase C.3），把连续视频的诉求交还给原生 App 窗口 |

不要在 Spike 之前投入 §B.1 的 Swift 改动。

### B.1 Agent Service 新端点

**文件**：`ScrcpyMac/AgentService.swift`、`ScrcpyMac/H264Decoder.swift`

| 方法 | 路径 | 说明 |
|------|------|------|
| `GET` | `/stream.mjpeg` | `multipart/x-mixed-replace; boundary=frame`，每帧 JPEG |
| `GET` | `/stream/info` | JSON：`{ fps, width, height, serial, format: "mjpeg" }` |

#### Swift 实现 sketch

```swift
// H264Decoder.swift — 新增
func latestFrameJPEG(quality: CGFloat = 0.75) -> Data? {
    guard captureEnabled, let ciImage = latestCIImage else { return nil }
    // CIContext.jpegRepresentation(of:quality:)
}

// AgentService.swift
case ("GET", "/stream.mjpeg"):
    return await mjpegStream(session: session)

private static func mjpegStream(session: ScrcpySession?) async -> AgentHTTPResponse {
    // 长连接：循环 latestFrameJPEG()，写入 multipart body
    // 目标 15–24 fps，可配置 X-ScrcpyMac-FPS header
}
```

**注意（v1.1 详细化）**：读过 `AgentHTTPServer.swift` 现有实现后确认，这不是"加一个 case"级别的改动：

- `receive()` 把整条请求攒进内存后才调用 `handler`；`send()` 写完整个响应体后，在
  `NWConnection.send` 的 completion closure 里**直接 `connection.cancel()`**——即当前架构里
  "一次 handler 调用 = 一次完整响应 = 连接关闭"是硬编码的。
- MJPEG 需要"一次 handler 调用 = header 写一次 + body 持续追加，直到客户端断开或 Agent 停止"，
  这是完全不同的生命周期。改造点至少包括：
  1. 新增一个 streaming 专用响应类型（区别于现在单块 `Data` 的 `AgentHTTPResponse`），或让
     `handler` 签名支持"拿到 `NWConnection` 自己写"而不是返回一个值。
  2. `send()` 不能对 streaming 连接调用 `cancel()`；需要新的循环：写一帧 → 等 completion →
     检查 stop 信号 → 写下一帧。
  3. 停止信号要接入 `AgentService.stop()` / `session?.setAgentCaptureEnabled(false)`，
     确保 App 侧关闭 Agent 或断开设备时，streaming 循环能干净退出，不留悬挂连接。
  4. 需要限流（比如 15–24 fps 循环里 `try await Task.sleep`），避免无限抢占 `queue`。

**性能**：JPEG 编码在后台队列，与 `captureDecoderFramePNG()` 共用 `latestCIImage` 缓存，避免重复解码。

### B.2 MCP App UI 切换数据源

```typescript
// mirror.ts — Phase B
const agentAvailable = await fetch("http://127.0.0.1:9477/health").then(r => r.ok);

if (agentAvailable) {
  // <img src="http://127.0.0.1:9477/stream.mjpeg"> 或 fetch + blob URL
  img.src = "http://127.0.0.1:9477/stream.mjpeg";
  img.onclick = mapClickToTap;  // 仍走 app.callServerTool({ name: "phone_tap", arguments })
} else {
  startPollingScreenshots();    // Phase A 降级
}
```

#### Resource CSP 元数据

```json
{
  "uri": "ui://scrcpymac/phone-mirror",
  "_meta": {
    "ui": {
      "csp": {
        "connectDomains": ["http://127.0.0.1:9477"],
        "resourceDomains": ["http://127.0.0.1:9477"]
      }
    }
  }
}
```

### B.3 Python 侧

`AgentClient` 可选增加 `stream_info()` 供 tool 返回给 Host；MCP App iframe **直连 localhost**，不强制经 Python 中转。

### B.4 Phase B 验收

- [ ] **§B.0 Spike 已通过**（目标宿主确认放行 iframe → localhost），否则本阶段不应开工
- [ ] ScrcpyMac 连接设备 + Agent 开启时，iframe 内 MJPEG ≥ 15 fps（1080p 设备）
- [ ] 点击 MJPEG 画面坐标正确映射到 `phone_tap`
- [ ] Agent 关闭后 UI 自动降级 Phase A 轮询
- [ ] `AgentHTTPServer` 单客户端 streaming 不阻塞 `/screenshot` 等 API
- [ ] 内存：长时间 streaming 无泄漏（CI 可加 60s soak 测试）

---

## Phase C — 深度集成（可选）

| 子项 | 说明 | 文件 |
|------|------|------|
| **C.1 微信确认 UI** | `phone_send_wechat` 关联 `ui://scrcpymac/wechat-confirm`，展示联系人/正文，用户点发送 | `ui/wechat-confirm/` |
| **C.2 多设备选择器** | `phone_list_devices` → 下拉选 serial，写入 `PHONE_AGENT_SERIAL` | `ui/device-picker/` |
| **C.3 App 深链** | UI 按钮「在 ScrcpyMac 中打开」→ `scrcpymac://connect?serial=…`（需 App 注册 URL scheme） | `ScrcpyMac/ScrcpyMacApp.swift` |
| **C.4 Codex/Cursor UI 适配** | 按各宿主 MCP Apps 支持情况测试 CSP / iframe 尺寸 | 文档 + 截图 |

---

## 4. 仓库改动清单

### 4.1 插件（Phase A 必做）

| 文件 | 动作 |
|------|------|
| `plugins/scrcpymac-phone-agent/ui/phone-mirror/*` | 新增 MCP App 源码 |
| `plugins/scrcpymac-phone-agent/server/phone_agent/mcp_ui.py` | 新增 UI resource 注册 |
| `plugins/scrcpymac-phone-agent/server/phone_agent/server.py` | tool `_meta.ui.resourceUri` |
| `plugins/scrcpymac-phone-agent/scripts/build-ui.sh` | UI 构建脚本 |
| `plugins/scrcpymac-phone-agent/server/pyproject.toml` | 如需静态文件路径依赖 |
| `plugins/scrcpymac-phone-agent/.cursor-plugin/plugin.json` | 可选：`ui` 字段说明 |
| `plugins/scrcpymac-phone-agent/.codex-plugin/plugin.json` | 更新 longDescription |
| `plugins/scrcpymac-phone-agent/README.md` | MCP Apps 使用说明 |
| `plugins/scrcpymac-phone-agent/CHANGELOG.md` | 版本记录 |
| `.github/workflows/ci.yml` | UI build + 测试 |

### 4.2 ScrcpyMac App（Phase B）

| 文件 | 动作 |
|------|------|
| `ScrcpyMac/H264Decoder.swift` | `latestFrameJPEG()` |
| `ScrcpyMac/AgentService.swift` | `/stream.mjpeg`、`/stream/info` |
| `ScrcpyMac/AgentHTTPServer.swift` | streaming 长连接支持 |
| `ScrcpyMac/AgentService.swift` 或 XCTest | streaming 集成测试（mock session） |

### 4.3 文档

| 文件 | 动作 |
|------|------|
| `docs/mcp-apps-scrcpy-integration-plan.md` | 本文档 |
| `docs/architecture.md` | 补充 MCP Apps 层示意图 |
| `plugins/scrcpymac-phone-agent/skills/scrcpymac-link/SKILL.md` | 说明对话内预览 vs App 镜像 |

---

## 5. 安全与合规

### 5.1 MCP Apps 沙箱

- View 运行在 sandbox iframe，无 DOM/ cookie 访问宿主
- 通信仅 `postMessage` + 声明的 MCP JSON-RPC
- localhost 直连仅用于 **本机已授权** 的 Agent Service

### 5.2 CSP

| 域名 | 用途 | 阶段 |
|------|------|------|
| `http://127.0.0.1:9477` | MJPEG / health | B |
| （无） | Phase A 仅经 host `callServerTool`，无额外 connect | A |

### 5.3 隐私

- 屏幕内容仍在本地；MJPEG 不离开本机
- 与现有 [PRIVACY.md](../plugins/scrcpymac-phone-agent/PRIVACY.md) 一致，补充「MCP Apps iframe 渲染」说明

### 5.4 敏感操作

- `phone_send_wechat`、转账类 Skill 在 Phase C 要求 **UI 确认** 或 Skill 内显式用户确认，与现有安全原则一致

---

## 6. 宿主兼容矩阵

| 宿主 | MCP Tools | MCP Apps Phase A | MJPEG localhost Phase B |
|------|-----------|------------------|---------------------------|
| Claude Desktop | ✅ | ✅ 预期 | ✅ |
| Claude Web | ✅ | ⚠️ UI 可显示，localhost 不可用 | ❌ |
| Codex Desktop | ✅ | 🔶 视版本 | 🔶 视版本 |
| Cursor | ✅ | 🔶 视版本 | 🔶 视版本 |
| 纯 MCP 配置 | ✅ | ❌ 降级 text | ❌ |

**规范要求**：UI 不可用时 tools 仍返回完整 text/image 结果。

---

## 7. 测试计划

### 7.1 单元测试

| 模块 | 内容 |
|------|------|
| `mcp_ui.py` | resource URI 返回非空 HTML；mime type 正确 |
| `build-ui.sh` | CI 构建产物存在 |
| `AgentClient` | `/stream/info` 解析（Phase B） |
| `H264Decoder` | JPEG 输出非空（Phase B，mock frame） |

### 7.2 集成测试（需真机 + ScrcpyMac）

| 场景 | 步骤 | 预期 |
|------|------|------|
| A-1 | Host 调 `phone_screenshot`，UI 渲染 | iframe 显示最新屏幕 |
| A-2 | iframe 内点击图标 | 设备打开对应 App |
| A-3 | Agent 关闭，仅 adb | 轮询仍更新，延迟可接受 |
| B-1 | Agent + MJPEG | iframe ≥ 15 fps |
| B-2 | 长时间 streaming 5min | 无 crash、内存稳定 |

### 7.3 手动清单（发布前）

- [ ] `./scripts/build-ui.sh` 成功
- [ ] `./bin/phone-agent doctor` 通过
- [ ] Claude Desktop ext-apps 或官方 sample host 验证 Phase A
- [ ] 更新 `assets/screenshots/` 含 MCP App 预览截图

---

## 8. 风险与缓解

| 风险 | 影响 | 缓解 |
|------|------|------|
| 官方 `mcp` 包（FastMCP 1.0）无 `io.modelcontextprotocol/ui` 能力协商/MIME 默认 | UI resource 注册不完全合规 | 手工补 `experimental_capabilities` + `mime_type`；写序列化单测验证 `_meta` wire 格式；必要时评估迁移到独立 `fastmcp` 包（见 §3.2 说明） |
| **iframe Local Network Access（LNA）拦截 localhost 请求**（高概率，Chrome 147+ / 新版 Chromium Electron 默认行为） | **Phase B 直连方案可能完全不可行**，与 Swift 实现质量无关 | **先做 §B.0 Spike**，不通过就砍掉直连方案，改走 Phase A 轮询或 App 深链 |
| `AgentHTTPServer` 连接模型改造引入的稳定性风险（长连接不清理、streaming 阻塞其他 API） | Agent 服务不稳定 | 独立 streaming 生命周期 + 停止信号；加 60s soak 测试（见 §7.2 B-2） |
| 坐标 letterbox 映射错误 | 点击偏移 | 复用 `phone_tap_image` 算法；加 E2E 测试 |
| 各宿主 MCP Apps 支持不一（Cursor/Codex 现状未经验证） | 体验不一致，甚至 Phase A 都可能无法渲染 | Phase A 先在**已知支持**的宿主（Claude Desktop / ext-apps 官方 sample host / MCP Inspector）验证，再扩展到 Cursor/Codex；保持 text fallback |

---

## 9. 版本与发布

### 9.1 版本号建议

| 里程碑 | 版本 | 内容 |
|--------|------|------|
| Phase A 完成 | **0.6.0** | MCP Apps 截图面板 |
| Phase B 完成 | **0.7.0** + App 小版本 | MJPEG + App Agent streaming |
| Phase C | **0.8.x** | 确认 UI、多设备 |

### 9.2 发布检查

1.  bump `plugin.json` / `__init__.py` / `CHANGELOG.md`
2.  CI 绿
3.  `MARKETPLACE.md` 补充 MCP Apps 截图
4.  ScrcpyMac DMG 若含 Phase B，同步 bump App 版本

---

## 10. 实施顺序（Immediate Actions）

1. **创建 `ui/phone-mirror`** — 从 `@modelcontextprotocol/ext-apps` quickstart _fork_
2. **实现 `mcp_ui.py` + `build-ui.sh`** — 本地 Claude / ext-apps sample host 验证
3. **为 `phone_screenshot` 添加 `_meta.ui.resourceUri`** — 端到通
4. **文档 + CHANGELOG 0.6.0** — Phase A 合入 master
5. **Swift `/stream.mjpeg`** — Phase B 独立 PR
6. **Phase C 按需求排期**

---

## 附录 A：Tool ↔ UI 映射表

| Tool | UI Resource | 阶段 |
|------|-------------|------|
| `phone_screenshot` | `ui://scrcpymac/phone-mirror` | A |
| `phone_device_info` | `ui://scrcpymac/device-status` | A.2 |
| `phone_doctor` | `ui://scrcpymac/device-status` | A.2 |
| `phone_send_wechat` | `ui://scrcpymac/wechat-confirm` | C |

---

## 附录 B：Agent API 增量（Phase B）

```
GET /stream/info
→ 200 application/json
  { "ok": true, "format": "mjpeg", "fps": 20, "width": 1080, "height": 2400, "serial": "..." }

GET /stream.mjpeg
→ 200 multipart/x-mixed-replace; boundary=frame
  --frame
  Content-Type: image/jpeg
  Content-Length: ...

  <jpeg bytes>
  --frame
  ...
```

---

## 附录 C：参考链接

| 资源 | URL |
|------|-----|
| MCP Apps 规范 | https://github.com/modelcontextprotocol/ext-apps/blob/main/specification/2026-01-26/apps.mdx |
| ext-apps SDK（`App` 类真实签名） | https://github.com/modelcontextprotocol/ext-apps/blob/main/src/app.ts |
| ext-apps CSP/CORS 文档 | https://github.com/modelcontextprotocol/ext-apps/blob/main/docs/csp-cors.md |
| 独立 fastmcp（PrefectHQ）MCP Apps 支持 PR | https://github.com/PrefectHQ/fastmcp/pull/3009 |
| Chrome Local Network Access 说明（LNA 拦截 iframe 案例） | https://github.com/webflow/mcp-server/issues/124 |
| 现有 Agent Service | `ScrcpyMac/AgentService.swift` |
| 现有 Agent HTTP 服务器（连接模型） | `ScrcpyMac/AgentHTTPServer.swift` |
| Phone Agent 插件计划 | [phone-agent-plugin-plan.md](./phone-agent-plugin-plan.md) |
| 架构 | [architecture.md](./architecture.md) |

---

## 附录 D：v1.0 → v1.1 审查记录（2026-07-22）

对 v1.0 方案做了一轮基于真实代码/规范/依赖的核实，发现并修正以下问题：

| 编号 | v1.0 假设 | 核实方式 | 结论 |
|------|-----------|----------|------|
| D-1 | `app.callTool(name, args)` 是 ext-apps 正确 API | 读取 `ext-apps` 仓库 `src/app.ts` 源码 | **错误**，正确方法是 `callServerTool({ name, arguments })`，已修正全部代码示例 |
| D-2 | "FastMCP 支持 `_meta.ui`" 笼统成立 | 下载 `mcp==1.28.1` wheel，反查 `FastMCP.tool()`/`.resource()` 签名与 `Tool.meta` 字段定义 | **部分成立**：本项目实际用的是官方 `mcp` 包冻结的 FastMCP 1.0，有通用 `meta` dict 可手工拼，但没有独立 `fastmcp`（PrefectHQ）包的能力协商/MIME 默认便捷 API；两者是不同代码库，已在 §3.2 补充二选一决策 |
| D-3 | Phase B 直连 `127.0.0.1:9477` 只需服务端声明 CSP `connectDomains` | 检索 Chrome Local Network Access（LNA）机制与真实踩坑 issue | **不成立/高风险**：现代 Chromium（含多数 Electron 桌面宿主）要求父 frame 显式 `allow="loopback-network"` 才放行嵌套 iframe 访问 loopback，CSP 声明管不到这层；已新增 §B.0 Spike 作为阻塞性前置步骤 |
| D-4 | Swift 端 MJPEG 端点约 "150 行" | 通读 `AgentHTTPServer.swift` 现有 `receive`/`send` 实现 | **低估**：当前是一次性请求-响应模型，`send()` 完成后即 `cancel()` 连接；MJPEG 需要长连接流式写入，属于连接模型的小型重构，已在 §B.1 补充具体改造点 |
| D-5 | Cursor/Codex 对 MCP Apps 的支持与 Claude 同等 | 复用此前调研（"支持varies，部分渐进"） | **未证实**：已把兼容矩阵与验收标准改为"先在已知支持宿主验证" |

**未变动、经核实成立的部分**：MCP Apps 规范本身（`ui://`、`text/html;profile=mcp-app`、
postMessage JSON-RPC）、CSP `connectDomains`/`resourceDomains` 机制、Phase A 复用现有
Agent Service `/screenshot` + `/tap` 零 Swift 改动的可行性、graceful degradation 原则。

---

*文档维护：每完成一个 Phase 更新状态与验收清单。*

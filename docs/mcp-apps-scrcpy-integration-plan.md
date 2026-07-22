# MCP Apps + ScrcpyMac 镜像集成实施方案

> 版本：1.0  
> 日期：2026-07-22  
> 状态：待实施  
> 前置：Phone Agent 插件 v0.5.x、ScrcpyMac Agent Service（Phase 5 已完成）

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
| **B** | Agent MJPEG 流 + iframe 直连 | Phase A + Swift Agent 新端点 | Swift ~150 行 + UI ~80 行 |
| **C** | 多面板、确认 UI、App 深链 | Phase A/B | 按需迭代 |

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

### A.3 View 实现要点（`mirror.ts`）

使用 `@modelcontextprotocol/ext-apps` 的 `App` 类：

```typescript
import { App } from "@modelcontextprotocol/ext-apps";

const app = new App();

app.oninit = async () => {
  const canvas = document.getElementById("phone") as HTMLCanvasElement;
  const ctx = canvas.getContext("2d")!;

  // 1. 启动时拉 device_info（经 host 调 tool）
  const info = await app.callTool("phone_device_info", {});
  const { width, height } = info.screen;
  canvas.width = width;
  canvas.height = height;

  // 2. 轮询截图（300–500ms，约 2–3 fps）
  setInterval(async () => {
    const shot = await app.callTool("phone_screenshot", { include_image: true });
    if (shot.image) drawBase64PNG(ctx, shot.image);
  }, 400);

  // 3. 点击 → 设备像素坐标 → phone_tap
  canvas.onclick = async (e) => {
    const rect = canvas.getBoundingClientRect();
    const scaleX = width / rect.width;
    const scaleY = height / rect.height;
    const x = Math.round((e.clientX - rect.left) * scaleX);
    const y = Math.round((e.clientY - rect.top) * scaleY);
    await app.callTool("phone_tap", { x, y });
  };
};
```

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

- [ ] Claude Desktop（或 ext-apps 示例 host）安装插件后，调用 `phone_screenshot` 出现 iframe 预览
- [ ] iframe 内点击能触发 `phone_tap`，设备响应正确
- [ ] Agent 未运行时，预览仍可用（adb 截图，帧率略低）
- [ ] 不支持 MCP Apps 的宿主调用 `phone_screenshot` 行为与现版一致（仅 PNG + JSON）
- [ ] `npm run build` + 单元测试通过

---

## Phase B — Agent MJPEG 流（流畅预览）

**动机**：轮询 PNG 仅 2–3 fps；Agent 已从 H264 解码器取帧，增加 **multipart MJPEG** 即可在 iframe 内接近实时镜像。

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

**注意**：`AgentHTTPServer` 当前为 request/response 短连接；MJPEG 需：

- 保持 connection 不 `Connection: close`，或
- 单独 `NWConnection` 处理 streaming（推荐在 `AgentHTTPServer` 增加 `streamHandler` 分支）

**性能**：JPEG 编码在后台队列，与 `captureDecoderFramePNG()` 共用 `latestCIImage` 缓存，避免重复解码。

### B.2 MCP App UI 切换数据源

```typescript
// mirror.ts — Phase B
const agentAvailable = await fetch("http://127.0.0.1:9477/health").then(r => r.ok);

if (agentAvailable) {
  // <img src="http://127.0.0.1:9477/stream.mjpeg"> 或 fetch + blob URL
  img.src = "http://127.0.0.1:9477/stream.mjpeg";
  img.onclick = mapClickToTap;  // 仍走 app.callTool("phone_tap")
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
| （无） | Phase A 仅经 host callTool，无额外 connect | A |

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
| FastMCP 对 `_meta.ui` 支持不完整 | UI 无法注册 | 降级手写 MCP resource handler；参考 ext-apps Python 示例 |
| iframe localhost CSP 被宿主拒绝 | Phase B 不可用 | Phase A 纯 tool 轮询仍可用 |
| MJPEG 长连接阻塞 Agent | 其他 API 变慢 | 独立 connection handler；限流 fps |
| 坐标 letterbox 映射错误 | 点击偏移 | 复用 `phone_tap_image` 算法；加 E2E 测试 |
| 各宿主 MCP Apps 支持不一 | 体验不一致 | 文档标明；保持 text fallback |

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
| ext-apps SDK | https://www.npmjs.com/package/@modelcontextprotocol/ext-apps |
| 现有 Agent Service | `ScrcpyMac/AgentService.swift` |
| Phone Agent 插件计划 | [phone-agent-plugin-plan.md](./phone-agent-plugin-plan.md) |
| 架构 | [architecture.md](./architecture.md) |

---

*文档维护：每完成一个 Phase 更新状态与验收清单。*

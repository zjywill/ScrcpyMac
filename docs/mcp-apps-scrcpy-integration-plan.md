# ScrcpyMac 完整集成到 Codex 插件实施方案

> 版本：2.2
> 日期：2026-07-22  
> 状态：独立 H.264 runtime 已完成，Codex Widget E2E 验收中
> 目标插件版本：0.7.2

---

## 1. 产品目标

用户安装 **ScrcpyMac Phone Agent Codex 插件** 后，应能直接在 Codex 任务中打开完整的
ScrcpyMac 工作区，不需要另外安装或打开 `ScrcpyMac.app`：

- 发现并选择 USB / Wi-Fi Android 设备
- 连接、停止和查看连接状态
- 在 Codex 内查看手机画面
- 鼠标点击、拖动滑动、Android 导航键
- 键盘与剪贴板输入
- Stay awake、Turn screen off、Audio only、Video only
- 查看运行日志和错误状态
- AI 与用户操作同一个手机会话

最终验收定义：

> 一台没有安装 `ScrcpyMac.app` 的 Mac，只安装 Codex 插件，也能在 Codex 中打开
> ScrcpyMac、启动原生 scrcpy 会话、看到低延迟画面并控制手机。

## 2. 非目标

- 不把 SwiftUI / AppKit 视图二进制嵌进 iframe
- 不要求 Web UI 自己实现 adb 或直接连接 Android TCP 端口
- 不做云端手机镜像
- 不在第一阶段同时保证 Cursor / Claude 的完整 UI 一致性
- 不移除现有 24 个 model-visible MCP tools

Codex 是完整 UI 的首要宿主；其他 MCP host 保持工具兼容，后续再验证 UI。

## 2.1 对标参考：Claude Code iOS Simulator

ClaudeDevs 在 2026-07-21 展示的 Claude Code Desktop iOS Simulator public beta：

- Simulator 作为对话右侧的专用设备面板，不脱离当前 coding task
- 显式提供 `60 FPS`、`Resolution 50%`、`Encoding H.264`
- 显示 “Claude is using this device”，让用户知道当前控制权归属
- 面板工具栏提供 Home、截图、录屏、旋转和外部打开
- Agent 操作后使用 accessibility tree 验证真实设备状态

参考：
`https://x.com/ClaudeDevs/status/2079674432038248611`

对 ScrcpyMac 的结论：

- 50% 分辨率是合理的默认预览档；1080px 设备默认传 540px
- JPEG screenshot polling 只能作为兼容 fallback，不能作为最终镜像架构
- 正式路径必须传输连续 H.264 帧并在 Widget 中解码
- 用户操作和 AI 操作必须共享一个带控制权状态的设备 session
- Widget 需要显示实际 FPS、分辨率、编码和 backend，不能只显示“running”

---

## 3. 架构

```text
┌──────────────────────────────────────────────────────────────┐
│ Codex Desktop                                                 │
│                                                              │
│  ui://widget/scrcpymac/app.html                              │
│  ┌────────────────────────────────────────────────────────┐  │
│  │ ScrcpyMac Web UI                                       │  │
│  │ - Device / Wi-Fi                                       │  │
│  │ - Mirror / touch / keyboard                            │  │
│  │ - Session options / logs                               │  │
│  └─────────────────────────┬──────────────────────────────┘  │
│                            │ MCP Apps host bridge             │
└────────────────────────────┼─────────────────────────────────┘
                             ▼
┌──────────────────────────────────────────────────────────────┐
│ Phone Agent MCP Server（Python）                              │
│                                                              │
│ - open_scrcpymac：唯一 widget 打开入口                        │
│ - scrcpymac_ui_*：app-only structured tools                  │
│ - phone_*：现有 model-visible tools                          │
│ - adb / scrcpy-server lifecycle                              │
│ - video / control socket                                     │
│ - token-protected loopback WebSocket                         │
│ - cleanup and backend fallback                               │
└────────────────────────────┬─────────────────────────────────┘
                             ▼
                       Android device
```

### 3.1 UI 层

SwiftUI 不能直接运行在 MCP App 的 sandbox iframe 中，因此现有
`ContentView.swift` 需要按功能重做为 HTML / TypeScript UI。

UI 使用 `@modelcontextprotocol/ext-apps`：

```ts
const app = new App(
  { name: "scrcpymac", version: "0.7.2" },
  { availableDisplayModes: ["inline", "fullscreen"] },
  { autoResize: true },
);

await app.connect();
await app.requestDisplayMode({ mode: "fullscreen" });
```

Widget 只能通过 `app.callServerTool()` 操作 MCP server。UI 不读取 Codex DOM、
cookie 或未声明的网络资源。

### 3.2 MCP 工具分层

#### Widget 打开工具

```text
open_scrcpymac
  _meta.ui.resourceUri = ui://widget/scrcpymac/app.html
```

只有该工具关联 UI resource。截图轮询和用户输入工具不关联 resource，避免内部调用反复创建
widget。

#### App-only 工具

```text
scrcpymac_ui_state
scrcpymac_ui_select_device
scrcpymac_ui_start_stream
scrcpymac_ui_stream_status
scrcpymac_ui_stop_stream
scrcpymac_ui_snapshot
scrcpymac_ui_tap
scrcpymac_ui_swipe
scrcpymac_ui_key
scrcpymac_ui_paste
scrcpymac_ui_connect_wifi
```

这些工具声明：

```json
{
  "_meta": {
    "ui": {
      "visibility": ["app"]
    }
  }
}
```

返回值使用 `structuredContent`，大图数据不重复写进 text content。

#### Agent tools

原有 `phone_*` 工具继续对模型可见，输出合同保持兼容。模型和 widget 最终必须共享同一个
runtime session，而不是分别启动两个 scrcpy server。

### 3.3 独立 runtime

0.7.2 不再从 App target 抽取或启动 Swift helper。Python MCP server 直接实现必要的
scrcpy 3.3.4 客户端协议，并独立拥有：

- 插件内置 `scrcpy-server`
- adb push / dynamic port forward / server process
- video 与 control socket 握手
- H.264 packet relay
- tap / swipe / key / clipboard control message
- session token、loopback WebSocket 和 teardown

因此 App 与插件在进程、API、资源路径和生命周期上彻底分开。后续只有音频、本地录屏等确实
需要 macOS media framework 的功能，才评估额外 native helper。

### 3.4 视频传输

按三级实现：

#### 兼容 fallback：ADB screenshot polling

- 默认 50% 宽度，1080px 设备输出 540px JPEG
- 快速 bilinear resize，不启用昂贵 JPEG optimize
- 上一帧完成后立即请求下一帧，不再固定额外等待
- 显示真实 FPS，避免把低帧率误报为 live stream

OnePlus 6 实测 `adb screencap` 单帧约 1502ms，压缩约 33ms，因此该路径物理上限约
0.65 FPS。它只用于无 runtime 时的可操作降级界面。

#### 正式路径：loopback H.264 stream（已实现）

- Python runtime 在动态 loopback 端口提供带随机 session token 的二进制 H.264 stream
- Widget 使用 WebCodecs `VideoDecoder` 解码 SPS/PPS 和连续 NAL units
- 默认 `Resolution 50%`、`Encoding H.264`
- 可选 30 / 60 FPS；decode backlog 超过阈值时丢弃 delta frame
- MCP tool bridge 始终保留为 fallback
- CSP、Private Network Access 或 WebCodecs 验证失败时明确显示降级原因

OnePlus 6 真机在 50% / 60 FPS 配置、连续滑动期间收到 157 帧，测得约 26.7 FPS。
Codex Widget 的最终可见 FPS 仍需完成宿主内验收。

### 3.5 音频

0.7.2 暂不启用音频 socket。后续若加入音频，使用独立 native helper 或浏览器可稳定消费的
音频流，不把 PCM 经过 MCP JSON-RPC。

---

## 4. 功能映射

| ScrcpyMac.app 功能 | Codex widget 实现 |
|---|---|
| Device picker | `scrcpymac_ui_state` + `scrcpymac_ui_select_device` |
| Refresh | 重新调用 state |
| Wi-Fi connect | `scrcpymac_ui_connect_wifi` |
| Connect / Stop | runtime session tools |
| Stay awake | runtime start option |
| Turn screen off | runtime start option |
| Audio only | runtime start option + Mac 本地音频 |
| Video only | runtime start option |
| Mirror | plugin H.264 WebSocket + WebCodecs；JPEG fallback |
| Click / drag / scroll | normalized tap/swipe tools |
| Keyboard | app-only key/type tools |
| Paste Clipboard | runtime 读取 Mac pasteboard，或显式文本 paste |
| Connection progress | runtime state events / polling |
| Logs | bounded runtime log buffer |
| Agent service toggle | 删除；插件 runtime 自身就是 Agent backend |
| Install plugin button | 删除；当前已经运行在插件内 |

---

## 5. 实施阶段

## Phase 0 — Codex native widget contract

目标：证明本地 stdio MCP plugin 可以稳定打开原生 Codex widget。

- [x] 新增 `open_scrcpymac`
- [x] 新增 `ui://widget/scrcpymac/app.html`
- [x] Resource MIME 为 `text/html;profile=mcp-app`
- [x] 单文件 Vite build
- [x] fullscreen request
- [x] app-only tool visibility
- [x] Python list/read/call contract 测试
- [x] Codex Desktop 真实打开
- [ ] teardown 验证

## Phase 1 — 可用的 adb 交互预览

目标：没有原生 runtime 时，widget 已经可以独立操作手机。

- [x] 设备发现和选择
- [x] Wi-Fi adb
- [x] JPEG screenshot polling
- [x] 点击与滑动
- [x] Back / Home / Recents / Menu
- [x] 文本粘贴
- [x] light / dark host theme
- [x] desktop / narrow responsive layout
- [x] 真机 Codex widget E2E
- [x] 50% 分辨率低延迟 JPEG profile
- [x] 实际 FPS 显示
- [x] active preview 隐藏空状态
- [ ] 5 分钟 screenshot polling soak

版本：`0.6.1`

## Phase 2 — 插件独立 runtime

目标：不打开 ScrcpyMac.app 也能启动真正的 scrcpy session。

- [x] Python runtime lifecycle manager
- [x] 插件内置 scrcpy-server 3.3.4
- [x] Device / video / control socket session
- [x] H.264 packet relay
- [ ] Mac audio playback
- [x] clipboard and control input
- [x] disconnect / explicit stop cleanup
- [ ] MCP 强制退出残留进程验收
- [x] 无 native helper，因此无 Universal Binary / code-signing 依赖

版本：`0.7.2`

## Phase 3 — 流畅镜像与完整功能对齐

- [x] Codex loopback H.264 stream
- [x] WebCodecs H.264 decode
- [x] 30 / 60 FPS control
- [x] 50% / 75% / 100% resolution control
- [x] H.264 / JPEG fallback 状态展示
- [x] “Codex is using this device” session lease
- [ ] Home / screenshot / record / rotate toolbar
- [ ] keyboard capture
- [ ] trackpad scroll
- [ ] Stay awake / screen off / audio-only / video-only
- [ ] runtime logs UI
- [ ] reduced motion

版本：`0.7.x–0.8.0`

## Phase 4 — 发布

- [ ] Clean Mac，只安装 Codex plugin
- [ ] 无 ScrcpyMac.app 时完整通过
- [ ] USB 与 Wi-Fi 设备
- [ ] Agent 与用户同一 session
- [ ] 30 分钟 video/audio soak
- [ ] helper 退出无残留 adb forward / child process
- [ ] plugin cache upgrade test
- [ ] README / privacy / screenshots / changelog

---

## 6. 测试策略

### 6.1 自动测试

- UI TypeScript strict typecheck
- Vite single-file build
- HTML 不含外部 `<script src>` / stylesheet URL
- tool `_meta` wire alias
- resource MIME 和 `_meta.ui.csp`
- app-only visibility
- screenshot resize / JPEG / base64
- existing 24 tools regression
- version metadata consistency
- packaged static resource existence

### 6.2 真机 E2E

1. 插件安装后启动新 Codex task
2. 调用 `open_scrcpymac`
3. widget 进入 fullscreen
4. 显示真实设备型号和 serial
5. Start preview 后显示真实手机
6. 点击一个图标，手机产生对应变化
7. 执行 swipe、Back、Home、paste
8. 停止并重新打开 widget
9. App 关闭时仍显示 `plugin-h264`，不是切换到 App/Agent backend
10. 删除 ScrcpyMac.app 后确认插件仍可低延迟镜像

---

## 7. 安全

- Widget 不直接执行 shell
- `phone_shell` 继续只对 model-visible tool surface 暴露
- App-only tools 使用明确参数，不提供任意 command
- loopback 只绑定 `127.0.0.1` 动态端口
- 每次 session 使用 32-byte 随机 token，URL 仅通过 app-only tool 返回
- scrcpy-server 路径必须位于插件目录或显式 `SCRCPY_SERVER_PATH`
- Wi-Fi adb 行为由用户在 UI 明确触发
- 微信发送和其他敏感动作继续走显式确认流程

---

## 8. 当前仓库改动

```text
plugins/scrcpymac-phone-agent/
├── ui/
│   ├── index.html
│   ├── src/main.ts
│   ├── src/styles.css
│   ├── package.json
│   ├── package-lock.json
│   └── vite.config.ts
├── scripts/build-ui.sh
├── bin/darwin/share/scrcpy-server
└── server/phone_agent/
    ├── mcp_ui.py
    ├── scrcpy_runtime.py
    └── static/scrcpymac-app.html
```

下一条主线是完成真实 Codex Widget 的 WebCodecs 可见画面验收、停止/崩溃清理 soak，
然后补截图、录屏、旋转、键盘和音频；不再扩展或调用 `ScrcpyMac.app AgentHTTPServer`。

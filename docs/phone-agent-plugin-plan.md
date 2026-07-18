# ScrcpyMac Phone Agent 插件实现计划

> 版本：0.2  
> 日期：2026-07-18  
> 状态：Phase 0–4 implemented (Phase 5 optional, not started)  
> 负责人：ScrcpyMac 团队

---

## 1. 背景与目标

### 1.1 问题

当前要让 Codex / Claude / Cursor 控制 Android 手机（例如发微信），用户需要：

1. 自己找 MCP 项目（scrcpy-mcp、mobile-mcp-ai 等）
2. 手动安装 adb、配置环境变量
3. 分别编写 MCP 配置（`mcp.json` / `config.toml` / Claude Desktop 配置）
4. 自己写 Skill 描述业务流程

步骤分散、门槛高，且与 ScrcpyMac 镜像 App 没有形成产品闭环。

### 1.2 目标

构建 **ScrcpyMac Phone Agent** 插件——一个可上架 Cursor Marketplace 与 Codex Marketplace 的完整安装包，用户 **一键安装** 后即可通过自然语言操作手机。

### 1.3 非目标（本期不做）

- 不做 iOS 支持（后续版本考虑）
- 不做云端远程控手机
- 不替代 ScrcpyMac 镜像 App 的 GUI 功能
- 不重写完整的 scrcpy 协议（优先封装/复用成熟 MCP 实现）
- 不做微信官方 API 对接（仅 UI 自动化）

### 1.4 产品定位

| 维度 | 定位 |
|------|------|
| 一句话 | macOS 用户开箱即用的 Android 手机 AI Agent 插件 |
| 目标用户 | 已用 ScrcpyMac 或愿意 USB 连手机的开发者 / 效率用户 |
| 差异化 | 插件打包（非裸 MCP）+ mac 捆绑 adb + 中文场景 Skill + ScrcpyMac 联动 |
| 竞品 | scrcpy-mcp、mobile-mcp-ai、Android-MCP（均为 MCP-only，无完整插件体验） |

---

## 2. 插件架构

### 2.1 核心原则

1. **Plugin First**：以插件为交付单元，MCP 是插件内的一个组件
2. **Launcher 单一入口**：所有平台（Cursor / Codex / Claude Desktop）共用 `bin/phone-agent` 启动
3. **双 Manifest**：同一代码库同时支持 `.cursor-plugin/` 与 `.codex-plugin/`
4. **薄封装 MCP**：不自研全套 tools，封装成熟开源 MCP + 补充 mac 特有逻辑
5. **Skill 承载业务**：微信等场景写在 Skill，而非硬编码在 MCP

### 2.2 分层架构

```
┌─────────────────────────────────────────────────────────────────┐
│  Marketplace 层                                                  │
│  Cursor Marketplace · Codex Marketplace · Team Marketplace       │
└───────────────────────────────┬─────────────────────────────────┘
                                │ 一键安装
                                ▼
┌─────────────────────────────────────────────────────────────────┐
│  Plugin 包（scrcpymac-phone-agent）                              │
│  ┌─────────────┐ ┌──────────────┐ ┌─────────────────────────┐  │
│  │ Manifests   │ │ Skills       │ │ Assets / Docs           │  │
│  │ .cursor-    │ │ wechat       │ │ logo, screenshots,      │  │
│  │ .codex-     │ │ phone-setup  │ │ README, privacy         │  │
│  └─────────────┘ └──────────────┘ └─────────────────────────┘  │
│  ┌─────────────┐ ┌──────────────┐ ┌─────────────────────────┐  │
│  │ mcp.json    │ │ bin/         │ │ scripts/                │  │
│  │ .mcp.json   │ │ phone-agent  │ │ install.sh, doctor.sh   │  │
│  └─────────────┘ └──────────────┘ └─────────────────────────┘  │
└───────────────────────────────┬─────────────────────────────────┘
                                │
                                ▼
┌─────────────────────────────────────────────────────────────────┐
│  MCP Server 层（server/）                                        │
│  设备管理 · 截图 · 触控 · UI 树 · 应用启动 · 剪贴板 · Shell        │
└───────────────────────────────┬─────────────────────────────────┘
                                │ adb / scrcpy control
                                ▼
┌─────────────────────────────────────────────────────────────────┐
│  设备层                                                          │
│  Android 手机（USB / Wi-Fi ADB）                                 │
│  可选：ScrcpyMac.app 本地 Agent Service（Phase 4）               │
└─────────────────────────────────────────────────────────────────┘
```

### 2.3 与 ScrcpyMac App 的关系

| 组件 | 关系 |
|------|------|
| ScrcpyMac.app | 可选：可视化镜像 + 未来本地 Agent Service |
| Phone Agent 插件 | 独立安装，不依赖 App 即可工作 |
| adb 二进制 | 插件自带；与 App B7 打包脚本共用来源 |
| 联动（Phase 4） | App 开启服务后，插件 MCP 优先连 `127.0.0.1:9477` |

---

## 3. 仓库结构

建议在 `scrcpyMac` 仓库内新建插件目录（后期可拆独立 repo）：

```
scrcpyMac/
├── ScrcpyMac/                          # 现有 Swift 镜像 App（不变）
├── plugins/
│   └── scrcpymac-phone-agent/          # 插件根目录
│       ├── .cursor-plugin/
│       │   └── plugin.json
│       ├── .codex-plugin/
│       │   └── plugin.json
│       ├── .agents/
│       │   └── plugins/
│       │       └── marketplace.json    # Codex repo marketplace
│       ├── mcp.json                    # Cursor MCP 配置
│       ├── .mcp.json                   # Codex MCP 配置（内容可与 mcp.json 同步）
│       ├── skills/
│       │   ├── phone-setup/
│       │   │   └── SKILL.md
│       │   ├── wechat/
│       │   │   └── SKILL.md
│       │   ├── android-nav/
│       │   │   └── SKILL.md
│       │   └── scrcpymac-link/
│       │       └── SKILL.md
│       ├── server/                     # MCP 实现
│       │   ├── pyproject.toml
│       │   └── phone_agent/
│       │       ├── server.py           # MCP 入口
│       │       ├── adb.py
│       │       ├── actions.py
│       │       └── recipes/
│       ├── bin/
│       │   ├── phone-agent             # 统一 launcher（bash）
│       │   └── darwin/
│       │       ├── arm64/adb
│       │       └── x86_64/adb
│       ├── scripts/
│       │   ├── install.sh              # 安装后处理
│       │   ├── doctor.sh               # 环境诊断
│       │   └── sync-mcp-config.sh      # 同步 mcp.json ↔ .mcp.json
│       ├── assets/
│       │   ├── logo.png
│       │   ├── icon.svg
│       │   └── screenshots/
│       ├── LICENSE
│       ├── CHANGELOG.md
│       └── README.md
├── docs/
│   └── phone-agent-plugin-plan.md      # 本文档
└── phone-agent/                        # 现有半成品 → 迁移到 plugins/.../server/
```

---

## 4. 组件规格

### 4.1 Plugin Manifest

#### Cursor（`.cursor-plugin/plugin.json`）

| 字段 | 值 |
|------|-----|
| `name` | `scrcpymac-phone-agent` |
| `version` | semver，与 CHANGELOG 同步 |
| `description` | Android phone control for AI agents — WeChat, apps, automation |
| `skills` | `./skills/` |
| `mcpServers` | `./mcp.json` |
| `keywords` | android, phone, wechat, adb, scrcpy, automation |

#### Codex（`.codex-plugin/plugin.json`）

除 Cursor 字段外，必须包含 `interface` 对象：

| 字段 | 说明 |
|------|------|
| `interface.displayName` | ScrcpyMac Phone Agent |
| `interface.shortDescription` | Control your Android phone from Codex |
| `interface.longDescription` | 完整产品介绍（Marketplace 展示） |
| `interface.category` | Productivity |
| `interface.brandColor` | `#4A90D9` |
| `interface.logo` | `./assets/logo.png` |
| `interface.defaultPrompt` | 示例 prompt 数组 |
| `interface.privacyPolicyURL` | 隐私政策链接 |
| `interface.websiteURL` | 项目主页 |

#### Codex Marketplace（`.agents/plugins/marketplace.json`）

```json
{
  "name": "scrcpymac",
  "plugins": [{
    "name": "scrcpymac-phone-agent",
    "source": { "source": "local", "path": "./plugins/scrcpymac-phone-agent" },
    "policy": {
      "installation": "AVAILABLE",
      "authentication": "NONE"
    },
    "category": "Productivity"
  }]
}
```

### 4.2 Launcher（`bin/phone-agent`）

统一入口，子命令：

| 命令 | 说明 |
|------|------|
| `phone-agent mcp` | 启动 MCP stdio 服务（Cursor/Codex/Claude 调用） |
| `phone-agent doctor` | 环境诊断，输出 JSON |
| `phone-agent devices` | 列出已连接设备 |
| `phone-agent version` | 版本信息 |

Launcher 职责：

1. 检测 OS/arch，设置 `ADB_PATH` 指向 `bin/darwin/${arch}/adb`
2. 若系统 adb 更新则优先用系统 adb（可配置 `PHONE_AGENT_PREFER_SYSTEM_ADB=1`）
3. 激活 Python venv 或调用 `uv run` 启动 MCP server
4. 传递 `PHONE_AGENT_SERIAL` 环境变量

### 4.3 MCP Server Tools

分 **P0（首发必做）** 和 **P1（后续迭代）**。

#### P0 Tools

| Tool | 说明 | 实现方式 |
|------|------|----------|
| `phone_list_devices` | 列出 adb 设备 | adb devices -l |
| `phone_device_info` | 分辨率、前台 App | wm size + dumpsys window |
| `phone_screenshot` | 截图，返回 base64 PNG | adb exec-out screencap -p |
| `phone_tap` | 点击坐标 | adb shell input tap |
| `phone_swipe` | 滑动 | adb shell input swipe |
| `phone_long_press` | 长按 | swipe 同点 |
| `phone_key` | back/home/enter 等 | adb shell input keyevent |
| `phone_type` | 短 ASCII 文本 | adb shell input text |
| `phone_paste` | 任意文本含中文 | cmd clipboard + KEYCODE_PASTE |
| `phone_launch_app` | 启动应用 | monkey / am start |
| `phone_current_app` | 当前前台 App | dumpsys window |
| `phone_ui_tree` | UI 无障碍树（精简 JSON） | uiautomator dump |
| `phone_find_and_tap` | 按 text/desc 查找并点击 | ui_tree + tap |
| `phone_wait_for_text` | 等待文字出现 | 轮询 ui_tree |
| `phone_shell` | 执行 adb shell | 兜底 |

#### P1 Tools

| Tool | 说明 |
|------|------|
| `phone_send_wechat` | 高层 recipe：联系人 + 消息 |
| `phone_connect_wifi` | Wi-Fi ADB 连接 |
| `phone_push_file` / `phone_pull_file` | 文件传输 |
| `phone_get_clipboard` / `phone_set_clipboard` | 剪贴板 |
| `phone_start_scrcpy_session` | 开启 scrcpy 快速控制通道 |
| `phone_screenshot_fast` | scrcpy 帧截图（需 session） |

#### Tool 输出规范

所有 tool 返回 JSON 字符串，统一字段：

```json
{
  "ok": true,
  "serial": "abc123",
  "action": "tap",
  "data": { },
  "error": null
}
```

截图类 tool 额外返回 MCP `ImageContent`（PNG base64），让 Agent 能「看见」屏幕。

### 4.4 Skills 规格

#### `phone-setup` — 首次使用 / 故障排查

触发：用户问如何连接手机、设备找不到、adb unauthorized

内容要点：

- 开启开发者选项 + USB 调试步骤
- `phone-agent doctor` 用法
- 多设备时设置 `PHONE_AGENT_SERIAL`
- Wi-Fi ADB 简要说明（P1）
- 安全提示：仅用于自己的设备

#### `wechat` — 微信自动化

触发：发微信、读消息、打开朋友圈、找联系人

Workflow（Agent 按 Skill 执行）：

```
1. phone_launch_app(com.tencent.mm)
2. phone_wait_for_text("微信") 或 phone_screenshot 确认主界面
3. phone_find_and_tap(content_desc="搜索") 或 tap 搜索区域
4. phone_paste("联系人名")
5. phone_wait_for_text("联系人名")
6. phone_find_and_tap(text="联系人名")
7. phone_paste("消息内容")
8. phone_find_and_tap(text="发送") 或 phone_key(enter)
9. phone_screenshot 验证发送成功
```

注意：

- 中文输入 **必须用 `phone_paste`**，禁止 `phone_type`
- 坐标因分辨率而异，优先 `phone_find_and_tap` / ui_tree
- 微信 UI 改版时 Skill 比 MCP 更容易更新

#### `android-nav` — 通用 Android 导航

触发：打开设置、安装 App、返回桌面、滑动列表

内容：通用导航模式、常用包名表、手势策略

#### `scrcpymac-link` — 与 ScrcpyMac App 联动

触发：用户提到 ScrcpyMac、想看屏幕镜像

内容：

- 引导下载/打开 ScrcpyMac.app
- 说明插件（headless）vs App（可视化）的分工
- Phase 4 后：检测本地 Agent Service 是否可用

### 4.5 捆绑二进制

| 文件 | 来源 | 架构 |
|------|------|------|
| `bin/darwin/arm64/adb` | Android platform-tools | Apple Silicon |
| `bin/darwin/x86_64/adb` | Android platform-tools | Intel Mac |

策略：

- Phase 1：依赖系统 adb，launcher 检测并提示安装
- Phase 2：捆绑 adb，launcher 默认用捆绑版本
- 许可证：adb 属 Apache 2.0 / Google 平台工具许可，README 注明

### 4.6 安装与诊断脚本

#### `scripts/install.sh`

- 检测 Python >= 3.10
- `pip install` 或 `uv sync` 安装 server 依赖
- `chmod +x bin/phone-agent`
- 运行 `doctor.sh`，输出结果
- 打印下一步：如何在 Cursor / Codex 中验证

#### `scripts/doctor.sh`

检查项：

| 检查 | 通过条件 |
|------|----------|
| adb 可用 | `adb version` 成功 |
| 设备连接 | 至少一台 state=device |
| USB 授权 | 非 unauthorized |
| Python 版本 | >= 3.10 |
| MCP 依赖 | `mcp` 包可 import |
| 屏幕分辨率 | `wm size` 可解析 |
| 剪贴板 API | Android 10+ `cmd clipboard` 可用 |

输出 JSON + 人类可读摘要。

---

## 5. 技术选型

| 决策点 | 选择 | 理由 |
|--------|------|------|
| MCP 实现语言 | Python 3.10+ | mcp SDK 成熟；与现有 phone-agent 半成品一致 |
| 是否 fork scrcpy-mcp | Phase 1 自研薄层，Phase 3 评估合并 | 插件需要自定义 launcher/bundling，薄层更可控 |
| 截图方案 | P0: adb screencap；P1: scrcpy 帧 | screencap 零依赖；scrcpy 更快 |
| 中文输入 | paste（clipboard + KEYCODE_PASTE） | 比 input text 可靠 |
| UI 定位 | uiautomator dump + text/desc 匹配 | 无需 root，微信可用 |
| 包管理 | uv（开发）+ pip（用户） | Codex/Cursor 环境兼容性好 |
| 配置同步 | `sync-mcp-config.sh` 维护 mcp.json ↔ .mcp.json | 避免双份漂移 |

---

## 6. 实施阶段

### Phase 0 — 准备（1-2 天）

| # | 任务 | 产出 |
|---|------|------|
| 0.1 | 确定插件目录结构，迁移 `phone-agent/` → `plugins/scrcpymac-phone-agent/server/` | 目录就绪 |
| 0.2 | 编写双 manifest 骨架 | plugin.json × 2 |
| 0.3 | 编写 Codex marketplace.json | marketplace.json |
| 0.4 | 确定品牌资产（logo、颜色、描述文案） | assets/ |

**验收**：目录结构符合第 3 节，manifest JSON 通过 schema 校验。

---

### Phase 1 — 最小可用插件（3-5 天）

| # | 任务 | 产出 |
|---|------|------|
| 1.1 | 实现 `bin/phone-agent` launcher | 可执行 |
| 1.2 | 完成 MCP server P0 tools（15 个） | server.py |
| 1.3 | 编写 4 个 Skill | skills/*/SKILL.md |
| 1.4 | 编写 mcp.json / .mcp.json | MCP 配置 |
| 1.5 | 实现 doctor.sh | 诊断脚本 |
| 1.6 | 本地测试：Cursor `~/.cursor/plugins/local/` | 测试记录 |
| 1.7 | 本地测试：Codex personal marketplace | 测试记录 |
| 1.8 | 编写插件 README | README.md |

**验收**：

- [ ] Cursor 安装插件后，Agent 可调用 `phone_screenshot`
- [ ] Codex 安装插件后，同样可用
- [ ] 对已连接 Android 设备执行：截图 → tap → paste → back
- [ ] `wechat` Skill 引导下完成一次发消息（手动验证）

---

### Phase 2 — 捆绑 mac 二进制 + 安装体验（2-3 天）

| # | 任务 | 产出 |
|---|------|------|
| 2.1 | 从 platform-tools 提取 adb，放入 bin/darwin/ | 二进制 |
| 2.2 | launcher 自动选择架构 adb | 更新 launcher |
| 2.3 | 实现 install.sh 一键安装流程 | 安装脚本 |
| 2.4 | 复用 ScrcpyMac `package-dmg.sh` 的 adb 来源逻辑 | 文档 + 脚本共享 |
| 2.5 | 无 adb 时给出明确错误和安装指引 | 错误信息 |

**验收**：

- [ ] 全新 Mac（无 Android SDK）上，安装插件后可连设备
- [ ] `phone-agent doctor` 全部检查通过

---

### Phase 3 — 高层 Recipe + 打磨（2-3 天）

| # | 任务 | 产出 |
|---|------|------|
| 3.1 | 实现 `phone_send_wechat` P1 tool | recipes/wechat.py |
| 3.2 | 评估接入 scrcpy 快速控制（可选） | 设计笔记 |
| 3.3 | Wi-Fi ADB 连接 tool | phone_connect_wifi |
| 3.4 | 完善错误处理与超时 | server 加固 |
| 3.5 | 编写 CHANGELOG、LICENSE、隐私政策 | 合规文件 |
| 3.6 | Marketplace 截图与展示素材 | assets/screenshots/ |

**验收**：

- [ ] `phone_send_wechat(contact, message)` 端到端成功
- [ ] 插件文档完整，可提交审核

---

### Phase 4 — Marketplace 上架（2-3 天）

| # | 任务 | 产出 |
|---|------|------|
| 4.1 | 提交 Cursor Marketplace | 审核中 |
| 4.2 | 配置 Codex marketplace（`codex plugin marketplace add`） | 文档 |
| 4.3 | ScrcpyMac README 添加插件安装入口 | README 更新 |
| 4.4 | 可选：ScrcpyMac App 设置页「安装 Phone Agent」按钮 | Swift UI |
| 4.5 | 收集早期用户反馈 | Issue 模板 |

**验收**：

- [ ] Cursor Marketplace 可搜索安装
- [ ] Codex 用户可通过 marketplace add 安装
- [ ] ScrcpyMac 官网/README 有清晰入口

---

### Phase 5 — ScrcpyMac App 联动（可选，3-5 天）

| # | 任务 | 产出 |
|---|------|------|
| 5.1 | 从 ScrcpyMac 抽出 ScrcpyCore（去 UI 依赖） | Swift package |
| 5.2 | App 内「Agent Service」：Unix Socket / HTTP | 本地服务 |
| 5.3 | 插件 MCP 检测并优先连本地服务 | 自动降级 adb |
| 5.4 | 更新 `scrcpymac-link` Skill | Skill 更新 |

**验收**：

- [ ] App 开启 Agent Service 后，插件截图延迟 < 100ms
- [ ] App 未运行时，插件仍可通过 adb 独立工作

---

## 7. 测试计划

### 7.1 单元测试

| 模块 | 测试内容 |
|------|----------|
| `adb.py` | 命令构建、设备列表解析、错误处理 |
| `actions.py` | bounds 解析、节点查找、JSON 输出 |
| `recipes/wechat.py` | mock actions 的步骤顺序 |

### 7.2 集成测试（需真机）

| 场景 | 步骤 | 预期 |
|------|------|------|
| 设备发现 | phone_list_devices | 返回 serial |
| 截图 | phone_screenshot | 返回有效 PNG base64 |
| 点击 | phone_tap 打开设置 | 设置 App 前台 |
| 中文输入 | phone_paste "测试" | 输入框出现中文 |
| 返回 | phone_key(back) | 回到上一页 |
| 微信发消息 | wechat skill 全流程 | 气泡出现 |

### 7.3 插件安装测试

| 环境 | 验证 |
|------|------|
| Cursor local plugin | skills 可见，MCP tools 可调用 |
| Codex personal marketplace | 插件目录可安装，tools 可用 |
| 无 adb 环境 | doctor 给出明确指引 |
| 多设备 | PHONE_AGENT_SERIAL 生效 |

### 7.4 兼容性矩阵

| 项目 | 最低版本 |
|------|----------|
| macOS | 13+ |
| Android | 10+（剪贴板 API） |
| Python | 3.10+ |
| Cursor | 支持 plugins 的版本 |
| Codex | 支持 plugins 的桌面版 |

---

## 8. 安全与合规

### 8.1 安全原则

- 插件仅操作用户本机已授权的 adb 设备
- 不收集、不上传屏幕内容或聊天记录
- 敏感操作（转账、删除消息）Skill 中要求用户确认
- `phone_shell` 限制危险命令（可选白名单模式）

### 8.2 隐私

- 截图仅在本地 MCP 进程与 Agent 之间传递
- 需编写 `PRIVACY.md` 供 Marketplace 审核
- 不内置任何遥测（P0）；若后续添加需 opt-in

### 8.3 许可证

- 插件代码：MIT
- adb：遵循 Google Platform Tools 许可
- scrcpy-server（若使用）：Apache 2.0

---

## 9. 风险与缓解

| 风险 | 影响 | 缓解 |
|------|------|------|
| 微信 UI 频繁改版 | wechat recipe 失效 | Skill 语义定位 + 视觉 fallback；recipe 与 MCP 分离 |
| Cursor/Codex pluginRoot 变量不一致 | MCP 启动失败 | launcher 用绝对路径；install.sh 写入配置 |
| 用户无 Python 环境 | MCP 无法启动 | install.sh 引导安装；文档推荐 uv |
| 中文输入法兼容 | paste 失败 | 多策略：clipboard API → scrcpy clipboard → 降级提示 |
| Marketplace 审核周期 | 上线延迟 | 先提供 local install + GitHub README |
| 与开源 MCP 功能重叠 | 维护成本高 | 薄封装 + 差异化在插件体验，不重复造轮子 |

---

## 10. 成功指标

### Phase 1 完成时

- 插件可在 Cursor 和 Codex 本地安装
- 15 个 P0 tools 全部可用
- 至少 1 人完成微信发消息全流程

### Phase 4 完成时

- Cursor / Codex Marketplace 可安装
- 安装到首次成功操作手机 < 10 分钟（含 adb 授权）
- README 有完整 troubleshooting

### 长期

- ScrcpyMac 用户插件安装转化率
- GitHub Issue / Marketplace 评分
- 社区贡献 Skill（钉钉、淘宝等）

---

## 11. 里程碑时间线（技术维度）

```
Phase 0 ──► Phase 1 ──► Phase 2 ──► Phase 3 ──► Phase 4 ──► Phase 5
 准备        MVP 插件      捆绑 adb      Recipe       上架        App 联动
 1-2天       3-5天         2-3天         2-3天        2-3天       3-5天（可选）
```

总计：**约 10-16 天**到 Marketplace 上架（不含 Phase 5）。

---

## 12. 下一步行动（Immediate）

1. **确认插件目录位置**：`plugins/scrcpymac-phone-agent/` vs 独立 repo
2. **迁移现有 `phone-agent/` 代码**到 `server/`
3. **创建 Phase 0 骨架文件**（manifest、marketplace、launcher 占位）
4. **在真机上验证** `phone-agent doctor` 与 P0 tools
5. **并行准备** Marketplace 品牌素材（logo、截图、描述）

---

## 附录 A：MCP 配置模板

### Cursor `mcp.json`

```json
{
  "mcpServers": {
    "scrcpymac-phone-agent": {
      "command": "./bin/phone-agent",
      "args": ["mcp"],
      "env": {}
    }
  }
}
```

### Codex `.mcp.json`

```json
{
  "mcpServers": {
    "scrcpymac-phone-agent": {
      "command": "./bin/phone-agent",
      "args": ["mcp"]
    }
  }
}
```

> 注意：实际路径在插件安装时由 launcher 或 install 脚本解析为绝对路径。

---

## 附录 B：参考项目

| 项目 | 借鉴点 |
|------|--------|
| [JuanCF/scrcpy-mcp](https://github.com/JuanCF/scrcpy-mcp) | scrcpy 快速控制、tool 设计 |
| [CursorTouch/Android-MCP](https://github.com/CursorTouch/Android-MCP) | uvx 分发、设备懒连接 |
| [mobile-mcp-ai](https://pypi.org/project/mobile-mcp-ai/) | 中文场景、微信示例 |
| [cursor/plugins](https://github.com/cursor/plugins) | Cursor 插件 scaffold 规范 |
| [OpenAI Codex Build plugins](https://developers.openai.com/codex/plugins/build) | Codex manifest、marketplace |

---

## 附录 C：现有代码迁移映射

| 现有文件 | 迁移目标 |
|----------|----------|
| `phone-agent/phone_agent/adb.py` | `plugins/.../server/phone_agent/adb.py` |
| `phone-agent/phone_agent/actions.py` | `plugins/.../server/phone_agent/actions.py` |
| `phone-agent/phone_agent/recipes/` | `plugins/.../server/phone_agent/recipes/` |
| `phone-agent/pyproject.toml` | `plugins/.../server/pyproject.toml` |
| （新增）`server.py` | MCP 入口，封装 actions 为 tools |

---

## 实施状态（2026-07-18）

| Phase | 状态 | 说明 |
|-------|------|------|
| 0 | ✅ 完成 | 目录、双 manifest、marketplace |
| 1 | ✅ 完成 | 21 MCP tools、4 Skills、launcher |
| 2 | ✅ 完成 | `download-adb.sh`、install/configure 自动化 |
| 3 | ✅ 完成 | Wi-Fi adb tools、PRIVACY、logo、wechat recipe |
| 4 | ✅ 文档就绪 | `MARKETPLACE.md` 提交清单，待人工上架 |
| 5 | ⏸ 未开始 | ScrcpyMac App Agent Service 联动 |

代码位置：`plugins/scrcpymac-phone-agent/`

---

*文档维护：每完成一个 Phase 更新状态和验收清单。*

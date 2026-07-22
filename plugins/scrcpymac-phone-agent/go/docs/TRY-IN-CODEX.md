# 在 Codex 里安装试验这个插件

## 先理解拓扑（否则会改了没反应）

```
~/.codex/config.toml
  [marketplaces.scrcpymac]
  source_type = "local"
  source      = "/Users/junyizhang/Git/scrcpyMac"      <- marketplace 就是本地 repo
        |
        | 安装时「拷贝」，不是软链、不是 git checkout
        v
~/.codex/plugins/cache/scrcpymac/scrcpymac-phone-agent/<version>/
        |
        +-- .venv/                 <- Codex 首次安装时 pip install 出来的
        +-- mcp-server.sh -> bin/phone-agent mcp
```

三个后果：

1. **改 repo 不会影响正在运行的插件**，必须同步到 cache 目录。
2. 安装目录**按版本号分开**。版本号不变，Codex 认为无需重装。
3. 版本号写在三处，必须一起改：
   `.agents/plugins/marketplace.json`（metadata + plugins[0] 两处）、
   `plugins/scrcpymac-phone-agent/.codex-plugin/plugin.json`、
   `.cursor-plugin/plugin.json`。

因为 marketplace 源就是本地 repo，**不需要 push 到 GitHub** 就能装。

---

## 路径 A：热同步（日常迭代用，最快）

把当前工作树直接盖进已安装的版本目录：

```bash
plugins/scrcpymac-phone-agent/scripts/install-local.sh
```

先看会改什么：

```bash
plugins/scrcpymac-phone-agent/scripts/install-local.sh --dry-run
```

同步会保留安装目录里的 `.venv`（不会重建 Python 环境），并跳过 `go/`、
`node_modules`、`__pycache__`。同步完脚本会跑一次 `doctor` 自检，并提示还有几个
**旧 MCP 进程在跑老代码**——必须重启 Codex 或把插件关掉再打开才会加载新代码。

## 路径 B：版本号发布（走真实用户路径，验收用）

```bash
plugins/scrcpymac-phone-agent/scripts/install-local.sh --bump 0.8.0
```

改完三处版本号后，去 Codex 插件界面从 `scrcpymac` marketplace 重新安装／升级，
它会读本地 repo 建出 `.../scrcpymac-phone-agent/0.8.0/`。这条路径会走真实的首装流程
（含 `.venv` 创建），所以是验证「装上即用」的唯一可信方式。

---

## 切换 Python / Go 后端

`bin/phone-agent` 现在是一个分发器，Go 二进制存在就优先用它，否则回落到 Python：

```bash
PHONE_AGENT_BACKEND=go      bin/phone-agent doctor   # 强制 Go，缺二进制则报错退出
PHONE_AGENT_BACKEND=python  bin/phone-agent doctor   # 强制旧的 Python 实现
PHONE_AGENT_BACKEND=auto    bin/phone-agent doctor   # 默认：有 Go 用 Go
PHONE_AGENT_BINARY=/path/to/phone-agent bin/phone-agent mcp   # 指定任意二进制
```

要让 Codex 用某个后端，在插件的 MCP 配置里加环境变量，或直接删掉／放回
`bin/darwin/*/phone-agent`。

**回滚**：删除 `bin/darwin/*/phone-agent` 即可回到 Python，无需改任何配置。

---

## 构建 Go 二进制

```bash
plugins/scrcpymac-phone-agent/scripts/build-go.sh          # arm64 + x86_64
plugins/scrcpymac-phone-agent/scripts/build-go.sh arm64    # 只构建本机架构
```

产物落在 `bin/darwin/arm64/phone-agent` 和 `bin/darwin/x86_64/phone-agent`。

工具链固定为 **go1.26.5**（当前上游稳定版）。本机装的是 go1.25.7，但 `GOTOOLCHAIN=auto`
是默认值，会按 `go.mod` 的 `toolchain` 指令自动下载 1.26.5——不用手动升级 brew，CI 也因此可复现。

> `build-go.sh` 会在构建前把 `server/phone_agent/static/scrcpymac-app.html` 拷进
> `go/internal/widget/assets/`（`//go:embed` 取不到模块目录之外的文件）。所以顺序是：
> `scripts/build-ui.sh` → `scripts/build-go.sh` → `scripts/install-local.sh`。

---

## 完整迭代循环

```bash
cd /Users/junyizhang/Git/scrcpyMac/plugins/scrcpymac-phone-agent
./scripts/build-ui.sh          # 只在改了 ui/src 时需要
./scripts/build-go.sh
./scripts/install-local.sh
# 重启 Codex，或把插件 off/on
```

## 验收（对应 1 FPS 那个老问题）

不要看静止的 Launcher 判断帧率——scrcpy 只在画面变化时才编码，静止画面本来就接近 0 FPS。
必须**持续滚动 12 秒以上**再看，并分层核对：设备编码器 → scrcpy socket → relay →
WebSocket → VideoDecoder → canvas。Go 版本会新增一个模型可见的只读 stream 诊断工具，
让这些数字不用再靠猜。

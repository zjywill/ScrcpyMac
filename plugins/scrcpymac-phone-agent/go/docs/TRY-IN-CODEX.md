# 在 Codex 里安装和验证 Go-only 插件

## 安装拓扑

本地 marketplace 指向仓库，但 Codex 安装时会把插件复制到版本缓存：

```text
/Users/junyizhang/Git/scrcpyMac/plugins/scrcpymac-phone-agent
  |
  | install/copy
  v
~/.codex/plugins/cache/scrcpymac/scrcpymac-phone-agent/<version>/
```

因此：

1. 修改仓库不会自动影响正在运行的插件。
2. MCP 进程必须重启，才能加载新的 Go binary 和内嵌 widget。
3. 插件只运行 `bin/darwin/<arch>/phone-agent`，没有 Python fallback。

## 完整构建

```bash
cd /Users/junyizhang/Git/scrcpyMac/plugins/scrcpymac-phone-agent

./scripts/build-go.sh
```

该命令会依次：

1. `npm ci`
2. UI `tsc --noEmit`
3. Vite 单文件构建
4. 更新 `go/internal/widget/assets/scrcpymac-app.html`
5. 使用 Go 1.26.5 构建 arm64 和 x86_64 release binary

## 热同步到已安装版本

```bash
./scripts/install-local.sh --dry-run
./scripts/install-local.sh
```

同步会跳过源码和构建依赖，并删除旧安装遗留的 `server/`、`.venv`、
`__pycache__` 等 Python runtime。同步后必须重启 Codex 或关闭再开启插件。

## 版本发布

版本号需要保持一致：

- `.agents/plugins/marketplace.json`
- `.codex-plugin/plugin.json`
- `.cursor-plugin/plugin.json`
- `ui/package.json`
- `go/internal/version/version.go`

Go packaging 测试会检查这些值。发布新版本后：

```bash
./scripts/install-local.sh --bump <version>
./scripts/build-go.sh
```

然后从 Codex 插件界面重新安装或升级。

## 本地验证

```bash
./bin/phone-agent version
./bin/phone-agent doctor
./bin/phone-agent devices

cd go
go test ./...
go vet ./...
scripts/smoke-stdio.sh ../bin/darwin/arm64/phone-agent
```

`phone_doctor` 必须显示 `binary`、`plugin_root`、bundled `adb` 和 bundled
`scrcpy_server`，不得出现 `python` 或 `mcp_package`。

## Native Widget 验收

1. 调用 `open_scrcpymac` 打开 native widget。
2. 确认标题旁显示插件版本。
3. 选择设备并点击 `Start preview`。
4. WebSocket 不可用时应切换到 `Live H.264 stream · MCP bridge`，不能直接进入 JPEG。
5. 记录 UI FPS、`src pkt/s` 和 GOP drop。
6. 在 Settings 前台执行一次无害 Back 或 swipe，确认控制和画面变化。
7. Stop 后确认本次 adb forward 和设备端 `scrcpymac-plugin-server` 进程已回收。

不要把终端 `TestLiveStream` 或 ADB 截图冒充 native widget 验收。

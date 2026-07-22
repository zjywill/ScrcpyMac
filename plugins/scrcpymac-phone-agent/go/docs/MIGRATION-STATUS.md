# Phone Agent Python → Go 迁移状态报告

> 本文所有构建/测试/设备输出均为本报告作者**亲自执行后粘贴的真实结果**，不是转述其他 agent 的结论。
> 凡是我没有亲自验证的说法，都明确标注为「未验证」并注明来源。
>
> 生成时间：2026-07-22 18:30–18:40（本机 `go1.26.5 darwin/arm64`）
> 分支：`cursor/mcp-apps-scrcpy-plan-3b10`
> Go 源码根目录：`/Users/junyizhang/Git/scrcpyMac/plugins/scrcpymac-phone-agent/go`

---

## 现状

### 三个门禁全部通过（我本人执行的原始输出）

```
### go build ./...
exit=0

### go vet ./...
exit=0

### go test ./... -count=1
ok  	github.com/zjywill/scrcpyMac/phone-agent/cmd/phone-agent	3.600s
ok  	github.com/zjywill/scrcpyMac/phone-agent/internal/adb	9.143s
?   	github.com/zjywill/scrcpyMac/phone-agent/internal/doctor	[no test files]
ok  	github.com/zjywill/scrcpyMac/phone-agent/internal/jsonresult	1.354s
ok  	github.com/zjywill/scrcpyMac/phone-agent/internal/mcpserver	6.734s
ok  	github.com/zjywill/scrcpyMac/phone-agent/internal/paths	1.787s
ok  	github.com/zjywill/scrcpyMac/phone-agent/internal/scrcpy	3.746s
ok  	github.com/zjywill/scrcpyMac/phone-agent/internal/tools	5.592s
?   	github.com/zjywill/scrcpyMac/phone-agent/internal/version	[no test files]
?   	github.com/zjywill/scrcpyMac/phone-agent/internal/widget	[no test files]
exit=0
```

补充门禁，同样是我本人执行：

| 检查 | 结果 |
|---|---|
| `gofmt -l .` | 无输出（干净） |
| `go test -race ./... -count=1` | 全部 `ok`，10 个包无 data race |
| `CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build ./...` | exit 0 |
| `CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build ./...` | exit 0 |
| `scripts/smoke-stdio.sh` | `OK: 4 JSON-RPC messages on stdout, nothing else; 195 bytes of log on stderr` / `OK: tool-name occurrences seen: 36` |
| `make parity-selftest`（无需设备） | `selftest passed: 35 regressions detected, 4 controls clean` |

**测试规模**：631 个测试，627 PASS / 4 SKIP / 0 FAIL。跳过的 4 个全部是需要真机或额外资源的 opt-in 用例：
`TestLiveStream`、`TestLiveDeviceReadOnlyTools`、`TestInputLiveDevice`、`TestInputChangeScoreOnCapturedScreens`。

分包测试数：`internal/tools` 329、`internal/adb` 120、`internal/mcpserver` 93、`internal/scrcpy` 75、
`internal/jsonresult` 7、`internal/paths` 6、`cmd/phone-agent` 1。
代码量：非测试 Go 源码 11,533 行，测试 12,596 行。

### 真实二进制的工具面（我用真的 stdio JSON-RPC 打出来的）

启动 `phone-agent mcp`，走完 `initialize` → `notifications/initialized` → `tools/list` → `resources/list`：

```
serverInfo : {"name":"scrcpymac-phone-agent","title":"ScrcpyMac Phone Agent","version":"0.7.2"}
capabilities: {"prompts":{},"resources":{},"tools":{}}
tool count : 36     （phone_* 24 + scrcpymac_ui_* 11 + open_scrcpymac 1）
resource   : ui://widget/scrcpymac/app.html | scrcpymac-app | text/html;profile=mcp-app
```

与 `docs/contract.json` 逐名对比：**missing from go: [] / extra in go: []**。
11 个 `scrcpymac_ui_*` 全部带 `_meta.ui.visibility = ["app"]`（这是唯一让 Codex 看不见它们的机制），
`open_scrcpymac` 带 `["model","app"]`，24 个 `phone_*` 无 `_meta`。
`phone_screenshot` 是唯一无 `outputSchema` 的 `phone_*` 工具 —— 与 Python 一致。

### 真机 parity（我本人跑的，OnePlus 6 / 2f019965）

`go/scripts/parity/run-parity.sh` 同时驱动 Python（进程内 import 真实 `phone_agent.server`）和 Go（真实 MCP stdio 会话），
用相同参数比对 JSON **包括 key 顺序**。我跑了全部只读用例，**10/10 通过，退出码 0**：

```
PASS  doctor               phone_doctor {}
PASS  devices              phone_list_devices {}          · raw bytes identical
PASS  screenshot_no_image  phone_screenshot {"include_image": false}
        · png: python 210147B (1080,2280) sha=6bd6a0420874 | go 210147B (1080,2280) sha=6bd6a0420874
PASS  screenshot_with_image phone_screenshot {"include_image": true}
PASS  cli_devices          phone-agent devices            · raw bytes identical
PASS  error_screenshot     · raw bytes identical
PASS  error_tap            · raw bytes identical
PASS  error_ui_tree        · raw bytes identical
PASS  ui_tree_compact      · raw bytes identical · 38 nodes
PASS  ui_tree_raw          · raw bytes identical · xml length 27464 chars
```

`doctor` 的唯一差异就是下文「行为差异」里那三条已批准的改动，harness 逐条列出并断言**只有这三条**：

```
· intended python-only check python: ok=True detail='3.13'
· intended python-only check mcp_package: ok=True detail='installed'
· intended go-only check binary: ok=True detail='phone-agent 0.7.2 darwin/arm64 (go1.26.5)'
· intended go-only check plugin_root: ok=True detail='...'
· intended: top-level 'uv_available' dropped (python had True)
· compared strictly after removing the two intended renames and uv_available
```

**我没有跑 `tap` 用例**：设备前台是第三方加密货币交易 App（`com.vayoh.vayoh/com.vayoh.ui.VayohActivity`），
在真实交易界面上点击不是我该替你做的决定。`tap` 的 parity 结果来自之前的 parity agent（12/12），我未复现。

### Go doctor 在真机上的真实输出

```json
{ "ok": true, "version": "0.7.2", "backend": "plugin-h264-ready",
  "checks": [ platform, binary, plugin_root, scrcpy_server(bundled:true),
              adb(bundled:false), adb_version, device(1 ready), screen_size 1080x2280,
              foreground_app, runtime_architecture ],
  "summary": "ready" }
```

注意 `"adb": {"bundled": false}` —— 详见「未完成 / 打包」。

### 泄漏检查（我本人跑的，真机）

启动真实二进制 → 完成握手 → 读 widget resource（拿到 CSP 里的 loopback 端口）→ 关闭 stdin 退出：

```
loopback ports advertised in CSP: [64243]
ports accepting connections WHILE RUNNING: [64243]
lsof: phone-age 62148  TCP 127.0.0.1:64243 (LISTEN)
exit code: 0 after 0.00s
process still alive: False
ports STILL accepting after exit: []          <- 无监听端口残留
forwards ADDED by this run: []                <- 无 adb forward 残留
forwards REMOVED by this run: []              <- 未误删他人的 forward
device scrcpy procs NEW: 0                    <- 无新增设备端进程
processes using the PLUGIN's jar: 0
VERDICT: LEAK CHECK PASS
```

> 第一次跑时这个检查**报了 FAIL**，我追查后确认是误报：设备上确实有一个 scrcpy 进程，但它用的是
> `/data/local/tmp/scrcpy-server.jar` 且带 `audio=true audio_codec=raw`，而插件用的是
> `/data/local/tmp/scrcpymac-plugin-server.jar` 且带 `audio=false video_codec=h264`（`internal/scrcpy/session.go:144 spawnArgs`）。
> 它属于并发进行的桌面 scrcpy / ScrcpyMac.app 调查，且与一条**运行前就存在**的 forward（`tcp:27192 → scrcpy_5aeba335`）配对。
> 加上前后差分基线后复跑即 PASS。

**重要限制**：这次泄漏检查**没有起流**。它覆盖了「启动时绑定的 loopback 监听端口」这条真实路径，
但 `adb forward` 与设备端 scrcpy 进程的拆除路径**我没有亲自验证过**——因为起流会抢占并发调查正在用的设备。
该路径的证据只有单测（`TestCloseLeavesNothingBehind`）和 opt-in 的 `TestLiveStream`（见「切换前的阻塞项」第 3 条）。

---

## 已迁移

契约口径先说清楚，因为任务书里的数字和代码里的不一致：

- 任务书写「24 个 `phone_*` + `open_scrcpymac` + 12 个 `scrcpymac_ui_*`」，也写过「24+13」。
- **真实源码**：`mcp_ui.py` 里是 `open_scrcpymac` + **11** 个 `scrcpymac_ui_*` = 12 个工具。
- 因此冻结契约总数是 **36 = 24 + 1 + 11**，`docs/contract.json` 的 `toolCounts` 即
  `{"total":36,"phone":24,"openScrcpymac":1,"scrcpymacUi":11}`。我在真实二进制上数到的也是 36。
- 另有 1 个 Go 独有的 `phone_stream_status`（默认**不注册**，见行为差异 §9），加上它才是 37。

### 24 个 `phone_*`（全部已迁移）

| # | 工具 | 状态 | Go 实现 |
|---|---|---|---|
| 1 | `phone_backend` | 已迁移 | `internal/tools/device.go` |
| 2 | `phone_doctor` | 已迁移（3 处已批准差异） | `device.go` → `internal/doctor` |
| 3 | `phone_list_devices` | 已迁移 · **真机 parity 字节一致** | `device.go` |
| 4 | `phone_device_info` | 已迁移（streaming 分支未验证） | `device.go` |
| 5 | `phone_screenshot` | 已迁移 · **真机 parity 字节一致（含 210KB PNG）** | `device.go` |
| 6 | `phone_shell` | 已迁移 | `device.go` |
| 7 | `phone_current_app` | 已迁移 | `device.go` |
| 8 | `phone_launch_app` | 已迁移（**真机未冒烟**） | `device.go` |
| 9 | `phone_tap` | 已迁移（verify/retry 全套；错误分支真机 parity 一致） | `internal/tools/input.go` |
| 10 | `phone_tap_relative` | 已迁移（**无 parity 用例**） | `input.go` |
| 11 | `phone_tap_image` | 已迁移（**无 parity 用例**） | `input.go` |
| 12 | `phone_swipe` | 已迁移 | `input.go` |
| 13 | `phone_long_press` | 已迁移 | `input.go` |
| 14 | `phone_key` | 已迁移 | `input.go` |
| 15 | `phone_type` | 已迁移 | `input.go` |
| 16 | `phone_paste` | 已迁移（本机 ROM 剪贴板服务未实现，Python 同样如此） | `input.go` |
| 17 | `phone_ui_tree` | 已迁移 · **真机 parity 字节一致（compact + raw）** | `internal/tools/uitree.go` |
| 18 | `phone_find_and_tap` | 已迁移 | `uitree.go` |
| 19 | `phone_wait_for_text` | 已迁移 | `uitree.go` |
| 20 | `phone_enable_wifi_adb` | 已迁移（**故意修了 Python 的 bug**，见差异 §8；**真机未验证**） | `internal/tools/wifi.go` |
| 21 | `phone_get_device_ip` | 已迁移 | `wifi.go` |
| 22 | `phone_connect_wifi` | 已迁移（**真机未验证**） | `wifi.go` |
| 23 | `phone_disconnect_wifi` | 已迁移（**真机未验证**） | `wifi.go` |
| 24 | `phone_send_wechat` | 已迁移（**只用 fake driver 测过，从未发过真消息**） | `internal/tools/recipes.go` |

### `open_scrcpymac` + 11 个 `scrcpymac_ui_*`（全部已迁移）

| # | 工具 | 状态 | Go 实现 |
|---|---|---|---|
| 1 | `open_scrcpymac` | 已迁移（`_meta` 含 `openai/outputTemplate`） | `internal/tools/widget.go` |
| 2 | `scrcpymac_ui_state` | 已迁移 | `widget.go` |
| 3 | `scrcpymac_ui_select_device` | 已迁移 | `widget.go` |
| 4 | `scrcpymac_ui_snapshot` | 已迁移（Pillow → 标准库，缩放算法逐位对齐 Pillow） | `widget.go` + `widget_image.go` |
| 5 | `scrcpymac_ui_tap` | 已迁移 | `widget.go` |
| 6 | `scrcpymac_ui_swipe` | 已迁移 | `widget.go` |
| 7 | `scrcpymac_ui_key` | 已迁移（runtime keycode 表补了 `paste=279`） | `widget.go` |
| 8 | `scrcpymac_ui_paste` | 已迁移 | `widget.go` |
| 9 | `scrcpymac_ui_connect_wifi` | 已迁移（唯一 `openWorldHint:true` 的工具） | `widget.go` |
| 10 | `scrcpymac_ui_start_stream` | 已迁移（**真机起流路径本次未验证**） | `internal/tools/scrcpy_stream.go` |
| 11 | `scrcpymac_ui_stream_status` | 已迁移 | `scrcpy_stream.go` |
| 12 | `scrcpymac_ui_stop_stream` | 已迁移（**真机停流路径本次未验证**） | `scrcpy_stream.go` |

**没有「部分迁移」和「未开始」的工具** —— 36 个全部注册、全部有 handler、全部被
`internal/mcpserver/contract_test.go` 逐字段（title / description / inputSchema / outputSchema /
annotations / `_meta` / 每个参数的 name·type·title·required·default·顺序）比对 `docs/contract.json`。
但「已注册且契约一致」≠「行为已在真机验证」，逐条实测状态见上表括号和下一节。

### 非工具部分

| 组件 | 状态 |
|---|---|
| widget 资源 `ui://widget/scrcpymac/app.html` | 已迁移，HTML 经 `//go:embed`（364,891 B，与 `server/phone_agent/static/scrcpymac-app.html` **cmp 字节一致**） |
| CSP `connectDomains` / `openai/widgetCSP` | 已迁移，改为 marshal 时惰性求值，真实端口已验证进入 CSP |
| adb 发现与调用（`adb.py`） | 已迁移 `internal/adb` |
| doctor（`doctor.py`） | 已迁移 `internal/doctor`（3 处已批准差异） |
| scrcpy 运行时 / H.264 中继（`scrcpy_runtime.py`） | 已迁移 `internal/scrcpy`，传输层**重新设计**（见差异 §11） |
| WeChat recipe | 已迁移 `internal/tools/recipes.go` |
| CLI `mcp` / `doctor` / `devices` / `version` | 已迁移 `cmd/phone-agent/main.go` |
| 打包（bundled adb、`share/scrcpy-server` 布局） | **未完成**，见下节 |

---

## 未完成

按「阻塞发布」→「影响正确性」→「可延后」排序。工时是我按代码现状估的。

### A. 阻塞 Marketplace 发布

| # | 事项 | 原因 | 工时 |
|---|---|---|---|
| A1 | **`THIRD_PARTY_NOTICES.md` 没有任何 Go 依赖条目**（我 grep 过：`go-sdk`、`jsonschema-go`、`segmentio`、`uritemplate`、`golang.org/x` 全部无匹配） | 需要补 7 个依赖：`modelcontextprotocol/go-sdk`（Apache-2.0 + 残留 MIT 尾巴，两段文本都要）、`google/jsonschema-go`、`segmentio/encoding`、`segmentio/asm`、`yosida95/uritemplate/v3`、`golang.org/x/oauth2`、`golang.org/x/sys`。该文件在 `go/` 之外，且许可文本不该由 agent 代写 | 1–2 h（人工） |
| A2 | **bundled adb 没有随包**。真机 doctor 输出 `"adb": {"bundled": false}`，实际落到 `~/Library/Android/sdk/platform-tools/adb`。`bin/darwin/adb` 与 `bin/darwin/{arm64,x86_64}/adb` 都不存在（`paths.BundledADBCandidates` 找的就是这两个路径） | 与目标「一个包，无外部运行时」直接冲突。当前 `bin/` 下只有 `bin/phone-agent`、`bin/darwin/README.md`、`bin/darwin/share/scrcpy-server` | 0.5 h（跑 `scripts/download-adb.sh` 并决定入库还是发布时打包） |
| A3 | **`share/scrcpy-server` 仍在旧位置**。Go 按目标布局 `<root>/share/scrcpy-server` 优先查找，但真机 doctor 命中的是 legacy 路径 `bin/darwin/share/scrcpy-server`（`internal/paths/paths.go:191-193` 三个候选都支持，所以能用，但不是目标布局） | 目标布局未落地；能工作，属打包整洁性 | 0.5 h |
| A4 | **`internal/version.Version` 不在版本一致性校验集合里**。`server/tests/test_packaging.py:50-59` 校验 marketplace.json（两处）、`.codex-plugin`、`.cursor-plugin`、`pyproject.toml`、`ui/package.json`、`__init__.py` 共 7 处 —— 不含 Go。今天全部是 `0.7.2`，但没有任何机制阻止漂移 | 该文件在 `server/` 下，本次迁移不允许修改 | 0.5 h（切换时随 `server/` 一起处理） |

### B. 影响正确性 / 尚无证据

| # | 事项 | 原因 | 工时 |
|---|---|---|---|
| B1 | **H.264 流的真机验收从未在本次跑过**。`TestLiveStream` 默认 skip（需 `PHONE_AGENT_LIVE_SERIAL`） | 并发调查正占用同一台设备的 scrcpy 会话，两个进程不能同时持有 | 15 min（设备空闲时） |
| B2 | **三个 streaming seam 只有单测，没有真机证据**：`phone_backend` 返回 `"plugin-h264"`、`phone_device_info` 的 video 子对象、`phone_tap`/`key` 的 `backend:"plugin-control"` 载荷形状 | 树内没有任何测试会自己起流 | 20 min（与 B1 一起做） |
| B3 | **`phone_tap` 的 success 分支从未真机验证**。parity 只跑到 give-up 分支（静态屏幕），`verified:true` + 提前返回 + 截断的 attempts 列表没走到 | 需要一块「点了确实会变」的屏幕，与 parity 依赖的静态屏幕前提冲突 | 30 min（用一个 scratch app） |
| B4 | **`phone_tap_relative` / `phone_tap_image` 无 parity 用例**。它们是全项目数值风险最高的两个（Python `round()` 是银行家舍入，`round(0.5*1077)` = 538，而 `math.Round` = 539） | 当初不在里程碑清单上。Go 侧已用 `jsonresult.PyRoundInt` + 22 行表驱动单测覆盖，但没有和 Python 对过 | 30 min |
| B5 | **Wi-Fi 三个工具（`enable_wifi_adb`/`connect_wifi`/`disconnect_wifi`）真机零验证** | `adb tcpip` 会重启 adbd、`adb disconnect` 会断掉所有 TCP/IP 设备，都会打断并发调查 | 20 min（备用机上做） |
| B6 | **`phone_send_wechat` 只用 fake driver 测过** | 真跑会真的发微信消息 | 需你本人决定是否验收 |
| B7 | **`phone_launch_app` 真机未冒烟** | 会改变前台 App | 5 min |
| B8 | **`ui_tree` 的 degraded / parse-error 分支真机未走到**。真机屏幕干净解析出 38 节点 | 只有树内 golden 覆盖 | 20 min |
| B9 | **两条错误模板不可达**：「No Android device connected...」和「Multiple devices connected (...)」 | 只接了一台设备 | 10 min（拔线 / 接两台） |

### C. 已知功能缺口（有意留下）

| # | 事项 | 原因 | 工时 |
|---|---|---|---|
| C1 | **scrcpy `RESET_VIDEO` 未实现**。丢 GOP 后客户端最长要等 10 s 才能等到下一个 IDR | 无法离线从 bundled dex 里提出数值常量（本机无 dexdump/baksmali），猜错会导致 scrcpy 拆掉 control socket、tap/swipe 全废。相关 agent 拒绝猜测是对的。实测该路径目前不可达（0 丢弃、客户端队列峰值 0 字节） | 常量确认后约 10 行 |
| C2 | **in-process adb 下载未实现**（`PHONE_AGENT_AUTO_DOWNLOAD_ADB`）。仍由 bash launcher 负责，Go 只打一条 warning 就继续（`cmd/phone-agent/main.go:140` 有注释说明） | 目标是「无外部运行时」，长期应内置 `net/http` + `archive/zip` 版本 | 2–3 h |
| C3 | **control socket 没有写超时**。`Runtime.controlSend`（`internal/scrcpy/runtime.go:835`）持 `controlMu` 跨一次阻塞 `conn.Write`，设备停止读会卡住所有输入工具直到拆除 | 与 Python 完全一致（Python 也是锁 + 阻塞 socket），属**未回归**而非新问题。但 Go 加一行 deadline 就能封顶，Python 做不到 | 15 min |
| C4 | **`internal/widget` 与 `internal/doctor` 没有自己的测试文件** | 行为由 `internal/tools` 和 `internal/mcpserver` 间接覆盖 | 1 h |
| C5 | **parity harness 的 `--cases` 遇到不存在的用例名会静默跑 0 个用例并 exit 0**。我实测：`--cases totally_bogus_case_name` → `EXIT=0`、0 个 PASS。我自己一开始误写 `ui_tree`（正确是 `ui-tree`）就静默什么都没跑 | 一旦进 CI，一个拼写错误会变成「永远通过」的假绿 | 15 min（加一个未知名报错） |

---

## 与 Python 的行为差异

分三类：**已验证**（我或 parity harness 在真机上确认过）、**已实现但未验证**、**规格等价**。

### 已验证的有意差异

| # | 差异 | 理由 | 验证方式 |
|---|---|---|---|
| 1 | **doctor：`python` 检查 → `binary`**（detail 变为 `phone-agent 0.7.2 darwin/arm64 (go1.26.5)`） | Go 里没有 Python 解释器可报 | 真机 parity，harness 断言这是**仅有的三条差异之一** |
| 2 | **doctor：`mcp_package` 检查 → `plugin_root`**（新增 `derived` extra） | 同上，且 plugin root 是否派生更有诊断价值 | 同上 |
| 3 | **doctor：顶层 `uv_available` 移除** | Go 不用 uv | 同上 |
| 4 | **`tools/list` 顺序不同**：Go SDK 按名字排序（`open_scrcpymac`, `phone_*`, `scrcpymac_ui_*`），Python 是注册顺序（`open_scrcpymac`, `scrcpymac_ui_*`, `phone_*`） | MCP spec 未定义顺序，客户端按名字索引。`contract_test.go` 有意只断言工具**集合**，参数顺序则严格断言（它会到 wire 上） | 我实测确认：名字集合完全一致（missing []、extra []），顺序 `False` |
| 5 | **`phone_stream_status` 默认不注册**，需 `PHONE_AGENT_STREAM_DIAGNOSTICS=1` | 它是 model-visible 的，注册就等于给 Codex 多一个 Python 从未有过的第 25 个 `phone_*`，违反 drop-in 约束 | 我实测：默认构建 `tools/list` 恰好 36 个，无 `phone_stream_status`；`smoke-stdio.sh` 也报 36 |
| 6 | **doctor 的两个 Python bug 未移植**：`"\0"` 哨兵（`PHONE_AGENT_ROOT` 为空时所有文件都报 `bundled:true`）、`current_app()` 在 `screen_size()` 成功后抛异常时重复发出的 `screen_size` 检查 | 都是明显缺陷 | parity 的 doctor 用例通过，说明未引入新差异 |

### 已实现但**尚未在真机验证**的差异

| # | 差异 | 理由 | 未验证的原因 |
|---|---|---|---|
| 7 | **`phone_enable_wifi_adb` 改跑 host 命令 `adb tcpip <port>`**，而非 Python 的 `adb shell tcpip <port>` | Python 那条是设备端不存在的二进制，之前有 agent 实测 exit 127 / `/system/bin/sh: tcpip: inaccessible or not found`——**该工具从未成功过一次**，所以没有任何东西能依赖旧输出。结果 key 不变，只有 `output` 从错误串变成 `restarting in TCP mode port: 5555` | `adb tcpip` 会重启 adbd，打断并发调查（B5） |
| 8 | **runtime keycode 表补入 `paste=279`** | Python 的 `scrcpy_runtime.KEYCODES` 只有 10 项漏了 `paste`，而 `actions.KEYCODES` 有 11 项——导致 `key("paste")` 走 adb 能用、一起流就报 `Unknown key`。这会改变 streaming 后端「Supported: ...」错误串的尾部 | 需要真流（B2） |
| 9 | **H.264 中继传输层重新设计（本次迁移的核心意图）**：每客户端有界队列（8 MiB / 4096 帧）+ 独立 writer goroutine；溢出时丢**整个 GOP**（绝不丢孤立 delta），在关键帧边界恢复；scrcpy video/control socket 与 loopback 全部开 `TCP_NODELAY`；packet pump 只做「读 → framing → 各客户端锁内 append」，绝不在 pump 上写 socket。新客户端接入时回放 config + **当前完整 GOP**（Python 只回放 config + 最后一个关键帧，等于把参考帧缺失的 delta 直接喂给解码器） | 任务书明令三条不得复刻的坏模式：`_broadcast_binary` 在 pump 线程上同步 `sendall()`、无队列无丢弃策略、全局无 `TCP_NODELAY` | 中继 agent 实测过 ~40 packets/s；**我本次没有起流复现** |
| 10 | **`SwipeRelative` 可被 context 取消**，取消前先抬手，不会让设备停在「手指按住」状态 | Python 无法打断一次 10 秒 swipe，会把关机拖满 | 需真流 |
| 11 | **拆除时追加 scid 定向 `adb shell pkill -f scid=<hex>`**；`Close()` 是终态的，关闭后拒绝重新绑定 loopback | Python 的 `close()` 把 listener 置 nil，之后 `_ensure_loopback()` 会绑到**不同端口**，而已发布的 `_meta` 还在宣传旧端口——只有四条通配 CSP 条目救了 widget | 单测 `TestCloseLeavesNothingBehind` 覆盖；真机拆除路径未验证（B1） |
| 12 | **中途拆除时的报错文案不同**：Python 每个事件重查 `_require_control_meta()` 抛「plugin scrcpy stream is not running」，Go 的 `sendTouch` 携带 meta 因此由 `controlSend` 抛「plugin scrcpy control socket is not available」。两者都是 model-visible | 结构不同导致 | 需要「流在 swipe 中途停掉」的时序 |

### 规格等价 / 不可见的差异

| # | 差异 | 说明 |
|---|---|---|
| 13 | `readOnlyHint: false` / `idempotentHint: false` 被省略 | Go SDK 把它们声明成带 `omitempty` 的 `bool`，显式 false 会被丢掉。spec 默认值就是 false，语义完全相同。影响 9 个 `scrcpymac_ui_*` |
| 14 | 成功时省略 `"isError": false`，capabilities 省略 `"experimental": {}` | 均与 spec 等价 |
| 15 | `serverInfo.version` 报插件版本 `0.7.2` 而非 MCP SDK 版本 | Python 报 SDK 版本（`1.28.1`）纯属 FastMCP 未传显式版本的意外。我实测 Go 报 `0.7.2` |
| 16 | `structuredContent` 的 key 顺序是字母序，Python 是插入顺序 | 共享的 `Structured` helper 交给 SDK 的是 `payload.Map()`，Go map 会排序。**可观测的地方顺序都保住了**：Shape A 的 text block 走 `jsonresult.Obj`，parity 实测 `phone_list_devices`/`phone_screenshot`/`phone_ui_tree` **字节一致**。若要连 `structuredContent` 也保序，只需让 `internal/tools/tools.go` 的 `payloadValue` 直接返回 `*jsonresult.Obj` |
| 17 | `internal/tools.JSON` 把**所有**错误转成 `{"ok":false,"error":...}`；Python 只 catch `AdbError` 和 `OSError`，别的逃逸成 `isError:true` 且无 structuredContent | 更一致；但确实改变了非 adb 异常的可见形状 |
| 18 | `scrcpymac_ui_snapshot` 的 JPEG 字节与 Pillow 不同（Go 的 `image/jpeg` 不是 libjpeg-turbo），因此 `sizeBytes`/`dataBase64` 不可逐字节比对 | 属契约的十个 key、`deviceWidth/Height`、`frameWidth/Height`、`mimeType` 全部一致；缩放步骤本身复刻了 Pillow 的 22-bit 定点系数，实测逐通道字节一致 |
| 19 | `phone_tap` 的 change score 用手写 box/area-average 重采样，而非 Pillow BICUBIC | `x/image/draw` 不在允许依赖内。实测四张真机截图上 Go 0/0.9912/0 vs Pillow 0/0.9897/0，差 0.15%，阈值同侧 |
| 20 | 各处 sleep（0.45 s settle、150 ms 剪贴板、poll backoff、recipe 里的 sleep）改为 select on `ctx.Done()` | Python 无条件 sleep。只在关机时可观测 |
| 21 | `null` 的 `arguments` 成员被规范化为「缺省」（`internal/mcpserver/middleware.go`） | **必须做**：go-sdk v1.6.1 + jsonschema-go v0.4.3 在这上面会 panic 掉整个进程。同一中间件还兜住 handler 的 panic，否则 panic 落在 SDK goroutine 上，`main` 的 `defer Env.Shutdown` 不会执行，scrcpy 进程、adb forward、loopback 端口全泄漏 |
| 22 | `phone_launch_app` 对参数做 `shlex.quote`，Python 不做 | 合法包名/Activity 名的每个字符都在 shlex 安全集内，实际输出对所有真实输入逐字节相同；只有恶意参数会变 |
| 23 | `internal/adb.RemoveForward` 用 `context.WithoutCancel` 剥离取消 | 否则 server context 一取消，exec 立刻杀掉 adb，`localabstract:scrcpy_*` forward 会活得比进程久。这是**修复**，不是差异 |

### 有意复刻的 Python「怪异行为」（不要当 bug 修掉）

- `phone_long_press` 返回 `action:"swipe"` 带 `from`/`to`，而不是 `action:"long_press"` 带 `x`/`y`。
- `phone_tap` 在 baseline 截图失败路径上，`verification.attempts` 是**整数 1**，其余路径都是列表。
  （spec-actions 建议改成单元素列表，但 `contract.json` 记录的是整数，按「复刻意外行为」的指示保留。）
- adb 路径拼 `duration_ms`，plugin-control 路径拼 `durationMs`；`phone_key` 只在 adb 路径返回 `code`。
- `phone_swipe` 不 clamp 坐标而 `phone_tap` clamp；`type_text` 在空串检查**之前**调 `_ready()` 而 `paste` 之后。
- `%` → `%25` 先于 ` ` → `%s` 的转义顺序（反了会毁掉每个含空格的字符串）。
- `wm size` 取**第一个** `\d+x\d+` 匹配，所以有 display override 时报的是物理尺寸。
- `screenshot()` 的 width/height 来自 `wm size` 而非 PNG 头（`tap_relative` 映射进的是同一空间）。
- `phone_connect_wifi` 不 strip host，`scrcpymac_ui_connect_wifi` 才 strip；`phone_disconnect_wifi` 无条件追加 `:5555` 且无 port 参数。
- ui_tree 缓存无 TTL、不被 `shell()` 失效。

### 尚未定论的小问题

- `tools.Clamp(v, lo, hi)` 在 `lo > hi` 时返回 `hi`，Python 的 `max(lo, min(hi, v))` 返回 `lo`。
  只有 `wm size` 报 `0x0` 才可达，真机不可能。未修（会给 tap 热路径加一个分支）。
- `scrcpymac_ui_stop_stream` 最坏约 24 s（terminate 2+2 s、scid pkill 10 s、`forward --remove` 10 s）。
  只有 adb 本身卡死时可达，但客户端侧超时会表现成「挂住」。

---

## 切换前的阻塞项

> **这是用户迁移的第 4 步，尚未执行，未经你批准不得执行。**
> 当前 `mcp-server.sh` 与 `mcp.json` 我确认**未被修改**（`git status` 对这两个文件为空）。

### ⚠️ 先说一件必须知道的事：`bin/phone-agent` 已经被改过了

任务书写的是「先不要改 `mcp-server.sh`、`mcp.json` 或 `bin/phone-agent`」，但 `bin/phone-agent`
**已被前序 agent 改成双后端 shim**（`git diff` 显示 +53/-1 行）。当前逻辑：

```bash
backend="${PHONE_AGENT_BACKEND:-auto}"      # 默认 auto
case "$backend" in
  go|auto)
    if select_go_binary; then               # 找 bin/<platform>/<arch>/phone-agent
      exec "$GO_BINARY" "$@"                # 找到就直接用 Go
    fi
    ...                                     # 找不到才落回 Python
```

**今天仍然走 Python**，因为 `bin/darwin/arm64/phone-agent` 不存在（我确认过）。
**但这意味着切换是「隐式」的**：任何人只要跑一次 `scripts/build-go.sh` 或 `make cross`
（后者的 `BIN_DIR := $(PLUGIN)/bin/darwin`），插件就会在**没有任何人显式批准**的情况下切到 Go。

**建议在做任何事之前先决定**：要么把默认值改成 `PHONE_AGENT_BACKEND:-python`（显式 opt-in 才用 Go），
要么接受 auto 语义但把它写进发布检查单。

### 切换检查单

**第 0 组 —— 决策（需要你拍板，不是技术工作）**

- [ ] 0.1 决定 `bin/phone-agent` 的默认后端语义（见上）。
- [ ] 0.2 决定 `phone_stream_status` 是否要对 Codex 可见。当前默认关闭；打开只需删掉
      `internal/tools/widget.go` 的 env gate 并从 `contract_test.go` 的 `deliberateAdditions` 移除该条。
- [ ] 0.3 决定 `phone_doctor` 的 description 是否要改。它仍逐字保留
      `"Run environment diagnostics (adb, device, ScrcpyMac Agent, Python dependencies)."`，
      而 Python 依赖检查已经不存在了。改动 model-visible 文案是产品决定。

**第 1 组 —— 真机验收（必须在设备空闲、并发调查结束后做）**

- [ ] 1.1 清掉 3 条陈旧 forward（**它们不是我造的**，运行前就在，且无对应存活进程）：
      `adb forward --remove tcp:27196`、`tcp:52287`、`tcp:27192`。
      顺带说明：它们证明**被 SIGKILL 的会话确实会漏 forward**，Go 的拆除路径覆盖了它能到达的每条分支，但 SIGKILL 不在其中。
- [ ] 1.2 跑流验收门：
      `ADB_PATH=... PHONE_AGENT_ROOT=.. PHONE_AGENT_LIVE_SERIAL=2f019965 go test ./internal/scrcpy -run TestLiveStream -v`
- [ ] 1.3 起流后手工过一遍 streaming 分支：`phone_backend` 应返回 `"plugin-h264"`、
      `phone_device_info` 应带 video 子对象、`phone_tap` 应返回 `backend:"plugin-control"`、`phone_key("paste")` 应成功。
- [ ] 1.4 跑完整 parity（含 `tap`）：`make parity`。约 90 s。**先把前台切到一个安全 App** ——
      我这次没跑 tap 就是因为前台是真实的加密货币交易界面。
- [ ] 1.5 补 `phone_tap_relative` / `phone_tap_image` 的 parity 用例（B4，全项目数值风险最高的两个）。
- [ ] 1.6 备用机上验 Wi-Fi 三件套 + `phone_launch_app`（B5、B7）。
- [ ] 1.7 你本人决定是否验收 `phone_send_wechat`（会真的发消息）。

**第 2 组 —— 打包（阻塞发布）**

- [ ] 2.1 补齐 `THIRD_PARTY_NOTICES.md` 的 7 个 Go 依赖条目（A1）。
- [ ] 2.2 把 adb 放进 `bin/darwin/adb`，并确认 doctor 报 `"adb": {"bundled": true}`（A2）。
- [ ] 2.3 把 `scrcpy-server` 挪到目标布局 `share/scrcpy-server`（A3）。
- [ ] 2.4 把 `internal/version.Version` 纳入 `server/tests/test_packaging.py` 的版本一致集合（A4）。
- [ ] 2.5 用 `scripts/build-go.sh all` 产出两个 arch 的二进制并确认体积/签名。
      当前 release 形态（`-trimpath -ldflags "-s -w"`）实测：arm64 **9,024,834 B**、amd64 **9,700,720 B**。
- [ ] 2.6 确认嵌入的 widget 是最新的：`make widget-check`。
      （我已确认当前嵌入件与 `server/phone_agent/static/scrcpymac-app.html` **cmp 字节一致**，364,891 B。）

**第 3 组 —— 切换动作本身**

- [ ] 3.1 在**不删 Python** 的前提下先让 `PHONE_AGENT_BACKEND=go` 跑满一整个工作日，作为灰度。
- [ ] 3.2 确认回滚路径可用：`PHONE_AGENT_BACKEND=python`，或删掉 `bin/darwin/*/phone-agent`。
- [ ] 3.3 才动 `mcp-server.sh` / `mcp.json`（如果确实需要动 —— 若 `bin/phone-agent` shim 语义定好了，
      这两个文件其实可以一直不动）。
- [ ] 3.4 **最后**才删 `server/`。

**关于回滚路径的一个重要提醒**（这条改变了风险计算）：

- `server/pyproject.toml` 里是 `mcp>=1.0.0`，**没有上界**（我确认过）。
- 本机 `.venv` 恰好装着 `mcp 1.28.1`，我实测 `FastMCP.resource()` 在这个版本**接受** `meta` 参数，
  Python server 也确实 `IMPORT OK`。所以**本机的 Python 后端目前是能用的**。
- 但前序 recon agent 报告说更新的 `mcp` 版本移除了 `resource()` 的 `meta` 参数，会让 `mcp_ui.py:125` 抛
  `TypeError`——而 `ensure-runtime.sh` 是往全新 venv 里做无上界 pip install 的，意味着**今天新装的
  Marketplace 用户可能直接拿到一个起不来的 server**。
  **这一条我没有独立验证**（需要联网装新版本）。如果属实，「回滚到 Python」并不是一条可靠的退路，
  切换前应当先把 `mcp` 版本钉死（例如 `mcp==1.28.1`），让回滚路径真实存在。

---

## CI

### 现状

`.github/workflows/ci.yml` 有两个 job：`macos-app`（xcodebuild）和 `phone-agent`。
后者跑在 **ubuntu-latest**，且**完全是 Python 的**：`ensure-runtime.sh` + `download-adb.sh linux` +
`unittest discover` + `bin/phone-agent doctor`。**目前没有任何 Go 步骤。**

### 需要新增的 Go 构建 job

必须跑在 **macos-14**（arm64 runner）而不是 ubuntu，理由有三：交叉编译 darwin/amd64 需要在 macOS 上验证；
`smoke-stdio.sh` 要真的起进程；`internal/paths` 的 bundled-adb 候选路径是 `runtime.GOOS`/`GOARCH` 相关的。

需要的步骤（全部我已在本机逐条实测通过）：

```yaml
  phone-agent-go:
    runs-on: macos-14
    defaults:
      run:
        working-directory: plugins/scrcpymac-phone-agent/go
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: '1.25.x'        # go.mod 要求 go 1.25.0；本机实测 1.26.5 亦可
          cache-dependency-path: plugins/scrcpymac-phone-agent/go/go.sum

      - run: test -z "$(gofmt -l .)" || { gofmt -l .; exit 1; }
      - run: go build ./...
      - run: go vet ./...
      - run: go test ./... -count=1
      - run: go test -race ./... -count=1

      # 契约不能漂移 —— 这是 drop-in 约束的自动化形式
      - run: go test ./internal/mcpserver/ -run TestContract -count=1 -v

      # 默认构建必须恰好 36 个工具；PHONE_AGENT_STREAM_DIAGNOSTICS 必须清空，
      # 否则开发者本地的 export 会掩盖回归
      - run: PHONE_AGENT_STREAM_DIAGNOSTICS= go test ./internal/mcpserver/ -run TestDefaultSurfaceIsExactlyTheContract -count=1

      # stdout 只能有 JSON-RPC —— 一行杂散日志就会毁掉 MCP framing
      - run: scripts/smoke-stdio.sh

      # 嵌入的 widget 不能过期（go:embed 会静默打包旧 UI）
      - run: make widget-check

      # parity harness 自身必须能抓到坏移植（无需设备）
      - run: make parity-selftest

      # 两个 arch 都要能出包
      - run: CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath -ldflags "-s -w" -o /tmp/pa-arm64 ./cmd/phone-agent
      - run: CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o /tmp/pa-amd64 ./cmd/phone-agent
      - run: file /tmp/pa-arm64 /tmp/pa-amd64
```

关于 arm64 / x86_64 的注意点：

- `CGO_ENABLED=0` 是必需的（任务书禁 cgo），也让交叉编译无需 SDK。两个 arch 我都实测 exit 0。
- **x86_64 二进制无法在 arm64 runner 上执行测试**，只能验证「能编译」。
  若要真跑 amd64 的测试，需要一个 `macos-13`（Intel）runner 或 Rosetta 2。
  值得注意的是 `internal/version` 已经会报告是否运行在 Rosetta 2 下（doctor 的 `binary` 检查）。
- 产物应作为 artifact 上传，供第 2 组打包检查单使用。

### 泄漏检查（你要求的三项）

**目前树内没有一个「进程退出后无残留」的端到端检查。** 现有的只是零件：

| 已有 | 覆盖 | 缺口 |
|---|---|---|
| `TestCloseLeavesNothingBehind`（`internal/scrcpy/runtime_test.go:377`） | loopback listener 停止接受连接、goroutine 数回到基线、`Close` 幂等、关闭后拒绝重绑 | 进程内，不含 adb forward、不含设备端进程 |
| `TestConcurrentClientsBroadcastAndTeardown` | 真实 loopback + 8 个 goroutine 抖动客户端后 Close，零 goroutine 泄漏、端口释放 | 同上 |
| `TestLiveStream`（`internal/scrcpy/live_test.go:150-168`） | `adb forward --list` 不含分配的端口；设备上 scid 唯一的进程已消失 | **默认 skip**，需真机 |
| 我本次跑的 leak check | 无残留监听端口、无新增 forward、未误删他人 forward、无新增设备端进程、进程退出码 0 | **没起流**，因此拆流路径未覆盖 |

**建议加一个 CI 步骤**，无需设备就能覆盖「监听端口」这一项（另两项需要设备，只能放进真机验收）：

```yaml
      - name: Leak check (no device) — process exit leaves no listening port
        run: |
          go build -o /tmp/pa ./cmd/phone-agent
          # 启动、握手、读 widget resource 拿到 CSP 里的 loopback 端口、关闭 stdin
          # 断言：进程退出码 0；该端口不再接受连接；lsof 无该 pid 的 LISTEN
```

我本机跑的等价脚本产出的正是这段证据：

```
loopback ports advertised in CSP: [64243]
ports accepting connections WHILE RUNNING: [64243]
lsof: phone-age 62148  TCP 127.0.0.1:64243 (LISTEN)
exit code: 0
ports STILL accepting after exit: []
forwards ADDED by this run: []
```

三项的最终覆盖建议：

| 检查 | 何处执行 | 现状 |
|---|---|---|
| 退出后无残留 scrcpy 进程 | 真机验收（第 1 组 1.2） | `TestLiveStream` 已断言（scid 定向），**本次未跑** |
| 退出后无残留 adb forward | 真机验收（第 1 组 1.2） | `TestLiveStream` 已断言，**本次未跑**。我本次验证了「不起流时不会创建 forward」 |
| 退出后无监听端口 | **可进 CI，无需设备** | 树内无端到端检查；我手工验证通过，建议按上面的 step 固化 |

另外三条 CI 硬化建议：

1. **`--cases` 拼错会静默通过**。我实测 `run-parity.sh --cases totally_bogus_case_name` → 0 个用例、**exit 0**。
   若 parity 进 CI，先让未知用例名报错，否则一个 typo 就是永久假绿（C5）。
2. **parity fixture 的新鲜度无人把关**。`scripts/parity/refresh-fixtures.sh` 的头注释说了要从通过的 run 刷新，
   但没有任何机制强制。一次被批准的契约变更若忘了刷新，selftest 就会把旧形状固化下来。
3. **现有的 ubuntu Python job 要保留到 `server/` 删除为止**，它是回滚路径的唯一守护者。

---

## 一句话结论

**代码层面：36 个工具全部迁移完毕，三个门禁与 race、跨架构、契约、stdio smoke 全部通过（我亲自验证），
真机 10 个只读 parity 用例字节级一致。**

**但这不等于可以切换。** 真正没有证据的是三块：**H.264 流的真机验收从未在本次执行**（这恰恰是整个迁移的核心动机）、
**Wi-Fi 与 WeChat 全部零真机验证**、**打包尚未完成**（bundled adb 缺席、`THIRD_PARTY_NOTICES.md` 无 Go 条目）。
外加一个隐蔽风险：`bin/phone-agent` 已被改成 auto 后端，**任何人跑一次构建脚本就会在无人批准的情况下完成切换**。

第 4 步（切换）没有做，也不应该在上面第 1 组和第 2 组清空之前做。

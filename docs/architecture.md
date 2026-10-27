# 架构与代码地图

浏览器里复刻 Rockchip `upgrade_tool` 的刷机路径：协议与编排用 Go 编译成 WebAssembly，USB 走 WebUSB，
完全本地运行。

当前阶段：M1（Go/WASM 核心 + WebUSB bridge + `ld`/`cd`/`sd` + Download Boot + 只读信息）、
M2（离线 `uf --dry-run` 计划）、M3a（写入层 + `uf` 状态机，mock/回放验证）、M3b（WebUSB 写入绑定 +
真机前自检）、M4（可安装 PWA + 离线 + 官方 `.zip` 直读），以及 M4b（五区块单页：镜像下载 / 校验 /
设备列表 / 刷写 / 日志）。

M3b 之后浏览器里已经有一条完整的写入路径：从 `update.img` 的 RKFW 头里切出**镜像自带的 loader** 做
Download Boot，再用 `internal/uf` 的同一套状态机执行 `gpt → wl → rd`；写入前必须通过页面的逐项自检。
M4 把这一页变成可以装到桌面、断网也能打开的应用，并且允许直接选择官方 `.zip`（ZIP64 + 流式 deflate，
不落地 3.8 GiB）。M4b 把杂乱的调试面板收成一条普通用户能看懂的主流程：**下载或选镜像 → 校验 → 选设备 →
刷写 → 看日志**，原来的 trace/命令/只读信息进“高级 / 诊断”折叠区。真机端到端由后续 stage 负责。

## 页面与运行时

```
main thread (app.js)                        worker (worker.js)
├─ WebUSB：requestDevice / open / claim      ├─ Go/WASM 核心（internal/rockusb + internal/device + internal/uf）
├─ controlOut / transferIn / transferOut     ├─ bridge.js：把 Go 的同步调用包成 Promise
├─ 镜像 File / plan / 自检清单 / 写入面板     ├─ imageplan：同一个 planner（CLI 与浏览器共用）
└─ RPC 服务端                                └─ 每次 USB 操作 → postMessage RPC → 主线程
```

- **主线程**独占 WebUSB：`requestDevice()` 需要用户手势，且 `navigator.usb` 只在 Window 上存在。
- **worker** 跑 wasm：`db` 要发 ~140 KiB、几十个控制包，放在 worker 里不阻塞 UI；`internal/uf` 的整段写入
  也在 worker 里跑，只把进度和日志发回主线程。
- Go 侧 `awaitPromise` 把 goroutine 停在 channel 上，Go 的 wasm 调度器把控制权还给事件循环，Promise 落地后再
  恢复——所以协议代码可以写成同步风格（`internal/rockusb` 不知道自己在浏览器里）。
- **镜像读取**：页面把 `File` 交给 worker，worker 包装成 `{name, size, read(offset, length)}`；Go 用
  `rkfw.OpenReaderAt` 分块读（头部、RKAF 表、parameter、loader、写入块），多 GiB 的镜像不会被整体搬进
  wasm 内存。`internal/imageplan` 是 CLI 与页面共用的**唯一** planner。
- **演示模式**把 RPC host 换成 worker 内的 mock 设备（`web/app/mock-host.js`），无需硬件即可演示
  `ld → info 被拒绝 → db → 重新枚举 → info`；写入演示则用 wasm 内的模拟器（`uf.SimConnector`），
  同一套状态机，不访问 USB。

## 阶段记录

| 阶段 | 内容 | 细节 |
|---|---|---|
| M0 | 只读探针 `/probe/`：授权、列设备、5 条只读命令、导出 JSON trace | 本文末节、[protocol-notes.md](protocol-notes.md) 第 1–8 节 |
| M1 | Go/WASM 核心、WebUSB bridge、`ld`/`cd`/`sd`、Download Boot、只读信息 | [device-commands.md](device-commands.md)、[protocol-notes.md](protocol-notes.md) 第 9–10 节 |
| M2 | 离线 `uf --dry-run` 计划：RKFW/RKAF/parameter 解析与校验 | [cli.md](cli.md)、[rkfw-rkaf-format.md](rkfw-rkaf-format.md) |
| M3a | 写入层 + `uf` 状态机，在 mock 设备与录制 trace 上验证 | [cli.md](cli.md)、[uf-write-path.md](uf-write-path.md) |
| M3b | WebUSB 写入绑定 + 打开设备前的 loader 自检与页面门禁 | [uf-write-path.md §9](uf-write-path.md#9-webusb-绑定与-loader-自检m3b) |
| M4 | 可安装 PWA、离线缓存、官方 `.zip` 直读 | [pwa-offline.md](pwa-offline.md) |
| M4b | 五区块单页（镜像 / 校验 / 设备 / 刷写 / 日志） | [m4b-ui.md](m4b-ui.md) |

阶段边界：M4 只改页面与工具链，`internal/` 的写入语义与 `web/app/preflight.js` 的门禁条件未动；M4b 只改
页面、下载/校验的前端编排与一处**新增的** wasm 导出（`ufVerify`），`ufPlan` / `ufRun` / `ufCancel` 与写入
状态机保持原样，`preflight.js` 的门禁条件一条没删、没有放宽（只新增“镜像已校验”）。

## 代码地图

- `internal/rockusb/`
  - `protocol.go`：命令表、CBW/CSW 编解码、错误码、只读守卫。
  - `crc.go`：CRC-CCITT 与 Rockchip 自己的 CRC-32（多项式 `0x04C10DB7`，见下）。
  - `transport.go`：`Transport` 接口、每传输超时、只对超时重试的策略、`Session.Exec`、传输 trace。
  - `downloadboot.go`：RKBOOT 容器解析与校验（含 LDR 版本/构建日期/`supportChip`）、vendor request 分包
    （含 4094/4095 两个上游特殊分支）、`DownloadBoot` 与按 loader 数据块 `index` 分类的错误。
  - `testdata/`：stage 2 真机 maskrom trace（5 条 CBW 全 STALL）与设备描述符，用作回放回归。
- `internal/device/`：设备枚举/DevNo/模式、`ld`/`desc`/`cd`/`sd`/`db`/`info` 的 CLI 风格 shell。
- `internal/imageplan/`：**唯一**的镜像 planner（RKAF item → parameter 分区 → LBA/阶段/未覆盖尾部），
  外加镜像自带 loader 的提取与 chip 校验；CLI 与浏览器共用。
- `internal/uf/`：写入状态机、IDB 模型、mock/replay 传输，以及把 `device.Bridge` 接进状态机的
  `BridgeConnector`（WebUSB 绑定）。
- `cmd/wasm/`：`syscall/js` 导出（`bindBridge` / `exec` / `inspectLoader` / `ufPlan` / `ufRun` /
  `ufCancel` / `ufVerify` / `imageLoader` / `demoImage` / `close`）与 JS bridge 传输。`ufVerify` 是 M4b
  新增的：结构报告 + PARM CRC + 可选 RKAF CRC / 整镜像 MD5，并在读取器上挂进度回调。
- `web/app/`：页面（五区块单流程 + 高级/诊断折叠区）。主线程 WebUSB host、module worker、mock 设备、
  `preflight.js` 门禁、`images.js` 镜像清单与流式下载、`verify.js` 校验结论、`devices.js` 设备列表、
  `logs.js` 日志仓库。
- `web/probe/`：M0 只读探针，保留下来用于抓 trace 与对照，不受后续阶段影响。
- `cmd/genprotocol/`：把 Go 的命令表/常量生成到 `web/app/protocol.js` 与 `web/probe/protocol.js`。

## 目录结构

```
pixi.toml / pixi.lock       依赖与任务（唯一入口）
internal/rockusb/           只读协议子集、RKBOOT 解析、Download Boot、单测
internal/device/            设备枚举与 shell（ld/desc/cd/sd/db/info）
internal/imageplan/         唯一的镜像 planner + 镜像自带 loader 提取 + 演示镜像
internal/uf/                写入状态机、IDB 模型、mock/回放、BridgeConnector、JSON 日志
cmd/uf/                     uf --dry-run 计划 + --mock/--replay 写入状态机
cmd/wasm/                   Go/WASM 核心导出（shell + ufPlan/ufRun/ufCancel/ufVerify/imageLoader/demoImage）
cmd/genprotocol/            生成 web/app/protocol.js 与 web/probe/protocol.js
web/app/                    页面：五区块单流程（镜像/校验/设备/刷写/日志）+ 高级诊断折叠区
web/app/images.js           镜像清单解析、CORS 探测、流式下载（File System Access API）
web/app/images.json         官方镜像清单（版本/文件名/大小/sha256/URL）
web/app/verify.js           校验结论与阻断/警告规则、2 分钟预算
web/app/devices.js          设备列表、模式判定与重新枚举匹配
web/app/logs.js             分级日志仓库与 TXT/NDJSON 导出
web/app/zip.js              ZIP32/ZIP64 中央目录解析（只读文件尾，不解压）
web/app/zip-source.js       按需解压的 image source（DecompressionStream，8 MiB 回退窗口）
web/app/sw.js               service worker：预缓存页面 + wasm，断网可开
web/app/manifest.webmanifest  PWA manifest（standalone、相对 scope）
web/app/icons/              PWA 图标（由 tools/make-icons.mjs 生成，已提交）
web/probe/                  M0 只读探针（保留用于抓 trace 与对照）
tools/serve.mjs             本地静态服务
tools/build-wasm.mjs        构建 wasm 并拷贝匹配的 wasm_exec.js
tools/zip-writer.mjs        测试/示例用的最小 zip 写入器（支持 ZIP64）
tools/make-sample.mjs       生成 web/samples/ 的演示镜像与 zip
tools/make-icons.mjs        生成 PWA PNG 图标（无图像依赖）
tools/demo-image/           把 imageplan.DemoImage 写成文件（供 sample 任务使用）
.github/workflows/ci.yml    pixi run build + test
.github/workflows/pages.yml 构建 web/ 并部署到 GitHub Pages
docs/README.md              文档索引
docs/quickstart.md          用户上手：安装、进 maskrom、刷写、救砖、日志导出
docs/usage-web.md           页面使用：真实设备、演示模式、离线验证
docs/cli.md                 uf 命令行：dry-run、mock、回放、日志、镜像片段
docs/architecture.md        本文件：运行时架构、阶段记录、代码地图
docs/development.md         开发环境、pixi 任务、构建产物与测试
docs/device-commands.md     设备模式、命令表、只读守卫与写入边界
docs/limitations.md         已知限制与待真机复验清单
docs/protocol-notes.md      协议笔记（含 §11 “为什么必须用镜像自带的 loader”）
docs/uf-write-path.md       写入命令、状态机、IDB/擦除假设、mock trace 与日志格式
docs/rkfw-rkaf-format.md    RKFW / RKAF / parameter 格式、GPT 计划与核对方法
docs/webusb-drivers.md      macOS arm64 / Linux / Windows 的 WebUSB 与驱动说明
docs/99-rockchip-webusb.rules  Linux udev 规则
docs/pwa-offline.md         安装、离线验证、zip 直读与日志导出
docs/m4b-ui.md              五区块页面：下载/校验/设备/刷写/日志、CORS 与 2 分钟规则
```

> 注：当前仓库快照未包含 `.github/workflows/`；CI/部署的既有约定见
> [development.md](development.md#ci-与部署)。

## M0 只读探针（`/probe/`）

`web/probe/` 是上一阶段的只读探针：授权、列设备、发 5 条只读命令、导出 JSON trace。它现在仍是抓取真机
trace 的工具（M1 的回归数据就是从它导出的），页面显著标注“只读，不会写入设备”。它的传输、命令和导出格式
见 `web/probe/` 源码与 [protocol-notes.md](protocol-notes.md) 第 1–8 节。

## 相关文档

- 第一次使用：[quickstart.md](quickstart.md)
- 页面操作：[usage-web.md](usage-web.md)
- 命令行：[cli.md](cli.md)
- 开发与测试：[development.md](development.md)

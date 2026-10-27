# 开发环境、构建与测试

依赖、脚本与运行时全部由 pixi 管理：不要用 `npm install`、`pip install` 或系统包管理器绕过
`pixi.toml` 添加依赖。Go 与 Node.js 都由 pixi 提供。

## 环境

```bash
pixi install    # 首次：go + nodejs 全部由 pixi 管理
```

## 常用任务

| 任务 | 做什么 |
|---|---|
| `pixi run dev` | 起本地静态服务 `http://localhost:5173/app/`（可安装页面；M0 探针在 `/probe/`） |
| `pixi run test` | `go test ./...` + `node --test`（协议单测、回放、mock、wasm 集成、worker RPC、zip/PWA） |
| `pixi run build` | 生成物检查 + `go build/vet` + wasm 构建 + 所有 JS 语法检查 |
| `pixi run wasm` | 只构建 `web/app/wasm/rk3588.wasm` 并拷贝匹配的 `wasm_exec.js` |
| `pixi run gen` | 改了 `internal/rockusb` 或 `internal/device` 的命令表后重新生成 `protocol.js` |
| `pixi run sample` | 生成 `web/samples/` 下的演示镜像与 zip（离线 / zip 直读用） |
| `pixi run icons` | 重新生成 PWA 图标 |
| `pixi run fmt` | `go fmt ./...` |
| `pixi run uf -- …` | 运行 `uf` CLI（见 [cli.md](cli.md)） |
| `pixi run slices …` | 从真实镜像切出核对用片段（见 [rkfw-rkaf-format.md §9](rkfw-rkaf-format.md#9-用任何真实镜像核对不需要把-8-gib-搬到开发机)） |

`pixi run dev` 起的是零依赖静态服务（`tools/serve.mjs`），只监听 `127.0.0.1`，不发任何网络请求。

## 构建产物与生成物

- `wasm_exec.js` 由 `tools/build-wasm.mjs` 从当前 Go 工具链的 `GOROOT` 拷贝，避免版本错配；
  `web/app/wasm/` 是构建产物，不进版本库。
- `web/app/protocol.js` 与 `web/probe/protocol.js` 由 `cmd/genprotocol` 从 Go 的命令表生成，
  是唯一事实来源；`pixi run build` 会用 `-check` 确认它们没有过期。
- `web/app/icons/*.png` 已提交，由 `pixi run icons` 生成；CI 会校验可重现。
- `web/samples/` 由 `pixi run sample` 生成，是演示与 zip 直读测试用的合成数据，不进版本库。

## 测试

```bash
pixi run test
```

- `internal/rockusb`：CBW 帧、CSW 校验、错误码、CRC 校验值、RKBOOT 解析与各种坏文件、vendor 分包
  （含 4094/4095 边界）、`db` 的成功/四类失败、超时与重试、**stage-2 trace 回放**
  （逐字节比对 5 条 CBW，断言 STALL 只尝试 1 次、失败码 -3、没有额外出站数据）。
- `internal/device`：`ld` 输出格式、DevNo 跨重新枚举保持稳定、`cd`/`sd`、模式门禁、`db` 各失败分支、
  `info` 各段、`db → 重新枚举 → info` 自动刷新、命令表白名单。
- `internal/rkfw`：RKFW/RKAF 布局、CRC/MD5、真实 r132 片段回放、`OpenReaderAt`（浏览器用内存读取器）。
- `internal/imageplan`：镜像自带 loader 的提取（sha256/长度/LDR 日期）、chip 错配拒绝、文件 loader 的
  “非镜像自带”标记、`WriterPlan` 携带 loader 身份、演示镜像可计划。
- `internal/uf`：状态机各阶段、失败/续跑、擦除尾部分支、**打开设备前拒绝错配 loader**、`db` 首块/中途失败
  分类、`BridgeConnector` 走完 maskrom → db → LOADER → 写入 → rd。
- `cmd/uf`：dry-run 文本/JSON、loader 默认来自镜像、`--loader` 覆盖、错配时 `--mock` 拒绝且不写、dry-run
  不构造连接器。
- `web/app/test/`：
  - `bridge.test.mjs`：worker 侧 bridge 契约（JSON 信封、handle 透传、字节规范化、超时、错误传播）。
  - `mock-host.test.mjs`：mock 设备的 CBW/CSW 行为、maskrom STALL、vendor index 校验、演示 loader 的 CRC。
  - `usb-host.test.mjs`：WebUSB host 的接口选择、setup 对象、STALL 传播、未授权句柄的错误提示。
  - `core.test.mjs`：**真实 wasm 核心** + mock 设备的完整 `ld → info 拒绝 → db → ld → info → cd/sd` 会话。
  - `worker.test.mjs`：直接加载生产 `worker.js`，走 mock 模式与 **worker↔主线程 RPC** 两条路径。
  - `preflight.test.mjs`：自检清单与门禁——maskrom 下 db 前只读按钮被禁、loader 错配阻止写入、未覆盖尾部
    必须显式确认、LOADER 态可读可写。
  - `uf.test.mjs`：**真实 wasm 核心**的 `ufPlan` / `ufRun`（mock 模拟器）——镜像自带 loader 身份、计划来自
    parameter、`db → gpt → wl → rd` 全流程与进度事件。

## CI 与部署

仓库原先记录的配置：`ci.yml` 跑 `pixi run build` + `pixi run test`（并校验图标可重现）；
`pages.yml` 构建 `web/` 并用 GitHub Pages 发布（相对路径 + `.nojekyll`，适配 `/<repo>/`）。
当前仓库快照里没有 `.github/workflows/` 目录；若 CI/Pages 需要重新启用，按上述内容补回工作流。

## 相关文档

- 架构与代码地图：[architecture.md](architecture.md)
- 命令行用法：[cli.md](cli.md)
- 已知限制与待验证项：[limitations.md](limitations.md)

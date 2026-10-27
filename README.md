# fydetab-flash-web

浏览器里复刻 Rockchip `upgrade_tool` 的刷机路径：协议与编排用 Go 编译成 WebAssembly，USB 走 WebUSB，完全本地运行。

**当前阶段：M1（Go/WASM 核心 + WebUSB bridge + `ld`/`cd`/`sd` + Download Boot + 只读信息）、
M2（离线 `uf --dry-run` 计划）、M3a（写入层 + `uf` 状态机，mock/回放验证）、M3b（WebUSB 写入绑定 +
真机前自检）、M4（可安装 PWA + 离线 + 官方 `.zip` 直读），以及 M4b（五区块单页：镜像下载 / 校验 /
设备列表 / 刷写 / 日志）。**
M3b 之后浏览器里已经有一条完整的写入路径：从 `update.img` 的 RKFW 头里切出**镜像自带的 loader** 做
Download Boot，再用 `internal/uf` 的同一套状态机执行 `gpt → wl → rd`；写入前必须通过页面的逐项自检。
M4 把这一页变成可以装到桌面、断网也能打开的应用，并且允许直接选择官方 `.zip`（ZIP64 + 流式 deflate，
不落地 3.8 GiB）。M4b 把杂乱的调试面板收成一条普通用户能看懂的主流程：**下载或选镜像 → 校验 → 选设备 →
刷写 → 看日志**，原来的 trace/命令/只读信息进“高级 / 诊断”折叠区。真机端到端由后续 stage 负责。

> 为什么 maskrom 下读不到信息：裸 maskrom 不接受 rockusb 的 bulk CBW，5 条只读命令的 31 字节 CBW 全部被
> STALL（M0b 真机实测，见 `internal/rockusb/testdata/maskrom-cbw-stall.json`）。所以**上电后的第一条命令必须
> 是 `db`**：在它成功之前，页面禁用所有会发 CBW 的按钮——真机上正是这条把上电周期弄脏、让 `db` 停在
> `index(0)` 的。
>
> loader 必须来自**同一份** `update.img`。上游 `download_boot()` 不校验 loader 与芯片是否匹配，错配的
> loader 会被完整发出去、再被 BootROM 在 `index(0)` 拒收（看起来像 USB 故障）。本仓库默认用镜像自带的
> loader，并在打开设备前解析 LDR 头比对 `supportChip`，详见
> [`docs/protocol-notes.md` §11](docs/protocol-notes.md)。

## 页面与架构

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

## 交付内容

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

## 状态与命令

| 状态 | 判定（`bcdUSB bit0`） | 能做 |
|---|---|---|
| **MASKROM** | `0` | `ld`、`desc`、`db`（**第一条命令必须是 `db`**；此前的任何 CBW 都会被 STALL 并弄脏上电周期） |
| **LOADER** | `1` | `ld`、`desc`、`cd`、`sd`、`info`，以及完整的 `uf` 写入 |

| 命令 | 说明 |
|---|---|
| `ld` | 列出已授权设备：`DevNo=1 Vid=0x2207,Pid=0x350b,Mode=Maskrom` |
| `desc` | 打印描述符（无 USB 传输） |
| `cd <n>` | ChangeDevice：按 DevNo 选择当前设备 |
| `sd <n>` | SwitchDevice：重新绑定当前设备（`db` 重新枚举后用） |
| `db` | Download Boot：把 loader 分块写入 maskrom RAM，然后等设备变成 LOADER |
| `info [chip\|flash\|cap\|storage]` | 读只读信息（仅 LOADER；在 MASKROM 下打开设备前就拒绝） |
| `help` / `version` | 帮助 / 核心版本 |

页面上的写入路径（`ufPlan` / `ufRun`）不在 shell 命令表里：它是 `internal/uf` 的状态机。页面的门禁在
`web/app/preflight.js`：设备模式、loader 与镜像 chip 匹配、计划与 parameter 一致且未覆盖尾部已显式选择、
上电后第一条命令是 `db`，以及 M4b 新增的“镜像已校验”。任何一条不满足，写入按钮都会禁用并写明缺什么。

`db` 的失败会区分 **BootROM 拒收 loader 的第 0 个数据块（index 0）**、**第 0 块之后的传输失败**、
**loader 文件无效**、**loader 芯片不匹配**、**设备不在 maskrom**——不会只报一个 `-1`。`index(0)` 是 loader
文件里的第 0 个数据块（不是第 0 条 USB 请求）：数据已经发到设备，是 BootROM 主动拒收。

## 快速开始

```bash
pixi install    # 首次：go + nodejs 全部由 pixi 管理
pixi run dev    # http://localhost:5173/app/  （可安装页面；M0 探针在 /probe/）
pixi run test   # go test ./... + node --test（协议单测、回放、mock、wasm 集成、worker RPC、zip/PWA）
pixi run build  # 生成物检查 + go build/vet + wasm 构建 + 所有 JS 语法检查
pixi run wasm   # 只构建 web/app/wasm/rk3588.wasm 并拷贝匹配的 wasm_exec.js
pixi run gen    # 改了 internal/rockusb 或 internal/device 的命令表后重新生成 protocol.js
pixi run sample # 生成 web/samples/ 下的演示镜像与 zip（离线 / zip 直读用）
pixi run icons  # 重新生成 PWA 图标
```

`pixi run dev` 起的是零依赖静态服务（`tools/serve.mjs`），只监听 `127.0.0.1`，不发任何网络请求。
`wasm_exec.js` 由 `tools/build-wasm.mjs` 从当前 Go 工具链的 `GOROOT` 拷贝，避免版本错配；`web/app/wasm/`
是构建产物，不进版本库。

### 真实设备

1. 平板进 **MASKROM**（按住恢复键上电或按板子说明短接），USB 接入。**这一轮上电后不要先点任何读取按钮。**
2. 打开 `http://localhost:5173/app/`，第 3 块点 **添加设备**，选 `0x2207` 设备；列表会显示
   `Maskrom · DevNo=1`（模式按 `bcdUSB` bit0 判定）。
3. 第 1 块选镜像：官方条目点“下载”（清单里有直链时）或 **选择本地镜像**。页面读 RKFW 头、切出镜像自带的
   loader，显示 sha256、长度、LDR 构建日期与 chip 比对结果。
4. 第 2 块自动校验：结构 + PARM CRC 秒级出结论，整镜像 MD5 + RKAF CRC 按本机实测速度决定是否默认做完整
   校验（页面会显示“预计约 X 秒”；超过 2 分钟就默认快速校验并明说“未做完整校验”）。
5. 第 4 块看前置条件：未覆盖尾部必须在下拉框里显式选择 `none / fill / erase-lba` 并打勾；其余条件和
   “缺什么”都逐条列出。全绿后 **开始刷写** 才可点。
6. 点 **开始刷写**：状态机自己执行 `db → gpt(IDB + 分区表 + parameter 8 份) → wl … → rd`，界面显示阶段、百分比、速度、已用/剩余时间，
   可取消；失败会给下一步（重试 / 重进 maskrom / 换线换口 / 导出日志）。成功后设备复位并重新枚举，第 3 块
   刷新后应显示 `Loader`。
7. 第 5 块随时有日志：分级、带时间戳，可导出文本或 NDJSON。

> loader 由镜像提供，仓库不分发 Rockchip loader blob，也不联网获取。手动指定 loader 的文件只在本机内存里使用。

### 无设备演示

勾选第 3 块的 **演示模式（mock 设备）**：

- 自动载入合成镜像与它的 loader，自动生成计划并跑完整校验（校验路径与真机镜像完全相同）；
- mock 设备一开始是 maskrom；`开始刷写` 会走完整的分包/CRC + `db → gpt → wl → rd` 路径，不访问 USB；
- 高级 / 诊断里的 `info` 只有在 mock 变成 LOADER 之后才可用（和真机一致）。

## 只读守卫与写入边界

- 协议层：`internal/rockusb` 只声明 5 条只读 opcode 作为命令表；`Session.Exec` 对不在表里的 opcode 直接拒绝，
  连 transport 都不碰。`TestNoWritePath` 会在有人把 `WRITE_LBA`(0x15)、`ERASE_NORMAL`(0x06) 等写/擦/复位
  操作码放进只读表时失败。
- 写入层：`WRITE_LBA`/`ERASE_LBA`/`DEVICE_RESET` 单独声明在 `internal/uf`，由 `uf.Run` 的状态机驱动；
  `uf --dry-run` 仍然完全不打开设备（`cmd/uf` 的 `TestDryRunNeverBuildsAConnector` 会在 dry-run 试图构造
  连接器时失败）。
- 命令层：shell 只有 `ld/desc/cd/sd/db/info/help/version`；`TestNoFlashWriteCommands` 钉住这一点。
- 上机前：页面按 `preflight.js` 的规则门禁——maskrom 下、`db` 成功前，所有会发 CBW 的按钮（`info`）都禁用，
  自检四项不通过时“开始写入”禁用。

## 为什么必须用镜像自带的 loader

真机上 `upgrade_tool DB` 两次都停在 `index(0)`。`index(0)` 是 **loader 文件里的第 0 个数据块**，不是第 0 条
USB 请求——数据其实已经发到设备，是 BootROM 拒收。上游 `download_boot()` 直接信 loader 头
（`dev.emDeviceType = pBoot->SupportDevice`），**不校验 loader 与芯片是否匹配**；于是错配的 loader 会被完整
发出去再被拒，看起来像 USB 故障。

实测的两份 loader：

| 来源 | 长度 | sha256 | LDR 构建日期 |
|---|---|---|---|
| 官方 v23 镜像自带 | 422,336 B | `24c1d188…57c47` | 2024-09-06 |
| SDK `rk3588_spl_loader_v1.06.111.bin` | 412,096 B | `4ca46b6f…e523ba` | 2022-06-28 |

之前一直拿 SDK 那份配 v23 镜像，属于**真实错配**。同一个人用官方 `upgrade_tool` + 同一根线刷同一份 v23 镜像
成功，所以硬件与线材可以排除。

因此本仓库：

1. **默认从 RKFW 头的 `loader offset/length` 切出镜像自带的 loader**，页面显示它的 sha256、长度、LDR 构建
   日期；手动覆盖可以，但会被显式标记“非镜像自带”。
2. 在打开设备前解析 LDR 头（`supportChip`、版本、日期）并与镜像 `chip` 比对，不一致就**拒绝发送 `db`**
   （`ERR_LOADER_MISMATCH`，category `loader-chip-mismatch`），并说明原因。
3. 把 `db` 失败分成 `first-block`（index 0 被拒：先查 loader 错配，再查是否干净上电周期，最后才是线材）与
   `mid-transfer`（第 0 块已被接受，之后中断：线材/供电/截断的 loader）。

细节与 LDR 头部字段见 [`docs/protocol-notes.md` §11](docs/protocol-notes.md)。

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

## 已知限制 / 待真机复验

- **真机端到端未复验**：`db → 重新枚举 → ld/info` 与整段 `uf` 写入需要板子从 USB 掉线中恢复（记录在 issue 里），
  本阶段以 mock + 单测 + Node 集成测试覆盖同一代码路径；真机由 stage 6 负责。
- **`db` 失败分类的证据分布**：`index(0)` 被拒有真机日志（`Boot_VendorRequest471 failed,err=-1`）；
  “中途失败”的分类目前只有单测，需要一次真实中途失败来确认。
- **浏览器 UI 未在真实 Chrome 里点过**：构建环境没有 Chrome/Chromium；`worker.js` 本身（含 RPC 与 wasm）
  已用 Node 集成测试跑通，DOM 层（`app.js`）只做了语法检查，`preflight.js` 的规则有单测。需要一次人工冒烟。
- 重新枚举后的授权：按“实例授权失效、需要重新点授权设备”实现（`connect` 事件会自动 `ld`），
  按 VID/PID 策略是否仍生效等真机结论。
- **大镜像读取已实现但未用真机大文件验证**：页面把 `File` 交给 worker，Go 通过 `rkfw.OpenReaderAt` 分块读；
  默认不做全镜像 MD5（页面里 `verify:false`），需要时可打开。
- **IDB 字节模型仍待真机核对**（`rk28-rc4` / `raw-loader`），真机第一次写入时应该先看日志里的
  `gpt` 阶段是否符合预期。

## M0b 真机验收清单

端到端同事在 maskrom 真机上执行：

- [ ] Chrome/Edge 打开 `pixi run dev` 给出的 `http://localhost:5173/`，页面无 JS 报错。
- [ ] 授权 `0x2207` 设备成功，界面显示 VID/PID 与 `mode = MASKROM`。
- [ ] 点“读取设备信息”后，5 条命令的结果都出现在页面上；成功的记录 `dataHex`，失败的记录错误码。
- [ ] 设备描述符里能看到配置 / 接口 / endpoint 的 number、direction、type、packetSize。
- [ ] “导出 JSON”下载的文件包含 `device`、`commands`、`transfers`、`errors` 四段。
- [ ] 通读 `transfers`：除 31 字节 CBW（`bulk-out`、`stage=cbw`）外，没有其他出站数据。
- [ ] 把 JSON 附到 SIL-510，并注明 Chrome 版本、OS、平板型号。

## M2：离线镜像计划（`uf --dry-run`）

M2 增加了一条完全离线的路径：解析 RKFW/RKAF/parameter、校验 MD5 与 CRC，并打印
`uf` 将会执行的阶段与分区写入计划。**它不打开 USB 设备，不写、不擦、不复位。**

```bash
# 计划（会流式校验整个镜像的 MD5 与 RKAF CRC）
pixi run uf --dry-run /path/to/update.img

# 只看布局、跳过全镜像校验；可指定真实 flash 大小
pixi run uf --dry-run --no-verify --flash-sectors 250069680 update.img

# 机器可读
pixi run uf --dry-run --json update.img
```

输出包含：

- `ul → gpt → wl <item> → rd` 阶段、每阶段字节数与累计进度；
- 每个 RKAF item 的源偏移、大小、目标 LBA 与所属分区；
- 按 parameter CMDLINE 推导的 12 个分区 GPT 计划（含只建 GPT、没有 payload 的分区）；
- **未覆盖区间**：分区大于镜像 payload 时显式列出，例如 `STATE` 5 GiB / 1 GiB payload
  会标出剩余 4 GiB 不会被写入，且旧内容是否擦除与镜像无关。

格式细节、CRC 算法、32 位长度回绕与核对结论见
[`docs/rkfw-rkaf-format.md`](docs/rkfw-rkaf-format.md)。

M2 的解析器已经用真实镜像核对过一遍：测试机会上唯一的大镜像是 **openFyde r132**
（sha256 `01fe1c04…42670ecc`），不是官方 v23.0-SP1。它的片段存在
`internal/testimage/testdata/r132/`，`internal/testimage` 会把片段按绝对偏移装进
8,348,478,026 B 的稀疏文件，所以 `go test ./...` 跑的是真实布局而不是小号 fixture。
r132 暴露并已修正的两个坑：RKAF item 的 `+0x68` 是 2 KiB 块计数（不是字节长度），
以及归档后有 `2^32` 字节 padding 导致 32 位长度不能盲猜回绕。

任何一张真实镜像都可以用同样的方式核对，不必把 8 GiB 搬到开发机：

```bash
# 在放镜像的机器上切片段（只读，不碰设备），再把 rk-slices/ 带过来
pixi run slices /path/to/update.img rk-slices
# 回放并打印这份镜像的全部测量值
RKFW_SLICES_DIR=$PWD/rk-slices pixi run test
```

细节见 [`docs/rkfw-rkaf-format.md` §9](docs/rkfw-rkaf-format.md)。

## M3a：写入层与 `uf` 状态机（mock/回放）

M3a 把 M2 的计划变成真实的 rockusb 写命令，但**只在内存设备和录制 trace 上执行**：

```bash
# 用内存 RK3588 设备走完整 uf（含 Download Boot、IDB、逐 item、rd）
pixi run uf --mock update.img

# 注入失败，看中止与续跑提示
pixi run uf --mock --mock-fault failed:uboot update.img

# 超时（默认重试 1 次）
pixi run uf --mock --mock-fault timeout:boot update.img

# 回放一份录制的控制/bulk 序列
pixi run uf --replay trace.json update.img

# 导出 NDJSON 日志（header / 每次 transfer 摘要 / 进度快照 / result）
pixi run uf --mock --log uf.ndjson update.img
```

- 写入命令：`WRITE_LBA(0x15)`、`ERASE_LBA(0x25)`、`DEVICE_RESET(0xFF)`；一次写 64 KiB，
  进度/重试块 1–4 MiB（`--chunk-size`），只有超时会重试。
- 状态机：`ul → td → chip → flash → gpt(IDB + GPT + parameter) → wl <item> → erase → rd`；
  失败时报告 `nextItem` 与 `--resume-from <item>`。
- `--erase-tail=none|fill|erase-lba` 显式回答“镜像没覆盖的分区尾部要不要擦”，
  dry-run 与运行日志都会打印这个选择。
- IDB 字节模型（512 对齐 + 逐扇区 RC4 + LBA 64）**待真机核对**，代码里是可替换的
  `uf.IDBModel`（`--idb-model rk28-rc4|raw-loader`）。
- 真机写入（WebUSB 绑定）是 M3b；本阶段不打开 USB 设备。

协议字段、mock trace 格式、日志 schema 与全部待核对项见
[`docs/uf-write-path.md`](docs/uf-write-path.md)。

## M3b：WebUSB 写入绑定 + 真机前自检

M3b 把 M3a 的状态机接到真实 WebUSB transport，并把上机前的检查做进页面。**它默认不碰真机**——目标是让第一次
真机尝试尽量一次成功；端到端真机验证是 stage 6。

- **默认用镜像自带的 loader**：`ufPlan` 从 RKFW 头的 `loader offset/length` 切出 loader，页面显示它的 sha256、
  长度、LDR 构建日期；与镜像 `chip` 不一致时报警。手动指定 loader 可以，但会显式标记“非镜像自带”。
- **loader 自检（上游没有的）**：`internal/uf` 在**打开设备之前**解析 LDR 头（`supportChip`/版本/日期）并与
  镜像 chip 比对，不一致直接拒绝（`ERR_LOADER_MISMATCH`，category `loader-chip-mismatch`），一个字节都不发。
- **真实 transport 绑定**：`uf.BridgeConnector` 把页面的 `device.Bridge`（WebUSB）接进同一个 `uf.Run`；
  第一次 `Open` 找 maskrom，`db` 之后的下一次 `Open` 找重新枚举出来的 LOADER。mock 与 replay 路径不变。
- **真机前自检清单**（页面上逐项显示、不通过则写入按钮禁用）：
  1. 设备在 maskrom（`2207:350b`，`bcdUSB` bit0=0）；
  2. loader 与镜像 `chip` 匹配（默认镜像自带）；
  3. 写入计划与 `parameter` 一致、未覆盖尾部（例如 STATE 的 4 GiB）已显式选择；
  4. **上电后第一条命令必须是 `db`**——`db` 成功前禁用所有会发 CBW 的按钮（`info` 等），并显示原因。
     规则在 `web/app/preflight.js`，有独立的 node 单测。
- **失败分类**：`db` 首块被拒（`first-block`，index 0）与中途失败（`mid-transfer`）分开报，并给出可操作结论
  （loader 与镜像不匹配 → 不是干净上电周期 → 线材，按这个顺序）。

```bash
# 计划 + 镜像自带 loader 的身份（sha256 / 长度 / LDR 构建日期）
pixi run uf --dry-run --flash-sectors 250069680 update.img

# 显式换一个 loader（报告里标成非镜像自带；错配会在打开设备前被拒）
pixi run uf --dry-run --loader rk3588_spl_loader_v1.06.111.bin update.img

# mock 全流程，默认用镜像自带 loader
pixi run uf --mock update.img
```

## M4：PWA、离线安装与官方 zip 直读

M4 只改页面与工具链，`internal/` 的写入语义与 `web/app/preflight.js` 的门禁条件未动（写入路径正在
另一条线做真机验证）。

- **可安装 PWA**：`web/app/manifest.webmanifest` + `web/app/sw.js`。service worker 在 `install` 时预缓存
  页面、全部 JS 模块、镜像清单、图标与 `wasm/rk3588.wasm`（24 项），策略是 **cache-first**；`activate` 清掉旧版本
  cache，页面发现等待中的 worker 时提示“刷新页面”。装成应用后**断网仍可打开**，写入能力与是否安装无关。
- **官方 `.zip` 直读**：`web/app/zip.js`（ZIP32/ZIP64 中央目录，只读文件尾几十 KiB）+ `web/app/zip-source.js`
  （把首字节为 `RKFW` 的条目包成与 `File` 相同的 `{name,size,read(offset,length)}`，用
  `DecompressionStream('deflate-raw')` 按需解压，保留 8 MiB 回退窗口）。解出来的字节与同一份 `.img`
  完全一致，所以**计划逐字段相同**；`web/app/test/uf-zip.test.mjs` 用真正的 Go/WASM 核心比较两份完整计划。
- **安装引导 / Chromium-only 提示 / 设备重连向导 / 错误恢复 / 日志导出**：重连向导把 `db` 前后的
  “重新枚举 → 授权 → `ld`”写成三步；日志导出为 NDJSON（shell 输出、uf 进度、每次 USB 传输、自检判定、
  zip 统计），与 CLI 的 `uf --log` 同形状。
- **CI 与部署**：`.github/workflows/ci.yml` 跑 `pixi run build` + `pixi run test`（并校验图标可重现）；
  `.github/workflows/pages.yml` 构建 `web/` 并用 GitHub Pages 发布（相对路径 + `.nojekyll`，适配 `/<repo>/`）。

## M4b：五区块单页（镜像 / 校验 / 设备 / 刷写 / 日志）

M4b 只改页面、下载/校验的前端编排与一处**新增的** wasm 导出；`internal/` 未改动，`ufPlan` / `ufRun` /
`ufCancel` 与写入状态机保持原样，`preflight.js` 的门禁条件一条没删、没有放宽（只新增一条“镜像已校验”）。

- **1 镜像**：`web/app/images.json` 硬编码官方清单（版本 / 文件名 / 大小 / sha256 / URL）。下载前先探测
  `Access-Control-Allow-Origin`，不允许就如实退化成“在新标签页打开下载”；允许则 `showSaveFilePicker` +
  `getReader()` 逐块写盘（8 GiB 不进内存），显示进度 / 速度 / 剩余时间。也可以用 `showOpenFilePicker`
  或文件输入框选本地 `.img` / `.zip`。**官方条目的直链目前为空**：仓库不分发厂商固件，也不猜第三方镜像站
  地址，页面会如实显示“清单未提供直链”；自建镜像源时把 `url` 填上即可。`demo` 条目指向 `pixi run sample`
  生成的演示包，用来完整演示下载流程。
- **2 校验**：`cmd/wasm` 新增 `ufVerify`（复用 `rkfw` / `parameter` / `imageplan`，不改它们）：结构解析、
  item 表边界、分区与未覆盖区间、PARM CRC 秒级出结论；RKAF CRC 与整镜像 MD5 分块流式扫描，按阶段上报
  进度。页面先用 64 MiB 样本测速，外推整镜像耗时：≤ 2 分钟默认做完整校验，超过则默认快速校验并在界面与
  日志里写明“未做完整校验”，完整 MD5 变成可勾选项。实测（Ryzen 7 5700X，8.36 GB 样本）原生 Go 764–886
  MiB/s、Go/WASM 约 700 MiB/s，7.79 GiB 口径约 11 s，策略细节见 [`docs/m4b-ui.md`](docs/m4b-ui.md)。
- **3 设备**：`web/app/devices.js` 用 `navigator.usb.getDevices()` + `connect`/`disconnect` 列出所有
  `0x2207` 设备（序列号、模式 = `bcdUSB` bit0、VID:PID、bcdUSB、是否可刷），与 core 的 `ld` 视图按序列号
  合并出 `DevNo`；`db` 后的重新枚举按序列号重新选中，匹配不上就要求重新授权/选择。
- **4 刷写**：一个按钮；前置条件不满足时逐条写明缺什么。进度含阶段、百分比、速度、已用/剩余时间，可取消；
  失败给出下一步（重试 / 重进 maskrom / 换线换口 / 导出日志）。
- **5 日志**：`web/app/logs.js` 的分级日志（时间戳 + 来源 + 级别），可折叠、可导出文本或 NDJSON
  （`application/x-ndjson`，含 header 与丢弃计数）。任何 `catch` 都落日志。
- **高级 / 诊断**：原始传输记录、只读信息、命令 shell、loader 覆盖与 PWA 状态收进折叠区，不占主视图。
  只读探针 `/probe/` 原样保留。
- **驱动文档**：macOS arm64 / Linux / Windows 的 WebUSB 说明见
  [`docs/webusb-drivers.md`](docs/webusb-drivers.md)；Linux 的 udev 规则可直接装
  [`docs/99-rockchip-webusb.rules`](docs/99-rockchip-webusb.rules)。安装、离线验证与 zip 直读细节见
  [`docs/pwa-offline.md`](docs/pwa-offline.md)。

```bash
pixi run sample   # 生成 web/samples/demo-update.img + 包住它的 ZIP64 zip（演示用，不下载厂商 blob）
pixi run icons    # 重新生成 web/app/icons/*.png（已提交；CI 会校验与生成器一致）
```

离线验证（不碰设备）：`pixi run dev` → 打开 `http://localhost:5173/app/` → 等 `#sw-note` 显示“离线缓存已
接管本页” → DevTools 勾 Offline → 刷新，页面与 `core` 徽标仍然正常，演示模式还能生成计划。



## 仍然成立的早期限制

- **ReadFlashInfo 的短读**：rkdeveloptool 对这一条读 512 B 并接受 ≥11 B；本页按 CBW 里声明的 11 B 读取，以保持 CSW 不错位。若某台设备实际发更多字节，会落成 `ERR_CMD_NOTMATCH` 并记录，不会顺手多读——这正好是要在真机上确认的事实。

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
docs/protocol-notes.md      协议笔记（含 §11 “为什么必须用镜像自带的 loader”）
docs/uf-write-path.md       写入命令、状态机、IDB/擦除假设、mock trace 与日志格式
docs/webusb-drivers.md      macOS arm64 / Linux / Windows 的 WebUSB 与驱动说明
docs/99-rockchip-webusb.rules  Linux udev 规则
docs/pwa-offline.md         安装、离线验证、zip 直读与日志导出
docs/m4b-ui.md              五区块页面：下载/校验/设备/刷写/日志、CORS 与 2 分钟规则
docs/quickstart.md          用户上手：安装、进 maskrom、刷写、救砖、日志导出
```

## 协议来源与许可

- 本仓库以 **GPL-2.0** 发布（见 `LICENSE`）。
- 命令码、CBW/CSW 布局、vendor request、RKBOOT 容器与 CRC 均转写自 Rockchip 的 GPL-2.0
  [rkdeveloptool](https://github.com/rockchip-linux/rkdeveloptool)（`RKComm.h` / `RKComm.cpp` / `RKDevice.h` /
  `RKBoot.h` / `RKBoot.cpp` / `crc.cpp` / `boot_merger.h`）；细节与两个上游特殊分支记录在
  `docs/protocol-notes.md`。
- 不分发 `upgrade_tool` 二进制，也不分发 Rockchip loader blob。

## M0 只读探针（`/probe/`）

`web/probe/` 是上一阶段的只读探针：授权、列设备、发 5 条只读命令、导出 JSON trace。它现在仍是抓取真机
trace 的工具（M1 的回归数据就是从它导出的），页面显著标注“只读，不会写入设备”。它的传输、命令和导出格式
见 `web/probe/` 源码与 `docs/protocol-notes.md` 第 1–8 节。

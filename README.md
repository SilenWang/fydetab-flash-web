# rk3588-webflasher

浏览器里复刻 Rockchip `upgrade_tool` 的刷机路径：协议与编排用 Go 编译成 WebAssembly，USB 走 WebUSB，完全本地运行。

**当前阶段：M1（Go/WASM 核心 + WebUSB bridge + `ld`/`cd`/`sd` + Download Boot + 只读信息）。**
M1 能把用户本地选择的 loader 通过 EP0 vendor request 写进 maskrom 的 RAM（Download Boot），让设备重新枚举成
LOADER，然后读取 chip / flash / capability / storage。**仍然没有写 flash、擦除、复位或切换存储的路径。**

> 为什么 maskrom 下读不到信息：裸 maskrom 不接受 rockusb 的 bulk CBW，5 条只读命令的 31 字节 CBW 全部被
> STALL（M0b 真机实测，见 `internal/rockusb/testdata/maskrom-cbw-stall.json`）。必须先 `db` 进 LOADER。

## 页面与架构

```
main thread (app.js)                     worker (worker.js)
├─ WebUSB：requestDevice / open / claim   ├─ Go/WASM 核心（internal/rockusb + internal/device）
├─ controlOut / transferIn / transferOut  ├─ bridge.js：把 Go 的同步调用包成 Promise
├─ UI、命令终端、loader 文件选择           └─ 每次 USB 操作 → postMessage RPC → 主线程
└─ RPC 服务端
```

- **主线程**独占 WebUSB：`requestDevice()` 需要用户手势，且 `navigator.usb` 只在 Window 上存在。
- **worker** 跑 wasm：`db` 要发 ~140 KiB、几十个控制包，放在 worker 里不阻塞 UI。
- Go 侧 `awaitPromise` 把 goroutine 停在 channel 上，Go 的 wasm 调度器把控制权还给事件循环，Promise 落地后再
  恢复——所以协议代码可以写成同步风格（`internal/rockusb` 不知道自己在浏览器里）。
- **演示模式**把 RPC host 换成 worker 内的 mock 设备（`web/app/mock-host.js`），无需硬件即可演示
  `ld → info 被拒绝 → db → 重新枚举 → info`，并且故意复现“maskrom 拒绝 CBW”这个真机结论。

## 交付内容

- `internal/rockusb/`
  - `protocol.go`：命令表、CBW/CSW 编解码、错误码、只读守卫。
  - `crc.go`：CRC-CCITT 与 Rockchip 自己的 CRC-32（多项式 `0x04C10DB7`，见下）。
  - `transport.go`：`Transport` 接口、每传输超时、只对超时重试的策略、`Session.Exec`、传输 trace。
  - `downloadboot.go`：RKBOOT 容器解析与校验、vendor request 分包（含 4094/4095 两个上游特殊分支）、
    `DownloadBoot` 与分类错误。
  - `testdata/`：stage 2 真机 maskrom trace（5 条 CBW 全 STALL）与设备描述符，用作回放回归。
- `internal/device/`：设备枚举/DevNo/模式、`ld`/`desc`/`cd`/`sd`/`db`/`info` 的 CLI 风格 shell。
- `cmd/wasm/`：`syscall/js` 导出（`bindBridge` / `exec` / `inspectLoader` / `close`）与 JS bridge 传输。
- `web/app/`：M1 页面（主线程 WebUSB host、module worker、mock 设备、演示 loader）。
- `web/probe/`：M0 只读探针，保留下来用于抓 trace 与对照，不受 M1 影响。
- `cmd/genprotocol/`：把 Go 的命令表/常量生成到 `web/app/protocol.js` 与 `web/probe/protocol.js`。

## 状态与命令

| 状态 | 判定（`bcdUSB bit0`） | 能做 |
|---|---|---|
| **MASKROM** | `0` | `ld`、`desc`、`db` |
| **LOADER** | `1` | `ld`、`desc`、`cd`、`sd`、`info` |

| 命令 | 说明 |
|---|---|
| `ld` | 列出已授权设备：`DevNo=1 Vid=0x2207,Pid=0x350b,Mode=Maskrom` |
| `desc` | 打印描述符（无 USB 传输） |
| `cd <n>` | ChangeDevice：按 DevNo 选择当前设备 |
| `sd <n>` | SwitchDevice：重新绑定当前设备（`db` 重新枚举后用） |
| `db` | Download Boot：把本地 loader 分块写入 maskrom RAM，然后等设备变成 LOADER |
| `info [chip\|flash\|cap\|storage]` | 读只读信息（仅 LOADER；在 MASKROM 下打开设备前就拒绝） |
| `help` / `version` | 帮助 / 核心版本 |

`db` 的失败会区分 **BootROM 拒绝首包**、**首包之后的传输失败**、**loader 文件无效**、**loader 芯片不匹配**、
**设备不在 maskrom**——不会只报一个 `-1`。

## 快速开始

```bash
pixi install    # 首次：go + nodejs 全部由 pixi 管理
pixi run dev    # http://localhost:5173/app/  （M1 页面；M0 探针在 /probe/）
pixi run test   # go test ./... + node --test（协议单测、回放、mock、wasm 集成、worker RPC）
pixi run build  # 生成物检查 + go build/vet + wasm 构建 + 所有 JS 语法检查
pixi run wasm   # 只构建 web/app/wasm/rk3588.wasm 并拷贝匹配的 wasm_exec.js
pixi run gen    # 改了 internal/rockusb 或 internal/device 的命令表后重新生成 protocol.js
```

`pixi run dev` 起的是零依赖静态服务（`tools/serve.mjs`），只监听 `127.0.0.1`，不发任何网络请求。
`wasm_exec.js` 由 `tools/build-wasm.mjs` 从当前 Go 工具链的 `GOROOT` 拷贝，避免版本错配；`web/app/wasm/`
是构建产物，不进版本库。

### 真实设备

1. 平板进 **MASKROM**（按住恢复键上电或按板子说明短接），USB 接入。
2. 打开 `http://localhost:5173/app/`，点 **授权设备**，选 `0x2207` 设备。
3. 点 **选择 loader**，选你自己机器上的 RK3588 loader（例如 `rk3588_spl_loader_v1.06.111.bin`，约 140 KiB）。
   页面会立刻在本地校验 tag / CRC-32 / 段表 / 芯片，并列出 471/472 各段大小。
4. 点 **Download Boot（db）**。成功后设备重新枚举为 LOADER，浏览器会撤销按实例的授权——再点一次
   **授权设备**，然后 `ld` 应显示 `Mode=Loader`。
5. 点 **读取信息（info）**，或直接在命令框里用 `ld` / `cd` / `sd` / `info`。

> loader 由用户提供，仓库不分发 Rockchip loader blob，也不联网获取。

### 无设备演示

勾选 **演示模式（mock 设备）**：

- mock 设备一开始是 maskrom，`info` 会被拒绝（和真机一致）；
- 自动载入一个**合成的** RKBOOT（段大小与真机 loader 相同，内容是填充字节，见 `web/app/mock-loader.js`）；
- `db` 走完整的分包/CRC 路径 → mock 变 LOADER → `ld` 显示 `Mode=Loader` → `info` 返回 3588 / eMMC 等。

## 只读与写入边界

- 协议层：`internal/rockusb` 只声明 5 条只读 opcode 作为命令表；`Session.Exec` 对不在表里的 opcode 直接拒绝，
  连 transport 都不碰。`TestNoWritePath` 会在有人加入 `WRITE_LBA`(0x15)、`ERASE_NORMAL`(0x06) 等写/擦/复位
  操作码时失败。
- 命令层：shell 只有 `ld/desc/cd/sd/db/info/help/version`；`TestNoFlashWriteCommands` 钉住这一点。
- 写入范围：`db` 只往设备 RAM 写 loader（EP0 vendor request，`0x40/0x0C`，index `0x0471`/`0x0472`），
  不碰 flash、不擦除、不复位、不切存储。

## 测试

```bash
pixi run test
```

- `internal/rockusb`：CBW 帧、CSW 校验、错误码、CRC 校验值、RKBOOT 解析与各种坏文件、vendor 分包
  （含 4094/4095 边界）、`db` 的成功/四类失败、超时与重试、**stage-2 trace 回放**
  （逐字节比对 5 条 CBW，断言 STALL 只尝试 1 次、失败码 -3、没有额外出站数据）。
- `internal/device`：`ld` 输出格式、DevNo 跨重新枚举保持稳定、`cd`/`sd`、模式门禁、`db` 各失败分支、
  `info` 各段、`db → 重新枚举 → info` 自动刷新、命令表白名单。
- `web/app/test/`：
  - `bridge.test.mjs`：worker 侧 bridge 契约（JSON 信封、handle 透传、字节规范化、超时、错误传播）。
  - `mock-host.test.mjs`：mock 设备的 CBW/CSW 行为、maskrom STALL、vendor index 校验、演示 loader 的 CRC。
  - `usb-host.test.mjs`：WebUSB host 的接口选择、setup 对象、STALL 传播、未授权句柄的错误提示。
  - `core.test.mjs`：**真实 wasm 核心** + mock 设备的完整 `ld → info 拒绝 → db → ld → info → cd/sd` 会话。
  - `worker.test.mjs`：直接加载生产 `worker.js`，走 mock 模式与 **worker↔主线程 RPC** 两条路径。

## 已知限制 / 待真机复验

- **真机段落未复验**：`db → 重新枚举 → ld/info` 需要板子从 USB 掉线中恢复（记录在 issue 里），
  M1 交付时以 mock + 单测 + Node 集成测试覆盖同一代码路径。
- **`db` 失败的真实分类**：首包被 BootROM 拒绝有真机日志（`Boot_VendorRequest471 failed,err=-1`），
  “传输中途失败 / loader 不匹配”的分类目前只有单测。
- **浏览器 UI 未在真实 Chrome 里点过**：构建环境没有 Chrome/Chromium；`worker.js` 本身（含 RPC 与 wasm）
  已用 Node 集成测试跑通，DOM 层（`app.js`）只做了语法检查。需要一次人工冒烟。
- 重新枚举后的授权：按“实例授权失效、需要重新点授权设备”实现（`connect` 事件会自动 `ld`），
  按 VID/PID 策略是否仍生效等真机结论。
- 大镜像、`File.slice()` 流式读取属于后续阶段；本阶段一次读入 loader（~140 KiB）。
- 没有 `uf`、没有写 flash 的路径。

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

格式细节、CRC 算法、32 位长度回绕与尚未核对的假设见
[`docs/rkfw-rkaf-format.md`](docs/rkfw-rkaf-format.md)。

## 已知限制（有意留到后续阶段）

- **ReadFlashInfo 的短读**：rkdeveloptool 对这一条读 512 B 并接受 ≥11 B；本页按 CBW 里声明的 11 B 读取，以保持 CSW 不错位。若某台设备实际发更多字节，会落成 `ERR_CMD_NOTMATCH` 并记录，不会顺手多读——这正好是要在 M0b 里确认的事实。
- **没有重连编排**：M1 起做设备选择与重新授权向导。
- **没有 wasm**：M1 才把 Go 逻辑编成 `GOOS=js GOARCH=wasm` 放进 worker；M0 的 Go 包是它的种子。
- **不支持切存储/复位**：`RKU_SwitchStorage`、`RKU_ResetDevice` 属于会改变设备状态的操作，不在只读范围内。

## 目录结构

```
pixi.toml / pixi.lock       依赖与任务（唯一入口）
internal/rockusb/           只读协议子集 + 单测
cmd/genprotocol/            生成 web/probe/protocol.js
web/probe/                  index.html / probe.js / rockusb.js / styles.css
                            protocol.js 为生成物；*.test.mjs 为单测与伪设备冒烟测试
tools/serve.mjs             本地静态服务
docs/protocol-notes.md      协议笔记与待确认项
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

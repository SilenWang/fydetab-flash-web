# rk3588-webflasher

浏览器里复刻 Rockchip `upgrade_tool` 的刷机路径：协议与编排用 Go，USB 走 WebUSB，完全本地运行。

**当前阶段：M0（只读探针）。** 本仓库这一版只做设备枚举与只读信息读取，不写、不擦、不复位设备。
写入能力从 M2 起由 Go/WASM 核心按 dry-run 计划逐步引入，不会在探针里提前出现。

## 这个阶段交付了什么

- `pixi.toml` + `pixi.lock`：`go` 与 `nodejs` 全部由 pixi 管理，任务 `dev` / `build` / `test` / `gen`。
- `internal/rockusb/`：rockusb 协议的只读子集（命令表、CBW 编码、CSW 解析、chip/flash/capability/storage 解码），带单测。
- `cmd/genprotocol/`：把 Go 里的命令表反序列化成 `web/probe/protocol.js`，浏览器不手抄常量。
- `web/probe/`：Chrome/Edge 静态探针页，授权、列设备、读只读信息、导出 JSON。
- `docs/protocol-notes.md`：命令码、报文布局、模式判定与 M1 待确认项的记录。

## 只读保证

三层，任何一层单独生效都足以拦住写入：

1. **协议层**：`internal/rockusb` 只声明 5 条只读命令，`TestNoWritePath` 会在有人加入 `WRITE_LBA`(0x15)、`ERASE_NORMAL`(0x06)、`DEVICE_RESET`(0xFF) 等写/擦/复位操作码时失败。
2. **传输层**：`probe.js` 的 `transferOut` 只接受长度恰为 31 字节的 CBW，且 opcode 必须在白名单内；不存在发送数据 payload 的代码路径。
3. **行为层**：命令失败只记录 rkdeveloptool 的错误码（`ERR_DEVICE_READ_FAILED`、`ERR_CMD_NOTMATCH` …），不重试、不绕过、不切换存储。

页面上有显著标识：“只读，不会写入设备”。

## 环境要求

- [pixi](https://pixi.sh)：唯一依赖入口，不需要系统安装 Go / Node。
- **Chrome 或 Edge 89+**（推荐 120+）。WebUSB 只在 Chromium 系可用，Safari / Firefox / iOS 不支持。
- macOS 12+ 或 Linux（Ubuntu 22.04+）；Windows 需要先确认 WinUSB 驱动，且可能与 Rockchip 官方驱动冲突。
- Linux 需要 udev 规则，否则 `device.open()` 报 `Permission denied`：

  ```bash
  # 参考 rkdeveloptool 的 99-rk-rockusb.rules，最小版本：
  echo 'SUBSYSTEM=="usb", ATTR{idVendor}=="2207", MODE="0666", GROUP="plugdev"' \
    | sudo tee /etc/udev/rules.d/99-rk-rockusb.rules
  sudo udevadm control --reload-rules && sudo udevadm trigger
  ```

## 快速开始

```bash
pixi install     # 首次：创建 go + nodejs 环境
pixi run dev     # http://localhost:5173/ —— WebUSB 需要 localhost 或 https
pixi run test    # go test ./... + node --test（协议单测 + 伪设备页面冒烟测试）
pixi run build   # 校验生成的 JS 是最新的 + go build + go vet + node --check
pixi run gen     # 改了 internal/rockusb 后重新生成 web/probe/protocol.js
```

`pixi run dev` 起的是零依赖静态服务（`tools/serve.mjs`），只监听 `127.0.0.1`，不发任何网络请求，也不上传设备数据。

`pixi run test` 里的页面测试用一套 stub DOM + 伪 MASKROM 设备跑完整的读取路径（CBW → 数据 → CSW → 解码 → 导出），
并断言失败命令只记录错误码、不重试、不额外写；它不能替代真机，但能在改页面时立刻发现回归。

## 授权与读取步骤

1. 平板进入 **MASKROM**（按住恢复键上电，或按板子说明短接），USB 接入电脑。
2. 打开 `http://localhost:5173/`，点 **授权设备**，在弹窗里选择 `0x2207` 开头的设备。
3. 选中设备，点 **读取设备信息**。页面依次发送：

   | 命令 | opcode | 数据阶段 | 作用 |
   |---|---|---|---|
   | `RKU_TestDeviceReady` | 0x00 | 0 B | 设备是否就绪 |
   | `RKU_ReadChipInfo` | 0x1B | 16 B | 芯片型号 ASCII（如 `3588`） |
   | `RKU_ReadFlashInfo` | 0x1A | 11 B | 容量/块/页/ECC/厂商/片选 |
   | `RKU_ReadCapability` | 0xAA | 8 B | 能力位 |
   | `RKU_ReadStorageList` | 0x2B | 4 B | 可用存储位图 + 当前介质 |

4. 点 **导出 JSON** 保存本次描述符 + 全部传输记录。

设备重新枚举（maskrom → loader、复位、拔插）后浏览器会撤销授权，需要再点一次 **授权设备**。

## 导出 JSON 里有什么

```jsonc
{
  "schema": "rk3588-webflasher/probe-trace@1",
  "readOnly": true,
  "probe": { "version": "0.1.0", "protocol": "rockusb/M0-readonly", "userAgent": "...", "commandTable": [...] },
  "device": {
    "vendorId": "0x2207", "productId": "0x110a",
    "bcdUSB": "0x0200", "usbVersion": "2.0.0", "bcdUSBLowBit": 0,
    "mode": "MASKROM",
    "configurations": [ { "interfaces": [ { "alternates": [ { "endpoints": [
      { "endpointNumber": 1, "direction": "in", "type": "bulk", "packetSize": 512, "address": "0x81" }
    ] } ] } ] } ]
  },
  "link": { "interfaceNumber": 0, "bulkIn": 1, "bulkOut": 2 },
  "commands": [
    { "opName": "RKU_ReadChipInfo", "ok": true, "errorCode": 0, "dataHex": "33 35 38 38 00 …", "decoded": { "text": "3588" } }
  ],
  "transfers": [
    { "seq": 1, "kind": "bulk-out", "endpoint": 2, "direction": "out",
      "requestType": null, "request": null, "value": null, "index": null,
      "requestedLength": 31, "length": 31, "dataHex": "55 53 42 43 …",
      "durationMs": 1.23, "opName": "RKU_ReadChipInfo", "stage": "cbw" }
  ],
  "errors": [],
  "counts": { "transfers": 15, "commands": 5, "failedCommands": 0, "errors": 0 }
}
```

`transfers[]` 的字段对 control 与 bulk 统一：`kind` 是 `bulk-in` / `bulk-out` / `control-in` / `control-out`，
`requestType` / `request` / `value` / `index` 在 bulk 传输里为 `null`（rockusb 的只读命令全走 bulk）。
每条记录都带 `requestedLength`、`length`、`dataHex`（前 64 字节）、`durationMs`、`opName`、`stage`（`cbw` / `data` / `csw`）。
失败传输同样记录，并在 `errors[]` 留下错误码与上下文。

这份 trace 就是 M1/M2 的“抓包”：它比 usbmon 更贴近我们要实现的代码路径，因为记录的是本页真实发出的报文。

## M0b 真机验收清单

端到端同事在 maskrom 真机上执行：

- [ ] Chrome/Edge 打开 `pixi run dev` 给出的 `http://localhost:5173/`，页面无 JS 报错。
- [ ] 授权 `0x2207` 设备成功，界面显示 VID/PID 与 `mode = MASKROM`。
- [ ] 点“读取设备信息”后，5 条命令的结果都出现在页面上；成功的记录 `dataHex`，失败的记录错误码。
- [ ] 设备描述符里能看到配置 / 接口 / endpoint 的 number、direction、type、packetSize。
- [ ] “导出 JSON”下载的文件包含 `device`、`commands`、`transfers`、`errors` 四段。
- [ ] 通读 `transfers`：除 31 字节 CBW（`bulk-out`、`stage=cbw`）外，没有其他出站数据。
- [ ] 把 JSON 附到 SIL-510，并注明 Chrome 版本、OS、平板型号。

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
- 命令码、报文布局转写自 Rockchip 的 GPL-2.0 [rkdeveloptool](https://github.com/rockchip-linux/rkdeveloptool)（`RKComm.h`、`RKComm.cpp`、`RKDevice.h`、`RKScan.cpp`）。
- 不分发 `upgrade_tool` 二进制，也不分发 Rockchip loader blob。

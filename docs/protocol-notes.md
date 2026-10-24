# rockusb 协议笔记（M0 只读部分）

本文件记录探针所依赖的报文细节，全部来自 Rockchip 的 GPL-2.0
[rkdeveloptool](https://github.com/rockchip-linux/rkdeveloptool)（master `304f073`）源码，不包含对
`upgrade_tool` 二进制的反编译内容。写在这里是为了让 M1/M2 的实现不必重新翻 C++。

## 1. 传输形态

rockusb **不是** control-transfer 协议。每条命令是一个 UFI 风格的 CBW/CSW 往返：

```
host ──bulk OUT──> 31 B CBW
host <─bulk IN ─── data stage（部分命令没有）
host <─bulk IN ─── 13 B CSW
```

`RKU_DeviceRequest`（`RKComm.cpp:811`）是唯一用 control transfer 的入口，且只在 **MASKROM** 下可用，
用途是给 maskrom loader 传数据（`wIndex` = `0x0471` / `0x0472`），属于后续阶段的事情，M0 不涉及。

## 2. CBW（31 字节，little-endian）

对应 `RKComm.h` 的 `CBW` 结构（`#pragma pack(1)`）：

| 偏移 | 长度 | 字段 | 只读命令的取值 |
|---|---|---|---|
| 0 | 4 | `dwCBWSignature` | `0x43425355`（"USBC"） |
| 4 | 4 | `dwCBWTag` | 随机 u32，CSW 必须回同一个值 |
| 8 | 4 | `dwCBWTransferLength` | 数据阶段期望字节数（见下表） |
| 12 | 1 | `ucCBWFlags` | `0x80`（device-to-host） |
| 13 | 1 | `ucCBWLUN` | 0 |
| 14 | 1 | `ucCBWCBLength` | `0x06`（读信息类命令都是 6 字节 CDB） |
| 15 | 1 | `cbwcb.ucOperCode` | opcode |
| 16 | 1 | `cbwcb.ucReserved` | 0 |
| 17 | 4 | `cbwcb.dwAddress` | 0（只读信息命令不用） |
| 21 | 1 | `cbwcb.ucReserved2` | 0 |
| 22 | 2 | `cbwcb.usLength` | 0 |
| 24 | 7 | `cbwcb.ucReserved3` | 0 |

`InitializeCBW()`（`RKComm.cpp:183`）里 `TEST_UNIT_READY` / `READ_FLASH_INFO` / `READ_CHIP_INFO` /
`READ_CAPABILITY` / `READ_STORAGE` 全部走 `DIRECTION_IN` + `ucCBWCBLength = 0x06` 这一支。

## 3. CSW（13 字节）

| 偏移 | 长度 | 字段 |
|---|---|---|
| 0 | 4 | `dwCSWSignature` = `0x53425355`（"USBS"） |
| 4 | 4 | `dwCSWTag`（必须等于 CBW 的 tag） |
| 8 | 4 | `dwCBWDataResidue`（`TEST_UNIT_READY` 用它回传进度：高 16 位当前、低 16 位总量） |
| 12 | 1 | `ucCSWStatus`：0 = passed，1 = failed |

`UFI_CHECK_SIGN(cbw, csw)` 同时校验 signature 与 tag，任何一项不匹配就是 `ERR_CMD_NOTMATCH (-5)`。

## 4. 只读命令表

| rkdeveloptool / upgrade_tool 名字 | opcode | `dwCBWTransferLength` | 数据阶段 | 备注 |
|---|---|---|---|---|
| `RKU_TestDeviceReady`（`TestUnitReady`） | `0x00` | 0 | 无 | 失败返回 `ERR_DEVICE_UNREADY (-6)` |
| `RKU_ReadChipInfo` | `0x1B` | 16 | 16 B | ASCII 型号串，NUL 补齐 |
| `RKU_ReadFlashInfo` | `0x1A` | 11 | ≥11 B | rkdeveloptool 读 512 B 并接受 11..512 |
| `RKU_ReadCapability` | `0xAA` | 8 | 8 B | 能力位 |
| `RKU_ReadStorageList`（rkdeveloptool 的 `RKU_ReadStorage`） | `0x2B` | 4 | 4 B | 32 位存储位图 |

只读名单里还有 `READ_FLASH_ID`(0x01)、`READ_LBA`(0x14)、`READ_SDRAM`(0x17)、`READ_EFUSE`(0x20)、
`READ_NEW_EFUSE`(0x24)，它们同样是读操作，但 M0 不需要，`internal/rockusb` 只把它们登记为允许的操作码，不提供命令定义。

**写/擦/复位操作码**（本仓库刻意不声明，`TestNoWritePath` 会拦住重新引入）：
`WRITE_SECTOR`(0x05)、`WRITE_LBA`(0x15)、`ERASE_NORMAL`(0x06)、`ERASE_FORCE`(0x0B)、
`ERASE_LBA`(0x25)、`ERASE_SYSTEMDISK`(0x16)、`CHANGE_STORAGE`(0x2A)、`DEVICE_RESET`(0xFF)、
`WRITE_SDRAM`(0x18)、`EXECUTE_SDRAM`(0x19)、`WRITE_EFUSE`(0x1F)、`WRITE_NEW_EFUSE`(0x23)、
`SET_RESET_FLAG`(0x1E)。

## 5. 数据阶段布局

### Chip info（16 B）

纯 ASCII，例如 `33 35 38 38 00 …` → `"3588"`。

### Flash info（11 B，`STRUCT_FLASHINFO_CMD`，`RKDevice.h:34`）

| 偏移 | 类型 | 字段 | 换算 |
|---|---|---|---|
| 0 | u32 | `uiFlashSize` | 单位是 512 B 扇区；rkdeveloptool 打印 `uiFlashSize/2/1024` MB |
| 4 | u16 | `usBlockSize` | 同样以扇区计；打印 `usBlockSize/2` KB |
| 6 | u8 | `bPageSize` | 扇区计；打印 `bPageSize/2` KB |
| 7 | u8 | `bECCBits` | 直接显示 |
| 8 | u8 | `bAccessTime` | 直接显示 |
| 9 | u8 | `bManufCode` | 索引 `szManufName`：SAMSUNG/TOSHIBA/HYNIX/INFINEON/MICRON/RENESAS/ST/INTEL |
| 10 | u8 | `bFlashCS` | 位图，逐位置 1 表示该片选在线 |

### Capability（8 B）

`main.cpp` 里只解码了明确的那几位：

- `capability[0] & 0x01` Direct LBA
- `capability[0] & 0x02` Vendor Storage
- `capability[0] & 0x04` First 4M Access
- `capability[0] & 0x08` Read LBA
- `capability[1] & 0x01` New IDB

上游还用十进制常量测了 `20` / `40` / `80`（即 `0x14` / `0x28` / `0x50`）并打印 "Read Com Log" /
"Read IDB Config" / "Read Secure Mode"。这看起来是 `0x20/0x40/0x80` 的十进制笔误，所以本仓库只输出 `rawHex`
与上述 5 个可信标志，不复制这三位。M0b 拿到真机字节后可以据此判断真实含义。

### Storage list（4 B）

小端 u32 位图，置位的 bit 表示该存储可用；`RKU_ReadStorage` 取**最低置位**作为当前介质
（`RKComm.cpp:632`）。命名沿用 rkdeveloptool `cs` 帮助：`1 = EMMC`、`2 = SD`、`9 = SPINOR`；
本仓库另外把 `3` 记为 `NAND`，其余报 `UNKNOWN(id)`。

## 6. 设备模式判定

`RKScan.cpp:390` 用 VID + `bcdUSB` 的低位区分模式：

- VID != `0x2207` → 不是 rockusb 设备；
- `bcdUSB & 1 == 0` → **MASKROM**；
- `bcdUSB & 1 == 1` → **LOADER**。

WebUSB 只暴露 `usbVersionMajor/Minor/Subminor`，探针把它们拼回 `major<<8 | minor<<4 | subminor`
再套用同一条规则。注意这个 bit0 不是真正的 USB 版本，而是 Rockchip 复用给模式的标记，所以页面会同时显示
原始 `bcdUSB`、合成版本号和 `bit0`。

## 7. M0b 需要在真机上确认的问题

1. RK3588 maskrom 的 `bcdUSB` 实际值（预期 `0x0200`，即 MASKROM）。
2. `RKU_ReadFlashInfo` 的数据阶段到底发多少字节：11、还是 >11（rkdeveloptool 按 512 读）。
   本页按 11 读，若设备发更多会得到 `ERR_CMD_NOTMATCH`，这个结果本身就是答案。
3. `RKU_ReadStorageList` 的位图内容（Fydetab Duo 预期 eMMC + 可能 SD）。
4. 两条 bulk endpoint 的 `packetSize` 与接口 `class/subclass/protocol` 是否为 `0xff/0x06/0x05`。
5. 从 loader 回落到 maskrom 时浏览器授权是否会被撤销（预期会，需要 M1 的重连向导）。

## 8. 未采用的路线（供后续对照）

- `rktool-webusb` 用 Emscripten 编了 rkdeveloptool 的 C++，通过 libusb→WebUSB 适配层复用整条 CLI；
  我们的路线是 Go 重写协议核心、JS 只做传输，因此只借鉴了它的 WebUSB 适配层结论：
  `requestDevice()` 必须在用户手势里、`getDevices()` 拿到的是已授权设备、大镜像要用 `File.slice()` 流式读。
- 原生 `upgrade_tool`（闭源，x86-64）只作为行为基准与抓包对照，不作为实现来源，也不打包分发。

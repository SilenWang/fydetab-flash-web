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

**写/擦/复位操作码**（`internal/rockusb` 刻意不声明，`TestNoWritePath` 会拦住重新引入）：
`WRITE_SECTOR`(0x05)、`WRITE_LBA`(0x15)、`ERASE_NORMAL`(0x06)、`ERASE_FORCE`(0x0B)、
`ERASE_LBA`(0x25)、`ERASE_SYSTEMDISK`(0x16)、`CHANGE_STORAGE`(0x2A)、`DEVICE_RESET`(0xFF)、
`WRITE_SDRAM`(0x18)、`EXECUTE_SDRAM`(0x19)、`WRITE_EFUSE`(0x1F)、`WRITE_NEW_EFUSE`(0x23)、
`SET_RESET_FLAG`(0x1E)。

M3a 起，`WRITE_LBA`/`ERASE_LBA`/`DEVICE_RESET` 的 CBW 布局与重试策略在 `internal/uf` 里实现
（不在只读包里），详见 [`uf-write-path.md`](uf-write-path.md)。

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

## 9. Download Boot（M1 实现，已回源码核对）

真机（SIL-511 / stage 2）结论：**裸 maskrom 不接受 rockusb 的 bulk CBW**。5 条只读命令的 31 字节
CBW 在 bulk OUT 全部被 STALL（`trace.json` 里 5 条 `ERR_DEVICE_WRITE_FAILED`），原版 `upgrade_tool RCI`/`TD`
同样失败（`RKU_Write failed,err=-1`）。只读信息只有先做 Download Boot、设备变成 LOADER 之后才读得到。

### 9.1 传输形态

`RKU_DeviceRequest`（`RKComm.cpp:811-882`）是**唯一**走 control transfer 的入口，且只在 MASKROM 下可用：

- `bmRequestType = 0x40`（vendor，host→device）、`bRequest = 0x0C`、`wValue = 0`；
- `wIndex = 0x0471`（DDR / USB head 段）或 `0x0472`（usbplug 段），其他 index 直接
  `ERR_REQUEST_NOT_SUPPORT`；
- 每包 ≤ **4096 B**，循环把整个 payload 发完；
- payload 末尾追加 **CRC-CCITT** 2 字节：init `0xFFFF`、poly `0x1021`、**高字节在前**；
- 非 maskrom（`m_deviceDesc.emUsbType != RKUSB_MASKROM`）直接 `ERR_DEVICE_NOT_SUPPORT`。

两个上游特殊分支（`dwsize % 4096`）：

| 余数 | 上游行为 | 本仓库 |
|---|---|---|
| `4095` | `dwDataSize++` 补 1 个 0，让 CRC 落进新包 → 包序列 `4096 + 2` | 一致，`PlanVendorRequest` |
| `4094` | CRC 恰好填满 4096，随后再发 **1 字节**控制传输，并要求返回 0 | 一致，`PendingZeroWrite`；返回 1 视为失败（上游 `iRet != 0`） |
| 其他 | 正常切包，最后一包可短 | 一致 |

RK3588 官方 loader 的三段长度（2048 / 51200 / 88064）都对 4096 取余 2048，两条特殊分支在真机上不会触发，但
单测覆盖它们，避免未来段长变化悄悄偏离上游。

### 9.2 RKBOOT 容器（`RKBoot.h` / `boot_merger.h`，packed）

102 字节头部：

| 偏移 | 长度 | 字段 |
|---|---|---|
| 0 | 4 | `uiTag`：`BOOT`(0x544F4F42) 或 `LDR `(0x2052444C) |
| 4 | 2 | `usSize`（头部长度，102） |
| 6 | 4 | `dwVersion` |
| 10 | 4 | `dwMergeVersion` |
| 14 | 7 | `STRUCT_RKTIME`（year u16 + month/day/hour/minute/second） |
| 21 | 4 | `emSupportChip`：RK3588 是 `convertChipType("3588") = 0x33353838` |
| 25 | 1 | 471 段数量 |
| 26 | 4 | 471 段表偏移 |
| 30 | 1 | 471 段表项长度（57） |
| 31 | 1 | 472 段数量 |
| 32 | 4 | 472 段表偏移 |
| 36 | 1 | 472 段表项长度 |
| 37 | 1 | loader 段数量（Download Boot 不用） |
| 38 | 4 | loader 段表偏移 |
| 42 | 1 | loader 段表项长度 |
| 43 | 1 | `signFlag`（`'S'` / `'N'`） |
| 44 | 1 | `rc4Flag` |
| 45 | 57 | reserved |

57 字节段表项：`size`(1) + `type`(4, 1=471 / 2=472) + `name`(20 × UTF-16LE，取低字节) +
`dataOffset`(4) + `dataSize`(4) + `dataDelay`(4，延迟毫秒)。

文件末尾 4 字节是 **CRC-32**，覆盖它之前的所有字节。

### 9.3 一个必须照抄的上游 bug：CRC-32 多项式

`rkdeveloptool` 的 `gTable_Crc32` 注释写着 `crc32 factor 0x04C10DB7` —— 比标准 CRC-32 的 `0x04C11DB7`
少了一个 `1`。`CRC_32` 用这张表、init 0、MSB-first、无最终异或。**用标准 CRC-32 会把每一个真实 loader 都判成
损坏**，所以 `internal/rockusb/crc.go` 原样复现 `0x04C10DB7`，单测同时钉住表的前 5 项并断言它与标准多项式不同。

### 9.4 报文顺序与真机现象

1. 先按文件顺序发所有 471 段，再发所有 472 段（`RKDevice.cpp:510-580`）。
2. 每段结束按 `dataDelay` 休息；全部发完 `sleep(1)` 等设备重新枚举。
3. 首包 = `UsbHead` 2048 B + 2 B CRC = **2050 B**，与 `upgrade_tool` 日志 `Total(2050),Sended(0)` 对得上。
4. 真机上 `upgrade_tool DB` 在 `index(0)` 被 BootROM 拒绝（`Boot_VendorRequest471 failed,err=-1`，4 ms）。
   本仓库据此把 `db` 失败分成三类（`DBError.Kind`）：
   `DB_BOOTROM_REJECTED`（首包 USB 传输错误）/ `DB_TRANSFER_FAILED`（首包之后的失败或短写）/
   `DB_LOADER_INVALID`、`DB_LOADER_CHIP_MISMATCH`（本地文件问题），另加 `DB_NOT_MASKROM`。

## 10. M1 的状态机与命令

| 状态 | 判定 | 可做 |
|---|---|---|
| MASKROM | `bcdUSB bit0 == 0` | `ld`、`desc`、`db` |
| LOADER | `bcdUSB bit0 == 1` | `ld`、`desc`、`cd`/`sd`、`info`（chip/flash/capability/storage） |

`info` 在 MASKROM 下**在打开设备之前**就拒绝（`ERR_DEVICE_NOT_SUPPORT`），因为真机已经证明 5/5 条 CBW 都会被
STALL；拒绝信息直接指向 `db`。`db` 成功后 `awaitingReenumeration = true`，下一条命中设备的命令（或 `ld`）会
重新枚举一次，看到 LOADER 就把这个标记清掉。

### 命令表（`internal/device/shell.go`）

| 命令 | 说明 |
|---|---|
| `ld` | 列出已授权设备，输出 `DevNo=1 Vid=0x2207,Pid=0x350b,Mode=Maskrom` |
| `desc` | 打印描述符（纯本地，无 USB 传输） |
| `cd <n>` | ChangeDevice：按 DevNo 选择设备 |
| `sd <n>` | SwitchDevice：重新绑定当前设备（重新枚举后用） |
| `db` | Download Boot：把本地 loader 写进 maskrom RAM |
| `info [chip\|flash\|cap\|storage]` | 读只读信息（仅 LOADER） |
| `help` / `version` | 帮助与核心版本 |

没有 `uf`、`wl`、`el`、`rd`、`ppt`、`gpt`、`cs`：本阶段不写 flash、不擦除、不复位、不切存储。
`TestNoFlashWriteCommands` 与 wasm 集成测试都盯着这一点。

### 重新枚举后的授权

浏览器授权绑定在 USB device **实例**上：设备重新枚举后旧实例消失，`getDevices()` 不再返回它。M1 的做法是
`connect` 事件触发自动 `ld`；如果列表为空/不含该设备，页面提示再点一次“授权设备”。这段在真机上的复验
（按实例失效、按 VID/PID 策略是否仍有效）**待设备恢复后补做**，代码路径已按“会失效、需要重新授权”实现。

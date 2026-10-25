# rockusb 写入层与 `uf` 状态机（M3a + M3b）

本文件记录 M3a 的实现依据、状态机、mock 与日志格式，M3b 的 WebUSB 绑定与 loader 自检，以及**必须等真机
才能确认**的部分。所有协议字段仍然来自 Rockchip 的 GPL-2.0
[rkdeveloptool](https://github.com/rockchip-linux/rkdeveloptool)（`RKComm.h`、`RKComm.cpp`、
`RKDevice.cpp`、`main.cpp`），IDB 字节模型来自公开重实现
[rk_image_tool](https://github.com/TheGammaSqueeze/rk_image_tool)。本仓库不分发 loader 或
`upgrade_tool` 二进制。

**M3b 把状态机接到了真实 transport，但本阶段的验收不碰真机**：`internal/uf` 只依赖
`rockusb.Transport`，`uf --mock/--replay` 用内存设备与录制 trace 走完整流程，浏览器里则用
`uf.BridgeConnector` 接到 WebUSB。`uf --dry-run` 仍然不打开设备（`TestDryRunNeverBuildsAConnector`）。

## 1. 为什么写操作码不在 `internal/rockusb` 里

`internal/rockusb` 的命令表是 M1 页面的**只读白名单**，`TestNoWritePath` 会拦住重新引入写/擦/复位。
写路径因此整体放在 `internal/uf`：opcode、CBW 编码、重试策略都在那里，读取命令仍复用
`rockusb.Session.Exec`（只读），两个会话共享同一个 `Transport` 与同一个 trace 序号。

## 2. 写命令与 CBW

| 命令 | opcode | 数据阶段 | `dwCBWTransferLength` | `cbwcb` 长度 | 关键字段 |
|---|---:|---|---:|---:|---|
| `WRITE_LBA` | `0x15` | host→device | `sectors * 512` | `0x0a` | `dwAddress = 起始 LBA`、`usLength = 扇区数`、`ucReserved = 0`（`RWMETHOD_IMAGE`） |
| `ERASE_LBA` | `0x25` | 无 | `0` | `0x0a` | `dwAddress`、`usLength` |
| `DEVICE_RESET` | `0xFF` | 无 | `0` | `0x06` | `ucReserved = 0`（`RST_NONE_SUBCODE`） |
| `TEST_UNIT_READY` | `0x00` | 无 | `0` | `0x06` | 预检 “Test Device” |

31 字节 CBW 的偏移与 `docs/protocol-notes.md` 第 2 节一致，写路径额外使用：

- `[12] ucCBWFlags = 0x00`（host→device）；
- `[14] ucCBWCBLength = 0x0a`（写/擦是 10 字节 CDB，复位/测试是 `0x06`）；
- `[15] ucOperCode`；
- `[17:21] dwAddress`（LBA，最多 32 位）；
- `[22:24] usLength`（扇区数，16 位）。

一次 `WRITE_LBA` 的数据阶段最多 **128 扇区（64 KiB）**，这是上游 `DEFAULT_RW_LBA`；一次
`ERASE_LBA` 最多 **32768 扇区（16 MiB）**，对应 `erase_partition`。`uf` 的“分块 1–4 MiB”是
**进度/重试粒度**（`--chunk-size`，默认 4 MiB）：一个 chunk 会被 `WriteLBAs` 再拆成多个 64 KiB 传输。

## 3. 超时与重试

- 每次 USB 传输有独立 deadline（`--timeout`，默认 5 s）。超时后调用 `Transport.Reset()`
  清掉半途的管道状态。
- **只有超时会重试**（`--retries`，默认 1 次额外尝试）。重试重发整个 CBW→data→CSW 序列；
  `WRITE_LBA` 对同一 LBA 同一数据是幂等的，所以安全。
- CSW failed、tag 不匹配、bulk STALL、断线都是设备的确定性回答，不重试，直接中止。
- 取消（浏览器里的按钮 / CLI 的 Ctrl-C）通过 `context` 在传输与 chunk 边界生效；取消后**不再发
  `rd`**，设备留在 LOADER。

## 4. 状态机

```
loader 自检（不碰设备）── 解析 LDR 头，supportChip 与镜像 chip 比对，不一致直接拒绝
                                              ▼
connect ─┬─ MASKROM ──> Download Boot(ul) ──> 等待重新枚举为 LOADER
         └─ LOADER ───────────────────────────┐
                                              ▼
                         Test Device (td) → Check Chip → Get FlashInfo
                                              ▼
                  gpt：IDB（必要时）→ 主/备 GPT → parameter 副本 → 全部读回校验
                                              ▼
                         wl <item> × N（按目标 LBA 顺序，1–4 MiB 分块）
                                              ▼
                         erase uncovered tails（显式策略，见第 6 节）
                                              ▼
                         rd（`--noreset` 时跳过）
```

每条命令都会写进 trace（`rockusb.Transfer`），`Log` 再补一条可读摘要。预检失败（loader 与镜像 chip
不匹配、设备芯片不匹配、目标 LBA 超出 flash）**在第一次写之前**中止。

### 失败、重新枚举与续跑

- Download Boot 完成后设备会重新枚举。runner 轮询 `Connector.Open` 直到看到 LOADER
  （`--` 默认 20 s）；浏览器授权绑定 USB 实例，通常需要用户再点一次“授权设备”，所以超时提示
  直接写明这一点。
- device 掉线 / CSW failed / 超时耗尽时，`RunError` 带：
  - `phase` / `item` / `lba` / `sectors` / `attempt`；
  - `completedItems`（已经完整写完的 item）；
  - `nextItem`（第一个未完成或失败的 item）；
  - `hint`，包含 `--resume-from <item>`。
- `--resume-from <item>` 跳过它之前的 item；**该 item 本身从头重写**（同数据同 LBA，幂等）。
  这是有意的：中途失败的 item 在 flash 上可能是半截内容，按 item 粒度重写比尝试续到扇区更安全。
- `rd` 之后设备会重新枚举，浏览器授权同样可能失效；这是成功路径的已知行为，不是失败。

## 5. gpt 阶段：IDB、分区表与 parameter

`gpt` 阶段写三样东西，顺序固定：**IDB（必要时）→ 主/备 GPT → parameter 副本**，全部在第一条
payload 之前完成。设备分区表不可信时，只写其中一部分没有意义。

### 5.1 IDB 字节模型（先读后判，默认不覆盖）

Fydetab Duo 的 LBA 64 是 `RKNS` 头（New IDB）；旧工具用未验证的 `rk28-rc4` 覆盖它，BootROM
因此读不出 uboot。现在的顺序是：

1. `READ_LBA` 读 LBA 64 的前 8 个扇区，识别 `RKNS` / `RK28` / 空 / 未知（`uf.IdentifyIDB`）；
2. **设备已有有效 IDB（RKNS 或 RK28）→ 默认不写**，只改 GPT 与数据分区；
3. 只有 `--rewrite-idb` 才重建，且按**识别出的类型**选模型（`uf.ModelForType`）：
   `RK28` 用 `rk28-rc4`，`RKNS` 用 `rkboot-new-idb`；
4. 覆盖前把原 IDB 字节写进日志（前 32 字节 hex，`--idb-backup <file>` 可落盘整段）；
5. 空块按所选模型写入；未知字节且未显式 `--rewrite-idb` 时报错停止，不静默破坏。

`rkboot-new-idb`（New IDB）由 RKBOOT loader 的三段组成：

1. 从 RKFW loader（archive 里的 `bootloader`/`MiniLoaderAll.bin`）的
   **ENTRYLOADER** 表（type 4）取 `FlashHead`、`FlashData`、`FlashBoot`；
2. 每段按 2048 字节对齐后依次拼接（FlashHead 在 0，FlashData 在
   `ceil(head/2048)*2048`，FlashBoot 紧随其后）；
3. loader 头要求加密时，对拼接结果每 512 字节做一次相同的固定 key RC4；
4. 写到 LBA 64。

v23 loader 的三段是 2048 + 73728 + 178176 = **253,952 B**，生成的 IDB 以 `RKNS`
开头；旧模型 `rk28-rc4`（整份 loader 按 512 字节 RC4，422,400 B）是错误布局，只保留
用于复现旧日志。`raw-loader` 仍可用于 A/B。

`Verified()` 仍为 false：New IDB 的字节布局已在真实镜像和设备现有 `RKNS` 头上核对过，
但还没有一次“用新模型刷完并启动”的真机确认。日志固定记录 `idb.existing`
（`RKNS`/`RK28`/`empty`/`unknown`）、`idb.overwritten`、`idb.existingHeadHex`、
`idb.backupPath` 与 `idb.model`/`idb.verified`。

### 5.2 分区表（GUID Partition Table）

镜像本身不带分区表字节：分区表是主机按 parameter 生成、写进设备的。几何是 UEFI 的标准布局：

| 位置 | 内容 |
|---|---|
| LBA 0 | 保护性 MBR：单个 `0xEE` 分区覆盖整盘 + `0x55AA` |
| LBA 1 | 主 GPT 头（`EFI PART`，92 字节头，头 CRC32） |
| LBA 2..33 | 主条目数组，128 × 128 B，条目数组 CRC32 |
| LBA N-33..N-2 | 备份条目数组（与主表逐字节相同） |
| LBA N-1 | 备份头（`my_lba=N-1`、`alternate_lba=1`、条目数组在 N-33） |

- 一次写主表 **34 个扇区**、一次写备份表 **33 个扇区**。
- `N` 取设备 `Get FlashInfo` 报告的扇区数，**不用镜像大小推**；设备没报容量时直接中止，因为
  备份表的位置就取决于它。计划里的容量（`--flash-sectors` 或派生值）与设备不一致时按设备重建，
  并在日志里 warn。
- 每个分区项取 parameter 的 `offset`/`size`：`starting_lba`/`ending_lba` 就是它，`bootable`
  标志映射到条目属性的 legacy-BIOS-bootable 位；分区名按 UTF-16LE 写入。GUID 由分区表内容
  确定性派生，所以同一张表重写得到逐字节相同的结果（读回校验与续跑都依赖这一点）。
- 参考设备的 12 行（起始 LBA + 大小）是验收对照；`internal/gpt/encode_test.go` 用真实
  parameter 片段逐项断言。

### 5.3 parameter 副本

parameter 块不是 mtdparts 里的分区，而是主机写进磁盘开头的保留区，设备用它建立分区表：

- 内容是镜像里 `parameter` 项的**原样字节**（PARM 头 + 长度 + 文本 + PARM CRC）；
- 写 **8 份**：起始 LBA = 该项的 flash offset + `0x2000`（设备具备 first-4-access 能力时；
  否则用 flash offset 本身），间隔 = 该项保留区扇区数 `>> 3`，每份长度 = 该块的槽位大小
  （扇区对齐）；
- 参考设备的值：起始 LBA `0x2000`、间隔 `0x800`、8 份 × 4 扇区。后几份会落在同一次运行
  还要重写的 uboot 分区里，这是无害的——parameter 在 payload 之前写。

写前检查与 mock/replay 的“跳过签名校验”开关无关（那个开关只针对设备侧的 payload 门禁）：PARM 包装
存在、CRC 通过、块里的 mtdparts 与即将写入的 GPT 逐项一致。写后逐份读回比对。

某份副本如果落在本次**不会重写**的数据上（`--resume-from` 跳过的 item、备份表、IDB），
该副本跳过并记 warning；一份都写不了则中止。这条规则保证“为了写分区表而破坏用户数据”
不会发生。

## 6. 写入前的擦除语义（未确认，因此显式）

STATE 的 64 位 `size` 高字为 1，镜像带的是**完整的 5 GiB** payload（与 parameter 的
`0xa0006d` 扇区逐字节相等），不是“1 GiB + 4 GiB 未覆盖”。真正未覆盖的只有 `resource`
的 3.64 MiB 尾部。`uf` 仍然不静默决定：

- `--erase-tail=none`（默认）：只写 payload，尾部保留原内容；
- `--erase-tail=fill`：用 `WRITE_LBA` 把尾部写 0xFF（对应 `EraseEmmcByWriteLBA`）；
- `--erase-tail=erase-lba`：对尾部发 `ERASE_LBA`（对应 `erase_partition`）。

`--dry-run` 会打印 `erase semantics (--erase-tail=...)`，运行日志在第一次写之前也会记录同样的
句子和每个尾部区间。三种模式的文案都带“待真机核对”，避免把未知当成已知。

## 7. mock 与回放

### 7.1 内存设备（`uf --mock`）

`uf.Simulator` 实现 `rockusb.Transport`：

- LOADER 态：回答 Test Device / Chip / FlashInfo / Capability / Storage，执行 `WRITE_LBA` /
  `ERASE_LBA` 到内存 flash，`DEVICE_RESET` 后拒绝后续传输；
- MASKROM 态：像真机一样 STALL 所有 CBW，只接受 Download Boot 的 vendor control 传输，收到后
  标记 `booted`，`Connector.Open` 随后返回 LOADER 实例（共享同一块 flash）；
- 故障注入（`--mock-fault` / `uf.SimFault`）：`timeout`（触发重试）、`failed`（CSW 失败）、
  `stall`、`disconnect`，可按 LBA 定位到某个 item。

```bash
pixi run uf --mock update.img
pixi run uf --mock --mock-fault failed:uboot update.img   # 看失败中止与续跑提示
pixi run uf --mock --log uf.ndjson update.img             # 导出 JSON 日志
```

### 7.2 trace 回放（`uf --replay`）

`uf.ReplayTransport` 按顺序消费录制帧。帧格式（`uf-trace/1`）：

```json
{
  "schema": "uf-trace/1",
  "device": {"mode": "LOADER"},
  "frames": [
    {"kind": "cbw", "op": 0, "length": 0},
    {"kind": "csw"},
    {"kind": "cbw", "op": 27},
    {"kind": "read-data", "length": 16, "respondHex": "333538380000..."},
    {"kind": "csw"},
    {"kind": "cbw", "op": 21, "lba": 64, "sectors": 7},
    {"kind": "write-data", "length": 3584},
    {"kind": "csw", "status": "csw-failed"},
    {"kind": "control-out", "request": 1171, "length": 2050}
  ]
}
```

- `kind`：`cbw` / `write-data` / `read-data` / `csw` / `control-out`；
- 可选字段 `op`、`lba`、`sectors`、`length`、`request` 会被断言，不匹配即失败；
- CBW tag 不记录：CSW 用“回答它的 CBW 的 tag”合成；
- `status`：`csw-failed`（CSW status=1）、`stall`、`timeout`、`disconnect`；
- 错帧是**粘滞**的：一旦不匹配，后续所有调用都失败，避免测试在漂移的字节序列上“通过”。

`uf --replay` 用一个 session（trace 的 `device.mode`）。要回放“maskrom → Download Boot →
重新枚举”的全过程，用 Go 的 `uf.NewReplayConnector(ReplaySession{...}, ReplaySession{...})`
（见 `internal/uf/trace_test.go`）。

## 8. JSON 日志（`uf-log/1`）

`--log <path>`（`-` 表示 stdout）输出 **NDJSON**：一行一个记录，便于 3.79 GiB 镜像产生的
十万级传输边跑边写，不必先攒在内存里。

| `kind` | 内容 |
|---|---|
| `header` | schema、开始时间、完整 plan（items/LBA/IDB/tails）与生效的 options（timeout、retries、chunk、eraseTail、idbModel/idbVerified） |
| `transfer` | 每条 USB 传输：`kind`/`endpoint`/`stage`/`attempt`/长度/耗时/状态 + 一行 `summary` |
| `progress` | 进度快照：phase、item、整体 done/total/percent、item done/total |
| `event` | 人可读的 info/warn/error 行（与 CLI stderr 相同） |
| `result` | 最终 `uf.Report`（阶段、item、IDB、擦除、写入字节数、`loader` 身份与 chip 比对、`dbFailure` 分类、`nextItem`/`hint`、错误码） |

日志写失败不会把成功的刷写变成失败：`Recorder` 记住第一个错误并丢弃后续记录，
`Recorder.Err()` 报告它。

## 9. WebUSB 绑定与 loader 自检（M3b）

### 9.1 loader 从哪来

默认是**镜像自带的 loader**：`internal/imageplan.FromImage()` 按 RKFW 头的 `loader_offset/length` 切出
blob，算 sha256，解析 LDR/RKBOOT 头（`supportChip`、版本、构建日期），并与镜像头的 `chip` 比对。
`uf --loader <file>` 可以覆盖，但 `LoaderPlan.Source` 会变成 `file`、`Report.Loader.NonImage = true`，
页面也会标成“非镜像自带”。

`Report.Loader` 记录：

```json
{"source":"image","name":"update.img @0x66","offset":102,"length":422336,
 "sha256":"24c1d188…","version":"1.6.6","buildTime":"2024-09-06 12:34:56 UTC",
 "chip":"RK3588","chipExpected":"RK3588","chipCheck":"ok","nonImage":false}
```

### 9.2 打开设备前的自检

`runner.checkLoaderPlan()` 在 `connect` 之前执行（`internal/uf/runner.go`）：解析 loader → 比对
loader 的 `supportChip` 与 `Plan.Chip` → 不一致返回 `ERR_LOADER_MISMATCH`（本地错误码 `-15`，
category `loader-chip-mismatch`），**一次 `Open` 都不会发生**。这是上游 `download_boot()` 缺的那一步：
上游直接把 `loader->SupportDevice` 赋给 `emDeviceType` 就发。原因与实测数据见
[`protocol-notes.md` §11](protocol-notes.md)。

### 9.3 `db` 失败的两种分类

| category | 判据 | 结论 |
|---|---|---|
| `first-block` | 第 0 个数据块（index 0）的首包 USB 传输失败 | 先查 loader 是否镜像自带，再查是否干净上电周期，最后线材 |
| `mid-transfer` | 首块已接受，之后（更后面的块或同块后续包）失败 | 线材/供电、loader 截断；不能从中间续传，整份重发 |

`Report.DB` 带 `category`/`index`/`blockName`/`detail`；`RunError.Category` 同步给出，页面据此换个说法。

### 9.4 `device.Bridge` → `uf.Connector`

`uf.BridgeConnector`（`internal/uf/bridge.go`）是 M3b 的绑定层：

- `Open` 先 `Bridge.List()`，按 `bcdUSB bit0` 选设备：第一次要 MASKROM，`db` 之后要 LOADER；
- 优先按页面给的 opaque `Handle` 打开（设备列表重排也不会开错板子），失败再退回到“期望模式的第一台”；
- `Link` 从 `device.Info` 派生，页面的接口/端点扫描仍然是唯一知道 descriptor 的地方；
- `LastSeen()` 留给页面解释“为什么没找到设备”（最常见的是重新枚举导致按实例授权失效）。

浏览器侧由 `cmd/wasm` 的 `ufPlan` / `ufRun` / `ufCancel` 驱动：镜像以
`{name, size, read(offset,length)}` 的形式交给 Go，`rkfw.OpenReaderAt` 分块读取，所以多 GiB 的镜像
不会被整体搬进 wasm 内存；`ufRun` 的 `mock:true` 走 `uf.SimConnector`（同一状态机，不碰 USB）。

## 10. 仍需真机确认的清单

1. `WRITE_LBA` 的单次 128 扇区上限、CSW 时序与 RK3588 loader 的实际行为；
2. IDB 字节模型（第 5 节全部三项）；
3. 擦除尾部是否必要/如何生效（第 6 节）；
4. `DEVICE_RESET` 后设备重新枚举的时序与浏览器授权失效方式；
5. `ERASE_LBA` 在 eMMC 上的耗时与返回值；
6. flash 容量与 plan 的偏差（`--flash-sectors` 是否需要用真机值）；
7. **镜像自带 loader 的一次通过率**：M3b 的自检与“第一条命令必须是 db”就是为了这一次；
   真机第一次失败时，先看 `Report.DB.category` 是 `first-block` 还是 `mid-transfer`。

真机结论在 SIL-511/SIL-515 恢复后回填，并据此调整 `internal/uf` 的模型与测试期望——不为了
让测试通过而改期望值。

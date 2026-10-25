# rockusb 写入层与 `uf` 状态机（M3a，mock/回放）

本文件记录 M3a 的实现依据、状态机、mock 与日志格式，以及**必须等真机才能确认**的部分。
所有协议字段仍然来自 Rockchip 的 GPL-2.0
[rkdeveloptool](https://github.com/rockchip-linux/rkdeveloptool)（`RKComm.h`、`RKComm.cpp`、
`RKDevice.cpp`、`main.cpp`），IDB 字节模型来自公开重实现
[rk_image_tool](https://github.com/TheGammaSqueeze/rk_image_tool)。本仓库不分发 loader 或
`upgrade_tool` 二进制。

**本阶段不碰真机**：`internal/uf` 只依赖 `rockusb.Transport`，`uf --mock/--replay` 用内存设备与
录制 trace 走完整流程；真正的 WebUSB 绑定在 M3b。`uf --dry-run` 仍然不打开设备。

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
connect ─┬─ MASKROM ──> Download Boot(ul) ──> 等待重新枚举为 LOADER
         └─ LOADER ───────────────────────────┐
                                              ▼
                         Test Device (td) → Check Chip → Get FlashInfo
                                              ▼
                         Prepare IDB → Download IDB (gpt, LBA 64)
                                              ▼
                         wl <item> × N（按目标 LBA 顺序，1–4 MiB 分块）
                                              ▼
                         erase uncovered tails（显式策略，见第 6 节）
                                              ▼
                         rd（`--noreset` 时跳过）
```

每条命令都会写进 trace（`rockusb.Transfer`），`Log` 再补一条可读摘要。预检失败（芯片不匹配、
目标 LBA 超出 flash）**在第一次写之前**中止。

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

## 5. IDB 字节模型（**待真机核对**）

`gpt` 阶段 = Prepare IDB + Download IDB。字节模型抽成 `uf.IDBModel`，默认实现
`rk28-rc4`（来自 `rk_image_tool/src/common/idblock.c`、`rc4.c`）：

1. loader 补齐到 512 字节的整数倍（零填充）；
2. **每个 512 字节扇区独立**用固定 16 字节 key 做 RC4（每个扇区重新初始化 S 盒）；
3. 结果写到 **LBA 64**（`RK_IDB_SECTOR`）。

因此 IDB 字节数 = `align512(RKFW loader_length)`，与 M2 dry-run 的 `gpt` 阶段字节数一致。
另一个可选模型 `raw-loader`（只补齐、不加密）用于真机 A/B：如果设备能用明文字节启动，说明
RC4 这一层是错的。

**尚未确认**（必须用真机或官方 `upgrade_tool uf` 日志核对，SIL-511/SIL-515 恢复后回填）：

- `uf` 写的是整个 RKFW loader blob，还是 RKBOOT 里 `FlashData`/`FlashBoot` 条目按 2048 字节
  对齐拼接（rkdeveloptool 的 `ul` 对新 IDB 走后者）？当前实现是前者，`LoaderInput.Parsed`
  已经保留了解析后的 RKBOOT，替换模型不需要改状态机。
- 是否所有 RK3588 设备都使用 RC4 路径（`Rc4DisableFlag` 的真实语义）。
- LBA 64 是否对 RK3588 + eMMC 成立。

日志里固定记录 `idb.model` 与 `idb.verified=false`（`--idb-model` 可切换），这样一份真机日志
能直接对应到产生字节的模型。

## 6. 写入前的擦除语义（未确认，因此显式）

`STATE` 分区 5 GiB、镜像只带约 1 GiB payload，剩余 4 GiB 不在镜像描述范围内。`uf` 不静默决定：

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
| `result` | 最终 `uf.Report`（阶段、item、IDB、擦除、写入字节数、`nextItem`/`hint`、错误码） |

日志写失败不会把成功的刷写变成失败：`Recorder` 记住第一个错误并丢弃后续记录，
`Recorder.Err()` 报告它。

## 9. 仍需真机确认的清单

1. `WRITE_LBA` 的单次 128 扇区上限、CSW 时序与 RK3588 loader 的实际行为；
2. IDB 字节模型（第 5 节全部三项）；
3. 擦除尾部是否必要/如何生效（第 6 节）；
4. `DEVICE_RESET` 后设备重新枚举的时序与浏览器授权失效方式；
5. `ERASE_LBA` 在 eMMC 上的耗时与返回值；
6. flash 容量与 plan 的偏差（`--flash-sectors` 是否需要用真机值）。

真机结论在 SIL-511/SIL-515 恢复后回填，并据此调整 `internal/uf` 的模型与测试期望——不为了
让测试通过而改期望值。

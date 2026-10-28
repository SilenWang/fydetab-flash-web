# `uf` 命令行：离线计划、mock 与回放

`uf` 是浏览器写入路径的 CLI 对照实现：同一份 planner、同一套状态机，可完全离线运行。所有命令通过 pixi
执行，不需要单独安装 Go：

```bash
pixi run uf --help
```

## 离线计划（M2）

解析 RKFW/RKAF/parameter、校验 MD5 与 CRC，并打印 `uf` 将会执行的阶段与分区写入计划。
**它不打开 USB 设备，不写、不擦、不复位。**

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

格式细节、CRC 算法、32 位长度回绕与核对结论见 [rkfw-rkaf-format.md](rkfw-rkaf-format.md)。

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

细节见 [rkfw-rkaf-format.md §9](rkfw-rkaf-format.md#9-用任何真实镜像核对不需要把-8-gib-搬到开发机)。

## 写入状态机：mock 与回放（M3a）

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

协议字段、mock trace 格式、日志 schema 与全部待核对项见 [uf-write-path.md](uf-write-path.md)。

## loader 自检（M3b）

```bash
# 计划 + 镜像自带 loader 的身份（sha256 / 长度 / LDR 构建日期）
pixi run uf --dry-run --flash-sectors 250069680 update.img

# 显式换一个 loader（报告里标成非镜像自带；错配会在打开设备前被拒）
pixi run uf --dry-run --loader rk3588_spl_loader_v1.06.111.bin update.img

# mock 全流程，默认用镜像自带 loader
pixi run uf --mock update.img
```

loader 的来源、LDR 头字段与拒绝规则见 [device-commands.md](device-commands.md#为什么必须用镜像自带的-loader)
与 [protocol-notes.md §11](protocol-notes.md#11-为什么必须用镜像自带的-loaderm3b-实测结论)。

## 日志

`--log <path>`（`-` 表示 stdout）输出 NDJSON，schema 见 [uf-write-path.md §8](uf-write-path.md#8-json-日志uf-log1)；
页面第 5 块导出的 NDJSON 与它同形状。日志写失败不会把成功的刷写变成失败。

## 相关文档

- 页面上的同一套流程：[usage-web.md](usage-web.md)
- 写入命令与状态机细节：[uf-write-path.md](uf-write-path.md)
- 镜像格式与核对方法：[rkfw-rkaf-format.md](rkfw-rkaf-format.md)

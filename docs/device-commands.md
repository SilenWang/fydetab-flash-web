# 设备状态、命令与写入边界

页面与 shell 共用同一套设备模型：模式由 USB 描述符判定，命令表只含只读操作，写入由 `internal/uf` 的
状态机单独驱动。

## 设备模式

| 状态 | 判定（`bcdUSB bit0`） | 能做 |
|---|---|---|
| **MASKROM** | `0` | `ld`、`desc`、`db`（**第一条命令必须是 `db`**；此前的任何 CBW 都会被 STALL 并弄脏上电周期） |
| **LOADER** | `1` | `ld`、`desc`、`cd`、`sd`、`info`，以及完整的 `uf` 写入 |

> 为什么 maskrom 下读不到信息：裸 maskrom 不接受 rockusb 的 bulk CBW，5 条只读命令的 31 字节 CBW 全部被
> STALL（M0b 真机实测，见 `internal/rockusb/testdata/maskrom-cbw-stall.json`）。所以**上电后的第一条命令
> 必须是 `db`**：在它成功之前，页面禁用所有会发 CBW 的按钮——真机上正是这条把上电周期弄脏、让 `db` 停在
> `index(0)` 的。

## shell 命令

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
文件里的第 0 个数据块（不是第 0 条 USB 请求）：数据已经发到设备，是 BootROM 主动拒收。更细的分类判据见
[uf-write-path.md §9.3](uf-write-path.md#93-db-失败的两种分类)。

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

细节与 LDR 头部字段见 [protocol-notes.md §11](protocol-notes.md#11-为什么必须用镜像自带的-loaderm3b-实测结论)。

## 相关文档

- 协议与命令码：[protocol-notes.md](protocol-notes.md)
- 写入状态机与失败分类：[uf-write-path.md](uf-write-path.md)
- 页面上的门禁与操作流程：[usage-web.md](usage-web.md)

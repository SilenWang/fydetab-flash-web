# RKFW / RKAF / parameter 格式（M2 解析依据）

本文件记录 `internal/rkfw`、`internal/parameter`、`internal/gpt` 与 `uf --dry-run`
所依赖的磁盘结构、校验算法和待真机确认的假设。**所有数值都来自公开源码，不是从
Fydetab Duo 镜像里猜出来的。** 真机/官方镜像的核对项列在第 8 节。

## 1. 数据来源

| 用途 | 来源 | 许可 |
|---|---|---|
| RKFW 头、RKAF 结构、CRC-32 表、GPT 常量 | [rockchip-linux/rkdeveloptool](https://github.com/rockchip-linux/rkdeveloptool) 的 `RKImage.h`、`RKImage.cpp`、`crc.cpp`、`gpt.h` | GPL-2.0 |
| RKAF 尾 CRC、32 位长度回绕、PARM 包装 | [suyulin/afptool-rs](https://github.com/suyulin/afptool-rs)（`src/lib.rs`、`src/unpack.rs`） | 见仓库 |
| PARM 校验、IDB 约定、GPT 几何 | [TheGammaSqueeze/rk_image_tool](https://github.com/TheGammaSqueeze/rk_image_tool)（`src/common/parameter.c`、`crc.c`、`rkimg.h`、`idblock.h`、`gpt.c`） | 见仓库 |
| 官方 RKFW 字段偏移对照 | `rk_image_tool/docs/FORMAT.md` | 见仓库 |

两份独立重实现在字段偏移上一致，且与 rkdeveloptool 的 packed struct 对得上，因此
`uf --dry-run` 采用这一套布局。若真机镜像与此不符，以官方镜像片段为准修正，不改
测试期望去迁就实现。

## 2. RKFW 容器（`update.img`）

固定头 0x66 = 102 字节，全部小端、无 padding：

| 偏移 | 长度 | 字段 | 说明 |
|---:|---:|---|---|
| 0x00 | 4 | `tag` | `"RKFW"` |
| 0x04 | 2 | `head_len` | 通常 `0x0066` |
| 0x06 | 4 | `version` | 打印为 `byte3.byte2.uint16(le)` |
| 0x0A | 4 | `code` | 常见 `0x01030000` |
| 0x0E | 2 | `year` | 发布年份 |
| 0x10 | 1 | `month` | |
| 0x11 | 1 | `day` | |
| 0x12 | 1 | `hour` | |
| 0x13 | 1 | `minute` | |
| 0x14 | 1 | `second` | |
| 0x15 | 4 | `chip` | 新镜像存 4 个 ASCII 数字的**逆序**（RK3588 → `"8853"`）；老镜像首字节是家族码 |
| 0x19 | 4 | `loader_offset` | 通常 `0x66` |
| 0x1D | 4 | `loader_length` | loader blob 长度 |
| 0x21 | 4 | `image_offset` | RKAF 起始偏移 |
| 0x25 | 4 | `image_length` | RKAF 长度，**含** 4 字节尾 CRC |
| 0x29 | 4 | `unknown1` | |
| 0x2D | 4 | `unknown2` | |
| 0x31 | 4 | `system_fstype` | |
| 0x35 | 4 | `backup_endpos` | |
| 0x39 | 0x2D | `reserved` | 到 0x66 |

尾部 MD5：

- 普通镜像：文件末尾 32 字节是**ASCII 十六进制 MD5**，覆盖 `[0, EOF-32)`。
- 带签名镜像：MD5 紧跟在 `image_offset + image_length` 之后，随后还有 ≥128 字节签名数据；
  rkdeveloptool 用 `file_size - image_end >= 160` 判断这一情形。
- `uf --dry-run` 两种都支持（`VerifyMD5`），流式读取，不把整镜像读进内存。

## 3. RKAF 归档

头部固定 2048 字节：

| 偏移 | 长度 | 字段 |
|---:|---:|---|
| 0x000 | 4 | `"RKAF"` |
| 0x004 | 4 | `length`：从归档起点到最后一个 payload 结束，**不含**尾 CRC |
| 0x008 | 0x22 | `model`（NUL 结尾） |
| 0x02A | 0x1E | `id` |
| 0x048 | 0x38 | `manufacturer` |
| 0x080 | 4 | `unknown1` |
| 0x084 | 4 | `version` |
| 0x088 | 4 | `num_parts`（≤ 16） |
| 0x08C | 16 × 112 | item 表 |
| 0x7E4 | 0x74 | reserved |

item（112 字节）：

| 偏移 | 长度 | 字段 | 说明 |
|---:|---:|---|---|
| 0x00 | 32 | `name` | partition 名，例如 `parameter`、`boot` |
| 0x20 | 60 | `filename` | 打包树里的路径 |
| 0x5C | 4 | `nand_size` | 分区扇区数；`0xFFFFFFFF` 表示 grow/未知 |
| 0x60 | 4 | `pos` | payload 在归档内的偏移 |
| 0x64 | 4 | `nand_addr` | 目标 flash 起始 **LBA（512 B 扇区）**；`0xFFFFFFFF` 表示没有 |
| 0x68 | 4 | `padded_size` | payload 在归档中占用的字节数（按 512 对齐） |
| 0x6C | 4 | `size` | payload 实际字节数 |

归档末尾 4 字节是 Rockchip CRC-32（见第 5 节），覆盖 `[归档起点, 尾 CRC)`。

`pos == 0xFFFFFFFF` 或 `size == 0xFFFFFFFF` 的 item 是设备侧生成的（常见于
`backup`/`RESERVED`），镜像里没有 payload。

### 32 位边界

- `length` 只有 32 位；归档超过 4 GiB 时字段里的值会回绕。
- 恢复方法（与 afptool-rs 相同）：真实长度是与字段值同余、且不超过文件可用字节数的
  **最大**值，即 `stored + ((available - stored) >> 32 << 32)`。
- `internal/rkfw` 在打开时做恢复并给出 warning；item 的 `pos + padded_size` 用 64 位
  计算，越界直接报错；`pos + size` 超过 4 GiB 时 warning（32 位偏移无法描述）。
- `padded_size < size`、item 落在 2048 字节头内、item 超出归档末尾都按**结构性错误**
  处理，不做“尽力而为”的截断解析。

## 4. parameter 分区与 PARM 包装

`parameter` item 的内容通常是：

```
"PARM" + uint32(内容长度) + parameter.txt 原文 + uint32(RKCRC)
```

CRC 覆盖 parameter.txt 原文（不含 8 字节头和 4 字节 CRC 本身）。也接受未包装的纯文本。

文本是 `KEY: value` 行；对刷机有用的是：

```
CMDLINE: mtdparts=rk29xxnand:0x00002000@0x00004000(uboot),-@0x0040000(rootfs:grow)
```

- 尺寸/偏移单位都是 **512 字节扇区**，十六进制（也接受十进制）。
- `-` 表示 grow 到磁盘末尾；`:grow` 等价；`:bootable` 等其它 flag 原样保留。
- 分区名重复、缺 `@`、缺括号、尺寸为 0 都是解析错误。

## 5. 校验算法

### Rockchip CRC-32（RKCRC）

- 多项式 `0x04C10DB7`，MSB-first，初值 0，无最终异或；与 GPT 用的 IEEE CRC-32 不同。
- 更新式：`crc = (crc << 8) ^ table[byte(crc>>24) ^ b]`。
- `internal/rkcrc` 生成表，并用独立位运算实现与 `rkdeveloptool/crc.cpp` 的表项
  （`table[1]=0x04C10DB7`、`table[2]=0x09821B6E`、`table[128]=0x6904C0EE`、
  `table[255]=0xB1F880B4`）交叉校验。
- 注意：该算法对前导 `0x00` 字节不变，`CRC32([]byte{0}) == 0` 是正确行为。

### 三类校验

| 校验 | 覆盖范围 | 存放位置 |
|---|---|---|
| RKFW MD5 | `[0, MD5 偏移)` | 文件尾 32 字节 ASCII，或 image 末尾（带签名时） |
| RKAF CRC | 归档头 + 所有 payload/padding | 归档 `length` 处 4 字节 |
| PARM CRC | parameter 文本 | 包装末尾 4 字节 |

`uf --dry-run` 默认三项都校验（`--no-verify` 可跳过流式两项）；PARM 总是校验。

## 6. GPT 计划

parameter 定义的是分区表，GPT 字节本身不随镜像分发，由设备上的 loader 在写
`parameter` 时生成。`internal/gpt` 复现目标几何：

```
LBA 0          protective MBR (0xEE)
LBA 1          primary GPT header
LBA 2..33      primary entry array（128 × 128 B = 16384 B）
LBA 34         first usable
LBA N-34       last usable
LBA N-33..N-2  backup entry array
LBA N-1        backup header
```

每项的 `first/last LBA` 直接取 parameter 的 offset/size，**不做 64 扇区圆整**：目前
没有证据表明 Fydetab Duo 的 loader 会圆整，而这正是需要和真机 GPT 对照的地方。
grow 分区在不知道 flash 大小时标记为 unknown，而不是猜一个值。

`--flash-sectors` 可指定真实 flash 大小；缺省时取 parameter 能容纳的最小磁盘
（最后一个分区末尾 + 34 扇区），并在输出里明确标注这是派生值。

## 7. `uf --dry-run` 的阶段模型

按 issue 的约定投影 4 类阶段，不访问设备：

| 阶段 | 字节数 | 说明 |
|---|---|---|
| `ul` | RKFW `loader_length` | Download Boot，把 loader 送进 RAM |
| `gpt` | `align512(loader_length)` | Prepare IDB（逐 512 B 扇区 RC4）+ Download IDB，写到 LBA 64 |
| `wl <item>` | item `padded_size` | 按目标 LBA 排序，逐个 payload 写入 |
| `rd` | 0 | 复位；`--noreset` 时标注跳过 |

进度按累计传输字节 / 总传输字节计算。IDB 的 LBA 64 与 RC4 约定来自
`rk_image_tool`（`RK_IDB_SECTOR 64`、`RK_IDB_BLOCK_SIZE 512`、固定 16 字节 key）；
M3 真机写入前需要再用官方 `upgrade_tool` 的日志核对一次。

item 到分区的映射规则：

1. `nand_addr != 0xFFFFFFFF` 时以它为准，找起始 LBA 相同（或落在区间内）的分区；
2. 否则按 item 名与 parameter 分区名做大小写不敏感匹配；
3. 两者都存在但指向不同分区 → warning，并采用 `nand_addr`；
4. 都对不上 → 不进入写入计划，明确 warning。

覆盖计算用 `padded_size`（padding 也会写入），分区大于 payload 时记入
`uncovered`：**未覆盖区间不会被写入或擦除**，旧内容保留，是否擦除取决于设备/loader，
镜像本身给不出答案。`STATE` 5 GiB / payload 1 GiB 的情形因此会显式列出 4 GiB。

## 8. 仍需官方镜像 / 真机确认的项

`internal/rkfw`、`internal/parameter`、`internal/gpt` 的测试全部使用合成 fixture，
没有把任何真机数值写死。以下项目等官方 v23.0-SP1 镜像片段或 stage 2 的真机 GPT：

1. RKFW 头 `head_len`/`chip`/`version` 的实际取值（决定 `ChipName` 的分支）。
2. `image_offset`/`image_length` 是否包含尾 CRC；是否带签名尾。
3. RKAF `num_parts`、item 名与 `nand_addr` 的实际取值（决定 name/addr 映射走哪条）。
4. `parameter` 是否带 PARM 包装、`DeclaredLength` 与 CRC 的实际值。
5. parameter 的 12 个分区名、顺序、LBA 与 grow 分区；KERN-B/ROOT-B/KERN-C/ROOT-C/
   reserved1 是否确实是“只建 GPT 不写数据”。
6. `STATE` 的 5 GiB / 1 GiB 数值与未覆盖 4 GiB 的起始 LBA。
7. 真机 GPT（`sgdisk -p` / `gdisk` / 探针读 GPT）与 `uf --dry-run` 的逐项对照。
8. `ul`/`gpt` 两个阶段的字节数是否与 `upgrade_tool uf` 的实际传输量一致（决定 IDB 假设）。

需要的片段很小（RKFW 头 + RKAF 头/item 表 + parameter 项 + 尾部 32 字节），
流程见 issue 评论。

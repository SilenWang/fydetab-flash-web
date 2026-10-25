# RKFW / RKAF / parameter 格式（M2 解析依据）

本文件记录 `internal/rkfw`、`internal/parameter`、`internal/gpt` 与 `uf --dry-run`
所依赖的磁盘结构、校验算法，以及**已经用真实镜像片段核对过的结论**。

核对用的样本是测试机上的 **openFyde r132**（Chromium 132 / platform 16093）：

```
/mnt/raid/openfyde/rk3588-image-maker/openfyde.r132.img
size    8,348,478,026 B
sha256  01fe1c0452606fc097d3816a73325c773013019954ed4f18fa55677842670ecc
md5     859643405c1bb4ec87d6e8cb29887b38
```

> **它不是官方 FydeOS v23.0-SP1，也不是 Fydetab Duo 的 v23 真值。** 官方
> `FydeOS_FydeTab_Duo_v23.0-SP1-io.img.zip`（sha256 `18eff9d8…2efffb`）在测试机上
> 没有，v23.0-SP1 的真值核对另开任务。凡本文写「实测」的数值，来源都是上面这张 r132
> 镜像，测试里也逐条注明了它的 sha256。
>
> 片段（2,766 B）在 `internal/testimage/testdata/r132/`，由 `internal/testimage`
> 按绝对偏移装进 8,348,478,026 B 的稀疏文件，测试因此能跑真实布局而不需要完整镜像。

## 1. 数据来源

| 用途 | 来源 | 许可 |
|---|---|---|
| RKFW 头、RKAF 结构、CRC-32 表、GPT 常量 | [rockchip-linux/rkdeveloptool](https://github.com/rockchip-linux/rkdeveloptool) 的 `RKImage.h`、`RKImage.cpp`、`crc.cpp`、`gpt.h` | GPL-2.0 |
| RKAF 尾 CRC、32 位长度回绕、PARM 包装、`padded_size` 单位 | [suyulin/afptool-rs](https://github.com/suyulin/afptool-rs)（`src/lib.rs`、`src/pack.rs`、`src/unpack.rs`） | 见仓库 |
| PARM 校验、IDB 约定、GPT 几何 | [TheGammaSqueeze/rk_image_tool](https://github.com/TheGammaSqueeze/rk_image_tool)（`src/common/parameter.c`、`crc.c`、`rkimg.h`、`idblock.h`、`gpt.c`） | 见仓库 |
| 官方 RKFW 字段偏移对照 | `rk_image_tool/docs/FORMAT.md` | 见仓库 |
| **实测样本** | openFyde r132（见上）+ SIL-520 切出的片段 | — |

原则不变：**实现与真实镜像不一致时改实现，不改测试期望去迁就实现**；真实镜像覆盖不到
的边界（32 位回绕、payload 越界、CRC 不符）继续由合成 fixture 覆盖。

## 2. RKFW 容器（`update.img`）

固定头 0x66 = 102 字节，全部小端、无 padding：

| 偏移 | 长度 | 字段 | r132 实测 |
|---:|---:|---|---|
| 0x00 | 4 | `tag` | `"RKFW"` |
| 0x04 | 2 | `head_len` | `102`（0x66） |
| 0x06 | 4 | `version` | `0x01000000` → `1.0.0` |
| 0x0A | 4 | `code` | `0x01060005` |
| 0x0E | 2 | `year` | `2025` |
| 0x10 | 1 | `month` | `9` |
| 0x11 | 1 | `day` | `4` |
| 0x12 | 1 | `hour` | `17` |
| 0x13 | 1 | `minute` | `11` |
| 0x14 | 1 | `second` | `54` |
| 0x15 | 4 | `chip` | `0x33353838` = 逆序 ASCII `"8853"` → **RK3588** |
| 0x19 | 4 | `loader_offset` | `102` |
| 0x1D | 4 | `loader_length` | `412,096` |
| 0x21 | 4 | `image_offset` | `412,198` |
| 0x25 | 4 | `image_length` | `4,053,098,500` = `rkaf.length + 4` |
| 0x29 | 4 | `unknown1` | `0` |
| 0x2D | 4 | `unknown2` | `1` |
| 0x31 | 4 | `system_fstype` | `0` |
| 0x35 | 4 | `backup_endpos` | `0x01494800` |
| 0x39 | 0x2D | `reserved` | 全 0 |

结论：`head_len`/`chip` 分支、`image_offset`/`image_length` 的含义都已对上——**`image_length`
包含 4 字节 RKAF 尾 CRC**（实测 = `rkaf.length + 4`）。

### 2.1 尾部 MD5（实测：位置不能靠算术猜）

- 普通镜像：文件末尾 32 字节是**ASCII 十六进制 MD5**，覆盖 `[0, EOF-32)`。
- 带签名镜像：MD5 紧跟在归档之后，随后还有 ≥128 字节签名数据。

r132 的实际情况把「跟随归档 / EOF-32」这条判据打穿了：

```
image_offset + image_length = 4,053,510,698     归档结束
file size                   = 8,348,478,026
其后的字节                   = 4,294,967,328 = 2^32（全 0 padding）+ 32（MD5）
EOF-32                      = "a2329d6f0af98a7379bec6faafbdcfe1"
归档结束处 32 字节           = 00 00 …（不是摘要）
```

所以 `upgrade_tool`/rkdeveloptool 的判据（`file_size - archive_end >= 160` 就当签名镜像）
在这张镜像上会**误判**：它会把 `4,053,510,698` 处当 MD5、把 4 GiB 的 0 当签名块。
`internal/rkfw` 因此改成**按内容选址**：先看归档结束处是否有 32 位 ASCII 十六进制，否则看
EOF-32；两者都不是才退回旧判据并报不匹配。`MD5Result.Signed` 只作为提示，不是可信标签。

已确认：`image_length` 与实际归档长度自洽；MD5 在 EOF-32，形状合法；但**摘要本身无法用
片段验证**（片段缺 3.78 GiB payload），所以测试只断言位置与摘要原文，不断言 `OK`。

## 3. RKAF 归档

头部固定 2048 字节（r132 实测：`model`/`id`/`manufacturer` 分别为 `" RK3588"`、`" 007"`、
`" RK3588"`，字段内确实带一个前导空格；`unknown1=0`、`version=0x01000000`、`num_parts=10`）：

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
| 0x00 | 32 | `name` | 分区名，例如 `parameter`、`STATE` |
| 0x20 | 60 | `filename` | 打包树里的路径 |
| 0x5C | 4 | `nand_size` | 分区扇区数；`0xFFFFFFFF` 表示 grow/未知 |
| 0x60 | 4 | `pos` | payload 在归档内的偏移 |
| 0x64 | 4 | `nand_addr` | 目标 flash 起始 **LBA（512 B 扇区）**；`0xFFFFFFFF` 表示没有 |
| 0x68 | 4 | `padded_size` | **2 KiB 块数量，不是字节数**（见 3.2） |
| 0x6C | 4 | `size` | payload 实际字节数 |

### 3.1 item 名/路径字段不是完整补零（实测）

`name` / `filename` 在第一个 NUL 之后**残留堆内存垃圾**，例如 `parameter` 后面跟着
`00 00 'e' 00`，`Image/parameter.txt` 后面跟着 `00 'b' 'i' 'n'`。因此：

- 解析**必须在第一个 NUL 处截断**，不能「删掉所有 NUL 再拼接」——后者会把
  `parameter` 读成 `parametere@…`，进而永远匹配不到 `parameter` 项（SIL-520 的原始脚本
  就是这么踩坑的）。
- `internal/rkfw` 的 `cstring` 一直是先截断再去的尾空格，这是正确的；测试
  `TestR132ItemNameStopsAtFirstNUL` 会在文件里复核「NUL 之后确实还有垃圾」，防止哪天
  片段被换成补零版本而让这条回归失效。

### 3.2 `padded_size`（+0x68）：单位是 2 KiB 块

**这是本次核对改掉的一条错误假设。** 旧实现把 +0x68 当「payload 在归档中占用的字节数」，
于是对 r132 直接报结构错误：

```
rkfw: item "package-file" padded size 0x1 is smaller than its payload size 0x19e
```

实测（10 项，`+0x68` / `+0x6C` / 真实归档槽位）：

| item | `+0x68` | `+0x6C` (payload) | 槽位（到下一项 pos） | `+0x68 × 2048` |
|---|---:|---:|---:|---:|
| package-file | 1 | 414 | 2,048 | 2,048 ✓ |
| bootloader | 202 | 412,096 | 413,696 | 413,696 ✓ |
| parameter | 1 | 590 | 2,048 | 2,048 ✓ |
| STATE | 2,621,468 | 1,073,797,632 | 1,073,799,168 | 5,368,766,464 ✗ |
| KERN-A | 16,384 | 33,554,432 | 33,554,432 | 33,554,432 ✓ |
| ROOT-A | 1,394,688 | 2,856,321,024 | 2,856,321,024 | 2,856,321,024 ✓ |
| OEM | 8,192 | 16,777,216 | 16,777,216 | 16,777,216 ✓ |
| resource | 451 | 923,136 | 923,648 | 923,648 ✓ |
| uboot | 2,048 | 4,194,304 | 4,194,304 | 4,194,304 ✓ |
| EFI-SYSTEM | 32,768 | 67,108,864 | 67,108,864 | 67,108,864 ✓ |

结论：

- `+0x68` 是**2 KiB 块计数**，9/10 项正好等于「槽位 / 2048」。与 afptool-rs 的说法一致
  （`padded_size is in 2048-byte sectors`）。
- **`STATE` 是唯一的例外**：它记录的是**分区容量**（5 GiB 的分区 = 2,621,468 块），不是
  1 GiB payload 的槽位。afptool-rs 对这类情形的备注可以解释它：打包器用这个字段编码
  分区的**真实长度**，而 payload 字节数在 4 GiB 处会回绕。所以这个字段永远不能当字节长度用。
- 实现改为：`Item.Size` 取 `+0x6C`（payload 字节数）；`Item.PaddedSize`（字节）由**相邻
  item 的 pos** 推导（最后一项到归档末尾）；`+0x68` 原样保留为 `Item.PaddedBlocks`。
- 结构校验也随之改：payload 必须落在自己的槽位内（越过下一项即报错），不再比较
  `+0x68` 与 payload 大小。

### 3.3 32 位长度、4 GiB 洞与「尾部 padding」

**这是 SIL-515 真机失败后改掉的结论。** 之前把归档后的 2^32 字节当成 padding；
实测（Fydetab Duo 读回）证明它是**打包器插在归档内部的洞**：

- STATE 分区 5 GiB、payload 1 GiB，打包器在文件里为 STATE 预留了整段 5 GiB 槽位，
  但 item 表的 32 位 `pos` 只描述「不含洞」的紧凑布局；
- 于是 STATE 之后的所有 item 在文件里的真实偏移 = `pos + hole`，照 `pos` 读会读到 0；
  真机证据：KERN-A 的 `pos=0x40077000` 处是 0，`+4 GiB` 处才是 `CHROMEOS`。

`internal/rkfw` 因此按结构推导洞，而不是加常量：

```
declared_end = rkaf_offset + rkaf.length + 4        (32 位字段说的归档末尾)
packed_end   = file size - 32                       (尾随 MD5 存在时)
hole         = packed_end - declared_end
```

两张真实镜像都得到 `hole = 4 GiB`（r132 与 v23 各自的 `packed_end - declared_end`）。
洞的**起点**再用 payload 签名判别：对每个 item 分别采样 `pos` 与 `pos+hole`，
预期魔数（`KERN-*`→CHROMEOS、`uboot`→FDT、`EFI-SYSTEM`→FAT、`resource`→RSCE）
出现在哪一侧就采用哪一侧；一旦有 item 落在 `pos+hole`，其后的 item 也一起位移。

- 判据能落地时 → `Image.DataHoleStart` = 第一个位移 item 的 `pos`，plan 里逐项打印
  `pos` / `dataOffset` / 实际读到的魔数；
- 采样不到任何签名（只有头部片段的测试镜像）→ 洞大小照报，但位置标为**未验证**，
  所有 item 仍按表内 `pos` 读，且 dry-run **拒绝写入**。这是有意的保守取舍：
  猜错偏移正是 SIL-515「7/7 分区成功、写进去的全是 0」的原因。

`length` 的回绕判据本身保留（容器 `image_length` 与 `rkaf.length` 互证时采用存储值）：
v23 的两个字段都回绕到同一个值，与 r132 一样，靠长度已无法区分「回绕」与「padding」，
洞是由文件布局 + 签名判定出来的。

## 4. parameter 分区与 PARM 包装（实测）

```
"PARM" + uint32(578) + 578 B 文本 + uint32(0x7dbc1c70)
```

- 实测包装长度 590 = 8 + 578 + 4，`DeclaredLength = 578`，PARM CRC 校验**通过**。
- 文本是 `KEY: value` 行；`MACHINE_MODEL: RK3588`、`TYPE: GPT`、`CHECK_MASK: 0x80` 等。
- `CMDLINE: mtdparts=rk29xxnand:` 给出 **12 个分区**，单位是 512 字节扇区：

| # | 分区 | first LBA | sectors | 字节 | 备注 |
|---:|---|---:|---:|---:|---|
| 1 | STATE | 0x00b02040 = 11,542,592 | 0xa0006d = 10,485,869 | 5,368,764,928 | ≈5 GiB + 53,248 B |
| 2 | KERN-A | 0x0000e040 = 57,408 | 0x00010000 | 33,554,432 | |
| 3 | ROOT-A | 0x005b0040 = 5,963,840 | 0x00552000 | 2,856,321,024 | |
| 4 | KERN-B | 0x0001e040 = 122,944 | 0x00010000 | 33,554,432 | 只建 GPT |
| 5 | ROOT-B | 0x0005e040 = 385,088 | 0x00552000 | 2,856,321,024 | 只建 GPT |
| 6 | KERN-C | 0x00006000 = 24,576 | 1 | 512 | 只建 GPT |
| 7 | ROOT-C | 0x00006001 = 24,577 | 1 | 512 | 只建 GPT |
| 8 | OEM | 0x0002e040 = 188,480 | 0x00008000 | 16,777,216 | |
| 9 | reserved1 | 0x00006002 = 24,578 | 1 | 512 | 只建 GPT |
| 10 | resource | 0x00006004 = 24,580 | 0x00008000 | 16,777,216 | |
| 11 | uboot | 0x00004000 = 16,384 | 0x00002000 | 4,194,304 | |
| 12 | EFI-SYSTEM | 0x0003e040 = 254,016 | 0x00020000 | 67,108,864 | `:bootable` |

- **没有 grow 分区**（此前的合成 fixture 假设过 `-` / `:grow`，真实镜像用不到；相关解析
  代码保留，供其它平台镜像使用）。
- 分区在 flash 上的物理顺序与表内顺序不同（例如 ROOT-B 在 ROOT-A 之前、KERN-C/ROOT-C/
  reserved1/resource 挤在 24,576 起的几个扇区里），`internal/gpt` 会按 first LBA 排序后
  检查重叠——实测无重叠。
- 12 个分区都落在 `first + sectors` 内，无重叠、无 grow。最后一项 STATE 的末 LBA 是
  22,028,460；未给 `--flash-sectors` 时派生磁盘 22,028,495 扇区（`max end + 34`），
  对应 last usable LBA 22,028,461，也就是说派生几何在末尾留了 1 个扇区的余量。

## 5. 校验算法

### 5.1 Rockchip CRC-32（RKCRC）

- 多项式 `0x04C10DB7`，MSB-first，初值 0，无最终异或；与 GPT 用的 IEEE CRC-32 不同。
- 更新式：`crc = (crc << 8) ^ table[byte(crc>>24) ^ b]`。
- `internal/rkcrc` 生成表，并用独立位运算实现与 `rkdeveloptool/crc.cpp` 的表项
  （`table[1]=0x04C10DB7`、`table[2]=0x09821B6E`、`table[128]=0x6904C0EE`、
  `table[255]=0xB1F880B4`）交叉校验。
- 该算法对前导 `0x00` 字节不变，`CRC32([]byte{0}) == 0` 是正确行为。

### 5.2 RKAF 尾 CRC 在 r132 里是 `00 00 00 00`：结论

实测位置与值：

```
image_offset + rkaf.length = 412,198 + 4,053,098,496 = 4,053,510,694
该处 4 字节                                                    = 00 00 00 00
```

**偏移是对的，不是解析错位**：容器里的 `image_length` 正好是 `rkaf.length + 4`，这 4 字节
就是归档末尾的 trailer 字段，两处独立字段互证。

那 `0` 是什么意思？**最可能是「这张镜像的打包器没有填这个字段」，但用现有片段无法证明。**
依据与不确定性写清楚：

- 参考实现 afptool-rs 的 `pack` 会把整包 RKCRC 追加成 4 字节尾，可见这字段在格式上确实存在；
  但它的 `unpack` **只比对长度、从不校验 CRC**（长度对不上时只打印
  `update_header.length cannot be correct, cannot check CRC`）。
- rkdeveloptool（GPL 的 `RKImage.cpp`）**完全不碰这 4 字节**，只处理尾部 MD5。
  也就是说官方工具链不依赖它，打包器留 0 不会有任何可见后果。
- 但「真实 CRC 恰好为 0」在概率上并不能排除（2^-32）。片段只有 2,766 B，缺 3.78 GiB
  payload，**算不出真实 CRC**，所以无法用计算来判定。

因此：**校验逻辑不动**。`VerifyRKAFCRC` 照旧把 `stored != computed` 判成不匹配，
**不把 0 当成「未使用 / 合法」**。这条不影响其它结论，但它是 M3 的决策项：如果官方
v23.0-SP1 镜像也是 0，那么 `uf` 在写入前就需要一个明确的产品决定（报错中止 / 标注后继续），
不能靠把 0 当合法来让它变绿。

### 5.3 三类校验

| 校验 | 覆盖范围 | 存放位置 | r132 |
|---|---|---|---|
| RKFW MD5 | `[0, MD5 偏移)` | EOF-32（普通）或归档后（签名） | 位置 EOF-32，形状合法，**无法用片段验证** |
| RKAF CRC | 归档头 + 所有 payload/padding | 归档 `length` 处 4 字节 | `00 00 00 00`（见 5.2） |
| PARM CRC | parameter 文本 | 包装末尾 4 字节 | `0x7dbc1c70`，**校验通过** |

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

每项的 `first/last LBA` 直接取 parameter 的 offset/size，**不做 64 扇区圆整**：目前没有
证据表明 Fydetab Duo 的 loader 会圆整，而这正是需要和真机 GPT 对照的地方。

r132 在未给 `--flash-sectors` 时派生的磁盘是 **22,028,495 扇区**（最后一个分区末尾
22,028,461 + 34），并明确标注为派生值。`--flash-sectors` 可指定真实 flash 大小。

## 7. `uf --dry-run` 的阶段模型

| 阶段 | 字节数 | r132 实测 |
|---|---|---|
| `ul` | RKFW `loader_length` | 412,096 |
| `gpt` | `align512(loader_length)` | 412,160 |
| `wl <item>` | item 的归档槽位（`PaddedSize`） | 7 项，合计 4,052,678,656 |
| `rd` | 0 | `--noreset` 时标注跳过 |

`uf --dry-run` 在 r132 上跑通，总计 4,053,502,912 字节（含 ul + gpt）。

item 到分区的映射规则：

1. `nand_addr != 0xFFFFFFFF` 时以它为准，找起始 LBA 相同（或落在区间内）的分区；
2. 否则按 item 名与 parameter 分区名做大小写不敏感匹配；
3. 两者都存在但指向不同分区 → warning，并采用 `nand_addr`；
4. 都对不上 → 不进入写入计划，明确 warning。

r132 实测：`package-file`、`bootloader`（`nand_addr = 0xFFFFFFFF` 且不是分区名）和
`parameter`（`nand_addr = 0`，没有起始 LBA 为 0 的分区）都落到第 4 条，不进入 `wl`；
其余 7 项按 `nand_addr` 精确命中分区。

> **未解决（M3 决策项）**：真实 `uf` 必然要写 `parameter`（loader 靠它生成 GPT），而当前
> 计划里它既没有目标 LBA、也不在 `gpt` 阶段的字节里。M2 的 `gpt` 阶段只覆盖 IDB；
> 写 `parameter` 归 M3（SIL-517）在写入编排里定位置，本阶段只如实报出来。

覆盖计算用槽位（padding 也写入）。分区大于 payload 时记入 `uncovered`：
**未覆盖区间不会被写入或擦除**，旧内容保留，是否擦除取决于设备/loader，镜像本身给不出答案。

r132 实测未覆盖区间：

| 分区 | 分区大小 | 覆盖 | 未覆盖 | 起始 LBA |
|---|---:|---:|---:|---:|
| STATE | 5,368,764,928 | 1,073,799,168 | **4,294,965,760** | 13,639,856 |
| resource | 16,777,216 | 923,648 | 15,853,568 | 24,580+1,804 |

STATE 的 4 GiB 缺口被显式列出（这就是 issue 要求「不要静默」的那 4 GiB）。

## 8. 核对状态

已用 r132 实测确认（测试见 `internal/rkfw/r132_test.go`、`internal/parameter/r132_test.go`、
`cmd/uf/r132_test.go`）：

1. RKFW `head_len`/`chip`/`version`/发布时间的取值与解码；RK3588 走 ASCII 逆序分支。
2. `image_offset`/`image_length` 的语义；`image_length` 含 4 字节尾 CRC。
3. **item 名/路径在第一个 NUL 处截断**（NUL 之后有堆垃圾）。
4. **`+0x68` 是 2 KiB 块计数，`+0x6C` 才是 payload 字节数**；`STATE` 的 `+0x68` 记录分区容量。
5. **32 位长度回绕判据**：容器字段互证时不做外推；r132 有 `2^32` 字节尾部 padding。
6. `num_parts = 10`、10 个 item 的 name/filename/pos/nand_addr/size 全部逐项对上。
7. parameter 带 PARM 包装（长度 578、CRC `0x7dbc1c70` 通过），12 个分区名/顺序/LBA 全部对上，
   **没有 grow 分区**；KERN-B/ROOT-B/KERN-C/ROOT-C/reserved1 确实是「只建 GPT 不写数据」。
8. `STATE` 的 5 GiB 与 1 GiB payload：未覆盖 4 GiB 的字节数与起始 LBA 已算出并进测试。
9. `uf --dry-run` 在 r132 上跑通：阶段、字节数、分区覆盖与 item 映射都与镜像一致。

仍未确认（需要官方 v23.0-SP1 镜像 / 真机）：

1. 官方 v23.0-SP1 的同一批数值（`head_len`、`chip`、item 表、分区表、payload 大小）。
   镜像已可用，核对流程见第 9 节。
2. `rkaf-crc-trailer` 在官方镜像里是否也是 `00 00 00 00`（决定 M3 的写入前校验策略）。
   同上，第 9 节的输出里会直接打印这个值。
3. 真机 GPT（`sgdisk -p` / 探针读 GPT）与 `uf --dry-run` 的逐项对照，含 loader 是否会
   圆整 LBA。
4. `ul`/`gpt` 两个阶段的字节数是否与 `upgrade_tool uf` 的实际传输量一致（IDB 假设、LBA 64）。
5. `parameter` 在真实 `uf` 里的写入位置（见第 7 节）。
6. r132 那 `2^32` 字节尾部 padding 的成因（与 STATE 的 4 GiB 未覆盖区间数值相同，但镜像
   本身无法证明两者相关）。

## 9. 用任何真实镜像核对（不需要把 8 GiB 搬到开发机）

`tools/extract-rk-slices.sh` 只读地切出核对所需的 5 个片段，`internal/testimage`
的 `LoadSlices` 再把它们按绝对偏移装回稀疏文件：

```bash
# 1) 在放镜像的那台机器上（.img 已解压；脚本检测到 .zip 会提示先解压）
pixi run slices /path/to/FydeOS_FydeTab_Duo_v23.0-SP1-io.img rk-slices
#    或：bash tools/extract-rk-slices.sh <update.img> <outdir>

# 2) 把 rk-slices/ 整个目录带到开发机（或把 5 个 .bin + manifest.txt 挂到 issue）

# 3) 在这里回放
RKFW_SLICES_DIR=$PWD/rk-slices pixi run test        # 跑全套；TestSlicesFromEnv 会跳过之外的用例
RKFW_SLICES_DIR=$PWD/rk-slices pixi run bash -c \
  'go test ./internal/rkfw -run TestSlicesFromEnv -v'   # 只看这份镜像的测量值
```

脚本做的事与踩过的坑：

- 在**第一个 NUL** 处截断 `name`/`path`（不是删 NUL），否则 `parameter` 读成
  `parametere@…`，参数项永远找不到；
- payload 长度取 `+0x6C`（`+0x68` 是 2 KiB 块计数，`parameter` 处只有 `1`）；
- 尾部 MD5 按内容定位（先看归档结束处、再看 EOF-32），不靠「后面还有 ≥160 B」的算术；
- 产出 `manifest.txt`（`size` / `sha256` / `image_offset` / `rkaf_length` / 各片段偏移），
  `LoadSlices` 按它装配。

`TestSlicesFromEnv`（`internal/rkfw/slices_test.go`）只断言格式保证（item 槽位求和等于
归档体、payload 不越槽、PARM CRC 自洽、`gpt.Build` 不报重叠），其余全部 `t.Logf` 打印，
包括 `head_len`、`chip`、item 表、12 个分区、`rkaf crc trailer`、MD5 位置与原文。
它的目的是**先看数值、再固化成断言**：确认后照 `r132_test.go` 的样子写一份
`<镜像名>_test.go`，在注释里标 `sha256`，不要直接把日志里的数字抄成期望值而不核对来源。

已知的官方 v23.0-SP1 事实只有父任务给的这几条（**尚未在本仓库实测**）：10 个 item、
12 个分区、payload ≈ 3.79 GiB、`STATE` 5 GiB 分区 / 1 GiB payload。补齐后应同时更新
第 8 节的清单。

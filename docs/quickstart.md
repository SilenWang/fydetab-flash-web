# 快速上手：用安装版 PWA 刷 Fydetab Duo

适用：Fydetab Duo（RK3588 / FydeOS v23.0-SP1）。整条流程在浏览器里完成，镜像不上传、不需要后端。
页面各区块的详细行为见 [usage-web.md](usage-web.md)，驱动与授权问题见 [webusb-drivers.md](webusb-drivers.md)，
文档总索引见 [README.md](README.md)。

## 0. 你需要

- Chrome / Edge（Chromium ≥ 89；本次实测 Chrome 145），macOS / Linux / Windows 均可。
- 一根**能传数据**的 USB-C 线（随机附带的线可能只能充电）。
- 官方镜像 `FydeOS_FydeTab_Duo_v23.0-SP1-com.img`（8,360,540,746 B，sha256 `5a3f7991…fe7010`）。
- Linux：装一条 udev 规则；Windows：把设备换成 WinUSB；macOS：不需要驱动。
  细节见 [`webusb-drivers.md`](webusb-drivers.md)。

Linux 一条命令装规则：

```bash
sudo install -m 0644 docs/99-rockchip-webusb.rules /etc/udev/rules.d/99-rockchip-webusb.rules
sudo udevadm control --reload-rules
sudo udevadm trigger --subsystem-match=usb --attr-match=idVendor=2207
```

## 1. 打开并安装

1. 打开 `http://localhost:5173/app/`（或部署好的 HTTPS 地址）。首次打开会把页面与 wasm 缓存下来
   （24 个文件，约 6.1 MiB）。
2. 点地址栏右侧的**安装图标** → 「安装」；或菜单 →「投放、保存和共享」→「安装页面为应用…」；
   页面底部「高级 / 诊断」→「安装与离线（PWA）」里也有「安装为应用」按钮。
   装好后从桌面/开始菜单启动，是独立窗口（无地址栏），图标与名称正确。
3. 之后**断网也能打开**：页面、脚本和 wasm 都由 service worker 从本地缓存提供。
   安装本身**不授予设备权限**，写设备仍要授权。

## 2. 让平板进入刷写模式

1. 平板完全关机，用 USB-C 接到电脑。
2. 按住 **音量 +（Vol+）** 不放，短按 **电源** 0.5–1 秒；先松电源，再松 Vol+。
   - 本机实测这样进入 **LOADER**，可以直接写，不需要额外的进入步骤。
   - 部分官方固件上同一组合进入 **MASKROM**；两种模式都能刷。MASKROM 下页面会自动先发
     Download Boot，**上电后不要先点任何读取按钮**。
   - 按键进不去（黑屏 / 系统损坏）：拆开后盖，按住主板上的 **MASKROM** 键再插 USB。
     官方 wiki 的 Unbrick 页面有按键位置图。
3. 第 3 块应列出设备：`2207:350b`，模式显示 LOADER 或 MASKROM。
   第一次要「授权设备」；授权按“来源 + 设备实例”发放，设备重新枚举后需要再授权。
   如果机器配置了 WebUSB 策略，会按 VID/PID 自动授权、不弹窗。

## 3. 刷写

1. 第 1 块选镜像：`.img`，或官方 `.zip`（直接流式解压，不落地 3.8 GiB）。
2. 第 2 块点「校验」：结构 + PARM CRC + 整镜像 MD5，等提示“完整校验通过”。
3. 第 3 块选设备。第 4 块把「未覆盖的分区尾部」设为 **none**（保持原内容），勾选“我确认这个处置”。
4. 五项自检全绿后点「开始刷写」。官方 v23 镜像实际写 **7.79 GiB**，本次实测 **519.5 s**
   （约 8 分 40 秒，其中 STATE 的 5 GiB 占 5 分半）。写的时候不要拔线、不要切窗口。
5. 完成后自动发 `rd` 复位。设备会离开 USB 并启动；首次启动 FydeOS 会把 STATE 自动扩到约 227 GiB，
   这是正常行为。

## 4. 失败了怎么办

- 页面会标出失败阶段（Download Boot / GPT / 某个分区 / 复位）并保留日志。
  **先导出日志**（第 5 块：导出 NDJSON 或导出文本），再决定下一步；不要连续重刷。
- 常见的下一步：重新授权设备、换 USB 口/线、重新进 Loader 或 MASKROM。
- 设备起不来（救砖顺序）：
  1. 先按第 2 节重新进 **Loader**；能进 Loader 就能用本页再刷一次。
  2. 按键无效就拆机按住主板 **MASKROM** 键再插 USB（硬件级入口，系统刷坏也能进）。
  3. 仍不行走**官方救砖路径**：另一台电脑 + FydeOS 官方 wiki 的 Unbrick / Flashing 页面 + 官方升级
     工具与官方镜像，按官方步骤恢复。官方工具与本页**不要同时占用**设备。
- 判断有没有救：`lsusb` 能看到 `2207:350b` 就还有入口；完全看不到时先换线、换口、换电脑。

## 5. 日志与诊断

- 第 5 块「日志」：导出 NDJSON（包含每次 USB 传输摘要、进度、校验结果与耗时）或文本。
- 页面底部「高级 / 诊断」：原始传输记录（最近若干条）、只读设备信息、错误面板。
- 驱动问题看 `chrome://device-log`（选 USB）；授权状态看 `chrome://settings/content/usbDevices`。
- macOS 不需要驱动；Windows 用 Zadig 换 WinUSB；Linux 见上面的 udev 规则和
  [`webusb-drivers.md`](webusb-drivers.md)。

## 本次验收实测（2026-10-26）

- **安装**：Chrome 145，独立应用窗口（`display-mode: standalone`，窗口类 `crx_…`），
  名称/图标正确；桌面入口指向同一个浏览器 profile。
- **离线**：停掉本地服务器后从安装图标启动，页面与 wasm 全部来自 service worker 缓存；
  缓存 24 项 / 6.1 MiB；DevTools 离线模式下刷新仍能加载，`core` 正常。
- **刷写**：安装版 PWA 完整 `uf` 官方 v23 镜像：7.79 GiB、7/7 items、主 GPT 与 parameter 读回校验、
  IDB 保留（`RKNS`，未重写）、`rd` 已发；519.5 s、记录到的 60,000 帧传输 0 错误。
- **授权**：本次测试机配置了 WebUSB 策略，授权弹窗 0 次、自动授权 1 台设备
  （serial `87a72dac1d78fc10`）。普通用户第一次会看到 1 次授权框。

# 浏览器与驱动：WebUSB 能不能看见板子（macOS / Linux / Windows）

本页要把 `0x2207` 设备拿到手里，靠的是 **WebUSB**，而 WebUSB 在三个平台上要求的“驱动程序”完全不同：
macOS 上什么都不用装，Linux 上要一条 udev 规则，Windows 上要 WinUSB。装错驱动是最常见的“页面里
什么都看不到”的原因，所以这份文档按平台写清楚**要什么、怎么验证、装错了怎么退回去**。

> 与驱动无关的两件事也在这里说明：**哪些浏览器能用**，以及**为什么每次重新枚举都要重新授权**。

## 0. 先确认不是浏览器的问题

| 浏览器 | WebUSB | 能否安装本页为 PWA | 结论 |
|---|---|---|---|
| 桌面 Chrome / Edge（Chromium ≥ 89） | ✅ | ✅ | 推荐 |
| ChromeOS / Android Chrome | ✅（Android 需 OTG） | ✅ | 可用，写入慢 |
| Chromium（发行版打包版） | ✅（部分发行版默认关闭，见下） | ✅ | 可用 |
| Firefox | ❌ 明确不实现 | ❌ | 只能看页面 |
| Safari（macOS / iOS/iPadOS） | ❌ | iOS 16.4+ 可“添加到主屏幕”，但没有 WebUSB | 只能离线查看页面/计划 |

判定方法（在页面控制台里跑）：

```js
// 三项都必须为 true
({
  isSecureContext,                 // https 或 http://localhost
  hasWebUSB: !!navigator.usb,
  chromium: /Chrome\/|Edg\//.test(navigator.userAgent),
})
```

页面自己也会在顶部给出提示：没有 WebUSB 时横幅会写明“请用桌面版 Chrome / Edge”。

- **安全上下文**：`file://` 打开不行（ES module 与 module worker 都被禁），必须 `http://localhost`
  或 `https://`。`pixi run dev` 提供 `http://localhost:5173/app/`。
- **Chromium 发行版默认关闭 WebUSB** 时，用 `chrome://flags/#enable-experimental-web-platform-features`
  或启动参数 `--enable-features=WebUSB` 打开；Chrome / Edge 官方包不需要。
- 页面需要**用户手势**才能弹授权框：这就是“授权设备”按钮存在的原因，脚本无法代替你点。

## 1. macOS（Intel 与 Apple Silicon / arm64）

**不需要安装任何驱动或 kext。** Rockchip 设备以 vendor-specific class（`0xff`）出现，macOS 自带
的 `IOUSBFamily` 就能枚举它，Chrome 直接 claim 接口。

要检查的地方只有三个：

```bash
# 1) 系统有没有看到设备（maskrom 与 loader 的 PID 不同，都在这条输出里）
system_profiler SPUSBDataType | grep -A 8 -i -E "rockchip|rk3588|2207"

# 2) Chrome 侧有没有拿到权限：chrome://device-log（选 USB）
#    正常会看到 "Found device 2207:xxxx" 与 "Opened device"
```

1. **USB-C 扩展坞 / 集线器**：Apple Silicon 上部分 hub 会把设备挂到不稳定的端口上，表现为
   `requestDevice()` 弹窗里根本没有条目。先直连 Mac 上的 USB-C 口，再换回 hub。
2. **虚拟机**：UTM / Parallels / VMware 里的 Chrome 看不到宿主机的 USB；需要在虚拟机设置里把
   Rockchip 设备**按 VID 直通**给客户机，否则永远“无设备”。
3. **授权是按来源、按设备实例的**：`http://localhost:5173` 与 `http://127.0.0.1:5173` 是两个来源，
   在其中一个授权过，换另一个地址要重新授权。`chrome://settings/content/usbDevices` 可以查看/撤销。
4. 安装成 PWA 之后，应用窗口仍是**同一个来源**，权限继续有效；但**设备重新枚举后实例变了**，
   需要重新授权（见第 4 节）。

## 2. Linux（udev 规则，必装）

Linux 上 `/dev/bus/usb/*` 默认 `root:root 0664`。Chrome 以普通用户运行，`open()` 会得到 `EACCES`，
页面里表现为设备**列表是空的**或 `Unable to open device`，而 `lsusb` 明明看得到。所以必须要一条 udev 规则。

仓库里提供了 `docs/99-rockchip-webusb.rules`：

```bash
sudo install -m 0644 docs/99-rockchip-webusb.rules /etc/udev/rules.d/99-rockchip-webusb.rules
sudo udevadm control --reload-rules
sudo udevadm trigger --subsystem-match=usb --attr-match=idVendor=2207
```

规则的内容（`TAG+="uaccess"` 让 systemd-logind 把设备交给当前登录用户，登出即失效）：

```udev
SUBSYSTEM=="usb", ATTR{idVendor}=="2207", TAG+="uaccess", MODE="0660"
# 没有 logind 的系统改用组：
# SUBSYSTEM=="usb", ATTR{idVendor}=="2207", GROUP="plugdev", MODE="0660"
```

**验证顺序**（每条都过再回到页面）：

```bash
# 1) 设备在不在总线上
lsusb -d 2207:
# 2) 权限对不对：注意是 /dev/bus/usb/<bus>/<dev>，不是 /dev/tty*
ls -l /dev/bus/usb/$(lsusb -d 2207: | sed -E 's/Bus ([0-9]+) Device ([0-9]+).*/\1 \2/' | awk '{printf "%03d", $1}')/
#    期望属主是你，或 mode 里对本组/本用户可读写（crw-rw---- + uaccess 由 ACL 控制：
getfacl /dev/bus/usb/<bus>/<dev>   # 有 user:<you>:rw- 即正常
# 3) 内核驱动有没有抢走接口：`Driver=` 应为空（vendor-specific 设备通常没有驱动）
udevadm info -q property -n /dev/bus/usb/<bus>/<dev> | grep -E "ID_USB_DRIVER|ID_USB_MODEL"
lsusb -t
```

常见坑：

- **`usb-storage` 抢接口**：少数内核会把 LOADER 态的 8 类接口（`bInterfaceClass=08`）绑给
  `usb-storage`，Chrome 就 claim 不到。`lsusb -t` 看到 `usb-storage` 时，用规则文件末尾被注释的
  `ID_USB_DRIVER=""` 两行（或临时 `sudo modprobe -r usb-storage` 后重试）。注意 `modprobe -r`
  会影响所有 U 盘，验证完请插回去或 `sudo modprobe usb-storage`。
- **相比 `0666`，优先用 `uaccess`**：`0666` 让机器上任何用户都能写 flash，`uaccess` 只给登录会话。
- **Chrome 是 snap / flatpak**：沙箱里的 Chrome 看不到 `/dev/bus/usb`（snap 需要
  `snap connect chromium:raw-usb`；flatpak 需要 `--device=all`）。最省事的做法是用发行版包或官方
  `.deb` / `.rpm`。
- **容器**：Docker 里跑浏览器要 `--device=/dev/bus/usb --privileged`，一般不建议。
- 规则改完**必须重新插拔**设备（`udevadm trigger` 只对已存在的节点补权限，有些内核仍会复用旧的 ACL）。
- 撤销/查看授权：`chrome://settings/content/usbDevices`；看底层报错：`chrome://device-log` 里选 USB。

## 3. Windows（WinUSB）

Windows 上 WebUSB **只能操作绑定了 WinUSB 驱动的设备**。Rockchip 官方工具（DriverAssistant /
RKDevTool / `upgrade_tool`）装的是自己的 `Rockusb` / `RKUSB` 驱动，Chrome 看到设备但打不开
（`chrome://device-log` 里是 `Access denied` 或 `Open failed`）。两者**不能同时**占用同一个设备。

把设备换到 WinUSB（Zadig，https://zadig.akeo.ie/）：

1. 板子进 maskrom 或 loader 模式，USB 线插到主机**后面板**的 USB 2.0 口（前置面板/集线器经常掉）。
2. 打开 Zadig → `Options → List All Devices`。
3. 在下拉里选 `2207` 的设备（名称可能是 `Rockusb Device`、`RK3588`、`USB Download Device` 等）。
4. 右侧目标驱动选 **WinUSB**，点 `Replace Driver`（或 `Install Driver`）。**不要**选 libusbK，
   Chrome 只认 WinUSB。
5. 装完在设备管理器里应看到 `Universal Serial Bus devices → WinUSB device`。重新插拔一次，回页面点“授权设备”。

**要退回 Rockchip 官方工具**（RKDevTool / `upgrade_tool` 又要用了）：在设备管理器里右键该设备 →
`卸载设备` → 勾选“删除此设备的驱动程序软件” → 重新插板 → 让 DriverAssistant 重装驱动（或重启后
Windows 自动装回）。**同一时刻只能二选一**：WinUSB 生效时官方工具会报“找不到设备”，反之 WebUSB 打不开。

其它 Windows 注意点：

- Windows 10/11 首次插入会弹“设备无法识别”：多半是线材/供电或 USB 3.0 xHCI 兼容性，换线、换口。
- 不要在 Zadig 里对**键盘鼠标/集线器**点 Replace Driver，会把它们变砖（重装系统驱动才能恢复）。
- 用 `usbview.exe`（Windows SDK）确认 VID `2207`、接口 class `0xff`、两个 bulk endpoint。
- 杀毒/EDR 有时拦截 `WinUsb_Initialize`，报错是 `Access denied`；临时排除 Chrome 再试。

## 4. 重新枚举后为什么必须再授权一次

浏览器的 USB 权限绑在**设备实例**上，不是 VID/PID。`Download Boot` 之后：

```
maskrom (2207:xxxx, bcdUSB bit0=0)  ──db──▶  重新枚举  ──▶  LOADER (2207:yyyy, bcdUSB bit0=1)
                                                  └─ 旧实例消失，权限作废
```

所以页面的流程是：`db` 成功 → 标记“等待重新枚举” → `connect` 事件触发自动 `ld` → 列表里没有设备
（或 `open` 失败）→ **重连向导**让你点“重新授权”→ 选择 `0x2207` 设备 → 自动 `ld` → 徽章变
`Loader` 才允许继续。同一件事在拔线、复位、写入中断后都会发生。

重连向导连续出现 3 次以上时，页面会提示回来查这份文档：那通常不是“再试一次”能解决的，而是
驱动/线材/供电/udev 之一。

## 5. 参考

- WebUSB 规范与实现状态：<https://wicg.github.io/webusb/>
- Chrome 的设备日志：`chrome://device-log`（USB）、`chrome://usb-internals`
- Linux 的 udev 规则来自本仓库 `docs/99-rockchip-webusb.rules`
- 写入路径与真机限制见 `docs/uf-write-path.md`；本阶段**不替代** stage 8 的真机验收。

# 页面使用：真实设备与演示模式

开发版页面：`pixi install` 后运行 `pixi run dev`，打开 `http://localhost:5173/app/`
（M0 只读探针在 `/probe/`）。安装版 PWA 的完整上手流程见 [quickstart.md](quickstart.md)；
页面结构（镜像 / 设备 / 写入 + 日志抽屉）与校验策略见 [m4b-ui.md](m4b-ui.md)。

## 真实设备

1. 平板进 **MASKROM**（按住恢复键上电或按板子说明短接），USB 接入。**这一轮上电后不要先点任何读取按钮。**
2. 打开 `http://localhost:5173/app/`，在 **设备** 面板点 **连接设备**，选 `0x2207` 设备；卡片会显示
   `Maskrom · DevNo=1`（模式按 `bcdUSB` bit0 判定）。
3. 在 **镜像** 面板选镜像：拖入文件、点 **从文件管理器选择**，或点官方条目的 **下载**。页面读 RKFW 头、
   切出镜像自带的 loader，并自动开始校验。
4. 校验：结构 + PARM CRC 秒级出结论，整镜像 MD5 + RKAF CRC 按本机实测速度决定是否默认做完整校验
   （页面会显示“预计约 X 秒”；超过 2 分钟就默认快速校验并明说“未做完整校验”）。
5. **写入选项** 里逐条列出前置条件：未覆盖尾部必须在下拉框里显式选择 `none / fill / erase-lba` 并打勾。
   全绿后底部操作栏的 **开始刷写** 才可点。
6. 点 **开始刷写**：状态机自己执行 `db → gpt(IDB + 分区表 + parameter 8 份) → wl … → rd`，底部操作栏显示阶段、
   百分比、速度、已用/剩余时间，可取消；失败会给下一步（重试 / 重进 maskrom / 换线换口 / 导出日志）。
   成功后设备复位并重新枚举，**设备** 面板刷新后应显示 `Loader`。
7. 右上角 **日志** 随时打开抽屉：分级、带时间戳，可导出文本或 NDJSON；**诊断** 页签里是命令、传输记录、
   只读信息、loader 覆盖与 PWA 安装。

> loader 由镜像提供，仓库不分发 Rockchip loader blob，也不联网获取。手动指定 loader 的文件只在本机内存里使用。

## 无设备演示

勾选 **设备** 面板底部的 **演示模式（mock 设备）**：

- 自动载入合成镜像与它的 loader，自动生成计划并跑完整校验（校验路径与真机镜像完全相同）；
- mock 设备一开始是 maskrom；`开始刷写` 会走完整的分包/CRC + `db → gpt → wl → rd` 路径，不访问 USB；
- 日志抽屉 → **诊断** 里的 `info` 只有在 mock 变成 LOADER 之后才可用（和真机一致）。

## 离线验证（不碰设备）

`pixi run dev` → 打开 `http://localhost:5173/app/` → 等 `#sw-note` 显示“离线缓存已接管本页” →
DevTools 勾 Offline → 刷新，页面与 `core` 徽标仍然正常，演示模式还能生成计划。安装、缓存项与 zip 直读
细节见 [pwa-offline.md](pwa-offline.md)。

## 驱动与授权

macOS arm64 / Linux / Windows 的 WebUSB 说明见 [webusb-drivers.md](webusb-drivers.md)；Linux 的 udev 规则
可直接装 [`99-rockchip-webusb.rules`](99-rockchip-webusb.rules)。设备重新枚举后授权会失效，需要重新选择或
授权设备；这是浏览器按“来源 + 设备实例”授权的行为，不是页面 bug。

## 相关文档

- 安装版 PWA 全流程（含救砖）：[quickstart.md](quickstart.md)
- 页面结构与校验策略：[m4b-ui.md](m4b-ui.md)
- 安装、离线、zip 直读：[pwa-offline.md](pwa-offline.md)
- 设备模式与命令：[device-commands.md](device-commands.md)

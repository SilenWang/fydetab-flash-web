# 安装、离线与官方 zip 直读（M4）

这一页讲三件事：**把页面装成应用**、**断网还能打开**、**直接选官方 `.zip` 而不是先解压出 3.8 GiB**。
三条都在浏览器里完成，不需要后端，也不需要把镜像上传到任何地方。

## 1. 装成应用

前提：Chromium 内核（Chrome / Edge / ChromeOS / Android Chrome）+ 安全上下文（`https://` 或
`http://localhost`）。Safari/Firefox 可以打开页面，但没有 WebUSB，也不能安装（iOS 16.4+ 的
“添加到主屏幕”只是书签，不提供 worker 之外的能力，仍然无法写设备）。

装法（任选其一）：

1. 页面顶部面板出现 **“安装为应用”** 按钮（Chrome 触发 `beforeinstallprompt` 时才有）→ 点它。
2. 地址栏右侧的 ⊕ / 显示器图标 → “安装”。
3. 菜单 → “投放、保存和共享” → “安装页面为应用…” / Edge 的“应用 → 将此站点安装为应用”。

装好之后：

- 从桌面/开始菜单/应用抽屉启动，窗口是独立的（`display: standalone`），有标题栏与图标。
- **安装不会给你设备权限**：第一次写入仍然要点“授权设备”，权限按来源+设备实例发放，与是否安装无关。
- `chrome://web-apps`（Edge 是 `edge://apps`）里可以看到并卸载。

## 2. 离线：缓存了什么，为什么可以不联网

`web/app/sw.js` 在安装时把运行页面所需的全部文件预缓存（`PRECACHE` 列表，19 项）：

```
./                       ./index.html            ./styles.css
./app.js                 ./worker.js             ./bridge.js
./usb-host.js            ./mock-host.js          ./mock-loader.js
./preflight.js           ./protocol.js           ./zip.js
./zip-source.js          ./manifest.webmanifest  ./icons/*.png (3)
./wasm/rk3588.wasm       ./wasm/wasm_exec.js
```

策略是 **cache-first**（不是 network-first）：写入工具是在“马上要动设备”的时候打开的，等一次网络
往返、或者在没有 Wi-Fi 的实验台上直接打不开，都是更糟的结果。代价是升级在一次启动之后生效：

- 新版本发布后，`install` 用新的 cache 名重新预缓存，`activate` 删掉旧 cache；
- 页面检测到等待中的 worker 后会显示“新版本已缓存：点刷新页面后生效”，不会偷偷换掉正在运行的页面。

`localStorage` / `IndexedDB` 不参与——页面没有保存任何镜像或设备信息；关掉标签页即清空内存中的计划。
缓存在 `chrome://settings/content/all`（站点数据）里可以清除，清除后需要联网打开一次才能重新离线。

### 断网验证（页面里做，不碰设备）

1. `pixi run dev` → 打开 `http://localhost:5173/app/`。
2. 等 `#sw-note` 显示 **“离线：离线缓存已接管本页”**（首次加载后几百毫秒）。
3. DevTools → Network → 勾 **Offline**（或者直接拔网线 / 关 Wi-Fi）。
4. 刷新。页面必须仍然出现：标题、徽标 `core …` 变绿、下方面板显示 `离线（缓存可用）`。
5. 再点一次“刷新（ld）”——会报没有设备（这是对的，WebUSB 与网络无关），但 core 仍然可用；
   勾“演示模式（mock 设备）”能生成计划，说明 wasm 是从缓存里来的。

> 想更接近真实场景：装成 PWA 后**先停掉 `pixi run dev`**，再从桌面图标启动。地址仍然可用是因为
> service worker 在浏览器里拦截了这些请求。
>
> 已经在仓库里自动验证过的部分：`web/app/test/sw.test.mjs` 用假的 Cache Storage 跑 install /
> activate / fetch（含离线、导航回退、错误响应不入缓存）；`web/app/test/pwa.test.mjs` 检查
> `PRECACHE` 里的每一项在磁盘上真实存在、manifest 与图标尺寸一致、app.js 查找的每个元素 id 都在
> `index.html` 里。

## 3. 官方 `.zip` 直读

官方固件包通常是 `RK3588_..._update.zip`，里面是一个 3.8 GiB 的 `update.img`（RKFW 容器）。M4 不再
要求你先解压：

1. “2 · 镜像与 loader” 里选 `.zip`（文件选择器已接受 `.zip`）。
2. 页面从**文件尾部**找 EOCD（必要时读 ZIP64 EOCD 记录），解析中央目录，只读几十 KiB。
3. 依次对候选条目解出**前 4 个字节**，第一个以 `RKFW` 开头的条目就是镜像（裸 `RKAF` 只在没有
   RKFW 时兜底）。厂商包里的 `README.txt`、`MiniLoaderAll.bin`、驱动压缩包都会被跳过，页面会列出
   跳过了哪些、为什么。
4. 该条目被包成与 `File` 完全相同的 `{name, size, read(offset,length)}` 对象交给 Go 核心，核心用
   `DecompressionStream('deflate-raw')` **按需**解压：读到哪解到哪，解出来的字节只保留 8 MiB 窗口。

因此：

- **不落地 3.8 GiB**，也不需要 3.8 GiB 的临时空间；峰值内存是窗口大小 + 单次读的缓冲（几 MiB）。
- **计划与 `.img` 完全相同**：同一份镜像字节，喂给同一个 `ufPlan`。计划的每一项、每个目标 LBA、
  每段未覆盖尾部、loader 的 sha256/LDR 构建日期都一致，只有“镜像显示名”不同
  （`demo.zip → Image/update.img`）。
- 只支持 `stored` 与 `deflate` 两种压缩方式（官方包就是 deflate）；`lzma`/`bzip2` 等条目会被跳过并
  在页面里说明，而不是猜着解。
- ZIP64（> 4 GiB 条目/偏移）已支持，中央目录里的 32 位字段被 `0xFFFFFFFF` 占满时走 ZIP64 扩展。
- **不解压整个包就不校验 zip 的 CRC**；镜像自身的 RKAF CRC / MD5 校验仍然可用（`uf --dry-run`
  默认会算，页面里的 `verify` 目前关闭）。

### 计划对照（怎么自己验一遍）

```bash
# 1) 造一份演示数据：合成 RKFW 镜像 + 包住它的 ZIP64 包（不下载任何厂商 blob）
pixi run sample          # → web/samples/demo-update.img / .zip
pixi run dev             # → http://localhost:5173/app/

# 2) 页面里先选 demo-update.img，记下“镜像/计划”面板的每一项（items、传输字节、未覆盖尾部）
#    再选 demo-update.zip，对比同一块面板：两个计划必须逐字段相同，只有第一行“镜像:”显示包名。
```

自动化的同一件事（用真正的 Go/WASM 核心比较两份计划，不靠肉眼）：

```bash
pixi run test            # web/app/test/uf-zip.test.mjs 里 "a zip package plans exactly like
                         # the .img it contains" 会比较 report/facts/loader 的完整 JSON
```

`web/app/test/uf-zip.test.mjs` 还固定了：逐字节相等、三种 `eraseTail` 下计划一致、
`imageLoader` 取出的 loader sha256 相同、mock 写入结果相同、以及“生成计划只解压了包的一部分”。

## 4. 出错时怎么带着证据来

页面右下“命令”面板里有 **导出日志（NDJSON）**：把 shell 输出、uf 进度、每一次 USB 传输、
自检判定、zip 统计（读了多少、回退重解几次、峰值缓冲）写成一份 `.ndjson` 下载下来。
它的形状与 CLI 的 `uf --log` 一致，可以直接贴到 issue 里。

同时保留了“传输记录”表格（最近 300 条）与“错误”面板；`chrome://device-log` 里的 USB 记录在
驱动问题时更有用，见 `docs/webusb-drivers.md`。

## 5. 已知限制

- iOS / iPadOS 上无法写入（没有 WebUSB），安装只提供离线查看。
- 安装后仍是 `standalone` 窗口，**不支持** `file://` 打开本地 zip——文件选择器需要真实用户手势，
  这个没问题，但页面必须来自 https/localhost。
- 3.8 GiB 镜像在低配机器上解压速度受 CPU 限制（单线程 `DecompressionStream`），界面不会因此卡住
  （解压在 worker 里），但写入前的“生成计划”会有几秒到几十秒。
- 真机写入仍然按 `docs/uf-write-path.md` 与 stage 8 的验收走；本阶段没有替代真机验证。

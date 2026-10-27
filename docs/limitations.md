# 已知限制与待真机复验

## 待真机复验

- **真机端到端未复验**：`db → 重新枚举 → ld/info` 与整段 `uf` 写入需要板子从 USB 掉线中恢复（记录在 issue 里），
  本阶段以 mock + 单测 + Node 集成测试覆盖同一代码路径；真机由 stage 6 负责。
- **`db` 失败分类的证据分布**：`index(0)` 被拒有真机日志（`Boot_VendorRequest471 failed,err=-1`）；
  “中途失败”的分类目前只有单测，需要一次真实中途失败来确认。
- **浏览器 UI 未在真实 Chrome 里点过**（历史状态）：构建环境没有 Chrome/Chromium；`worker.js` 本身（含 RPC
  与 wasm）已用 Node 集成测试跑通，DOM 层（`app.js`）只做了语法检查，`preflight.js` 的规则有单测。
  需要一次人工冒烟。后续的安装版 PWA 实测记录见 [quickstart.md](quickstart.md#本次验收实测2026-10-26)。
- 重新枚举后的授权：按“实例授权失效、需要重新点授权设备”实现（`connect` 事件会自动 `ld`），
  按 VID/PID 策略是否仍生效等真机结论。
- **大镜像读取已实现但未用真机大文件验证**：页面把 `File` 交给 worker，Go 通过 `rkfw.OpenReaderAt` 分块读；
  默认不做全镜像 MD5（页面里 `verify:false`），需要时可打开。
- **IDB 字节模型仍待真机核对**（`rk28-rc4` / `raw-loader`），真机第一次写入时应该先看日志里的
  `gpt` 阶段是否符合预期。

具体的写入侧待确认项（`WRITE_LBA` 上限、CSW 时序、擦除语义、重新枚举时序等）见
[uf-write-path.md §10](uf-write-path.md#10-仍需真机确认的清单)。

## 仍然成立的早期限制

- **ReadFlashInfo 的短读**：rkdeveloptool 对这一条读 512 B 并接受 ≥11 B；本页按 CBW 里声明的 11 B 读取，
  以保持 CSW 不错位。若某台设备实际发更多字节，会落成 `ERR_CMD_NOTMATCH` 并记录，不会顺手多读——
  这正好是要在真机上确认的事实。

## M0b 真机验收清单（历史）

端到端同事在 maskrom 真机上执行：

- [ ] Chrome/Edge 打开 `pixi run dev` 给出的 `http://localhost:5173/`，页面无 JS 报错。
- [ ] 授权 `0x2207` 设备成功，界面显示 VID/PID 与 `mode = MASKROM`。
- [ ] 点“读取设备信息”后，5 条命令的结果都出现在页面上；成功的记录 `dataHex`，失败的记录错误码。
- [ ] 设备描述符里能看到配置 / 接口 / endpoint 的 number、direction、type、packetSize。
- [ ] “导出 JSON”下载的文件包含 `device`、`commands`、`transfers`、`errors` 四段。
- [ ] 通读 `transfers`：除 31 字节 CBW（`bulk-out`、`stage=cbw`）外，没有其他出站数据。
- [ ] 把 JSON 附到 SIL-510，并注明 Chrome 版本、OS、平板型号。

## 相关文档

- 写入侧的待确认清单：[uf-write-path.md §10](uf-write-path.md#10-仍需真机确认的清单)
- 格式核对状态与未确认项：[rkfw-rkaf-format.md §8](rkfw-rkaf-format.md#8-核对状态)
- 页面校验策略与限制：[m4b-ui.md](m4b-ui.md)

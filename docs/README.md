# 文档索引

## 使用者

| 文档 | 内容 |
|---|---|
| [quickstart.md](quickstart.md) | 安装版 PWA 刷 Fydetab Duo：安装、进刷写模式、刷写、失败处理、日志 |
| [usage-web.md](usage-web.md) | 页面使用：一次完整刷写、离线、驱动与授权 |
| [webusb-drivers.md](webusb-drivers.md) | macOS / Linux / Windows 的 WebUSB 与驱动说明 |
| [pwa-offline.md](pwa-offline.md) | 安装、离线缓存、官方 `.zip` 直读、日志导出 |
| [limitations.md](limitations.md) | 已知限制、待真机复验项、历史验收清单 |

## 开发者

| 文档 | 内容 |
|---|---|
| [architecture.md](architecture.md) | 运行时架构、阶段记录、代码地图、目录结构、M0 探针 |
| [development.md](development.md) | pixi 环境与任务、构建产物、测试矩阵、CI/部署 |
| [device-commands.md](device-commands.md) | 设备模式、shell 命令、只读守卫与写入边界、loader 规则 |
| [cli.md](cli.md) | `uf` 命令行：dry-run 计划、mock、trace 回放、日志、镜像片段 |
| [uf-write-path.md](uf-write-path.md) | 写入命令、`uf` 状态机、IDB/擦除假设、mock trace 与日志 schema |

## 协议与格式

| 文档 | 内容 |
|---|---|
| [protocol-notes.md](protocol-notes.md) | rockusb 传输、CBW/CSW、只读命令、Download Boot、loader 错配实测 |
| [rkfw-rkaf-format.md](rkfw-rkaf-format.md) | RKFW / RKAF / parameter 格式、CRC、GPT 计划、核对方法 |
| [m4b-ui.md](m4b-ui.md) | 页面设计：选系统 / 设备 / 日志三块、版本清单、下载与校验策略 |
| [99-rockchip-webusb.rules](99-rockchip-webusb.rules) | Linux udev 规则（可直接安装） |

界面截图在 `docs/images/web-ui/`，6 张（空状态、版本对话框、写入中、完成、暗色、390px 窄屏），
README 与 [usage-web.md](usage-web.md) 都从这里引用。

项目入口见仓库根目录的 [README](../README.md)（英文）与 [README.zh-CN.md](../README.zh-CN.md)（中文）。

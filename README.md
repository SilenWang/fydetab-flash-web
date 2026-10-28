# fydetab-flash-web

在浏览器里复刻 Rockchip `upgrade_tool` 的刷机路径：协议与编排用 Go 编译成 WebAssembly，USB 走 WebUSB，
镜像与设备都不离开本机。

面向 Fydetab Duo（RK3588）。不需要后端、不上传镜像、不安装额外工具：选本地 `.img` 或官方 `.zip`
即可校验并刷写，也可以安装成离线 PWA。当前已实现 M0–M4b（只读探针、Go/WASM 核心、离线计划、
写入状态机、WebUSB 写入与页面门禁、PWA/离线/zip 直读、五区块单页），真机端到端验证仍由后续阶段负责。

## 文档

| 我想…… | 从这里开始 |
|---|---|
| 第一次刷机 | [docs/quickstart.md](docs/quickstart.md) |
| 了解页面怎么用（真机 / 演示模式） | [docs/usage-web.md](docs/usage-web.md) |
| 解决驱动、授权、离线安装问题 | [docs/webusb-drivers.md](docs/webusb-drivers.md)、[docs/pwa-offline.md](docs/pwa-offline.md) |
| 参与开发（环境、构建、测试） | [docs/development.md](docs/development.md) |
| 看架构、代码地图与阶段记录 | [docs/architecture.md](docs/architecture.md) |
| 用 CLI 做离线计划 / mock / 回放 | [docs/cli.md](docs/cli.md) |
| 查协议、镜像格式与写入细节 | [docs/protocol-notes.md](docs/protocol-notes.md)、[docs/rkfw-rkaf-format.md](docs/rkfw-rkaf-format.md)、[docs/uf-write-path.md](docs/uf-write-path.md) |
| 确认已知限制与待验证项 | [docs/limitations.md](docs/limitations.md) |

完整索引见 [docs/README.md](docs/README.md)。

## 参考项目

- [rockchip-linux/rkdeveloptool](https://github.com/rockchip-linux/rkdeveloptool)（GPL-2.0）：命令码、
  CBW/CSW 布局、vendor request、RKBOOT 容器、CRC 与 GPT 常量。
- [TheGammaSqueeze/rk_image_tool](https://github.com/TheGammaSqueeze/rk_image_tool)：parameter 解析、
  IDB 约定与 GPT 几何。
- [suyulin/afptool-rs](https://github.com/suyulin/afptool-rs)：RKAF 尾 CRC、32 位长度回绕与 PARM 包装。
- [WebUSB 规范](https://wicg.github.io/webusb/)：浏览器侧的设备访问模型。

## 许可

本仓库以 **GPL-2.0** 发布，见 [LICENSE](LICENSE)。不分发 `upgrade_tool` 二进制，也不分发 Rockchip loader
blob；loader 默认从用户自己的镜像里切出。

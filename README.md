# displayKvm

displayKvm 用于管理和切换显示器输入源，包含 Go 命令行客户端、本机后台服务和桌面界面。显示器、输入方案、目标、分组、规则和定时任务保存在单一完整设置文件中。

实际控制能力取决于显示器、DDC/CI、驱动和连接链路。构建成功或收到命令回执，不代表显示器画面已经完成切换。

## 组成

- `displaykvm`：命令行客户端。
- `displaykvmd`：本机后台服务。
- 桌面应用：管理配置、查看状态和测试已保存目标。
- `third_party/m1ddc/`：macOS Apple Silicon 随包工具的源码、许可证和来源材料。

## 开始使用

以下 `go run` 示例用于源码开发。macOS 裸 `go run` 不会自动附带 `m1ddc`；程序不在 `.app` 内且未设置高级工具路径覆盖时，会报告缺少依赖。macOS 日常运行请优先构建下方的 `.app` 并使用其内嵌后台。源码高级路径覆盖见[完整使用说明](docs/usage/README.md)。

从源码创建并检查设置文件，按实际设备和校准结果填写配置：

```sh
go run ./cmd/displaykvm config new --file '/absolute/settings.json'
go run ./cmd/displaykvm config check --file '/absolute/settings.json'
```

使用完整设置文件的绝对路径启动后台：

```sh
go run ./cmd/displaykvmd -settings '/absolute/settings.json'
```

保持后台运行，另开终端使用 CLI：

```sh
go run ./cmd/displaykvm list
go run ./cmd/displaykvm status
go run ./cmd/displaykvm --json status
```

macOS Apple Silicon 桌面可在本机构建后启动。先构建并启动包内后台，保持该终端运行：

```sh
sh desktop/build-macos.sh
desktop/bin/DisplayKvm.app/Contents/MacOS/displaykvmd -settings '/absolute/settings.json'
```

另开终端启动桌面：

```sh
desktop/bin/DisplayKvm.app/Contents/MacOS/displaykvm
```

使用候选程序时，macOS 后台位于 `DisplayKvm.app/Contents/MacOS/displaykvmd`，Windows 后台位于解压目录的 `displaykvmd.exe`；同样使用 `-settings` 指定设置文件。后台与桌面应在同一用户环境中运行。

**更新时需同时换用新版后台和桌面。** 关闭主窗口只会隐藏桌面；从托盘显式退出才会结束桌面客户端，且不会停止后台。界面不会自动替换或停止旧后台，请先按原启动方式退出旧程序，再启动新版后台和桌面。

## 五秒输入测试

保存目标后，在本机画面可见时点击“测试已保存目标”。后台读取测试前的真实输入、切到目标、保持五秒，再尝试恢复原输入。计时和恢复由后台完成，页面关闭不会取消恢复责任。

预读失败时不切换；输入相同则无需切换；发现第三个输入值时不会覆盖。只有读回原输入才显示恢复成功。倒计时结束、网络超时或界面断线都不能证明已经恢复。

若切走后 DDC/CI 控制通路断开，或后台被强杀、机器断电，自动恢复可能失败，需要通过显示器菜单手动切回。详见[五秒测试说明](docs/usage/input-preview.md)。

## 平台状态

| 平台 | 当前边界 |
| --- | --- |
| macOS Apple Silicon | 提供 arm64 桌面候选，随包使用固定版本 `m1ddc v1.3.1`；本地桌面构建最低部署版本为 macOS 13。 |
| macOS Intel | 显示器控制后端尚不可用。 |
| Windows amd64 / arm64 | 使用系统原生显示器接口，无需另装显示器控制工具；已有核心及桌面交叉编译候选，最低候选基线为 Windows 10 1803（build 17134）。 |
| Linux amd64 / arm64 | 核心可交叉编译；真实随包工具、依赖及许可载荷尚未完成交付。 |

Windows 仍依赖显示器开启 DDC/CI、链路及驱动允许访问，以及相关本地文件系统能力。WebView2、安装、权限、登录启动和显示器切换等尚未完成 Windows 实机验收。Windows 开机启动管理仍待实现。

候选程序不是正式安装包。macOS 当前使用 ad-hoc 签名，未经公证；真实显示器、桌面 WebView 和非当前输入的 DDC/CI 能力仍需实机验证。完整边界见[兼容说明](docs/compatibility.md)。

## 构建与包装

源码最低 Go 版本为 1.26.10，当前候选构建使用 Go 1.27.3。桌面是独立 Go 模块；macOS 桌面构建还需要 Xcode/Clang 和 CGO。macOS 桌面脚本使用本机工具链（`GOTOOLCHAIN=local`）。

构建并包装核心候选：

```sh
sh build/build-core.sh --target all --version 0.0.1-dev --run-id local-core
sh packaging/package.sh core --run-id local-core
```

构建并包装 macOS 桌面候选：

```sh
sh desktop/build-macos.sh
sh packaging/package.sh desktop \
  --app desktop/bin/DisplayKvm.app \
  --run-id local-desktop
```

构建输出、缓存、原始验证记录和本地配置不纳入 Git。构建或交叉编译通过仅证明程序可构建，不代表目标系统或硬件已运行验证。详细前置条件、输出与参数见[构建说明](build/README.md)和[包装说明](packaging/README.md)。

## 文档与许可

- [完整使用说明](docs/usage/README.md)
- [五秒测试说明](docs/usage/input-preview.md)
- [兼容与交付边界](docs/compatibility.md)
- [贡献指南](CONTRIBUTING.md)

项目代码采用 [MIT License](LICENSE)。Go、Wails、m1ddc 等第三方组件保留各自许可证，随包材料见 `packaging/` 和 `third_party/`。

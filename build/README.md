# 构建准备

构建使用截至2026-11-06核对的最新稳定Go 1.27.3。模块声明的最低Go 1.26.10是源码兼容要求；新增构建入口会检查实际工具链版本。Wails v3的固定版本见[桌面模块清单](../desktop/go.mod)。

核心CLI与后台是独立程序；六个候选目标为darwin、linux、windows各自的amd64与arm64。目标、构建参数和限制见[targets.json](targets.json)。交叉编译只证明产物可构建，运行和硬件兼容须在目标环境验证。

在项目根执行：
```sh
sh build/build-core.sh --target all --version 0.0.1-dev --run-id local-core
sh packaging/package.sh core --run-id local-core
```
归档版本参数只作为工件标签。运行编号独占自己的输出，命令会报告实际位置；已有产物保留，后续构建使用新编号。

桌面目前复用现有macOS入口：
```sh
sh desktop/build-macos.sh
sh packaging/package.sh desktop --app desktop/bin/DisplayKvm.app --run-id local-desktop
```
该构建需要Xcode/Clang与CGO。应用最低macOS 13是部署声明，当前签名为本地ad-hoc签名。其他桌面平台尚无已验证的构建和运行记录。

本地构建使用隔离缓存、GOWORK=off、GOTOOLCHAIN=local、GOPROXY=off、GOSUMDB=off和只读模块模式。依赖须事先按锁定清单准备；缺少缓存时构建应失败，避免隐式下载或改写模块。实际工具链、构建目标、参数和哈希随工件记录。

[手动CI](../.github/workflows/build.yml)使用固定官方动作、Go 1.27.3与当前GA镜像macos-26。工作流可以显式预取锁定依赖，随后调用离线入口；它只在手动触发时执行，没有上传或发布步骤。CI交叉构建不替代目标主机和显示器实测。参阅[包装说明](../packaging/README.md)与[发布检查](../docs/releases.md)。

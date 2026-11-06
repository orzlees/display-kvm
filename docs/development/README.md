# 开发说明

## 模块与执行路径

- [cmd/displaykvm](../../cmd/displaykvm) 与 [internal/cli](../../internal/cli)：命令行解析与请求呈现。
- [cmd/displaykvmd](../../cmd/displaykvmd)：配置加载、本地服务、运行代及管理提交事务。
- [internal/ipc](../../internal/ipc) 和 [internal/model](../../internal/model)：本地通信、认证及协议模型。
- [internal/backend](../../internal/backend)：平台显示器传输边界。
- [internal/automation](../../internal/automation)、[internal/scheduler](../../internal/scheduler)：事件规则与按时区计算的日程。
- [desktop](../../desktop)：独立 Go 模块；公共管理服务连接后台，页面以原生表单维护草稿和提交状态，静态资源由程序内嵌。

桌面使用 Wails v3，固定依赖版本以桌面模块清单为准。最低 Go 版本及固定模块信息见[根 go.mod](../../go.mod)和[桌面 go.mod](../../desktop/go.mod)。不需要 Node 前端开发服务器。

## 数据与兼容边界

当前显示器配置为版本3，包含可选校准数据；规则和日程文档分别为版本1。准确字段和兼容处理以[配置代码](../../internal/config/config.go)、[规则代码](../../internal/automation/rules.go)、[日程代码](../../internal/scheduler/schedules.go)为准。

现存 `docs/contracts.md` 的部分文字和示例仍描述配置版本2，`docs/README.md` 也保留早期规划措辞；它们不能作为当前配置格式或交付状态依据。本说明和使用示例采用当前代码的版本3，不据此增加任何运行时兼容行为。

管理模型采用严格 JSON 解码。新增日程操作通过独立的 `Data` 负载传递，不向旧快照固定增加字段。日程状态子文档使用 `{"items":[...]}` 对象，空列表为 `{"items":[]}`，不能将顶层数组交给只接受对象的解码器。

管理保存复用排空、关闭旧队列、原子文件替换和新运行代构建。文件已保存与运行状态已生效分别报告；失败恢复依据实际文件，不回用已关闭的运行队列。结果未知时按原提交 ID 查询，不能自动重放。后续配置或规则提交应保留当前日程。

## 本地验证与构建

测试入口见[贡献指南](../../CONTRIBUTING.md)。根模块与桌面模块分别运行对应测试；无界面合约测试不能替代真实 GUI 或硬件验收。

macOS 本地构建入口为 [desktop/build-macos.sh](../../desktop/build-macos.sh)，使用本机 Go 工具链（GOTOOLCHAIN=local），要求 Xcode/Clang 与 CGO，会生成 `.app` 并执行本地临时签名。该产物的 macOS 13 构建下限不等于所有设备和链路均已通过验证，也不代表已完成正式签名或公证。

开发时保留可行动的错误和状态区别，不输出私有端点、认证材料或不必要的外部工具原始数据。新增协议字段、持久化结构或默认行为前，应核对现有调用链、历史兼容及失败恢复路径。

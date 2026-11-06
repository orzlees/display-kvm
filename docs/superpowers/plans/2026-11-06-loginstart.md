# 服务与桌面独立登录启动实施计划

> 执行方式：子代理分域实施，独立只读验证者统一测试和验收。用户已批准连续实施，不再请求执行方式确认。项目无Git，不初始化、不提交，也不创建额外进度账本；当前会话待办跟踪执行。

**目标：** 服务与桌面分别登录启动，支持仅后台静默运行；关闭窗口驻留，按需恢复界面。

**架构：** 两个当前用户LaunchAgent由共用`internal/loginstart`管理；独立启动参数文件由CLI与GUI共同配置。GUI只提供操作入口，不承担服务运行条件。登录界面使用Accessory和Hidden，手动界面使用Regular；构建同包提供GUI、daemon、核心CLI。

**技术栈：** 现有Go1.27.3、Wails v3.1.0、Go标准库、macOS用户级LaunchAgent，最低macOS13，不增加依赖。

**Spec：** [已批准设计](../specs/2026-11-06-loginstart-design.md)

## 全局约束

- 两个开关独立、首次均关闭；仅服务时不打开GUI，手动打开不隐式启停服务。
- 保存只安排下次登录，不调用bootstrap、bootout、kickstart、load或unload；不修改业务文件，不自动安装应用。
- 参数文件版本1，配置和工具绝对路径必填，规则与日程可选；不搜PATH，不从旧管理快照猜来源或改旧协议。
- 用户级参数文件0600、新私有目录0700；核对owner、拒链接和无关同名文件；原子保存、锁和读回不得省略错误。
- 未知状态用nil表达，不能当关闭；部分保存和系统查询错误分别返回，不能自动重发副作用。
- 只支持已实现的macOS注册，其它平台明确unsupported；不改系统权限、正式签名策略或全局工具配置。
- 实现者先写行为测试再实现，不执行测试；验证者不改代码。各文件只有一个writer，依赖接口固定后并行。
- 保留旧应用、归档和证据。新macOS产物使用新输出目录；不重跑原根/桌面全集或六目标构建。

## Review Focus

1. 首次没有profile与损坏profile必须不同：A保留os.ErrNotExist链，B禁用错误读取后的覆盖；测试首次GUI单独开启与坏配置拒覆盖。
2. 禁用启动项时旧路径失效：A/B不得重新保存或校验未改profile；测试单项关闭不改变另一项或业务文件。
3. 外部disabled覆盖或命令查询失败：A状态读回不得伪成功，测试精确标签、nil未知和部分结果。
4. 自动登录后用户再次打开：C不得产生双实例或抢前台，测试登录/手动模式分支并记录实际GUI观察边界。
5. 包内daemon与CLI及许可：D构建实际三程序，验证者核BuildInfo、签名、清单和实际链接许可，不把GUI副本当CLI。

## 固定核心契约

`Target`为string，常量`Service="service"`、`Desktop="desktop"`。

`ServiceProfile`字段为`Version int`与`ConfigPath/RulesPath/SchedulesPath/ToolPath string`，JSON使用version、config_path、rules_path、schedules_path、tool_path。

`ItemStatus`字段为`Target Target`、`Present/Enabled/Loaded/Ready *bool`、`AppPath/Message string`。`Status`含`Service/Desktop ItemStatus`、`Profile *ServiceProfile`、`ProfileMessage string`。`Result`含`Saved *bool`与`Status Status`。

`Environment`含`ConfigDir/HomeDir string`与`UID int`；`CommandRunner`为`func(context.Context,string,...string)([]byte,error)`；`Options`含`Environment func()(Environment,error)`及`Run CommandRunner`。

```go
func New(Options) (*Manager, error)
func (m *Manager) Profile(context.Context) (ServiceProfile, error)
func (m *Manager) SaveProfile(context.Context, ServiceProfile) (Result, error)
func (m *Manager) Status(context.Context) (Status, error)
func (m *Manager) Enable(context.Context, Target, string) (Result, error)
func (m *Manager) Disable(context.Context, Target) (Result, error)
func LoadServiceProfile(string) (ServiceProfile, error)
```

Enable最后参数为已安装.app绝对路径。构造和查询不写目录。Profile缺失保留errors.Is(err,os.ErrNotExist)，其它错误不伪装成首次。

参数路径为`os.UserConfigDir()/DisplayKVM/startup.json`。登录项为`~/Library/LaunchAgents/io.github.orzlee.displaykvm.service.plist`和`io.github.orzlee.displaykvm.desktop.plist`，RunAtLoad=true，不配置KeepAlive。启用只写该项并按需launchctl enable清除禁用覆盖，禁用只删除核实本功能所有的对应文件；状态分别读取plist、print-disabled及是否已装载。

稳定应用仅`/Applications/DisplayKvm.app`或当前用户`~/Applications/DisplayKvm.app`；拒绝workspace/AppTranslocation永久注册，不自动复制安装。程序布局为Contents/MacOS下displaykvm、displaykvmd、displaykvm-cli。服务参数`-startup-profile PATH`，界面参数`--login-start`。

## Lane A：核心、CLI与daemon参数

**唯一writer：** 核心实施代理。文件为`internal/loginstart/{loginstart.go,profile.go,launchagent_darwin.go,launchagent_unsupported.go,loginstart_test.go,launchagent_darwin_test.go}`、`internal/cli/{cli.go,loginstart.go,loginstart_test.go}`、`cmd/displaykvmd/{main.go,loginstart_test.go}`。

- [ ] 先写TestLoginStart前缀的行为测试：四组合、私有文件/锁/owner/链接、坏profile、外部状态、部分结果、无进程启停和旧flags兼容。
- [ ] 实现固定契约，优先复用现有原子保存、错误类型和业务加载器。
- [ ] CLI增加startup status、configure --config/--tool/可选--rules/--schedules、enable service|desktop --app PATH、disable service|desktop；沿现有JSON/错误码，不连接daemon；startup拒非空旧IPC参数--config-dir。
- [ ] daemon新增-startup-profile，与显式业务路径flags混用拒绝；LoadServiceProfile只校验参数结构/私有路径，随后走原run及规则缺失退化；SaveProfile才校验业务内容。
- [ ] 静态读回并停写，交接口、文件和测试清单，测试交独立验证者。

## Lane B：启动设置页面与托盘模型

**唯一writer：** designer。写`desktop/views/startup/**`、必要`desktop/tests/startup/**`、`desktop/frontend/index.html`、`desktop/tray/tray.go`及对应定向测试；不写host/main。

**固定消费：** 核心Manager全部方法。页面本地`Backend`提供Read/Save；必要`backend.go`适配核心，不把核心结果映射重复放到host。

```go
func Page(Backend, FilePicker) app.Page
func NewBackend(Manager, string) Backend
func AdaptFilePicker(func(context.Context, string) (string, bool, error)) FilePicker
```

FilePicker类型为`func(context.Context,PathField)(path string,selected bool,err error)`；宿主bool表示canceled，适配必须转换selected=!canceled。kind仅config/tool/rules/schedules。

- [ ] 先写真实handler/adapter行为测试：离线读取、首次/损坏、字段校验、选文件取消、只改指定项、部分结果、草稿保留。
- [ ] 实现/startup/普通GET/POST表单，两个开关和四路径统一编辑，只有保存才提交；无额外管理协议或细API要求。
- [ ] 增加底部“启动设置”链接及托盘“启动设置…”动作，不另加并行快捷开关或偏好体系。
- [ ] 注入核心adapter；禁用不重写未改坏profile，首次仅桌面项可用；页面保留分项读回/错误，不声称多步全局原子成功。
- [ ] 交准确Page、Picker、托盘动作签名并停写，不执行测试。

## Lane C：宿主生命周期与接线

**唯一writer：** 宿主fixer。写`desktop/app/host/**`中必要生命周期、StartupBridge和定向测试，以及`desktop/main.go`；不写tray/startup页面。

**消费：** Lane A Manager和Lane B Page/NewBackend/AdaptFilePicker。宿主不导入views/startup；main负责组装。

```go
func NewStartupBridge() *StartupBridge
func (b *StartupBridge) PickFile(context.Context, string) (string, bool, error)
```

桥进入原生主线程对话框；未绑定或已退出报错，取消不改草稿。AppPath由自身可执行文件定位，仅供核心验证，不自动安装。

- [ ] 先写生命周期行为测试：关闭取消与隐藏、真实Quit旁路、登录/手动模式、手动二次打开恢复、二次登录请求不打扰。
- [ ] 使用WindowClosing同步RegisterHook执行Cancel+Hide，取消最后窗口关闭即退出；显式Quit先标记真实退出。
- [ ] 手动Regular+显示；--login-start Accessory+Hidden，禁止主动activate/Show/Focus。使用Wails真实reopen和SingleInstanceOptions恢复同一窗口。
- [ ] main组装Manager、页面和picker；托盘打开同窗口/startup/，不生成第二窗口/启动daemon。
- [ ] 静态读回、交签名并停写；实际托盘可见性与不抢前台仍由后续有限GUI观察核实。

## Lane D：构建、包装和使用说明

**唯一writer：** 构建fixer。写`desktop/build-macos.sh`、`packaging/notices/main.go`的桌面多程序必要部分及有意义定向测试、`docs/usage/README.md`、`docs/releases.md`。packaging/package.sh已有--app入口，无证据不改；六核心包逻辑不变。

- [ ] 构建脚本新增严格--output PATH，新目标拒覆盖，本次候选为`build/out/loginstart-20261106/DisplayKvm.app`。
- [ ] 同包构建真实GUI、daemon、核心CLI；辅助程序分别签名再签整体，production/CGO1/本机Go和最低macOS边界沿现有约定。
- [ ] 包装逐程序核BuildInfo、模块来源、Go/第三方许可和清单；保持已有真实路径、限定本地替换与Module.Error拒绝边界。
- [ ] 更新使用说明和CLI例子，明确稳定安装、延迟生效、静默恢复与其它平台限制；不把未测登录写成已通过。
- [ ] 静态读回停写，不运行脚本语法检查、构建或包装。

## 独立验证与收尾

**owner：** 独立fast-generic验证会话。模块缓存、Go构建缓存、临时目录及GOPATH使用项目隔离目录；GOWORK=off、GOTOOLCHAIN=local、GOPROXY=off、GOSUMDB=off、CGO_ENABLED=1。不改变用户登录项或权限，不读用户业务内容。完整命令、输出和真实exit保存在`docs/verification/2026-11-06-loginstart/`。

- [ ] 全部writer停写后检查实际范围和接口；从现有baseline对照，不以实施者摘要代替实际文件。
- [ ] 一次根模块定向回归：`go test -race -mod=readonly -count=1 ./internal/loginstart ./internal/cli ./cmd/displaykvmd ./packaging/notices`；只在实际新增包存在时包括对应路径。
- [ ] 一次桌面受影响包回归：`go -C desktop test -race -mod=readonly -count=1 ./views/startup ./tray ./app/host ./tests/overview`，包含实际新增tests/startup包时一并执行。
- [ ] 仅相关脚本语法和格式核查；通过后一次`sh desktop/build-macos.sh --output <候选绝对路径>`，保留旧desktop/bin产物。
- [ ] 一次`sh packaging/package.sh desktop --run-id loginstart-review-20261106 --app <候选绝对路径>`；使用完整可信离线缓存，核三程序/签名/清单/归档哈希/许可。
- [ ] 检查CLI可不连接daemon读取帮助、测试使用隔离目录与fake runner；不在真实Home自动enable/disable。
- [ ] 可安全观察候选GUI手动显示、关闭隐藏、再次打开、显式退出及--login-start无窗口/不激活；无截图或辅助功能权限时只记录可证实状态，不申请权限或抓全屏。
- [ ] 真实注销登录、重启、系统背景项拦截、真实硬件和其它平台缺证如实保留。新功能验收不抹掉旧24未完成项。
- [ ] 授权内失败由原writer定点修复，独立验证仅重跑受影响项，最多两轮补做；成功不重跑。
- [ ] 按设计R1—R8逐项列已验证/部分完成/受阻、实际证据及残留项；机械文档writer读回链接与范围，不再重验产物。

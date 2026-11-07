# 使用说明
新增[输入源五秒测试](input-preview.md)：保存目标后从本机画面发起后台五秒往返。必须同时使用新版后台与新版桌面，界面不会自动替换后台。候选与验证见[本轮记录](../verification/2026-11-07-input-preview/summary.md)。


## 设置

在桌面菜单中打开“设置”。配置文件保存设备、分组、规则、定时任务、语言及必要运行设置。可新建、加载或另存完整配置文件；不再支持旧多文件导入。

点击“保存”会提交整个设置页：先确认配置文件写入，再保存文件选择，分别处理“开机启动服务”和“开机启动桌面客户端”。语言可选“跟随系统”“中文”“English”，默认跟随系统；保存成功后当前界面、托盘和应用提供的对话框文案同步更新。系统文件面板中由操作系统提供的侧栏等文案仍由系统控制。

保存不会立即启动、停止或重启当前服务。加载另一文件也不会切换已连接后台正在编辑的业务数据。语言可以立即更新，这不代表当前后台已经换用另一配置。

macOS arm64客户端随包提供固定版本m1ddc，并按应用实际位置自动定位，无需另行下载或填写路径。高级CLI仍可显式覆盖工具路径；解析后的安装绝对路径不会自动写回文档。

高级命令示例：

```sh
displaykvm config new --file /absolute/settings.json
```

```sh
displaykvm config check --file /absolute/settings.json
```

```sh
displaykvm startup configure --config /absolute/settings.json
```

```sh
displaykvmd -settings /absolute/settings.json
```

```sh
displaykvm config set-tool --file /absolute/settings.json --tool /absolute/control-program
```

配置被服务持有时，离线整体写入会报占用；可以另存到不同文件。覆盖已有有效完整文档仍需明确确认和版本核验。config set-tool只供高级覆盖，不是普通用户初始化步骤。

`displaykvmd -settings` 是显式手动启动服务的命令，不是配置操作的自动副作用。macOS应用包中的命令行程序名为 `displaykvm-cli`，后台程序名为 `displaykvmd`，均位于应用的 `Contents/MacOS`。

macOS写者会在配置旁创建私有隐藏目录 `.displaykvm-settings-locks`，无需在界面中管理。只读加载不会创建它。锁协调使用同一布局的产品写者，不承诺阻止外部编辑器在最后检查后替换文件。

源码命令在项目根目录执行；Windows便携程序命令在所选程序目录执行。大写路径和标识符是占位值；请替换成已确认的本地数据。先阅读[兼容表](../compatibility.md)，不要把输入代码或设备枚举结果当作通用兼容保证。

## 准备配置

完整配置文件中的 `configuration` 部分版本为3，最小结构如下。以下片段不能单独作为完整配置文件加载；完整文件应由“设置”或 `config new` 创建。空配置可用于格式检查，但没有可切换的显示器或目标。

```json
{"version":3,"displays":[],"input_schemes":[],"targets":[],"groups":[]}
```

显示器、输入方案、目标和分组应依据实际设备填写，可通过桌面管理页维护。输入值需来自设备能力与校准确认，不能直接套用其他型号的值。当前字段定义以[配置实现](../../internal/config/config.go)为准。

规则和日程分别保存在完整文档的 `rules` 和 `schedules` 列表中，可在相应页面维护，不再单独准备文件或传入路径。

保留文件备份。使用 `config check --file /absolute/settings.json` 检查完整文件，不将内部片段作为用户配置文件传入。

## 启动后台和连接

macOS arm64客户端自动使用随包m1ddc，不搜索PATH。源码启动如需工具覆盖，使用上文的高级 `config set-tool` 命令。rc12 已生成 Linux amd64/arm64 的 ddcutil、busctl、udevadm 随包载荷，并通过构建、依赖闭包、来源和许可发布门禁。生成基线为 Debian 12 / glibc 2.36；发布包尚未实机运行，不代表所有发行版或显示器兼容。宿主仍需 I2C 设备权限及 D-Bus/logind/udev 环境，工具不会自动安装服务或修改权限。见[兼容表](../compatibility.md)。

```sh
go run ./cmd/displaykvm config check --file /absolute/settings.json
go run ./cmd/displaykvmd -settings /absolute/settings.json
```

Windows已加入原生实现，可读取显示器当前输入并按配置指定值切换，无需另装显示控制工具；显示器须开启DDC/CI，链路与驱动须允许访问。原生后端、工具就绪解析器、IPC及设置写锁已完成独立静态审查，无待处理关键问题；主机七包回归、六份Windows双架构专测编译、三入口双架构共六个程序构建及带许可候选ZIP核验通过，exe均未运行。最终ZIP及完整哈希见[本轮摘要](../verification/2026-11-07-windows-display/summary.md)，手动使用见[候选说明](windows-candidate.md)，设计边界见[本轮设计](../superpowers/specs/2026-11-07-windows-display-design.md)与[实施计划](../superpowers/plans/2026-11-07-windows-display.md)。这些结果不代表Windows真实运行通过，候选程序集不是完整安装包。

本次接入的整体候选最低正式基线为Windows 10 1803（OS build 17134）及以后；本地文件系统必须支持目录大小写规则查询及端点扩展替换，仅本地磁盘不保证支持，能力不足时拒绝，不静默降级。Windows各版本、驱动及真实系统行为仍未实机验证。

Windows桌面当前不会自动启动相邻后台或CLI。先在PowerShell中手动启动后台，将参数替换为已创建的完整配置文件绝对路径：

```powershell
.\displaykvmd.exe -settings 'C:\Users\USERNAME\Documents\DisplayKvm\settings.json'
```

保持后台运行，再从另一个PowerShell窗口启动桌面：

```powershell
.\DisplayKvm.exe
```

桌面通过系统用户配置目录（`os.UserConfigDir`）发现后台的本机连接端点，双方须使用同一用户及配置环境；程序无需放在同一目录。Windows文件名大小写不敏感，桌面与CLI不能仅靠大小写区分后放在同一目录，推荐布局见[便携包装](../../packaging/README.md)。

Windows启动会核验设置写锁、用户私有运行目录和单实例锁；已有权限过宽、所有者不匹配、重解析路径或无法核验的路径会被拒绝，不自动修改用户已有权限。Windows 已实现当前用户登录后启动，后台与桌面登录项须分别配置；它不是登录前系统服务，桌面也不会自动启动后台。发布包的实际登录、系统和硬件行为仍待实机验证。手动启动后台和桌面.exe不等于完整安装包，WebView2及Windows桌面真实运行尚未验证。

规则和日程从同一完整配置文件加载，不使用时保留空列表。后台不自动安装为系统服务，也不自动启动桌面程序。桌面和 CLI 应使用同一用户连接后台。

macOS还可在“设置”中保存服务使用的完整配置文件位置。内部入口 `-startup-profile PATH` 读取已保存的完整文件选择，与 `-settings` 互斥；无需手工编辑内部启动档案。

后台只监听本机回环地址的随机端口，并将带随机令牌的端点写入用户私有运行目录。macOS/Linux 要求该目录归当前用户所有，目录权限0700、私有文件权限0600。不要分享端点或令牌。CLI 的全局 `--config-dir DIR` 用于客户端连接配置目录，不是后台 `-settings` 完整配置文件路径。

## CLI 操作

```sh
go run ./cmd/displaykvm list
go run ./cmd/displaykvm --json status
go run ./cmd/displaykvm diagnose --display DISPLAY_ID
go run ./cmd/displaykvm switch --target TARGET_ID --display DISPLAY_ID --dry-run
```

`switch` 支持重复的 `--display`，或使用 `--group GROUP_ID`；`--timeout-ms N` 设置请求等待时间。确认计划和目标后，才按需要移除 `--dry-run` 发起真实切换。请求已发送后取消等待不会撤回物理动作。结果不明时先查询状态和排查，不盲目重复切换。

`status` 中的输入观测可能为 `unknown`。命令发送回执、输入值读回和主机实际显示画面是不同证据，不能相互替代。

## 桌面管理与校准

按[项目入口](../../README.md)构建并启动桌面应用。概览和业务管理功能需要连接后台；“设置”在后台离线时也可打开。概览用于查看状态和手动切换；设备、输入方案、目标、规则及定时页面用于相应配置维护。

校准应明确选择设备和输入方案，先查看能力，再按校准流程确认候选值。CLI 提供 `calibrate begin/prepare/apply/query` 和 `calibrate trial begin/step/stop`。试切换会操作真实设备，操作前应确认可通过显示器物理菜单恢复输入。

提交结果未知时按原提交 ID 查询，不重新应用：

```sh
go run ./cmd/displaykvm calibrate query --commit COMMIT_ID
```

## macOS登录启动与桌面驻留

服务和桌面是两个独立的当前用户 LaunchAgent（登录任务），首次均关闭，只在该用户登录后运行。它们不提供未登录时的系统服务。Windows 已实现当前用户登录后启动，后台与桌面登录项须分别配置；它不是登录前系统服务。各平台的前置条件与验证边界见[兼容性说明](../compatibility.md)。

| 开机启动服务 | 开机启动桌面客户端 | 下次登录的安排 |
| --- | --- | --- |
| 关闭 | 关闭 | 两者均不自动启动 |
| 开启 | 关闭 | 仅后台运行，桌面按需打开 |
| 关闭 | 开启 | 仅桌面静默驻留，后台需要另行启动 |
| 开启 | 开启 | 系统分别启动后台和静默桌面 |

从主窗口或托盘进入“设置”，选择完整配置文件，设置“开机启动服务”和“开机启动桌面客户端”，再点击“保存”。启用服务时会验证完整配置和随包工具是否就绪，普通页面无需填写工具或独立规则、日程路径。读取失败会显示错误，保存失败保留草稿；配置写入、文件选择和两个登录任务的结果分别反馈，结果未知时先重新读取。

登记登录任务前，请自行将新候选包的 `DisplayKvm.app` 放到 `/Applications/DisplayKvm.app` 或 `~/Applications/DisplayKvm.app`。应用不会自动安装自身，工作区构建目录和 AppTranslocation（系统临时转移应用的位置）不能登记为持久登录项。macOS arm64应用包在 `Contents/MacOS` 下需要桌面 `displaykvm`、后台 `displaykvmd`、核心 CLI `displaykvm-cli` 及随包控制工具 `m1ddc`；旧的单程序应用不能代替它们。

CLI与页面共用核心登录启动能力，以下是各操作的独立示例，不表示当前后台运行状态：

```sh
cli=/Applications/DisplayKvm.app/Contents/MacOS/displaykvm-cli
"$cli" startup status
"$cli" startup configure --config /absolute/settings.json
"$cli" startup enable service --app /Applications/DisplayKvm.app
"$cli" startup enable desktop --app /Applications/DisplayKvm.app
"$cli" startup disable service
"$cli" startup disable desktop
```

`configure` 只接收完整配置文件位置。`--app` 也可使用展开后的用户应用目录绝对路径。全局 `--json` 放在 `startup` 前，例如 `"$cli" --json startup status`；`startup` 不接受非空的全局 `--config-dir`，因为它不使用后台连接目录。`configure` 将完整文件选择保存到当前用户配置目录中的 `DisplayKVM/startup.json`；无需手工复制或编辑这个私有参数文件。服务登录任务引用包内后台和该文件，桌面登录任务引用包内桌面及 `--login-start`。

页面“保存”会写入完整配置文件；页面保存及CLI的 `configure`、`enable`、`disable` 都不启动、停止或重启当前进程，登录安排在下次登录生效。上述CLI登录安排操作不修改业务文档内容。关闭一项不移除另一项的任务或业务文件；已运行的后台不会因关闭开机启动服务而停止。查看当前后台运行情况仍使用普通 `status`。

手动打开应用时显示正常窗口和 Dock 图标；登录自动打开时使用隐藏窗口及 Accessory（不显示 Dock 图标）模式，不抢前台。关闭主窗口会隐藏并驻留，通过托盘恢复入口或在 Finder 再次打开应用可恢复窗口。显式退出只结束桌面，不停止后台，也不改变已保存的开机启动安排。托盘对象成功创建不能替代实际菜单栏可见性检查。

实际注销、重启、登录后的恢复和系统权限拦截仍需人工验收；这些使用约定不表示已取得真实登录、权限或硬件证据。

## 事件与规则

完整文档中 `rules` 部分的类型见[规则实现](../../internal/automation/rules.go)。可用事件取决于后台实际报告的平台能力；未提供的电源、会话或设备事件不能视为已支持。

```sh
go run ./cmd/displaykvm event emit --name EVENT_NAME --attribute KEY=VALUE
```

请使用规则中约定的事件名与属性。`accepted` 只表示事件接收，不保证规则命中或显示器已切换。

## 定时日程

后台从完整配置文件的 `schedules` 部分加载日程，可在定时页面创建、编辑或启停。填写唯一标识、目标、每日 `HH:MM` 时间和明确的 IANA 时区或 `UTC`；保存前处理页面给出的校验错误。取消草稿不会提交；启停同样需要由后台合并并持久化完整文档，保留其它部分。

下次触发时间来自后台真实状态，草稿不代表已生效日程。错过策略固定为 `skip`，不补跑错过任务；恢复事件来源不可用时，页面会说明跳过保证的限制。

完整配置文件不可写或连接到不支持编辑的后台时，页面不能保存。查看保存结果时区分“已落盘”和“已生效”；结果未知时只查询原提交，不重复提交。新建首个日程、空列表和全部停用状态仍可编辑。

## 停止与恢复

先停止后台，再备份或调整配置。完整配置可用 `config check --file /absolute/settings.json` 只读检查；修复完整配置文件中无效的规则/日程后重新启动。文件恢复失败时后台会停止相关运行能力，不应把旧状态当作成功恢复。详见[故障排查](../troubleshooting.md)。

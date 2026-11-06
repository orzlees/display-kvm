# 单一完整配置实施计划

依据：[已批准设计](../specs/2026-11-06-unified-settings-design.md)。用户已批准U1—U8；旧CLI显式写v1兼容例外已固定。父主会话ses_e5298d591ffe38RnrkdRHwwEOq仅编排，五个独占实施域及独立验证者执行文件操作。项目无Git/CodeGraph，不初始化、不提交，不新增第三方依赖或任务账本。

## 需求、依赖与验收

U1完整文档包含业务配置、规则、日程和工具；U2新建/加载/另存为及独立自启；U3仅选择下次启动，当前页面继续编辑当前服务；U4后台同文件分区原子保存；U5旧格式及命令兼容、显式导入不改原件；U6取消、草稿、焦点、滚动、版本和未知项保护；U7实际词值解析兼容；U8独立定向回归、真实原生操作及新候选归档，保留原产物和人工/硬件缺口。

修改前基线：docs/verification/2026-11-06-unified-settings/baseline.json，50个文件逐项哈希一致；internal/settings原本不存在。U7两文件修复已独立通过91pass/1skip，但当前旧应用未包含；profile变更后定向整包回归覆盖整合。

步骤：先固定本计划；S实现完整文档与存储，P实现profile；D实现daemon/CLI，U实现启动页，H实现宿主桥。公共签名已固定，五域可并行写入，消费者等待生产者交付后接线自检。全部停写后独立核对实际差异与U1—U8，执行一次受影响包回归和真实Wails操作。失败仅返修关联域并复验受影响证据；最终构建新候选和归档。没有证据的实机事项保留未完成。

## 固定完整文档

```json
{"kind":"displaykvm-settings","version":1,"configuration":{"version":3,"displays":[],"input_schemes":[],"targets":[],"groups":[]},"rules":[],"schedules":[],"runtime":{"tool_path":"","busctl_path":"","udevadm_path":""}}
```

configuration保留完整原配置及可选校准；runtime后两个字段可省略。规则、日程均为现有typed列表，不再引用独立业务文件。外部工具程序本身仍为可执行文件，位置作为设置内容，不搜索PATH。

## S：完整文档与存储

独占新增internal/settings/{document.go,import.go,store.go,atomic.go,lock_unix.go,lock_unsupported.go,document_test.go,store_test.go}；可以在该新包增加必要直接相关测试，不能改消费者文件。document聚合现有类型，避免config反向导入规则/日程造成循环；import负责明确旧来源；store负责版本、写者生命周期及锁；atomic机械承接现有daemon持久化算法；平台文件隔离锁的编译边界。不得复制另一套原子算法，daemon后续保留薄适配及原注入点。

```go
const Kind = "displaykvm-settings"
const Version = 1
const MaxBytes = config.MaxConfigBytes
type RuntimeSettings struct {
    ToolPath string `json:"tool_path"`
    BusctlPath string `json:"busctl_path,omitempty"`
    UdevadmPath string `json:"udevadm_path,omitempty"`
}
type Document struct {
    Kind string `json:"kind"`
    Version int `json:"version"`
    Configuration config.Config `json:"configuration"`
    Rules []automation.Rule `json:"rules"`
    Schedules []scheduler.Schedule `json:"schedules"`
    Runtime RuntimeSettings `json:"runtime"`
}
type LegacySources struct { ConfigPath, RulesPath, SchedulesPath string; Runtime RuntimeSettings }
func NewDocument() Document
func Load(io.Reader) (Document, error)
func Encode(io.Writer, Document) error
func (d Document) Validate() error
func (d Document) ValidateForService() error
func ImportLegacy(context.Context, LegacySources) (Document, error)
type Revision string
type Snapshot struct { Path string; Document Document; Revision Revision; Legacy bool }
type SaveResult struct { Saved *bool; Snapshot *Snapshot }
type Environment struct { ConfigDir string; UID int }
type Options struct { Environment func() (Environment, error) }
func NewStore(Options) (*Store, error)
func (s *Store) ReadFile(string) (Snapshot, error)
func (s *Store) OpenWriter(context.Context, string) (*Writer, error)
func (w *Writer) Read() (Snapshot, error)
func (w *Writer) Save(context.Context, Document, Revision) (SaveResult, error)
func (w *Writer) Close() error
type PersistenceResult struct { Saved *bool; Err error }
func ReadManagedFile(string) ([]byte, bool, error)
func SaveManagedFile(context.Context, string, []byte) PersistenceResult
func SaveManagedFileWith(context.Context, string, []byte, func(context.Context, string, []byte) error) PersistenceResult
func AtomicManagedWrite(context.Context, string, []byte) error
```

NewDocument生成合法v3、非null空列表及空工具路径。Load仅接完整文档，严格拒绝未知字段、重复键、错误kind/version及缺失/null分区；总上限1MiB，复用既有校验。Validate允许空工具以支持初次保存，非空路径须绝对路径；ValidateForService进一步验证必要工具是普通可执行文件。ReadFile可读取旧v1/2/3业务配置，经config.Load迁移内存，Legacy=true且只能显式导入另存。ImportLegacy只读取明确来源；指定缺失报错，未指定不猜测或搜索，调用者如实列出未导入部分。

Revision为原始字节SHA256，空值只代表确认目标不存在。NewStore不创建目录；普通配置无需私有目录权限，拒末级链接/非普通文件并核对打开前后身份。OpenWriter规范化绝对路径并排他加锁；Darwin使用配置父目录内的私有隐藏目录 `.displaykvm-settings-locks`，锁文件basename与目标一致，由同一文件系统处理大小写及Unicode等价，不转小写或哈希。隐藏目录须当前用户所有、0700、非链接且与目标父目录同设备；锁文件仍为当前用户所有、0600、普通单链接文件。只读加载不创建该目录，配置父目录不强制私有或自动chmod。Linux继续使用内部DisplayKVM/settings-locks路径摘要锁。写入拒符号链接和硬链接；其它平台可编译并明确报告不支持。

daemon从OpenWriter、Read、ValidateForService到退出Close持同一个Writer；每次Apply合并后Save。离线调用短持锁；Writer.Read/Save只取自己的互斥锁，不能再次取文件锁。Save先比较预期Revision，再原子替换并读回判Saved=true/false/unknown；锁保护遵守协议的产品写者，不承诺对外部编辑器的绝对原子比较交换。已有旧格式、损坏、未知格式不能覆盖升级。新建和另存默认拒已有目标；明确覆盖仅限读得有效完整文档且预期版本一致。

先写TestUnifiedSettings前缀行为测试：严格格式和默认、复用校验、旧导入不改源、其它分区保留、陈旧版本、锁冲突、不自锁、链接和文件身份、失败与未知写入结果。实施者不运行测试，交独立验证者。

审查补做S-Alias：macOS大小写及Unicode等价路径须共享同一写锁，包括目标尚不存在、已存在以及多次原子Rename后；同目录另一配置仍可独立保存。测试先探测临时文件系统等价性，不具备时仅跳过对应平台场景；通用锁路径保真测试不能跳过。修正等待首轮根测试自然完成后实施，保留首轮证据，仅对受影响用例补测。

## P：启动档案

独占internal/loginstart/{loginstart.go,profile.go,profile_settings_test.go}，不改launchagent_darwin解析文件。消费S，保留ServiceProfile类型字段Version/ConfigPath/RulesPath/SchedulesPath/ToolPath和现有LoadServiceProfile、Manager.Profile/SaveProfile/Status/Enable/Disable签名。磁盘v2严格为：

```json
{"version":2,"config_path":"/absolute/settings.json"}
```

v2拒绝旧路径键，即便为空。v1读取与显式旧startup configure写入保留；新GUI只写v2。SaveProfile仅保存启动选择，验证服务就绪但不启停当前进程；Enable服务再次核验工具，Disable不依赖配置有效。空工具文档可保存但不可保存为服务启动选择或开启服务登录。补TestUnifiedSettings档案严格编码、v1兼容、v2就绪与未知状态边界。

## D：daemon及CLI

独占cmd/displaykvmd/{main.go,runtime.go,persistence.go,settings.go,unified_settings_test.go}和internal/cli/{cli.go,commands.go,loginstart.go,settings.go,unified_settings_test.go}；消费S/P。main/options新增settingsPath，新入口displaykvmd -settings /absolute/settings.json。按flag是否显式出现分流，显式空值也不能混用：settings、startup-profile、旧config/rules/schedules/tool/busctl/udevadm三类入口互斥。profilev2映射settingsPath；v1映射旧四路径。旧flags语义不变。

统一模式持Writer及完整Snapshot，复用唯一Prepare/Apply/运行代管理循环；候选只替换本次configuration、rules或schedules，相邻部分与runtime保留，一次Save。规则/日程Writable=true。失败恢复只重读同一完整文件，运行工具变化无法由旧后端表示则明确失败，不拼接启动旧副本、不静默回滚/自动重发。旧多文件分支不变。旧persistence保留注入与兼容函数，通过settings公共原子函数薄适配。

CLI固定：

```text
displaykvm config check --file FILE
displaykvm config new --file FILE [--tool PATH] [--overwrite]
displaykvm config import --config OLD --output NEW [--rules PATH] [--schedules PATH] [--tool PATH] [--overwrite]
displaykvm config set-tool --file COMPLETE_FILE --tool PATH
displaykvm startup configure --config COMPLETE_FILE
displaykvm startup configure --config OLD --tool PATH [--rules PATH] [--schedules PATH]
displaykvm startup status
displaykvm startup enable service|desktop --app APP
displaykvm startup disable service|desktop
```

旧专属参数显式出现保持原v1写行为；仅config且为完整文档写v2。完整文档与旧参数混用拒绝，不猜格式降级。new/import默认拒已有目标；overwrite受锁与Revision限制，不能覆盖导入源。set-tool短持锁只改Runtime.ToolPath。配置命令不自动选择启动配置或启停服务。

补TestUnifiedSettings：同文件各section保存互不覆盖、运行代/失败恢复、旧flags和v1写兼容、v2和混用拒绝、CLI操作闭环与导入原件保护。

## U：启动页

独占desktop/views/startup/{backend.go,handler.go,startup.html,assets/startup.js,assets/startup.css}及desktop/tests/startup/startup_test.go。designer独占布局交互；复用现有页面和原位请求，不改共享壳或其它业务页。消费S/P，公共接口：

```go
type LoginState struct { Supported, Known, Enabled bool; Ready *bool; Error string }
type State struct {
    ConfigPath string; ConfigKnown, ProfileExists bool; ConfigError string
    LegacyProfile *loginstart.ServiceProfile
    Service, Desktop LoginState
    Version string
}
type SaveRequest struct { ConfigPath *string; ServiceEnabled, DesktopEnabled *bool; Expected State }
type Outcome struct { Attempted, Succeeded, Unknown bool; Message string }
type Problem struct { Field, Message string }
type SaveResult struct { Configuration, Service, Desktop Outcome; Problems []Problem }
type Backend interface {
    Read(context.Context) (State, error)
    Save(context.Context, SaveRequest) (SaveResult, error)
    ReadDocument(context.Context, string) (settings.Snapshot, error)
    SaveDocument(context.Context, string, settings.Document, settings.Revision) (settings.SaveResult, error)
    ImportLegacy(context.Context, settings.LegacySources) (settings.Document, error)
}
func NewBackend(manager Manager, files *settings.Store, appPath string) Backend
type FileDialogs struct {
    Open func(context.Context, string) (string, bool, error)
    Save func(context.Context, string) (string, bool, error)
}
func Page(Backend, FileDialogs) app.Page
func NewHandler(Backend, FileDialogs) http.Handler
```

Manager为现有Profile/SaveProfile/Status/Enable/Disable五方法接口。配置文档写入和启动选择保存分开报告；新建生成草稿并提供保存流程，默认建议UserConfigDir/DisplayKVM/settings.json；加载只读草稿，保存选择后才安排下次服务启动；新建/另存只有完整写入成功才能更新文件位置。另存来源为当前草稿，已有有效完整目标须明确覆盖确认；未知/坏目标不覆盖。旧文档显式导入另存，不覆盖原件，可用LegacyProfile给出已知来源。工具在文档‘运行工具’区选择写入，不回到四路径主界面。

当前服务位置无可靠协议来源则如实未知，业务页继续编辑已连接daemon。保两个独立登录开关。取消和失败保滚动/焦点/草稿；继续getAttribute("action")避免命名按钮遮蔽。保请求版本、受控409、取消不重发、部分未知保护。补TestUnifiedSettings首次流程、加载坏文件、另存/覆盖确认/取消、连续动作、版本和零即时启停。

## H：宿主桥

独占desktop/app/host/{startup.go,startup_test.go}及desktop/main.go，消费U/S。保现有PickFile，新增SaveFile；原生Open/Save共用选择占用门禁：

```go
func (b *StartupBridge) PickFile(context.Context, string) (path string, canceled bool, err error)
func (b *StartupBridge) SaveFile(context.Context, string) (path string, canceled bool, err error)
```

PickFile kind为config/tool/rules/schedules，后两项仅旧导入。SaveFile第二参为建议保存的绝对路径，只用于完整配置。复用已安装Wails能力，不新增依赖；调用API前核对当前文档/本地依赖源码，不凭旧版本记忆。main组装settings.Store、NewBackend、FileDialogs。机械接线不改窗口/单实例/托盘/关闭驻留逻辑。补TestUnifiedSettings保存与取消、错误、并发选择门禁和接线断言。

## 独立验证与收口

所有writer先行为测试再实现，仅patch、精确文件gofmt和静态读回，不运行测试/构建/GUI/真实launchctl或用户业务配置操作。各自交实际范围、必要新函数、证据行号、风险、停写声明和token口径。API需变更由父协调生产/消费者，不能各自改签名。

全部停写后独立只读验收者按原始需求、spec、计划和baseline审查实际差异，不以实施者摘要代替证据。独立命令执行者在既有Go1.27.3、离线缓存、CGO1、GOWORKoff/GOTOOLCHAINlocal环境运行；不得改HOME/模块/依赖：

```sh
go test -race -mod=readonly -count=1 -json ./internal/settings ./internal/loginstart ./internal/cli ./cmd/displaykvmd
go -C desktop test -race -mod=readonly -count=1 -json -tags production ./views/startup ./tests/startup ./app/host ./tests/overview
```

不再全仓/六目标重复测试；这是本次受影响包整合回归，测试确切argv/exit/pass/skip/fail及完整日志存docs/verification/2026-11-06-unified-settings/，核模块哈希和范围。随后复用docs/verification/2026-11-06-loginstart/native-picker-diagnostic/真实Wails夹具验证新建、加载、另存、覆盖、取消、连续版本、滚动/草稿与红字读取；不得仅用Node VM替代真实WebView。

新候选输出build/out/unified-settings-20261106/DisplayKvm.app，若已存在选新后缀而不覆盖。构建和包装各一次并核三程序身份、签名、归档及旧产物保护。不得注册真实登录项、注销重启、启停既有daemon或改真实业务文件。正式签名公证、其它平台实机、真实显示器/USB/定时/登录休眠与Close/Tray/Quit缺证继续保留，原24不能据本次候选自动完成。

最终报告简明U1—U8对照表、实际变更/原因/影响及验证结果；不隐瞒未完成项。docs/usage/README.md待实际接口验收后由文档owner按父指定文本更新，不与产品并发改写。

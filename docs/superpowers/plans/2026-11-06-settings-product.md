# 设置产品实施计划

依据../specs/2026-11-06-settings-product-design.md及用户逐条明确授权，R1—R9保持。父ses_e5298d591ffe38RnrkdRHwwEOq仅编排；不初始化Git/CodeGraph、不另建状态账本。旧导入兼容要求被取消；用户已确认旧候选红字消失/操作正常，旧产物保留。

## 实施顺序与写域

先S/P/D/U各自完成去旧/整页保存并停写；再以下生产者与消费者按固定API独占文件并行。测试由独立验证者在整合停写后执行，实施者只先改测试、patch、精确gofmt和静态读回。

- S：internal/settings/document.go及本包对应测试，新增Desktop字段/默认和控制工具校验接线；不修改已验锁/原子算法。
- T：新增internal/controltools/**，唯一工具定位和验证，无业务执行。
- I：新增desktop/i18n/{manager.go,catalog.go,manager_test.go}及词典加载机制；desktop/app/service.go与新language.go的实例注入/结构化消息支持。I不写词典具体文案和UI文件。
- U：designer独占desktop/views/**、desktop/frontend/**、desktop/tray/**及对应desktop/tests/**、desktop/i18n/locales/{zh-CN,en}.json词典文案。原设置去旧/整页保存先完成后再扩语言；不与I写同文件。
- H：desktop/main.go、desktop/app/host/**，系统语言获取、通知和原生应用提供文案；不改业务页/词典。
- P：internal/loginstart/**的结构/就绪校验接线及对应测试，保parser算法。
- D：cmd/displaykvmd/**、internal/cli/**的工具resolver接线及对应测试，保分区事务。
- M：third_party/m1ddc/**固定官方源pin，只vendor原字节，不执行安装或上游脚本。
- B：desktop/build-macos.sh、packaging/**相关源码/测试、build/**新增依赖构建接线；工具归档/许可证/来源和签名，不动业务代码。
- 文档：docs/usage/README.md、README.md、docs/releases.md、packaging/README.md在实际接口停写后统一更新，不与B并发。

## 去旧与整页保存

Snapshot不再含Legacy，删除LegacySources/ImportLegacy；完整文档Store只settings.Load。ServiceProfile仅Version/ConfigPath，版本仍2；v1、旧字段和裸配置明确拒绝，不自动改原件。daemon只-settings/-startup-profile，工具来自完整文档和包内定位；CLI config check/new/set-tool及startup configure --config完整文件，删除import/旧四路径。

用户入口/标题为‘设置’，checkbox为‘开机启动服务’/‘开机启动桌面客户端’，主‘保存’一次提交所有本页改动。先保存文档并核验，再保存文档选择，最后分别处理两开关；文档/选择确认后应用语言。取消对话框不改系统项；写失败/unknown不启用依赖未确认文档的服务，仍允许安全禁用或其它可信独立项并真实报告。保存不能因自身文档写入导致Expected.Version误判；不声称跨系统原子事务，不自动重发unknown。

## 完整文档扩展

```go
type DesktopSettings struct { Language string `json:"language"` }
// settings.Document新增：
Desktop DesktopSettings `json:"desktop"`
```

有效语言system/zh-CN/en；NewDocument默认system。旧完整文档缺desktop仅内存归一system，不自动写回；出现desktop必须非null对象、language明确有效、未知字段拒。Encode标准化desktop。每次设备/规则/日程/校准保存保留Desktop和Runtime。

## 工具定位公共API

```go
// internal/controltools
type Paths struct {
 ToolPath string `json:"tool_path"`
 BusctlPath string `json:"busctl_path,omitempty"`
 UdevadmPath string `json:"udevadm_path,omitempty"`
}
type Resolver struct { GOOS, Executable string }
func Current() (Resolver,error)
func (r Resolver) Resolve(Paths) (Paths,error)
// settings
type RuntimeSettings = controltools.Paths
func (d Document) ValidateForService(controltools.Resolver) (controltools.Paths,error)
```

空ToolPath使用实际运行程序所在包内工具：macOS .app/Contents/MacOS/m1ddc；Linux工具与当前程序相邻ddcutil/busctl/udevadm，运行库放相邻lib并由受控构建配置相对RUNPATH。显式绝对路径override保留给开发CLI/高级文档，不在普通页面暴露。解析器核实际普通可执行文件，拒符号链接/不可执行/错误路径，不搜PATH/不下载、不把解析的安装绝对路径写回Document。Windows显示backend未实现时明确Unsupported。

Current取真实可执行路径，daemon基于自身Resolver取返回路径传原backend；P启服务/Ready基于选定.app内daemon位置构造同一Resolver。SaveProfile仅校验完整文档结构和可读性，服务Enable才严格运行就绪；语言保存不被缺工具阻断。状态检查依旧如实表达Ready/unknown。

## 单实例语言管理

```go
// desktop/i18n
type Message struct { Key string; Values map[string]string }
type Catalog struct {
 Preference string `json:"preference"`
 Locale string `json:"locale"`
 Revision uint64 `json:"revision"`
 Messages map[string]string `json:"messages"`
}
func (c Catalog) Text(string,map[string]string) string
type Manager struct { /* 实例私有同步状态 */ }
func New(string,[]string) (*Manager,error)
func (m *Manager) Snapshot() Catalog
func (m *Manager) SetPreference(string) error
func (m *Manager) Changes() <-chan struct{}
// desktop/app
func NewWithLanguage(Connector,*i18n.Manager) *Service
func (s *Service) Language() *i18n.Manager
// desktop/app/host
func SystemLanguages() ([]string,error)
func Run(fs.FS,*StartupBridge,*i18n.Manager,...Page) error
```

I加载designer独占的locales/zh-CN.json和en.json作为唯一消息源，Snapshot返回独立副本。原app.New仍创建独立默认Manager，不用全局可变locale。Page.Handler原签名func(*app.Service)http.Handler不变；设置NewHandler新增第三参数*i18n.Manager，Page由Service.Language取得。

系统默认按首选列表匹配zh前缀→中文、en前缀→英文、无支持项→英文。macOS宿主cgo调用CFLocaleCopyPreferredLanguages并释放资源，Windows用GetUserPreferredUILanguages，Linux只读LANGUAGE及LC_ALL/LC_MESSAGES/LANG，不启动shell。获取失败回退英文并保诊断。

main只读profile及选中文档初始化语言，后台离线不影响显示；首次默认system，坏文件不写默认覆盖。整页文档及选择成功后SetPreference；无关启动开关失败不撤销已确认语言；文档/选择失败或unknown不切当前语言。

Go模板和JS使用同一Catalog，Go模板t翻译，JS只读注入词典和locale；不自行navigator.language另作决策。宿主监听语言Changes，在主线程重建托盘菜单/标题/提示并Emit desktop:language catalog事件，前端适配原位更新文字/属性，不刷新丢草稿。原生每次对话读取当前Catalog，翻译应用提供标题/消息/按钮；系统面板侧栏等OS自有文案仍由OS控制，不承诺强制覆盖。

## 随包工具

macOS固定m1ddc v1.3.1、commit7339ceebb03f691b1eaeb8471dca301d215d19c8、MIT，原始source/hash/license由third_party/m1ddc/SOURCE.json记录；构建输出到新候选，不写vendor，clang arm64后同三Go程序逐一签名再签整.app。包内Contents/Resources/bundled-tools.json记录schema_version1和原生工具相对路径/hash/平台/版本/来源/许可/SOURCE摘要；归档manifest新增omitempty native_tools，不将Objective-C程序伪作Go BuildInfo。

Linux固定ddcutil v3.0.2 commit5085ae9bb1a8e5bd8db717d80dd80a279a0f05fb(GPL-2.0-or-later)，systemd v258 busctl(LGPL-2.1-or-later)/udevadm(GPL-2.0-or-later)；分别amd64/arm64受控Linux构建，带必要运行库闭包、对应完整源码/构建脚本/许可。事件工具不替代宿主DBus/logind/udev系统设施，不自动安装systemd daemon或修改设备权限。环境不足时准确标记真实载荷未生成，不写假成功、不要求用户另行下载工具。

## 验收

独立只读review按R1—R9核实际差异及仍有效基线；最终命令专员一次受影响root与desktop整包测试/race/readonly离线，具体新增包包含settings/controltools/loginstart/cli/displaykvmd及i18n/app/host/views/tests/tray。记录确切argv/returncode/完整日志与case包级统计；不为补旧记录重跑未影响全仓/六目标。

重点：完整文档缺desktop默认和非法值；分区保存保语言；实例隔离/系统默认/显式覆盖和保存失败不切语言；Go/JS/托盘/应用原生文案同步且无草稿丢失；整页一次保存/partial/unknown/取消；旧入口拒绝原件不变；包内工具缺失/权限/别名/移动包；pin来源/原字节/许可证/manifest/签名/归档字节一致。

真实GUI复验先检查旧实例，不能强关用户草稿或把SingleInstance转回旧版误称新版；之前原生夹具三轮hidden/RAF失败证据保留，不无限叠探针。新应用与归档用独立候选目录，保旧产物；构建驱动必须直接持久returncode，避免run/status等变量名错误。其它平台真实GUI/硬件/登录唤醒和原24缺口继续按真实证据记录。

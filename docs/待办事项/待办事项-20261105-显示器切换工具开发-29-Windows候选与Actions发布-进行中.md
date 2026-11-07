# 20261105-显示器切换工具开发-29 Windows候选与Actions发布

- 系列标识：20261105-显示器切换工具开发；组内序号：29
- 任务编号：20261105-显示器切换工具开发-29；任务类型：实施任务
- 项目归属：`/Users/orzlee/work/project/other/displayKvm`，工作名称 displayKvm
- 总计划：[显示器切换工具开发](../任务计划/【20261105-显示器切换工具开发】.md)
- 批准设计：[显示器控制与交付缺口补全设计](../superpowers/specs/2026-11-07-display-control-completion-design.md)
- 对应需求：补全设计 R7；状态：进行中（本轮workflow STOP、A1–A4 static PASS，最后Go严格ELF/Release门控核查只读成功；pwsh未运行/native pending，准备rc7但尚无实际结果）
- 创建时间：2026-11-07T09:52:33+08:00；更新时间：2026-11-07T16:13:55+08:00（父转交静态验证及只读门控结果，不是完成时刻）
- 实际完成时间：未完成；权威完成字段为本字段，归档时间另记
- 当前文件位置：`docs/待办事项/待办事项-20261105-显示器切换工具开发-29-Windows候选与Actions发布-进行中.md`
- 所属系列主会话 ID：`ses_e4cc8a233ffeDDjHm7v7yPI8S3`；文档执行者：`ses_e4ca67d8affe48ddN4S1MR1ZXf`
- 专属实施子代理会话：`ses_e4c2221a5ffe465gK11LdyFkpr`，本轮仅workflow STOP；独验 `ses_e4bff311bffetgk8CWAyNcrqpC` A1–A4 static PASS，diffcheck/YAML/structure/actualdiff各exit0，原cmd/exit/TEMP TMP/诊断/jobs/gates不变。最后门控核查同29角色仅只读成功，无新tests/build；pwsh未运行、native pending，各旧轮次/失败/计数保留
- 执行批次/并行组：补全第2组/共享脚本串行接管28，先于30
- 本次选中范围：25、26、27、28、29、30及24软件验证部分；本轮workflow交付STOP、静态已验，User/SYSTEM/Admin准入与cleanup获准owner快照检查，不任信arbitrary TokenOwner。父据只读Go/Release门控证据放行已授权24 rc7原生链准备，无rc7 SHA/runURL/通过；本轮extra1已用、旧两轮/各extras不重置、本轮失败无自动更多，不新编号，原生/九档/download仍未验
- 授权依据与停止边界：用户2026-11-07已通过 question 明确批准“按设计实施”；本任务已获准修改workflow与脚本，实现artifact上传和Release发布入口；实现入口不等于已执行发布。本轮不实际推送、触发Actions或创建Release，实际外部执行及下载核对在24软件验证阶段按已有授权和届时明确指令执行。各角色仅执行本编号，不扩到其他任务；终点为真实硬件验收准备，不提交、不新增安装器、不操作硬件。
- 版本背景：主会话转交分支 `feat/display-control-completion`、基点 `f26ecf2`；文档执行者未执行 Git 核验或改变 Git 状态
- 历史路径迁移状态：2026-11-07随开工从未完成后缀改为进行中后缀，不覆盖目标并同步链接；01–23归档身份和历史证据不改

## 单一目标、交付物与背景

补齐 Windows 配套候选产物、原生无屏测试与 Actions 制品/条件发布入口，沿用现有构建包装体系，真实运行与下载结果由24末尾核实。

交付物：初次8文件、第1轮PE夹具和第2轮诊断静态PASS证据保留；rc1当时原生FAIL/根因未知，rc2已取到Windows编译与跨平台测试夹具错误，但仍root go test exit1。28 Linux依赖/来源flag、25删除未用import和24额外授权两测试文件修复分别归原任务，不计29第三轮；完整候选/下载/Release仍未验。

## 前置依赖、放行与调度

直接依赖28共享build/package脚本交付、代码定向独验及终态停写。主会话已确认这些代码交接条件并明确放行29 Windows/CI独立部分，可先实现CI生产路径；28真实Linux载荷仍受阻，不记为28完成。29串行承接共享脚本，不改28原生生成器/source-lock，不与28并发写同一脚本。

CI依赖顺序必须先在Linux两架构原生环境生成并校验真实bin/lib/source/license，成功后才能进行全量软件构建、完整候选与Release。24末尾统一环境阶段按明确指令先执行该生成/校验，再放行全量；失败保留受阻，不制造假包或删除支持目标。本阶段29只编写workflow/scripts，实际全量test/build与push/tag/Actions/Release仅24统一末尾指令执行。

执行批次：补全第2组/共享脚本串行接管28，先于30。编号本身不构成依赖；主会话按真实交付、独立验证与写域放行。同文件/逻辑区域单一写入者，验证期间冻结其输入。任务24复用原编号作唯一末尾纯验证，不新建第二全量任务。

## 准确写入范围与逐文件理由

| 文件或已授权逻辑范围 | 修改原因、不改影响与兼容风险 |
| --- | --- |
| `.github/workflows/build.yml` | 增加 Windows 原生无屏测试、制品上传与明确tag/手动条件的发布入口；不改则无相应交付入口 |
| `build/` 与 `packaging/` 内仅 CI/Windows 所需既有脚本及 `build/targets.json` | 复用候选包装、资源/许可/SHA256与目标矩阵；保留六核心与现有Mac桌面路径，不顺手关闭失败目标，与28串行 |
| `build/linux/bundle.py`、`build/linux/source-lock.json` | 属28原生生成器和来源锁定，不归29写；仅复用其接口编排CI生产路径，不并发修改或擅自放宽校验 |

范围内只改必要位置；未定位的既有路径由本编号实施者只读定位并记录，不发明目录/API，也不因范围较宽顺手修改其他逻辑。所有任务状态、总计划、归档与锁只能交专属串行文档执行者。不得新增第三方依赖、额外应用、检查点、SDD ledger 或平行计划；不得处理其他任务改动。

## 已有能力、交付接口与必要新增

复用现有构建/包装脚本、目标矩阵、许可与 SHA256 机制及双进程候选边界。origin 已确认 `github.com/orzlees/display-kvm`；本机无 gh CLI 不代表不能设计其他发布入口。具体脚本签名与测试命令引用当前已存在接口，由实施者只读核实后登记，不发明工具或安装器。

Windows GUI+daemon+CLI配套，amd64/arm64支持程度依据实际证据；增加适用 Windows runner 真正执行无屏原生单测，保留六核心目标及现有 Mac 桌面构建。不以交叉编译代替原生测试，不改支持表掩盖失败。上传 Actions artifact，生成 SHA256/许可产物，版本发布仅明确标签或手动条件触发。写好 workflow 不等于已运行、已发布或可下载；后台先于桌面等启动顺序、WebView2/系统/文件系统前提由30准确说明。本任务已获准修改workflow与脚本，实现artifact上传和Release发布入口；实现入口不等于已执行发布。本轮不实际推送、触发Actions或创建Release，实际外部执行及下载核对在24软件验证阶段按已有授权和届时明确指令执行。不提交、不新增安装器。

25与29的同包入口交接固定为同目录 `displaykvm.exe`（GUI）、`displaykvmd.exe`（daemon）、`displaykvm-cli.exe`（CLI），不依赖 Windows 文件名大小写区分。core 原始构建仍以 `displaykvm.exe` 命名 CLI，仅 desktop 候选包装复制时改为 `displaykvm-cli.exe`；按同包入口验证。25的 `Enable` 支持 GUI 绝对可执行文件路径或包目录，无需修改 GUI；本任务不修改由30负责的 packaging README。

只在明确收益和本任务必要范围内增加函数/模块，保留有效注释、既有错误和返回语义；涉及边界、资源、安全或历史兼容处补客观中文说明。若需突破当前依赖、配置含义或范围，先报告主会话，不自行扩大。

## 可执行步骤

- [x] 主会话确认28代码定向检查通过、实施终态停写，并明确放行29 Windows/CI独立部分；仅满足代码交接，不代表28真实载荷已完成。
- [x] 完成目标矩阵、脚本及三程序、来源/架构配对、许可合同的范围内静态核查；定向用例与全包重测通过，真实候选仍待24。
- [x] 第1轮完成workflow/CI门控范围内静态核查，Ruby/Psych解析通过；该软件scope PASS保留，后续实际CI失败见当前记录，不称构建发布已成功。
- [x] 初次实施8文件交付已停止写入；未将实际CI/native/Linux生成、九包、下载或发布认作已执行。
- [x] 首次专属独验已执行唯一packaging定向命令与局部静态核查，结果未通过；准确退出码未回报，失败证据保留，不据此勾选验收通过。
- [x] 第1轮补做仅在 `packaging/notices/windows_test.go:72-74` 设置 `NumberOfRvaAndSizes` 为16并补原因注释，已停写；生产代码及全部拒绝/成功断言未改。
- [x] 新同编号独验 `ses_e4bff311bffetgk8CWAyNcrqpC` 按首次失败原因完成唯一有因重测并终态PASS；首次失败证据与旧验证者输入计量未知的记录保留，不再向旧会话追加。
- [x] 第2轮仅windows-native步骤捕获stdout/stderr及有界安全错误annotation，保留原exit、flags、timeout/范围；已停写，不猜原生根因或改产品逻辑。
- [x] 原专属独验报告最终静态PASS，YAML/Psych/diff0及纯数学Python核算exit0；撤回20,577byte核算错误的唯一FAIL理由，非第三轮代码补做。
- [ ] rc2已实际取得Windows root失败annotations，不能将原诊断静态PASS当作Windows全测试通过；本机pwsh入口exit1、未安装未执行的历史保留，动态语义按24实际证据核查，不冒称已完成。rc3尚未创建/推送或触发CI。

## 定向验收标准

| 验收编号 | 对应补全需求 | 完成标准与关键断言 | 预期证据与检查角色 |
| --- | --- | --- | --- |
| A01 | R7 | Windows候选GUI/daemon/CLI、资源许可与校验完整；原有核心/Mac目标未被掩盖或禁用 | 目标矩阵、脚本及包装契约静态核查 |
| A02 | R7 | 原生无屏测试、artifact上传和明确tag/手动release条件准确，失败可见 | workflow与事件/路径静态核查 |
| A03 | R7 | 编写、执行、发布、可下载分别记录；无自动外部副作用，28未完成如实保留 | 授权边界及24待验证项记录 |

## 验证角色、命令与预算

首次专属独验的唯一 `go test ./packaging/notices -count=1 -v` 失败，准确退出码未回报，保留原记录不猜数值；当轮diffcheck、shell语法、Python `ast.parse` 与targets JSON通过，无yq、未执行YAML解析。第1轮夹具补做后，新独验恰一次有因重测exit0，4个PE样例/全包、限定diffcheck及Ruby/Psych解析exit0，`"on"` 已引用，不受YAML 1.1误转影响。以上是第1轮软件历史，不冒充后续rc1实际CI通过；rc1真实Linux/Windows测试失败见当前记录。第2轮诊断最终仅静态PASS，YAML/Psych/diff及数学Python exit0，动态pwsh未执行留24；不追加Go测试或构建/外部运行。

实施者先写必要测试再实现，但不运行任务测试。独立验证者只读代码/测试且不修复实现，只执行本任务一次定向单测（无适用单测按契约记录）与契约检查。命令、工作目录、环境、退出码、关键错误与证据定位保留在对应专属会话；任务文档只收短摘要。仅上次失败或代码/测试/验收输入变化才允许有因重测，记录原因与次数；换会话或收口不重复测试。项目全量测试、完整编译/构建、集成及多环境仅24末尾执行一次。

## 风险、当前证据与执行记录

28脚本与本任务写域重叠，必须先交付停写再接管。CI权限、Windows架构支持、WebView2和真实下载可能受阻，不能因入口存在虚报成功，不新增通用安装器或依赖。

- 当前实施/验证摘要（父转交，taskdocs未读源码或跑测试）：原实施 `ses_e4c2221a5ffe465gK11LdyFkpr` 第2轮仅workflow Windows诊断已停写；原独验 `ses_e4bff311bffetgk8CWAyNcrqpC` 最终更正静态PASS、动态pwsh留24。第1轮scope PASS与首次失败/逐文件历史不删；本轮静态通过不表示真实原生CI通过或任务完成。
- 首次失败证据：唯一 `go test ./packaging/notices -count=1 -v` 失败，准确退出码未回报；唯一失败 `TestWindowsPERejectsWrongMachineDLLAndConsoleGUI/arm64-gui`，定位 `packaging/notices/windows_test.go:63`、`packaging/notices/windows_test.go:77`，错误为 `size of data directories(128) is inconsistent with number of data directories(0)`。主会话转交根因是最小PE测试夹具不完整；其余定向用例通过，但总体未通过。第1轮只修夹具，不放松生产PE校验。
- 第1轮补做与精确证据：仅 `packaging/notices/windows_test.go:72-74` 修正最小PE夹具，设置 `NumberOfRvaAndSizes` 为16并补原因注释；生产解析器 `packaging/notices/windows.go:94-116` 与测试拒绝/成功断言 `packaging/notices/windows_test.go:56-59`、`packaging/notices/windows_test.go:79-82` 均未改。相关测试文件未跟踪，diff无内容；独验读取实际文件确认，不因此暂存或改变Git状态。
- 有因重验与静态证据：新独验唯一 `go test ./packaging/notices -count=1 -v` exit0，4个PE样例/全包通过，限定diffcheck exit0；本轮新增Ruby/Psych解析workflow exit0，`"on"` 已引用，不受YAML 1.1误转影响。三程序、来源/架构配对、许可和CI门控合同静态PASS；首次shell/Python/JSON证据复用，无yq未解析YAML是首轮历史，本轮由Ruby/Psych补充解析，不记为yq执行。
- 当前验证状态：原软件/诊断PASS及rc1–rc3真实失败保留；本次24实际两测试diff及仅拒绝诊断的lockdiff审阅PASS，hostrace 1package/47testevents pass、0fail/0skip，双arch两包四条compile-only exit0、三路径diffcheck0。锁实际根因仍未知，需下一CI必要owner/ACE，安全政策未改、PE未运行、无后台helper；Linux静态PASS无真实生成，九包/下载/Release仍缺。本机/交叉编译PASS不等原生通过，不归档。
- 次数与停止边界：原Go定向累计2次（首次失败1+第1轮有因重测1 exit0）、Go有因重测1；产品总补做2轮，均已停写，第2轮仅诊断静态PASS。验证者20,577byte理由撤回为核算错误，不计第三轮产品补做；后续不得自动第三轮29返修。本轮无动态pwsh执行、无CI/API/Git或产品tests/build，旧失败/次数不清零。
- 依赖放行证据：28实施终态停写、notices单测/静态检查exit0，真实载荷仍缺；主会话明确允许29先实现Windows/CI独立生产路径。全量与实际外部执行仍归24末尾统一指令。
- 外部准备历史（父转交，taskdocs未执行Git）：原push dryrun `HEAD→refs/heads/feat/display-control-completion` exit0、would-create无副作用，不等于当时已推送。随后真实commit `37cf7d819ef2d870faa0350328bab32c05be8549`、feature branch与tag `v0.0.1-rc.20261107.1` 推送核验exit0并触发[run42335499920](https://github.com/orzlees/display-kvm/actions/runs/42335499920)；CI结果见下条，保留tag路径，不称已发布或可下载。
- rc1真实环境结果（父转交）：Windows amd64 job `128291408104`、arm64 job `128291408141` 的“原生无屏产品测试”均failure，step分别03:47:07Z→03:57:28Z、03:47:41Z→03:58:04Z；根因未知，不凭耗时认定timeout、不补造exit码。Linux两生成job失败且日志403、annotations仅泛exit1，详见24；core/release/desktop-windows/desktop-macos-arm64均skipped，无候选归档/Release可验。最后direct run仍in_progress、停止查询前未核terminal，不写run已终结。
- 第2轮诊断最终范围：仅workflow windows-native捕获stdout/stderr并保留原exit/scope、flags/timeout/范围，输出有界安全error annotation；原独验最终静态PASS，YAML/Psych/diff0，相关数学核算及纠正保留。该阶段冻结时rc2尚未commit/push/触发，后续真实rc2结果另列，不把静态检查冒充当时已通过CI。
- 验证者纠正与动态缺口：先前20,577byte FAIL由汉字UTF8与%转义重复乘3的核算错误导致，唯一FAIL理由已明确撤回；不是第三轮代码补做。实际纯CJK/%/CR/LF样例6890byte、emoji4622byte，保守上界6942<8192，纯数学Python核算exit0；不冒充PowerShell运行。本机pwsh入口exit1、未安装未执行，动态pwsh留24，不能从静态推断其执行结果。
- 批次放行、批次收口及系列收尾：尚未发生；26/29返修独验已PASS且30交付/范围验收完成后，24先生成核验Linux两arch物料，成功后补28实际证据、独立核验并一次批次收口；不要求预先28归档或收口才运行CI生成，失败挡全量/完整发布。系列收尾复用证据。
- 文档短锁：本轮由实际文档执行者正常获取，写后核对后释放；不把随机令牌写入正文、不持锁等待实施；系列占用仍归所属主会话。
- rc2及修复归属（父转交）：tag `v0.0.1-rc.20261107.2`、commit `0563fe07c6652b9bd881541537cfd46f75ce6a0b`、[run 42338704107](https://github.com/orzlees/display-kvm/actions/runs/42338704107)。Windows amd64 job128301407269/arm64 job128301407117 root go test exit1、annotations HTTP200，未用os import及Darwin后端/设置测试的跨平台夹具问题见24；core/全部GUI/release skipped，无包/下载/Release可验。28第2最后补做仅Linux flag/开发依赖/说明，Windows workflow、九包门控及发布条款不变；25第1补做删import、24用户额外授权1轮仅两测试文件，不计29新补做或增加额度。
- 24额外授权结果：原独验独立执行两包联合hostrace一次exit0，2package+188test pass、0fail/0skip，另Windows双arch每arch两包compile-only共4commands exit0、diffcheck0，两文件实际范围审阅PASS、无缺口，无重跑、生成PE未运行/未打包。历史24两轮不清零，root25包复验只计有因命令复验；本次额外1轮只执行一次、无失败返修，不扩29/28额度，整个24仍未完成。
- rc3实际结果（父转交）：commit `e3d67c533fe968e0dea4548cb5582ee0481fad0a`、tag `v0.0.1-rc.20261107.3` 已真实推送，对应[run42341866318](https://github.com/orzlees/display-kvm/actions/runs/42341866318)。Windows arm64 job128311118696/amd64 job128311118855原生无屏产品测试root go test exit1，annotations含配置锁路径permission_denied/不可信主体、loginstart `flag provided but not defined: -config`；具体生产/测试归因待查。Linux两arch也失败，prepare成功、core/GUI/release全部skipped，无archive/Release证据；证据为24记录的rc3-ci目录下 `annotations-128311118696.json`、`annotations-128311118855.json`，本角色不读取重验。
- rc4真实失败（父转交）：commit `fd41e9eeedb76c154fd867ae989e0790ff530907`、tag `v0.0.1-rc.20261107.4` 对应[run42345018809](https://github.com/orzlees/display-kvm/actions/runs/42345018809)，completed/failure。Windows arm64 job128320803866/amd64 job128320803813 root go test exit1，配置锁permission_denied；runner祖先AuthenticatedUsers危险写被现policy真实拒绝，准确ACE/目录见[24当前记录](待办事项-20261105-显示器切换工具开发-24-全量测试构建与多环境验收-进行中.md)。正常AppData生产路径及最终测试环境方案尚未验，不称纯fixture或已修产品，`-config` Usage为测试故意拒旧参数。Linux两arch亦失败，prepare PASS，core/WinGUI/MacGUI/release skipped，无九包/artifact/Release/下载hash；真实rc4-ci目录和annotation HTTPstatus未知边界见24。
- 新extra1终态与下一步：仅上述workflow step，private LocalApplicationData新GUID、metadata/owner等检查及仅测试进程TEMP/TMP已交付，finally恢复、失败保留、成功时仅owned空根非递归删除；不改ACL/生产安全/可信SID、不写GITHUB_ENV，GOTMPDIR、原两Go command/flags/range/exit/Tee/diagnostic/gate不变。实施STOP，原独验A1–A4静态PASS：git diff/diffcheck、YAML Ruby/Psych、Python结构commands均exit0；pwsh此前已确认缺失，该局部验证阶段无动态执行，不替代原生证据。新extra1已使用、局部验证结束，旧两轮及24历史两轮/两次分别批准extra1、28旧两轮/此前extra1全部保留，本次失败不授权后续自动修复。前一 `/private/var/folders/82/9v48ldbs4ng513qh7_wtlx8m0000gn/T/opencode/displaykvm-final-rc4-local/summary.json` 局部PASS保留；该批5产品+4文档共9paths已提交推送rc5，真实结果见下条，不提前记整体PASS。两codewriter均STOP、无helper；taskdocs读回释放本人锁后FREEZE、无待写、父占用保留，23/30不归档。
- rc5真实结果与stdio补充（父转交）：tag `v0.0.1-rc.20261107.5` 对应[run42348600908](https://github.com/orzlees/display-kvm/actions/runs/42348600908)，两Windows测试step exit1，原annotation仅 `Process completed with exit code 1`。用户真实日志定位 `C:\a\_temp\d91b0d81-524a-4641-86a4-ad61761dcd46.ps1:80` throw“测试临时根目录必须由当前令牌账户所有”，确定所给stdio的owner guard在root Go tests前抛错；finally保留本次临时目录与RunnerTemp日志是结果而非根因，另一架构是否同一错误未证实。原SID未提供，随后Oracle只读结论与新诊断授权见下节；不写已修复，不沿用rc4生产ACL测试失败。原24独验已terminal、无monitor/下载程序；完整分组提交、证据目录、Windows架构→job编号映射未完整提供及annotation HTTP未知口径见[24 rc5记录](待办事项-20261105-显示器切换工具开发-24-全量测试构建与多环境验收-进行中.md)。core/desktop-windows/desktop-macos-arm64/release全skipped，无九包/artifact/Release/下载hash/PE/许可证闭环，旧轮次/局部PASS和前一extra1已用事实保留。

## rc5后新诊断extra1（仅workflow既有step，静态结论已交）

- 本轮交付（父转交）：原实施STOP，仅workflow windows-native step:172–404。两个布尔关系“目录所有者是否等于当前用户”“目录所有者是否等于当前进程的默认对象所有者”或unknown，固定类别“当前用户/固定可信组/其它”及phase/reason/type/scriptline受控；inner catch冻结首fail、outer catch bare throw，不输出rawSID/path/Message，不记录账户名或凭据，不改owner比较安全if、Go cmds/exit/TEMP TMP/cleanup/gates，不调整所有权或扩大测试范围。
- 本轮静态验收（父转交）：原独验A1诊断/A2安全/A3异常退出清理/A4输出上限静态PASS已交，actual diff、限定diffcheck、RubyPsych YAML均exit0；two Go tests原命令各1处及flags/Tee/立即LASTEXITCODE/root fail阻断desktop保留，TEMP/TMP与cleanup、other jobs/gates/release不变。pwsh本机不可用、不安装、不虚假dynamic，native两arch仍24；最终交付计量尚未提供，不为等计量持锁。
- rc6前只读结论保留：两SID.Value比较没有类型错，默认对象owner可不等当前用户；rc5所给stdio中Go未跑、另一架构是否同错当时未证实。该历史缺口不被后来结果冒充当时已知，也不能据此放宽安全。
- rc6真实诊断（父转交）：[run42351223970](https://github.com/orzlees/display-kvm/actions/runs/42351223970) FAIL；amd64 job128339608123、arm64 job128339608340均为preflight-validate、scriptline135、reason=owner-mismatch，owner_matches_user=false、owner_matches_default_owner=true、owner_category=fixed-trusted-group。类别只覆盖当前代码的SYSTEM/Admin范围，具体哪个未输出，不能猜Admins；两架构root/desktop Go均未执行。prepare PASS，core/GUI/release SKIP，无九包/archive/artifact/Release/download/hash/PE/license实物证据；准确提交/tag、annotations HTTP未知口径及实际目录见[24 rc6记录](待办事项-20261105-显示器切换工具开发-24-全量测试构建与多环境验收-进行中.md)。
- rc6初报边界：当时原诊断静态PASS有效但未修根因，Windows oracle `ses_e4b7f3b88ffetEO1lmkZQEB677` 仅只读评估、尚无新写授权。该失败与当时边界保留，后续用户追加兼容修正见下节，不把新规则当旧历史已验。

## rc6后8文件兼容修正extra1（workflow STOP，静态/门控结果已交）

- 本轮交付/独验（父转交）：仅 `.github/workflows/build.yml`，实施 `ses_e4c2221a5ffe465gK11LdyFkpr` STOP；:297–307复用分类只接受User/SYSTEM/Admin，TokenOwner仅diag；:364–370 cleanup当前owner必须等当初获准ownerSID快照，path/created/ordinary/nonreparse/nonrecursive empty delete保持。独验 `ses_e4bff311bffetgk8CWAyNcrqpC` A1–A4 static PASS，diffcheck/YAML/structure/actualdiff各exit0，原cmd/exit/TEMP TMP/诊断/其它jobs与门控未变；pwsh未运行、native pending，不把静态PASS当候选通过。
- 最后门控核查同29角色仅只读成功，无新tests/build：Linux generate/pack-linux只产内部artifact，core依赖linux-tools/windows-native并经set-eu merge-linux→build-core all→package.sh core；build-core无条件record-core，package重算Linux记录，linuxBundle对3tools+全libs逐Go inspectLinuxELF，err沿shell gate退出；release依赖core/desktop、collect-release要求9档+hash。未发现完整Release绕过Go严格ELF校验路径，准确定位集中见[24本轮门控证据](待办事项-20261105-显示器切换工具开发-24-全量测试构建与多环境验收-进行中.md)，本writer不亲查源码。
- 父据此不为28既有Python文本解析边界追加hardening/修复轮次，保留其A1部分通过与real ELF未证实，进入已授权24 rc7原生链准备；不是提升28全PASS，也不写已发现真实漏洞。24的三文件策略校准仍按其独立compile+scope结论，native待验；不扩本任务文件或重启30。
- 本轮extra1已用，旧28/29两轮、24历史两轮及历次extras不重置、本轮失败无自动更多，不新编号；三sourcewriter全STOP，真实native/两arch payload/九archive/download/实机未验，尚无rc7 SHA/runURL/通过。taskdocs必要读回/相对链接确认后核验并正常释放本人短锁、FREEZE全部四文档、无待写/无正在写入者、父占用保留，供后续Git；本writer不验source/重测/build/Git/API或新报告，23/30不归档。

## 完成、归档与迁移

当前状态进行中，实际完成时间未完成，未归档。仅在本次全部选中实施任务交付且独验证据齐备、主会话决定唯一一次批次收口后，由串行文档执行者按真实完成日期维护状态及归档；缺证/受阻不虚标完成。任务24软件部分通过仍不代表硬件通过或全系列完成。迁移记录：2026-11-07仅从未完成后缀改为进行中后缀并同步链接，不覆盖目标。

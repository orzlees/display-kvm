# 20261105-显示器切换工具开发-29 Windows候选与Actions发布

- 系列标识：20261105-显示器切换工具开发；组内序号：29
- 任务编号：20261105-显示器切换工具开发-29；任务类型：实施任务
- 项目归属：`/Users/orzlee/work/project/other/displayKvm`，工作名称 displayKvm
- 总计划：[显示器切换工具开发](../任务计划/【20261105-显示器切换工具开发】.md)
- 批准设计：[显示器控制与交付缺口补全设计](../superpowers/specs/2026-11-07-display-control-completion-design.md)
- 对应需求：补全设计 R7；状态：进行中（第1轮软件scope PASS保留；rc1 Windows原生测试实际FAIL，第2轮诊断补做停写且最终静态PASS，动态pwsh留24）
- 创建时间：2026-11-07T09:52:33+08:00；更新时间：2026-11-07T12:09:27+08:00（第2轮诊断最终静态PASS及验证者核算纠正登记时间，不是完成时刻）
- 实际完成时间：未完成；权威完成字段为本字段，归档时间另记
- 当前文件位置：`docs/待办事项/待办事项-20261105-显示器切换工具开发-29-Windows候选与Actions发布-进行中.md`
- 所属系列主会话 ID：`ses_e4cc8a233ffeDDjHm7v7yPI8S3`；文档执行者：`ses_e4ca67d8affe48ddN4S1MR1ZXf`
- 专属实施子代理会话：`ses_e4c2221a5ffe465gK11LdyFkpr`，第2轮仅Windows workflow诊断补做完成停写；原独验 `ses_e4bff311bffetgk8CWAyNcrqpC` 本轮最终更正为静态PASS、动态pwsh留24，最新输入估计61K由父转交。首次独验 `ses_e4c0fbbb4ffey6Rn74YMyPvUUB` 失败/输入计量未知不追加；第1轮软件PASS、首败及累计次数均保留
- 执行批次/并行组：补全第2组/共享脚本串行接管28，先于30
- 本次选中范围：25、26、27、28、29、30及24软件验证部分；第1轮PE夹具补做证据保留，当前第2轮只改 `.github/workflows/build.yml` 的windows-native测试步骤：捕获stdout/stderr、保留原exit和测试scope，输出有界安全错误context为public error annotation；不改测试flags/timeout/范围，不修猜测产品，不执行CI/API/Git
- 授权依据与停止边界：用户2026-11-07已通过 question 明确批准“按设计实施”；本任务已获准修改workflow与脚本，实现artifact上传和Release发布入口；实现入口不等于已执行发布。本轮不实际推送、触发Actions或创建Release，实际外部执行及下载核对在24软件验证阶段按已有授权和届时明确指令执行。各角色仅执行本编号，不扩到其他任务；终点为真实硬件验收准备，不提交、不新增安装器、不操作硬件。
- 版本背景：主会话转交分支 `feat/display-control-completion`、基点 `f26ecf2`；文档执行者未执行 Git 核验或改变 Git 状态
- 历史路径迁移状态：2026-11-07随开工从未完成后缀改为进行中后缀，不覆盖目标并同步链接；01–23归档身份和历史证据不改

## 单一目标、交付物与背景

补齐 Windows 配套候选产物、原生无屏测试与 Actions 制品/条件发布入口，沿用现有构建包装体系，真实运行与下载结果由24末尾核实。

交付物：初次8文件、第1轮仅PE夹具补做及软件scope PASS证据保留；第2轮仅workflow诊断补做已停写并取得最终静态PASS，动态pwsh留24。24首CI的Windows两arch原生测试已实际FAIL、根因未知，诊断静态PASS只为rc2取得真正错误，不等于CI已修复或成功。28真实载荷、完整候选/下载/Release缺口保留，任务未完成。

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
- [ ] 动态pwsh留24；本机pwsh入口exit1、未安装未执行，不称PowerShell运行通过。rc2尚未commit/push/触发，静态PASS仅放行取得真实错误。

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
- 当前验证状态：第1轮软件scope PASS与第2轮诊断最终静态PASS均保留；真实Windows两原生测试FAIL、根因未知，不按耗时判timeout。动态pwsh留24，本机入口exit1、未安装未执行；九包/完整Release/下载未验证，不据静态结果宣称运行通过，不归档或增加完成数。
- 次数与停止边界：原Go定向累计2次（首次失败1+第1轮有因重测1 exit0）、Go有因重测1；产品总补做2轮，均已停写，第2轮仅诊断静态PASS。验证者20,577byte理由撤回为核算错误，不计第三轮产品补做；后续不得自动第三轮29返修。本轮无动态pwsh执行、无CI/API/Git或产品tests/build，旧失败/次数不清零。
- 依赖放行证据：28实施终态停写、notices单测/静态检查exit0，真实载荷仍缺；主会话明确允许29先实现Windows/CI独立生产路径。全量与实际外部执行仍归24末尾统一指令。
- 外部准备历史（父转交，taskdocs未执行Git）：原push dryrun `HEAD→refs/heads/feat/display-control-completion` exit0、would-create无副作用，不等于当时已推送。随后真实commit `37cf7d819ef2d870faa0350328bab32c05be8549`、feature branch与tag `v0.0.1-rc.20261107.1` 推送核验exit0并触发[run42335499920](https://github.com/orzlees/display-kvm/actions/runs/42335499920)；CI结果见下条，保留tag路径，不称已发布或可下载。
- rc1真实环境结果（父转交）：Windows amd64 job `128291408104`、arm64 job `128291408141` 的“原生无屏产品测试”均failure，step分别03:47:07Z→03:57:28Z、03:47:41Z→03:58:04Z；根因未知，不凭耗时认定timeout、不补造exit码。Linux两生成job失败且日志403、annotations仅泛exit1，详见24；core/release/desktop-windows/desktop-macos-arm64均skipped，无候选归档/Release可验。最后direct run仍in_progress、停止查询前未核terminal，不写run已终结。
- 第2轮诊断最终范围：仅workflow windows-native步骤捕获stdout/stderr并保留原exit/scope，将有界安全错误context写public error annotation，flags/timeout/范围未改、不修猜测产品。实施已停写、原独验最终静态PASS；两go test参数scope、原退出码、root失败阻断desktop、成功无annotation及有界摘要均静态通过，YAML/Psych/diff0。28第1轮诊断scope PASS复用，rc2 `v0.0.1-rc.20261107.2` 尚未commit/push/触发，静态PASS只为取得两类真实错误，不称根因已解决。
- 验证者纠正与动态缺口：先前20,577byte FAIL由汉字UTF8与%转义重复乘3的核算错误导致，唯一FAIL理由已明确撤回；不是第三轮代码补做。实际纯CJK/%/CR/LF样例6890byte、emoji4622byte，保守上界6942<8192，纯数学Python核算exit0；不冒充PowerShell运行。本机pwsh入口exit1、未安装未执行，动态pwsh留24，不能从静态推断其执行结果。
- 批次放行、批次收口及系列收尾：尚未发生；26/29返修独验已PASS且30交付/范围验收完成后，24先生成核验Linux两arch物料，成功后补28实际证据、独立核验并一次批次收口；不要求预先28归档或收口才运行CI生成，失败挡全量/完整发布。系列收尾复用证据。
- 文档短锁：本轮由实际文档执行者正常获取，写后核对后释放；不把随机令牌写入正文、不持锁等待实施；系列占用仍归所属主会话。
- 下一步：29第2轮诊断已停写并最终静态PASS，动态pwsh留24；rc2尚未commit/push/触发，由父另发Git契约，不由taskdocs执行。24本机root/desktop及原scope PASS、30文档PASS复用，首CI真实失败/未知根因及28真实物料缺口保留，累计23/30、24未完成。总补做2轮，不自动第三轮29返修；全部产品writer已停写，四文档读回释放本人锁、父占用保留并FREEZE停止所有写入，无额外报告/检查点或业务复验。

## 完成、归档与迁移

当前状态进行中，实际完成时间未完成，未归档。仅在本次全部选中实施任务交付且独验证据齐备、主会话决定唯一一次批次收口后，由串行文档执行者按真实完成日期维护状态及归档；缺证/受阻不虚标完成。任务24软件部分通过仍不代表硬件通过或全系列完成。迁移记录：2026-11-07仅从未完成后缀改为进行中后缀并同步链接，不覆盖目标。

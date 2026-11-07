# DisplayKVM领域、配置与接口契约

本文件是任务03的唯一共享契约，代码定义位于`internal/model`、`internal/config`、`internal/backend/types.go`和`internal/events/types.go`。交付状态：实施已落稿，等待03专属独立验收评审；本文不代判验收通过，不自行放行下游。变更公共签名、版本、错误或字段时必须同步代码、文档、受影响任务并重新评审。

依据：[01已批准决策](decisions/技术与硬件边界.md)。名称为DisplayKVM，模块为`github.com/orzlee/displayKvm`，最低版本声明为`go 1.26.10`，核心许可方向为MIT。本任务不创建许可证文件。当前仅使用Go标准库，不增加第三方模块或`go.sum`，不依赖任何桌面框架。宿主Go1.27.3及最低Go版本均不在本实施会话运行；最低版本兼容性仍未验证。

## 1. 身份、目标、分组与输入方案

`DisplayID`、`TargetID`、`GroupID`、`SchemeID`、`RequestID`分别表示用户持久化的显示器逻辑身份、主机目标、分组、输入方案和请求关联标识，底层均为字符串。`model.ValidID(value string) bool`统一要求1至128个ASCII字节，只允许`A-Z a-z 0-9 . _ : -`。不同种类有独立命名空间，同一种类不得重复。名称允许中文，不作为唯一键。

| 类型 | JSON字段 | 语义与边界 |
| --- | --- | --- |
| `Display` | `id`、`name`、`identity`、可选`binding` | 配置中的逻辑显示器；不会根据序列号自动合并 |
| `IdentityHints` | 可选`manufacturer`、`product_code`、`serial`、`edid_sha256`、`path` | 识别候选信息；空值、重复序列号或相同EDID均合法；摘要非空时必须为64位十六进制SHA256 |
| `Binding` | `path` | 人工确认的非空路径约束；同一配置不能绑定给两个逻辑显示器；路径失效须重新绑定 |
| `Device` | `backend_id`、`name`、`identity` | 后端当前枚举对象；`backend_id`仅在该后端和枚举周期内有效，不能代替持久化逻辑ID |
| `InputScheme` | `id`、`protocol`、可选`vcp_code`、`value` | 每个输入方案保存真实控制参数，不包含默认厂商猜测 |
| `Target` | `id`、`name`、`inputs` | 主机是用户命名的视频来源，不是远端主机发现；每项`TargetInput`含`display_id`及`scheme_id` |
| `Group` | `id`、`name`、`display_ids` | 有序非空显示器列表，不得重复或引用不存在的显示器 |

`InputScheme.Validate() error`接受两类协议：

| `protocol` | 参数与边界 |
| --- | --- |
| `vcp` | `vcp_code`必须显式为96（0x60），`value`为1至65535 |
| `lg_input_alt` | `vcp_code`须省略或为0，`value`为1至65535，保留设备校准值 |

值的合法数值范围不证明该显示器接受此值；后端能力与校准仍须核实。未知协议返回`unsupported_protocol`。标准和alternate不得相互静默回退；alternate不是所有LG设备的默认方案。AMD专用库未获批准，不能借协议名纳入实现。`Display.Validate() error`只校验身份格式和显式绑定，匹配/歧义消解属于后续身份任务；多个候选必须返回`identity_ambiguous`，不能选择第一项。

一个目标至少映射一屏，每屏在该目标内恰好出现一次；允许不同目标给同一屏使用不同方案，允许多个映射引用同一已校准方案。目标可以只覆盖部分屏。请求选择了目标未映射的显示器时，执行入口必须在任何写入前拒绝，不能取其他目标的端口、忽略该屏或猜测默认值。

## 2. 配置加载、校验与兼容迁移

唯一当前配置类型为`config.Config`，JSON字段为`version`、`displays`、`input_schemes`、`targets`、`groups`。`CurrentVersion = 2`，`MaxConfigBytes = 1048576`。空集合允许，用于尚未配置的首次使用；已创建目标/分组则必须非空。

```go
func (c Config) Validate() error
func Load(reader io.Reader) (Config, error)
func Encode(writer io.Writer, cfg Config) error
```

`Load`读取至多上限加1字节，拒绝超限、缺失版本、非法结构、未知字段、重复JSON键和尾随第二个文档。`model.DecodeJSON(data []byte, destination any) error`为配置和消息复用同一对象解码规则，递归深度最多64，根必须是非null对象且字节必须为有效UTF-8；调用者先限制字节数。字段的规范拼写以代码的JSON标签为准。JSON中的整数使用十进制；VCP代码范围0至255，输入值范围由方案校验约束；不能截断或溢出转换。

`Config.Validate`拒绝所有重复逻辑ID、重复每屏映射、悬空引用、重复分组成员和冲突人工绑定。识别候选信息重复不会合并或删除对象。未知配置版本报`unsupported_version`；未知输入协议保留`unsupported_protocol`；一般结构或引用错误报`invalid_config`并提供字段位置。I/O失败返回`io`并保留进程内根因，超限返回`message_too_large`，不能静默回退空配置。

当前v2示例：

```json
{
  "version": 2,
  "displays": [{"id":"left","name":"左屏","identity":{"serial":""}}],
  "input_schemes": [{"id":"work-hdmi","protocol":"vcp","vcp_code":96,"value":17}],
  "targets": [{"id":"work","name":"工作机","inputs":[{"display_id":"left","scheme_id":"work-hdmi"}]}],
  "groups": [{"id":"desk","name":"桌面","display_ids":["left"]}]
}
```

v1是本契约专门定义的兼容输入格式，不代表已有历史发布、已存在真实用户数据或已承诺支持其他未说明格式。v1保留`version`、`displays`、`targets`、`groups`，没有`input_schemes`；每项目标输入内联`display_id`、`protocol`、`vcp_code`、`value`：

```json
{
  "version": 1,
  "displays": [{"id":"left","name":"左屏","identity":{}}],
  "targets": [{"id":"work","name":"工作机","inputs":[{"display_id":"left","protocol":"vcp","vcp_code":96,"value":17}]}],
  "groups": []
}
```

加载v1会把内联参数逐项提取到v2方案，方案ID为`migrated-`加`SHA256(target_id + NUL + display_id)`的完整小写十六进制值。迁移保留显示器、绑定、名称、目标、分组、顺序、协议、VCP代码和校准值。合法ID不能含NUL，因此拼接无分隔歧义。生成结果必须再通过当前完整校验；重复数据不自动去重，未知厂商协议不替换为标准协议。

`Encode`只输出校验后的v2，先完整编码并检查大小再写入流；非法配置不写任何字节，写入失败仍可能产生部分流输出。它不创建目录、不修改文件权限、不自动保存迁移。后续保存实现必须使用私有临时文件和原子替换，保留原文件直到成功；读取失败不能覆盖原文件。并发入口应读取不可变配置快照，更新协调由后台负责。

Windows 配置基目录可以由当前用户、SYSTEM 或 Administrators 所有，但必须通过既有访问控制及完整路径检查。配置锁的私有目录和锁文件仍必须属于当前用户，并保持私有权限。固定可信主体的准入不包括任意令牌默认所有者；非可信所有者、危险访问控制、重解析路径和异常链接仍拒绝。该边界与 IPC 对既有配置基目录及私有运行目录的分层规则一致。

### 文章参数与绑定字段示例

以下为独立字段片段，用于解释配置结构，不是一份可直接启用的完整配置。显示器 ID 需要先从枚举结果建立稳定绑定；不得用文章中的显示器序号、名称、真实序列号或路径替代。当前平台与协议边界见[兼容说明](compatibility.md)。

```json
{"input_schemes":[{"id":"lg_dp_example","protocol":"lg_input_alt","value":208},{"id":"lg_hdmi_example","protocol":"lg_input_alt","value":145},{"id":"standard_dp_example","protocol":"vcp","vcp_code":96,"value":15},{"id":"standard_hdmi_example","protocol":"vcp","vcp_code":96,"value":17}]}
```

LG 私有方案不填写 vcp_code（或按既有契约设为 0）；标准 VCP 的 96 即 0x60。所有 JSON 数值使用十进制。208/145 和 15/17 仅为文章中的端口值示例，必须按自己的显示器核对。

以下仅为一个 `targets[].inputs[]` 元素：

```json
{"display_id":"display_lg_example","scheme_id":"lg_dp_example"}
```

display_id 和 scheme_id 必须引用本配置中已经存在且有效的对象，同一目标不能重复同一显示器。

以下仅为 USB 匹配字段片段：

```json
{"vendor_id":1507,"product_id":1552}
```

1507/1552 仅是文章中的 USB 厂商/产品标识示例，使用整数，不使用十六进制文本字符串。应用不会据此自动绑定当前设备；请先枚举并确认自己的 USB 事件与显示器绑定，再启用规则。

## 3. 统一请求、逐屏结果与错误

`SwitchRequest`字段：`id`、`target_id`、可选`display_ids`、`group_id`、`timeout_ms`。唯一校验入口为`SwitchRequest.Validate() error`；`EffectiveTimeout() time.Duration`须在校验后调用。

显示器列表和分组互斥；都省略或空列表时选择目标已配置的全部映射。列表不得重复。`timeout_ms`为0或省略时使用10秒，显式值允许1至30000毫秒。校验只负责语法；执行者还必须针对同一个配置快照检查目标/分组/每屏映射和绑定是否存在、是否歧义，再进入队列。取消、截止时间覆盖排队和执行，不能因某屏重试刷新总预算。请求ID用于关联，不单独承诺幂等；连接断开或超时后可能已有屏发送，客户端不得据此盲目重发整个请求。

`SwitchResult`包含`request_id`、`displays`以及可选请求级`error`；每项`DisplayResult`包含`display_id`、`state`、`desired_value`、可选`sent_at`、`observation`、`error`。逐屏ID不得重复，空列表必须有请求级失败原因，调用者须与原请求选择集合核对。`SwitchResult.Validate() error`保证关联ID、状态与证据内部一致；协议响应还必须与发起请求的ID/操作匹配，不得把其他请求的结果并入当前请求。

| `state` | 必需证据与含义 |
| --- | --- |
| `accepted` | 已接收，尚未发送，不携带发送时间或失败 |
| `sent` | 有非零`sent_at`、有效目标值，传输已提交；无画面成功含义 |
| `confirmed` | 除发送证据外，`observation.value`等于目标且`observed_at`不早于本次发送；只表示读回一致 |
| `failed` | 必须携带明确错误；其他屏可能已经发送，不承诺事务回滚 |
| `unknown` | 必须说明无法确认原因，如读回不支持或切走后失联；允许保留原时间的最近观测 |

`InputObservation`含可选`value`和`observed_at`；nil值表示没有有效观测，不是输入0。所有时间为Go `time.Time`的RFC3339/RFC3339Nano JSON时间（含时区），不是毫秒整数。只读到旧值不能确认当前发送。没有任何状态承诺目标主机已经显示画面，多屏操作不保证原子性，不自动回滚。

唯一结构化错误为`model.Error`：`code`、非空`message`、可选`field`、`retryable`。`NewError(code ErrorCode, message string, cause error) *Error`保留底层原因，`Unwrap() error`供`errors.Is/As`追踪，底层原因不进入JSON。`CodeOf(err error) ErrorCode`对取消和截止时间优先归为`canceled`/`timeout`，nil返回空串，未分类错误归`internal`。`Error.Validate() error`拒绝未知类别或空原因。`retryable`默认false，不授权自动重试，调度器仍须判断是否可能已经发送。

| 错误类别 | 使用边界 |
| --- | --- |
| `invalid_config` / `invalid_request` | 配置或请求格式、引用/选择、证据不一致 |
| `unsupported_version` / `unsupported_protocol` / `unsupported_operation` | 未知版本、未知或后端不支持的协议、已知协议但操作不可用（如无法读回） |
| `identity_ambiguous` / `not_found` | 多个设备候选无法唯一绑定 / 引用或枚举对象不存在 |
| `permission_denied` / `backend_unavailable` | 实际权限拒绝 / 架构、许可、已配置传输或外部工具不满足 |
| `unreachable` / `transport` | 设备通路不可达 / 已知传输执行失败 |
| `canceled` / `timeout` | 调用者取消 / 截止时间到达，均须释放资源 |
| `io` / `message_too_large` | 配置流读写失败 / 配置或消息超过上限 |
| `authentication_failed` / `daemon_unavailable` | 本机认证失败 / 后台端点不存在、过期或连接不可用 |
| `internal` | 未分类内部错误，不能改成成功或吞掉 |

## 4. 后端唯一接口

定义位置为`internal/backend/types.go`：

```go
type Backend interface {
    Name() string
    Enumerate(ctx context.Context) ([]model.Device, error)
    Capabilities(ctx context.Context, device model.Device) (model.DeviceCapabilities, error)
    ReadInput(ctx context.Context, device model.Device, scheme model.InputScheme) (model.InputObservation, error)
    SetInput(ctx context.Context, device model.Device, scheme model.InputScheme) (model.WriteReceipt, error)
}
```

`DeviceCapabilities`含`protocols`、`can_read_input`、可选`unavailable_reason`；它报告当前能力，不能根据操作系统或品牌推断实机兼容性。`WriteReceipt`只有`sent_at`；只有`err == nil`且`SentAt`非零时，回执才能作为传输提交的发送证据，不能证明读回或画面成功。

`SetInput`返回非nil错误时，即使同时返回非零回执，该回执也不作为已发送的证明；错误或零回执同样不能证明未发送。后端必须保留实际失败阶段与原因。若无法证实在发送前失败（例如提交后等待完成时超时），上层用已有`DisplayResult`表达`state:unknown`，保留原分类错误和已有观测，不凭失败回执填充`sent_at`、不标记`sent`或`confirmed`，也不盲目重试。只有明确未发送的失败（例如设备I/O前方案校验拒绝）才可据此认定本次未发送并报告`failed`。这一不确定性由现有状态和错误表达，不要求改变`(WriteReceipt, error)`签名或增加字段。

`ReadInput`和`SetInput`的后端实现每次必须在设备I/O前调用`scheme.Validate()`，上层配置校验不替代该边界责任；`Enumerate`和`Capabilities`不接收输入方案。所有阻塞调用响应取消/截止时间，闭合本次句柄、子进程和缓冲区生命周期；枚举返回描述信息，不能让原生裸句柄跨调用泄漏。后端不承担跨入口并发调度、自动无限重试、自动降级或画面判断。上层单一写入者串行访问设备并管理有限重试。可注入传输在平台实现构造时提供，公共接口不强制原生API、GUI或第三方依赖。04/05可在macOS宿主用注入测试验证参数、分派和清理；注册、原生接线和实机访问不属于03。

## 5. 事件来源与消息

定义位置为`internal/events/types.go`：

```go
type EventSource interface {
    Name() string
    Capabilities(ctx context.Context) ([]model.EventCapability, error)
    Run(ctx context.Context, out chan<- model.Event) error
}
```

能力项包含`kind`、`available`、`reason`和`filter_fields`。是否可用以`available`为准：`false`时`reason`必须为非空字符串；`true`时`reason`可以省略/为空，也可补充能力边界说明，不因非空而变成不可用。此规则与`EventCapability.Validate`一致，不增加可用时必须清空原因的约束。筛选字段为非重复合法标识，可用字段由平台真实能力列出，例如`usb.vendor_id`、`usb.product_id`、`usb.serial`、`usb.path`、`name`、`source`、`attributes.key`。接口存在不代表系统支持所有事件；无动态插件承诺。

规范种类为`usb_added`、`usb_removed`、`session_started`、`resume`、`custom`、`timer`。`Event`含`id`、`kind`、`source`、`observed_at`，可选`name`、`usb`、`attributes`。USB字段含16位`vendor_id`/`product_id`以及可选`serial`/`path`，空序列号合法；USB事件必须有USB字段，其他事件不得带USB字段。custom/timer必须有有效名称。属性最多64项，键遵循标识规则，每值最多1024字节。`Event.Validate`与`EventCapability.Validate`是统一校验入口。

`Run`阻塞运行，输出通道由调用者拥有，来源不得关闭；发送时也须响应取消，返回前清理监听、子进程及自身协程。取消错误应可被`errors.Is`识别。事件只记录观察事实，过滤、防抖、去重、启动补扫和规则执行留后续任务。`EventMessage{version,event}`是版本化事件序列化容器，`Validate`检查协议版本及事件；不表示03已实现订阅或网络推送。

## 6. 本机IPC与私有端点契约

这里只定义值类型、无副作用的解码/校验和传输规范，不实现监听、连接、后台启动、认证存储或平台权限。

| 常量 | 值与语义 |
| --- | --- |
| `ProtocolVersion` / `EndpointVersion` | 均为1，配置版本2与协议版本独立 |
| `MaxMessageBytes` | 1048576，单帧JSON正文的最大字节数 |
| `ConnectTimeout` | 2秒，连接预算 |
| `DefaultRequestTimeout` / `MaxRequestTimeout` | 10秒 / 30秒，服务器操作含排队的总预算 |
| `ResponseTimeout` | 35秒，单次收/发帧的硬期限；客户端等待响应也不超过此值 |

后台只通过`net.Listen("tcp4", "127.0.0.1:0")`申请随机本机端口；发现文件记录实际非零端口。禁止通配地址、外网地址、DNS名称或IPv6。每连接只处理一条请求和一条响应，然后关闭；不复用连接，不在此版本建立订阅流。帧由4字节网络序无符号长度N，加N字节UTF-8 JSON对象组成，N须在1至1048576之间；读取长度后先校验再分配正文缓冲。连接/帧截止时间都必须设置，部分帧、超限、认证拒绝后关闭连接，不允许无限阻塞或无界读取。35秒是硬上限，取消或更早截止时间优先；35秒不是执行预算扩展。

私有端点位置为`filepath.Join(os.UserConfigDir(), "DisplayKVM", "runtime", "endpoint.json")`；`EndpointPath(configDirectory string) string`只拼接路径，调用者须先确认配置目录取得成功且非空，不得回退当前目录或共享临时目录。`Endpoint`字段为`version`、`protocol_version`、`network`、`address`、`token`、`pid`、`started_at`，`Validate`只检查格式，不校验所有权或实际监听进程。

每次后台启动用`crypto/rand`生成32个随机字节，编码为43字符、不带填充的标准base64url令牌，并在重启时更换。请求携带令牌，服务器用恒定时间比较验证其与当前端点令牌一致；格式校验不等于认证成功。禁止记录、错误回显或把令牌放入命令行参数。PID和启动时间用于诊断，不可代替认证或私有文件检查。回环地址只限制网络可达范围，不能隔离同机用户。

平台接线必须在读写前确保父目录和文件仅当前用户可访问，防止符号链接替换及其他用户可写路径；Unix目录0700、文件0600并核查所有者，Windows必须建立和核验当前用户私有ACL，不能用`os.Chmod(0600)`冒充。私有性无法保证时拒绝启动/连接并返回`permission_denied`，不得静默放宽权限。原子发布、并发启动保护、端点回收及真实ACL验证由后台任务实现；03不声称这些安全能力已经落地。

`Request`字段为`version`、`id`、`token`、`operation`及可选`switch`或`event`。`Response`字段为`version`、`id`、`operation`及对应负载或请求级`error`：

| `operation` | 请求负载 | 成功响应负载 |
| --- | --- | --- |
| `switch` | `switch`，内层ID与外层ID相同 | `switch_result`，其中`request_id`与外层ID相同；逐屏结果保留部分失败 |
| `enumerate` | 无 | `devices`，空集合可省略 |
| `event_capabilities` | 无 | `event_capabilities`，空集合可省略 |
| `emit_event` | `event`，只接受`custom`，禁止伪造平台系统/USB事件 | `accepted:true`，只确认事件已接受，不代表发生切换 |

一个消息不能混合不同操作负载；请求级错误不携带成功负载。未知操作可用原操作名关联错误响应。`Request.Validate() error`和`Response.Validate() error`检查消息形状，`DecodeRequest(data []byte) (Request, error)`与`DecodeResponse(data []byte) (Response, error)`先限大小，再严格解码和校验。它们接受已拆帧正文，不读取网络、不校验令牌真实性；解码失败返回零值和错误。服务器在版本/结构识别及认证后才允许产生业务动作，客户端必须另行核对响应ID与操作。不能获得可信关联ID的非法帧可以关闭连接；有有效关联的已认证请求应返回明确版本、操作或参数错误，不生成孤立逐屏结果。

后台未启动、端点过期或连接不可用时返回`daemon_unavailable`并提示“后台未启动或不可用，请手动启动DisplayKVM后台后重试”。不自动拉起后台，不回退CLI直接操作显示器。CLI与GUI共用登录后用户会话后台的单一写入者；登录前、登录窗口及注销后能力未获本批实现授权。

## 7. 验证范围与保留缺口

行为测试位于`internal/model/contracts_test.go`和`internal/config/config_test.go`，先于实现落稿。覆盖非法/重复映射与引用、硬件提示重复/空值、配置往返、v1语义保留与确定性迁移、未知版本/协议、严格JSON/大小/数值边界、I/O根因、请求关联、逐屏发送/确认/未知状态、旧观测拒绝、端点格式及事件字段。

实施者未运行测试、构建或lint。唯一计划定向命令为`go test ./internal/model ./internal/config`，由03独立验收会话在实施停止写入后执行一次，并只读评审公共契约。最低Go、真实IPC/认证/ACL、Windows/macOS原生接线、显示器控制及硬件组合都未验证；模拟和静态接口不能作为项目硬件支持证据，实机与全量验证仍留24。

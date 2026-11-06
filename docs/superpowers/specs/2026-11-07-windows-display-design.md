# Windows 原生显示控制设计

## 目标

补齐已有 Windows Backend 的原生 Transport，实现枚举、真实能力读取、当前输入源读取及配置指定输入切换；接通服务启动。
无需外部控制程序或新增第三方依赖。

## 实施前现状

`internal/backend/windows/backend.go` 已有 Transport、能力筛选、操作前重新枚举与身份核验、同步取消后的结果和句柄清理；`NewNative` 当前未接原生传输。
`cmd/displaykvmd` 已按 Windows 路由，`controltools` 的旧 Unsupported 门禁须按实际调用链最小修正。
`cmd/displaykvmd/main.go:126` 先调用 `openUnifiedSettings`，设置存储的 `OpenWriter` 调用 `acquireSettingsLock`；`internal/settings/lock_unsupported.go` 的 `!darwin && !linux` 构建约束覆盖 Windows，锁获取及检查均返回“本平台尚不支持完整配置写锁”，此门禁早于 IPC 和原生显示后端。
`cmd/displaykvmd/main.go:138` 在创建显示 Backend 前调用 `ipc.Acquire`，而 `internal/ipc/private_unsupported.go` 的 `!darwin && !linux` 构建约束覆盖 Windows，`privateRuntime`、`privateFile`、`privateLock` 均明确拒绝，因此仅修工具就绪门禁仍无法启动服务，须补齐 Windows IPC 私有权限与单实例适配。
`model.WriteReceipt` 只有 `SentAt`，`SetInput` 调用后错误由 switcher 处理为 unknown，不另造发送标记协议。

## 方案

采用 Windows 自带 user32/dxva2 的 Monitor Configuration API，用 Go 标准库 syscall 绑定。
复用现有 Backend，仅增加可注入系统调用边界及 Windows 实现，非 Windows 原生入口继续明确不可用。
相比打包第三方控制程序，该方案无新增下载、许可或外部工具路径；相比重写后端，保留已验证业务规则。
设置写锁：Windows 提供独立平台实现并从 unsupported 构建条件排除，沿用现有 `acquireSettingsLock`/`checkSettingsLock` 接口、私有哈希锁文件布局、身份核验与 Writer 生命周期；使用系统非阻塞独占文件锁，不引第三方依赖、不把锁检查改为空操作；锁冲突沿用 `CodeInvalidRequest` 语义，设置事务 Revision、保存三态和配置格式不变。
设置锁核验完整目标路径的祖先链，任何启用目录大小写敏感规则的祖先均拒绝，以保持锁键稳定；沿用不支持 UNC、设备路径、8.3 短路径及重解析点的既定边界，不新增用户修复 ACL 流程。

## 身份与安全

使用原生监视器设备接口路径关联逻辑与物理显示器；不把裸句柄、枚举序号、友好名称伪作稳定设备身份，不伪造 EDID 或序列号。
无法唯一关联的克隆显示或多物理显示器拒绝控制，避免误切屏。
操作前仍走现有重新枚举核验。VCP 输入代码 `0x60`，值使用已有配置/设备能力，不猜端口编号。
IPC 私有目录/文件限定当前 SID；系统祖先与私有对象分层核验，不把常规 SYSTEM/管理员祖先 ACE（访问控制项）误作私有目录规则，也不允许任意主体改变已选择路径；同 SID 恶意进程及可绕过权限的管理员沿现有权限模型边界处理，无法核验的路径拒绝，不用缓存“验证过”路径代替句柄所有权。

## 资源与错误

句柄所有权明确，任何成功取得的物理句柄最终都释放；部分枚举、读写、清理失败保原错误，不吞错误。
Win32 同步调用无法强制抢占，取消只能在原调用返回后观察，不能提前释放在用句柄或制造遗弃 goroutine。
成功读取保观测；写中取消返回零回执并沿上层 unknown 语义处理，禁止自动重复发送。
IPC 采用 Windows 自带 SID（安全标识符）/ACL（访问控制列表）与文件句柄互斥，复用 Endpoint、auth、原子写及 Release；新建私有目录使用精确安全描述符，既有宽松权限、foreign owner（非当前用户所有者）或 reparse（重解析点）目录及文件须拒绝，不能用 Chmod 伪造 Windows 私密性，也不擅修用户已有 ACL。
Windows IPC 沿用现有 Endpoint 认证材料、配置快照与永久 `instance.lock` 协议；`privateRuntime` 改为返回目录及 release 闭包，`Acquire` 失败释放、成功将 lease（目录持有凭据）交 `Instance.Close`，关闭顺序为自有文件清理→锁句柄关闭→目录 lease 释放；`LoadEndpoint` 与 `LoadConfiguration` 只在读取/复核期间持有 lease。
Windows 通过不共享删除的目录句柄固定祖先，检查真实 SID/ACL/非 reparse；Unix release 为空操作并保持原目录检查。
新增 `createPrivateTemp`/`replacePrivate`/`removeOwned` 平台操作接缝，Windows 使用显式 owner/DACL 创建临时文件、句柄身份核验后同目录替换、按句柄删除同身份自有文件；非 Windows 承接原 CreateTemp/Rename/SameFile 清理，不改变协议/默认行为；只验证既有 ACL，不修改用户已有宽松或其他主体对象。
为兼容仍持有旧端点的读取者，Windows 端点替换使用 `SetFileInformationByHandle` 的 `FileRenameInfoEx`（信息类值 `22`），将 `FILE_RENAME_INFO` 的 `DWORD Flags` 设为 `REPLACE_IF_EXISTS | POSIX_SEMANTICS`（`3`）；旧读取者仍可读旧对象，新路径读取新对象，不回退到普通 rename；原身份核验、lease、锁与认证格式不变。
IPC 扩展替换的局部门槛为 Windows 10 1709（OS build 16299），但设置锁使用的 `FileCaseSensitiveInfo`（信息类值 `23`，用于查询目录大小写规则）自 Windows 10 April 2018 Update，即 1803（OS build 17134）正式支持，因此整体候选最低正式基线为 Windows 10 1803（OS build 17134）及以后；不能从 `GetFileInformationByHandleEx` 函数早于该版本可用推断该信息类也受支持。
本地文件系统须支持目录大小写规则查询及端点扩展替换，仅“本地磁盘”不保证支持；能力不足时拒绝并返回原生失败，替换失败时保留旧端点，不静默降级。Windows 各版本（包括 Windows 11/NTFS）、驱动及真实系统行为仍未实机验证，不能按版本号认定运行支持已确认。

## 范围

改 Windows 显示后端与必要工具就绪门禁及直接相关测试/平台说明；不改 mac/Linux 控制、UI、配置格式、启动开关语义、公共返回协议、依赖/CI 或发布设置。
为接通服务启动，补齐 Windows IPC 平台适配、必要共享操作接缝与测试，不跳过安全检查、不重写共享 IPC 协议、不新增依赖或全局权限，保留 mac/Linux 共享行为。
IPC 文件范围包括 `internal/ipc/{endpoint.go,configuration.go,private_unix.go,private_unsupported.go,private_windows.go,private_portable.go,ipc_test.go,private_windows_test.go}` 及确有必要的私有布局测试。
设置写锁范围为 `internal/settings/lock_windows.go`、`internal/settings/lock_unsupported.go` 构建条件及直接相关 Windows 锁测试；仅在调查证明必要时调整最少共享接点，不泛改 Store。
旧 mac 应用/归档/历史失败证据不动。Windows 功能作为后续修订，不并入任务 24 的纯验证。
Linux 未实机测试已获接受，文档如实标注；这不免除 Linux 实际工具载荷的缺口，本修订不做 Linux 功能。

## 验收

主机注入系统 API 边界测试必须执行真实适配算法，覆盖能力、映射、句柄、错误、取消和调用参数。
IPC 验证分为主机可控算法测试和 Windows 专用行为测试；保留跨平台锁/换轮/快照/关闭行为，仅权限位等 POSIX 特定断言分离；Windows ACL/共享删除/跨进程互斥/原子替换需 Windows 实机运行，本机仅编译其专用测试，不记作运行通过。
设置写锁验收涵盖同/跨进程互斥、关闭后可重开、身份替换拒绝、失败释放，执行主机既有 settings 回归并双架构编译 Windows 专用测试；真实 Windows 锁、文件替换和目录同步仍需 Windows 实机验证，不记作已运行或通过。
Windows amd64/arm64 无 cgo 交叉编译 CLI/daemon 及 Windows 专用测试。
编译通过不等于 Windows 实机、显示器 DDC/CI 链路或桌面安装包已验，未实机项如实文档标注。
当前原生后端、Resolver、IPC、设置写锁的独立静态审查及已发现 Critical/Important（关键问题）均已闭合，无待处理项；主机七包回归返回码 0、7 包通过，427 个叶测试通过、1 个叶测试跳过、66 个父级测试通过，无失败，先前原生后端诊断失败项本次通过，父级与叶测试不合并计数，证据见[主机结果](../../verification/2026-11-07-windows-display/host-results.json)和[命令元数据](../../verification/2026-11-07-windows-display/host-tests.meta.json)。
原生后端与 IPC 的 Windows amd64/arm64 四条测试编译命令，以及设置专测双架构编译、三入口程序双架构共八条后续命令均返回码 0，见[两包测试编译记录](../../verification/2026-11-07-windows-display/native-ipc-windows-builds.results.json)与[后续构建记录](../../verification/2026-11-07-windows-display/program-builds.results.json)。六份专测仅编译、六个程序均未运行；最终带许可 ZIP 已核验，路径、完整哈希与许可材料见[本轮摘要](../../verification/2026-11-07-windows-display/summary.md)，手动运行见[候选说明](../../usage/windows-candidate.md)。主机注入 API 测试不记作 Windows 运行，真实 ACL/锁/替换/启动/DDC/CI/桌面/WebView2/安装均未验证，Windows 登录项管理仍未实现，候选程序集非完整安装包。

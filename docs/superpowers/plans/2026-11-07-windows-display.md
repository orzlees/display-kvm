# Windows 原生显示控制实施计划

> 执行方式：按现有子代理实施、独立只读验收和独立命令验证分工；本机非 Git，不创建 worktree 或提交。不把新增实现并入任务 24。

- 目标：补齐 Windows 原生显示控制并接通服务启动，见[设计目标](../specs/2026-11-07-windows-display-design.md#目标)。
- 架构：复用现有 Backend 与 Transport 边界，见[设计方案](../specs/2026-11-07-windows-display-design.md#方案)。
- 技术栈：Windows 自带 user32/dxva2 Monitor Configuration API 与 Go 标准库 syscall，见[设计方案](../specs/2026-11-07-windows-display-design.md#方案)。
- 规格：[Windows 原生显示控制设计](../specs/2026-11-07-windows-display-design.md)。

## 约束

仅本次直接相关文件；无新依赖/全局配置/用户数据/硬件调用；已有受影响实现优先复用。
中文注释解释同步取消、句柄所有权和防误切条件；保存不启停当前服务、退出 UI 不停止服务等既有设置契约保持。

## 结构与步骤

1. 核对已有 Transport、`NewNative` 及 controltools 启动门禁，研究官方 Win32 ABI，基线存 `docs/verification/2026-11-07-windows-display/baseline.json`。
   完成标准：必要改动范围与接口约束明确，无未知第三方依赖。

2. Windows 适配：`internal/backend/windows/backend.go` 最小接线；新增 `native.go`（可注入原生适配算法）、`native_windows.go`（真实 syscall/DLL 边界）、`native_other.go`（其它平台明确不可用）。
   对应 `native_test.go` 及必要 Windows 专用布局/构造测试。
   保既有 Transport 签名；同步取消/写后 unknown 沿现有契约。

3. 工具就绪：只在调用链确认必要时修 `internal/controltools/resolver.go` 与对应测试，使 Windows 自带 API 不被外部工具缺失误拦。
   不清空文档内保存的跨平台 Runtime 字段或引入新文档版本。
   其它门禁按证据最小调整，不顺手改 daemon 路由。

4. Windows IPC 平台接入：补齐服务启动所需的私有权限与单实例适配，沿用现有 Endpoint 认证材料、配置快照与永久 `instance.lock` 协议，保留 `private_unsupported.go` 排除 Windows 的构建约束调整。
   `privateRuntime` 改为返回目录及 release 闭包；`Acquire` 失败释放、成功将 lease（目录持有凭据）交 `Instance.Close`，关闭顺序为自有文件清理→锁句柄关闭→目录 lease 释放；`LoadEndpoint` 与 `LoadConfiguration` 只在读取/复核期间持有 lease。
   Windows 通过不共享删除的目录句柄固定祖先，检查真实 SID/ACL/非 reparse；Unix release 为空操作并保持原目录检查。
   新增 `createPrivateTemp`/`replacePrivate`/`removeOwned` 平台操作接缝，Windows 使用显式 owner/DACL 创建临时文件、句柄身份核验后同目录替换、按句柄删除同身份自有文件；非 Windows 承接原 CreateTemp/Rename/SameFile 清理，不改变协议/默认行为。
   Windows 端点替换采用 `SetFileInformationByHandle` 的 `FileRenameInfoEx`（信息类值 `22`），`FILE_RENAME_INFO` 的 `DWORD Flags` 为 `REPLACE_IF_EXISTS | POSIX_SEMANTICS`（`3`），以保持旧读取者可读旧对象、新路径读新对象；无普通 rename 回退，原身份核验、lease、锁与认证格式不变。
   IPC 扩展替换的局部门槛为 Windows 10 1709（OS build 16299）；整体候选最低正式基线按设置锁更高要求为 Windows 10 1803（OS build 17134）及以后。本地文件系统须支持目录大小写规则查询及端点扩展替换，仅“本地磁盘”不保证支持；能力不足时拒绝并返回原生失败，替换失败时保留旧端点，不静默降级。Windows 各版本（包括 Windows 11/NTFS）、驱动及真实系统行为仍未实机验证，版本号不代表运行支持已确认。
   采用 Windows 自带 SID/ACL 与文件句柄互斥，复用 Endpoint/auth/原子写/Release；新建私有目录使用精确安全描述符，既有宽松权限、foreign owner 或 reparse 目录及文件须拒绝，不用 Chmod 伪造私密性；只验证既有 ACL，不修改用户已有宽松或其他主体对象。
   私有目录/文件限定当前 SID；系统祖先与私有对象分层核验，不把常规 SYSTEM/管理员祖先 ACE 误作私有目录规则，也不允许任意主体改变已选择路径；同 SID 恶意进程及可绕过权限的管理员沿现有权限模型边界处理，无法核验的路径拒绝，不用缓存“验证过”路径代替句柄所有权。
   文件范围包括 `internal/ipc/{endpoint.go,configuration.go,private_unix.go,private_unsupported.go,private_windows.go,private_portable.go,ipc_test.go,private_windows_test.go}` 及确有必要的私有布局测试，不跳安全检查、不改共享 IPC 协议、不加依赖或全局权限。
   主机可控算法测试与 Windows 专用行为测试分开，保留跨平台锁/换轮/快照/关闭行为，仅权限位等 POSIX 特定断言分离。
   Windows ACL/共享删除/跨进程互斥/原子替换需 Windows 实机运行，本机仅编译其专用测试，不记作运行通过。

5. Windows 设置写锁启动前置：补 `internal/settings/lock_windows.go` 与 `internal/settings/lock_unsupported.go` 构建条件及直接相关 Windows 锁测试；如调查证明必要才改最少共享接点，不泛改 Store。
   Windows 提供独立平台实现并从 unsupported 构建条件排除，沿用现有 `acquireSettingsLock`/`checkSettingsLock` 接口、私有哈希锁文件布局、身份核验与 Writer 生命周期；使用系统非阻塞独占文件锁，不引第三方依赖、不把锁检查改为空操作。
   核验完整目标路径祖先链，任何启用目录大小写敏感规则的祖先均拒绝，保留稳定锁键及不支持 UNC、设备路径、8.3 短路径、重解析点的边界；`FileCaseSensitiveInfo`（信息类值 `23`）正式支持始于 Windows 10 1803（OS build 17134），不能用查询函数本身更早可用推断该信息类支持。
   锁冲突沿用 `CodeInvalidRequest` 语义；设置事务 Revision、保存三态和配置格式不变。
   验收涵盖同/跨进程互斥、关闭后可重开、身份替换拒绝、失败释放；执行主机既有 settings 回归，Windows 专用测试按 amd64/arm64 双架构编译。
   真实 Windows 锁、文件替换和目录同步仍需 Windows 实机验证，本机不记作已运行或通过。

6. 独立验证：先测试新增行为与既有 windows 后端、controltools/IPC/settings/loginstart/CLI/daemon 受影响边界。
   测试先写、执行归独立命令者；确切包集合依据实际变更冻结。
   用 `go test -race -mod=readonly -count=1 -json` 定向测试。
   用 `GOOS=windows GOARCH=amd64/arm64 CGO_ENABLED=0 go test -c` 和 `go build` 编译 Windows 代码/CLI/daemon，所有产物仅新验证域。
   不得执行 Windows 程序、真实控制/登录项或重跑无关桌面全量。

7. 独立验收与文档：审查实际产物、测试断言/日志和调用链，修复授权范围内确定失败。
   待实现停写并完成验证后，另派更新当前兼容性/使用说明，按实际结果区分原生后端实现状态与 Windows 实机/桌面安装包验证状态，不将交叉编译称硬件支持。
   记录验证 summary，保留旧应用/归档哈希证据，不发布。

## Review Focus

- 克隆/多物理/缺路径/同路径重复：拒绝错误关联和写入。
- 拔插导致重新枚举变化：不得误用旧句柄、描述或顺序。
- 读取成功后取消与写中取消：分别保观测、零回执及未知结果，不自动重发。
- 部分取得句柄或释放失败：所有资源都尝试清理，原错误与清理错误同时可追溯。
- API 不支持/能力损坏/超大长度/同步失败：保阶段与 Win32 错误，不能臆造 VCP 支持。
- IPC 权限：拒绝宽松 ACL、空 DACL、foreign SID（非当前用户安全标识符）及 reparse 目录或文件。
- IPC 单实例：同用户第二个服务必须遇到锁冲突，检查句柄同步持有、释放与替换。
- 平台兼容：原 mac/Linux 共享行为不得退化。

## 证据归属

实施者只写代码与行为测试，不运行命令验证；命令专员运行最小必要测试和双目标交叉编译；独立只读验收者核真实差异与原始证据。
接口/输入变化后仅重验失效证据，不无限扩测，不跨轮重复合计通过数。
无 Windows 环境只限制实机结论，不阻止完成实现与编译。
当前原生后端、Resolver、IPC 及设置锁独立静态审查均已完成，Critical/Important（关键问题）已闭合、无待处理项。主机已执行一次 `go test -race -mod=readonly -count=1 -json ./internal/backend/windows ./internal/controltools ./internal/settings ./internal/ipc ./internal/loginstart ./internal/cli ./cmd/displaykvmd`，返回码 0、7 包通过；427 个叶测试通过、1 个叶测试跳过、66 个父级测试通过，无失败，父级与叶测试不合并计数，原生后端诊断失败项本次通过；证据见[主机结果](../../verification/2026-11-07-windows-display/host-results.json)及[命令元数据](../../verification/2026-11-07-windows-display/host-tests.meta.json)。
原生后端与 IPC 的 Windows amd64/arm64 四条测试编译，以及设置专测双架构编译与三入口双架构程序构建共八条后续命令均返回码 0，见[两包测试编译记录](../../verification/2026-11-07-windows-display/native-ipc-windows-builds.results.json)与[后续构建记录](../../verification/2026-11-07-windows-display/program-builds.results.json)。六份专测仅编译、六个程序均未运行；最终两个带许可 ZIP 的内容、完整性、逐项 SHA 及程序架构/入口已核验，最终路径、完整哈希、许可材料及需求状态见[本轮摘要](../../verification/2026-11-07-windows-display/summary.md)，手动运行见[候选说明](../../usage/windows-candidate.md)。不将主机注入测试写为 Windows 运行通过，真实 ACL/锁/替换/启动/DDC/CI/桌面/WebView2/安装仍未验，Windows 登录项管理仍未实现；候选程序集非完整安装包，不据此将原始全部系列或任务 24 标记完成，文档独立只读验收待进行。

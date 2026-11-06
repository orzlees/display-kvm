# 便携包装

先按[构建说明](../build/README.md)生成二进制，再包装同一运行编号的核心产物；桌面包装读取已经生成的.app。脚本只管理build目录内自己创建的工件，不安装系统服务、不修改用户配置。

```sh
sh packaging/package.sh core --run-id local-core
sh packaging/package.sh desktop --app desktop/bin/DisplayKvm.app --run-id local-desktop
```
Unix核心便携包采用tar.gz，Windows核心包采用zip；macOS桌面包保留完整.app。每份包须包含项目[MIT许可](../LICENSE)、实际Go与模块许可文本、实际工件清单及SHA-256侧文件。

许可辅助程序读取二进制构建信息，并按相同目标和参数查询实际包依赖及模块替换来源。声明模块列表不能代替实际载荷；缺少许可、来源或匹配信息时停止包装。基础[NOTICE](NOTICE)须与生成的逐工件许可目录一起保留。

安装时先校验哈希，解压到新目录，再由使用者选择程序位置。macOS应用可复制到使用者选择的Applications目录；核心CLI和后台分别启动。macOS arm64包包含三个Go入口、固定源码编译的m1ddc以及来源/源码/许可证清单；原生工具和应用均须通过签名与复制字节校验。当前为本地候选，不代表正式签名公证。

升级前停止程序并备份配置，在新目录验证新版本后再替换程序。卸载只移除程序与本次解压目录，保留用户配置及备份。回滚时需使用旧程序能够读取的配置备份；当前v3配置不能假定可由更旧程序直接读取。程序撤回也不能撤回已经发出的显示器输入切换。

Windows已加入原生实现，无需随包附带外部显示控制工具；原生后端、工具就绪解析器、IPC及设置写锁已完成独立静态审查，无待处理关键问题。主机七包回归、六份Windows双架构专测编译、三入口双架构共六个程序构建及最终带许可ZIP核验通过，exe均未运行。最终交付推荐`build/out/windows-native-20261107/with-notices/`下的amd64/arm64两个ZIP；路径、完整哈希及实际模块许可材料见[本轮摘要](../docs/verification/2026-11-07-windows-display/summary.md)，手动使用见[候选说明](../docs/usage/windows-candidate.md)。初始七文件ZIP保留为历史归档，不作为最终推荐；许可材料已收集不表示法律审查已完成。设计边界见[本轮设计](../docs/superpowers/specs/2026-11-07-windows-display-design.md)与[实施计划](../docs/superpowers/plans/2026-11-07-windows-display.md)。Windows真实ACL、锁、端点替换、启动、DDC/CI、桌面、WebView2及安装均未实机验证。

本次Windows接入的整体候选最低正式基线为Windows 10 1803（OS build 17134）及以后，本地文件系统须支持目录大小写规则查询及端点扩展替换；仅本地磁盘不保证支持。能力不足时拒绝并返回原生失败，替换失败时保留旧端点，不静默降级；Windows各版本（包括Windows 11/NTFS）、驱动及真实系统行为仍未实机验证，不能仅凭版本号确认可运行。

Windows核心便携包与完整桌面安装包是不同交付物；核心CLI/后台、单独桌面.exe及手动启动流程均不能当作完整安装包。桌面通过同用户系统配置目录发现后台，无需与后台或CLI放在同一目录，且当前不会自动启动相邻程序。若手工摆放已取得的程序，可采用以下推荐布局：

```text
DisplayKvm.exe
displaykvmd.exe
core/
  displaykvm.exe
```

Windows文件名大小写不敏感，桌面`DisplayKvm.exe`与CLI`displaykvm.exe`不能仅靠大小写区分后放在同一目录。先手动运行后台再打开桌面，PowerShell示例见[启动后台和连接](../docs/usage/README.md#启动后台和连接)。Windows登录项管理仍未实现；WebView2及Windows桌面真实运行尚未验证，不据此推断系统已具备或会自动安装运行条件。

macOS Intel后端仍不可用；既有macOS arm64 App/归档的交付记录仍有效，原硬件/系统验收仍未完成。Linux未实机测试的限制不代表实际工具载荷已经具备。实际安装、运行、签名和硬件状态以[兼容表](../docs/compatibility.md)及[发布记录](../docs/releases.md)为准。

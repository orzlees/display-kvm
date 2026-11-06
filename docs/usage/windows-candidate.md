# DisplayKvm Windows 候选程序

本候选集包含桌面界面、后台和命令行程序，amd64 与 arm64 分别提供，按 Windows 系统架构选择。

## 运行条件

Windows 10 1803（OS build 17134）及以后。本地文件系统须支持配置锁使用的目录规则查询和后台端点替换；无法核验时程序会拒绝启动，不自动修改现有目录权限。桌面使用系统 WebView2 环境。

显示器需启用 DDC/CI，显示器、转接链路和驱动需支持输入源控制。程序使用 Windows 自带接口，无需另装显示器控制工具；无法唯一匹配的显示器不会执行切换。

## 文件布局

DisplayKvm.exe 为桌面界面，displaykvmd.exe 为后台，core/displaykvm.exe 为命令行。请完整解压后使用。桌面通过同一用户配置环境中的端点连接后台，当前不会自动启动后台。

## 手动启动

准备一份完整的 displaykvm-settings/v1 设置文件。在解压目录打开 PowerShell，将示例替换为该文件在本机的绝对路径：

```powershell
.\displaykvmd.exe -settings 'C:\path\displaykvm-settings.json'
```

保持后台运行，再从资源管理器打开 DisplayKvm.exe。两者使用同一 Windows 用户。退出桌面不会停止后台；需要停止后台时，在启动它的终端按 Ctrl+C。

Windows 开机启动管理尚未实现，本候选采用手动启动。

## 验证范围

本轮包含主机侧行为回归及 Windows 交叉编译。Windows 实机中的显示器控制、权限、互斥、端点替换、桌面与 WebView2 运行仍未验证。候选集供上述后续验证使用，不能据编译成功承诺所有显示器或链路可用。

许可文件和 SHA256SUMS 校验清单随候选集提供。

# DisplayKvm Windows 候选程序

本候选集包含桌面界面、后台和命令行程序，amd64 与 arm64 分别提供，按 Windows 系统架构选择。候选包名为 `displaykvm-desktop-<version>-windows-<arch>.zip`；实际工件与下载状态见[发布记录](../releases.md)。

## 运行条件

Windows 10 1803（OS build 17134）及以后。本地文件系统须支持配置锁使用的目录规则查询和后台端点替换；无法核验时程序会拒绝启动，不自动修改现有目录权限。桌面使用系统 WebView2 环境。

显示器需启用 DDC/CI，显示器、转接链路和驱动需支持输入源控制。程序使用 Windows 自带接口，无需另装显示器控制工具；无法唯一匹配的显示器不会执行切换。

候选 `v0.0.1-rc.20261109.1` 将包含 NVIDIA LG 实验性私有输入通道。该通道仅限 Windows amd64，需已安装 NVIDIA 驱动及 Windows 系统目录 `System32` 中的 `nvapi64.dll`，并能唯一匹配 GDI 名称、完整 EDID、单 GPU 和单 bit 输出目标。它尚未在 NVIDIA 硬件验证，不支持可靠读回或五秒预览，也不会在失败后自动回退标准 VCP。

## 文件布局

`displaykvm.exe`：桌面界面；`displaykvmd.exe`：后台；`displaykvm-cli.exe`：命令行。三者位于同一目录，必须来自同一个版本、架构和源码构建。核心包中原有的 `displaykvm.exe` 是命令行，复制进入桌面候选时更名为 `displaykvm-cli.exe`；不要用核心包中的同名文件覆盖桌面界面。Windows 文件名大小写不敏感，不能以大小写区分桌面与命令行程序。

解压到同一受支持的本地目录，保留三程序及许可证、清单和校验文件。桌面通过同一用户配置环境中的端点连接后台。

## 手动启动

准备一份完整的 displaykvm-settings/v1 设置文件；文章参数仅作字段示例，见[配置契约](../contracts.md#文章参数与绑定字段示例)。在解压目录打开 PowerShell，将示例替换为该文件在本机的绝对路径：

```powershell
.\displaykvmd.exe -settings 'C:\path\displaykvm-settings.json'
```

手动验证时先启动 `displaykvmd.exe`，保持后台运行，再从资源管理器打开 `displaykvm.exe`。两者使用同一 Windows 用户。退出桌面不会停止后台；需要停止后台时，在启动它的终端按 Ctrl+C。

登录启动由用户在现有启动设置中分别启用后台和桌面。该功能在当前用户登录后生效，不是登录前系统服务，也不意味着桌面程序会自动拉起后台。

登录项使用当前用户的 HKCU Run，只管理本应用值。路径或文件身份不符合安全校验时会拒绝设置；请保留原始文件布局。Windows 原生登录行为、驱动、WebView2、睡眠唤醒与真实硬件仍需实机验收。

## 验证范围

本轮包含主机侧行为回归及 Windows 交叉编译。Windows 实机中的显示器控制、权限、互斥、端点替换、桌面与 WebView2 运行仍未验证。候选集供上述后续验证使用，不能据编译成功承诺所有显示器或链路可用。

许可文件和 SHA256SUMS 校验清单随候选集提供。

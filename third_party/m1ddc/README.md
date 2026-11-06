# m1ddc 固定版本源码

此目录保存 macOS 客户端随包工具所需的官方 m1ddc 源码，供构建机生成客户端携带的程序。客户端运行时应使用随包程序，Apple Command Line Tools（苹果命令行开发工具，简称 CLT）及 clang 编译器只属于构建机依赖，不要求最终用户安装或自行寻找 m1ddc。

## 来源与完整性

- 官方仓库：<https://github.com/waydabber/m1ddc>。
- 固定版本：`v1.3.1`，发布于 `2026-10-29`。
- 固定提交：`7339ceebb03f691b1eaeb8471dca301d215d19c8`。官方 GitHub tag 引用直接指向该提交，该版本的 release 没有二进制附件。
- `upstream/` 中九个文件均来自上述固定提交的官方 raw URL，按原始字节保存，没有本地修改。仅纳入原始 Makefile、完整 LICENSE、上游 README，以及 Makefile 所需的三个源码文件及其三个本地头文件，不含 `.git` 或外来二进制。
- `SOURCE.json` 记录每个文件的上游路径、固定提交 URL、实际 SHA-256、字节数和末尾换行状态。`headers/ioregistry.h` 与 `headers/utils.h` 上游原件没有末尾换行，必须保留这一状态，避免格式化后校验不一致。
- 许可为 MIT，版权为 `Copyright (c) 2021 waydabber`；完整许可保存在 `upstream/LICENSE`，再分发时须随程序保留版权及许可声明。

## 构建边界

原始 Makefile 以 clang 编译 `sources/i2c.m`、`sources/ioregistry.m`、`sources/m1ddc.m`，头文件搜索路径为 `headers/`，编译参数包含 `-Wall -Werror -Wextra -fmodules`，链接参数为 `-framework CoreDisplay`。源码导入系统模块 Darwin、Foundation、IOKit、ApplicationServices、CoreGraphics；`headers/ioregistry.h` 还导入 `<CoreGraphics/CoreGraphics.h>`。

构建接线应把目标架构设为 `arm64`，并将目标文件、依赖文件、模块缓存和最终程序全部放在 `upstream/` 以外的构建目录。以下仅说明命令形态，`<源码目录>` 指本目录内 `upstream/` 的绝对路径，`<构建目录>` 指项目选定的独立输出目录；本次源码纳入没有执行这些命令：

```text
clang -arch arm64 -c <源码目录>/sources/<名称>.m -o <构建目录>/<名称>.o -I <源码目录>/headers -Wall -Werror -Wextra -fmodules -fmodules-cache-path=<构建目录>/module-cache -MMD
clang -arch arm64 -framework CoreDisplay <构建目录>/i2c.o <构建目录>/ioregistry.o <构建目录>/m1ddc.o -o <构建目录>/m1ddc
```

第一条命令中的 `<名称>` 分别为 `i2c`、`ioregistry`、`m1ddc`。原始 Makefile 保留用于来源追溯；直接在其目录运行默认目标会写入 `.objects/` 和 `m1ddc`，因此实际构建必须使用独立输出目录或在构建目录中的源码副本进行。客户端打包接线由项目构建流程负责，本目录不执行上游安装规则，不向 `/usr/local/bin` 安装工具。

## 平台与验证边界

m1ddc 面向 Apple Silicon Mac（`arm64`），上游明确不支持 Intel Mac（`x86_64`）。上游声明支持通过 USB-C/DisplayPort Alt Mode 及受支持的内置 HDMI 端口连接的外置显示器；具体 DDC/CI 功能仍取决于显示器、端口和连接链路。

`sources/ioregistry.m` 调用私有 CoreDisplay 接口 `CoreDisplay_DisplayCreateInfoDictionary` 获取显示器信息；`headers/ioregistry.h` 和 `headers/i2c.h` 还声明私有 IOAVService 接口，用于访问显示器及进行 I²C 通信。系统升级可能改变这些接口或设备行为，不能据源码纳入推断所有 macOS 版本及硬件均可用。

本次仅核对官方 tag/commit、依赖文件树及逐字节 SHA-256，没有编译、运行程序或访问显示硬件，也未验证 macOS 13 的兼容性。后续构建与硬件验收需单独完成。

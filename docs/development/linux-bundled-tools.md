# Linux 随包工具构建契约

状态：已提供原生生成器、源码锁定文件和核心包装校验。当前工作机为macOS，未安装Linux容器或交叉工具链，尚未生成或验收Linux真实载荷。以下命令是原生Linux执行入口，不是本次执行结果；Linux安装、GUI、硬件和ABI兼容仍未验证。

## 固定来源

| 工具 | 版本及提交 | 许可 |
| --- | --- | --- |
| ddcutil | v3.0.2；5085ae9bb1a8e5bd8db717d80dd80a279a0f05fb | GPL-2.0-or-later |
| busctl、udevadm | systemd v258；781d9d0789379d1ea1f2ecefb804d41e9c8b6c38 | busctl为LGPL-2.1-or-later；udevadm为GPL-2.0-or-later，最终按实际链接对象核完整许可 |

上游签名tag与提交已核对。固定提交的官方codeload归档已读取并计算SHA-256，写入唯一来源锁定文件 `build/linux/source-lock.json`，生成器下载后必须逐字节匹配；不接受自动更新摘要：

| 归档 | SHA-256 |
| --- | --- |
| rockowitz/ddcutil固定提交tar.gz | `c5c271131ba40e009f0a71a04e8856f4ca4647e9e22d38e23344fdc7714fcc28` |
| systemd/systemd固定提交tar.gz | `9421b006c0e4c816fe06c004204091b09fd628891403dcdf6511ceb8ce384f13` |

载荷保留完整上游归档、实际传递依赖的Debian原始二进制包及完整对应源码（`.dsc`、原始归档、发行版补丁），记录每份归档的官方来源URL和SHA-256。Debian运行库版本必须与原生机已安装版本一致，不能用另一个版本的源码替代。同次固定systemd源码构建的私有共享库复用已登记的systemd归档和完整许可，不伪造Debian原包来源。

## 原生环境与依赖

分别使用原生Linux x86_64和aarch64构建主机，固定Debian12、glibc2.36、Python3.11环境；生成器拒绝其他发行版、较新glibc和架构不符的主机。这是构建边界，尚不是已支持系统承诺；systemd258上游要求Linux内核至少5.4。macOS及缺少原生环境时不能以纯Go交叉编译冒充这些原生工具成功。

原生镜像须预先具备以下工具和开发文件。生成器只检查和使用现有能力，不调用安装、APT索引更新、系统安装、ldconfig或系统服务操作：

- C/C++编译与Autotools：GCC、Make、Autoconf、Automake、Libtool、m4，以及标准Linux开发头文件。
- systemd构建：Meson至少0.62、Ninja、gperf、Python3.11和Jinja2。
- 元数据与归档：pkg-config、patchelf、binutils的readelf、ldd、getconf、dpkg-query、dpkg-deb、apt-cache、apt-get、gzip、xz、tar。
- 开发库：GLib、Jansson、ACL、libdrm至少2.4.67、libudev、libcap、libmount/libblkid和libxcrypt。Debian包通常对应 `build-essential autoconf automake libtool pkg-config meson ninja-build gperf python3 python3-jinja2 patchelf binutils dpkg-dev apt gzip xz-utils tar libglib2.0-dev libjansson-dev libacl1-dev libdrm-dev libudev-dev libcap-dev libmount-dev libblkid-dev libcrypt-dev`，最终缺项以固定源码配置错误为准。
- 已经由APT验证的Debian官方二进制和deb-src索引，必须仍包含每个已安装运行库的精确版本。只接受 `deb.debian.org`、`security.debian.org` 的无凭据来源。索引缺失、版本已被镜像淘汰或只配置第三方镜像时停止，不降级版本或隐式切换镜像。CI镜像提供者须固定原生镜像和相应索引，不能只指定浮动的Debian标签。

ddcutil沿用现有裁剪配方，额外关闭构建时间戳以减少时间变化；生成器设置 `NOCONFIGURE=1` 后运行autogen，再显式配置：

```sh
NOCONFIGURE=1 ./autogen.sh
./configure --prefix=/usr --disable-usb --disable-systemd \
  --disable-dbus --disable-x11 --disable-lib --disable-build-timestamp --enable-drm
make -j2
```

生成器直接选取本次构建树中的ELF，不执行install，不指向系统安装位置。该配置保留项目现有I2C DDC/CI路径，关闭USB显示器控制；项目USB事件监听仍由独立事件工具负责。DRM不可关闭，配置后检查 `USE_LIBDRM=1`，防止上游在缺开发文件时静默缩减能力。

构建 ddcutil 3.0.2 还需要 libudev-dev；生成时由现有闭包生成器收集并校验运行库及其 Debian 对应源码、版权声明。

systemd v258使用Meson、Ninja、C编译器、Python/Jinja2、gperf、pkg-config、libcap及libmount等构建依赖。采用上游 `-Dstandalone-binaries=true`，编译实际名为 `busctl` 和 `udevadm` 的目标，从构建目录读取同名产物，随包客户端名称也保持为 `busctl` 和 `udevadm`。该选项只筛选现有目标，不产生 `.standalone` 后缀，也不保证消除systemd私有动态库或完全静态可移植；glibc及其它外部库仍需检查。具体可选依赖须按最终Meson配置和实际产物核验，不能在本机未构建时宣称闭包完整。

完整Meson参数保存在来源锁定文件：禁止自动下载子项目，禁用可选自动功能、测试、文档、bootloader和BPF框架，只编译 `busctl` 和 `udevadm` 两个目标。构建进程使用固定PATH/区域设置、SOURCE_DATE_EPOCH及源码路径映射，不继承个人编译参数、LD_LIBRARY_PATH或包查找路径。配方会记录实际工具及已安装包版本，但尚无两次真实构建的字节一致性证据。

## 原生生成入口与CI交接

在两个独立原生主机、相同源码检出上分别执行，编号相同但本地目录各自独占。不得在同一工作树并发写入同一编号：

```sh
# 原生Debian12 amd64
python3 build/linux/bundle.py generate --arch amd64 --run-id linux-tools-001 --jobs 2
# 另一台原生Debian12 arm64
python3 build/linux/bundle.py generate --arch arm64 --run-id linux-tools-001 --jobs 2
```

成功输出 `build/out/linux-tools-001/.displaykvm-run`（内容为 `displaykvm-linux-tools-v1`）及 `linux/<arch>/`：

```text
ddcutil  busctl  udevadm
lib/<SONAME普通文件>
sources/<完整上游归档及Debian源码/原包>
licenses/<上游许可、每包copyright、Debian公共完整许可>
recipe/bundle.py  recipe/source-lock.json  recipe/environment.json
bundled-tools.json
```

清单最后写入；任意失败保留本次目录及 `work/build.log`，不能覆盖重试，须使用新编号。每次网络获取最多三次重试，归档摘要不符立即停止。此目录仍是原生构建候选产物；硬件或发行版验收不会由生成器自动标为通过。

任务29的CI只需调用上述入口，不需要另一套载荷体系。每个架构仅收集身份文件和 `linux/<arch>/`，排除work目录；必须先用tar封装后再通过工件服务传递，以保留ELF执行位。合并到同一检出的 `build/out/linux-tools-001/`，两份身份文件字节必须一致，禁止用新写身份文件冒充生成器成功。随后使用实际Go1.27.3工具链：

```sh
sh build/build-core.sh --target all --version candidate-001 \
  --run-id core-001 --linux-tools-run-id linux-tools-001
sh packaging/package.sh core --run-id core-001
```

也可只构建 `--target linux-amd64` 或 `linux-arm64` 并提供对应原生载荷。非Linux目标无需此参数。构建记录锁定原生清单摘要；包装再次检查清单与所有文件，工具、库、源码、许可或任何摘要缺失均失败，不能产出只有Go程序的Linux包。`packaging/package.sh`继续复用现有入口，无需新增包装模式。

## 随包与验收

最终客户端的daemon和CLI所在工具目录应与ddcutil、busctl、udevadm相邻；非基线共享库放相邻lib，采用受控相对RUNPATH，不写入构建机绝对路径。运行时通过统一controltools.Resolver按真实程序位置定位，不搜索用户PATH，也不要求终端用户自行下载工具。

在受信任的本地构建产物上用readelf核架构、动态依赖和RUNPATH，ldd核依赖闭包；除约定的系统加载器/glibc外，依赖均须从包内解析。glibc符号需求不得高于最终声明基线。任何缺依赖、错误架构、构建机绝对路径、缺源码或缺许可均不能产出可分发包。每份工具与随包库均记录实际SHA-256、版本、来源、完整对应源码和构建脚本。程序必须恰有一个PT_INTERP；在来源、原始摘要及目标架构已核验的ET_DYN共享库中，允许没有PT_INTERP或恰有一个正确加载器。amd64仅接受 `/lib64/ld-linux-x86-64.so.2`，arm64仅接受 `/lib/ld-linux-aarch64.so.1`；空段、重复段、未知路径和错误架构的加载器均拒绝。共享库可以借此提供直接执行入口，仍可按SONAME供动态链接，不能单凭PT_INTERP把它认定为误收的程序。生成器与独立包装校验采用相同规则，不删除该段或针对单个库放行。

运行基线只豁免 `libc.so.6 libm.so.6 libdl.so.2 libpthread.so.0 librt.so.1 libresolv.so.2 libutil.so.1`，以及架构约定的系统加载器；其他传递依赖均复制为无链接的 `lib/<SONAME>`，实际ELF的唯一SONAME必须与文件名及依赖映射一致。工具RUNPATH必须为 `$ORIGIN/lib`，每个库为 `$ORIGIN`；不接受旧RPATH、绝对依赖、额外系统回退路径或高于GLIBC_2.36的符号。库经patchelf修改前后的摘要都记录；Debian库原字节须存在于所下载精确.deb内。glibc和加载器由宿主提供，不随包混装。

同次构建的systemd私库仅接受 `work/systemd-build` 内目标架构的普通ELF共享库，链接入口和解析后的真实文件都须位于该目录；其他构建树、链接逃逸、同名不同来源或SONAME不匹配均拒绝。清单结构版本仍为1，库记录仅新增可选 `build_path`：它是该构建目录内真实文件的受限相对路径，不含宿主绝对路径。此类库的 `source` 为已核固定归档及完整许可的 `systemd`，`version` 为258，并记录 `architecture`、`original_sha256` 和打包后的 `sha256`；`package`、`package_archive` 必须为空。Debian库继续要求完整原包及对应源码和许可信息，且不得填写 `build_path`。生成器对工具和每个随包库复核传递依赖，非基线依赖均须从随包lib解析；当前尚无两个架构的成功实物证明。

包装校验在macOS也能静态读取两个架构ELF，无需执行它们；原生生成器才对本次可信编译结果执行ldd，并在设置RUNPATH后确认全部非基线解析路径位于随包lib。静态DT_NEEDED检查不能证明所有运行时dlopen路径或硬件路径通过，两Linux环境运行和完整项目验收仍由任务24执行。

Linux内核、I2C驱动/设备节点、访问权限和宿主D-Bus/logind/udev服务是系统运行前提；工具随包不替代这些设施，不安装systemd daemon、不自动更改设备权限或系统服务。

## 当前缺口

尚缺amd64/arm64原生Linux执行入口的实际执行结果、两个架构的三个真实ELF、非glibc共享库闭包及其实际源码/许可输出、目标发行版实机与显示器证据。仓库内生成器和来源锁定文件不能替代这些实物，当前不创建空payload或占位完成清单。macOS上的解析器临时文件测试只能证明路径逻辑，不能证明上述项目通过。Windows显示器控制后端与macOS Intel支持也不能据核心可编译产物标为完成。

独立验收最小范围为一次 `go test ./packaging/notices -count=1 -v`、改动shell的语法检查、Python静态语法检查，以及原生载荷的manifest/lib/source/license定点核对；实施者未执行这些验证。原生生成、两个Linux环境运行与项目全量构建不在本机执行。

官方材料：https://github.com/rockowitz/ddcutil/tree/v3.0.2 、https://github.com/systemd/systemd/tree/v258 。

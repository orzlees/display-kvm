# Linux 随包工具构建契约

状态：构建配方已核对官方固定版本，当前工作机为macOS，未安装Linux容器或交叉工具链，尚未生成或验收Linux真实载荷。以下命令是Linux原生构建机的配方，不是本次执行结果。Linux安装、GUI、硬件和ABI兼容仍未验证。

## 固定来源

| 工具 | 版本及提交 | 许可 |
| --- | --- | --- |
| ddcutil | v3.0.2；5085ae9bb1a8e5bd8db717d80dd80a279a0f05fb | GPL-2.0-or-later |
| busctl、udevadm | systemd v258；781d9d0789379d1ea1f2ecefb804d41e9c8b6c38 | busctl为LGPL-2.1-or-later；udevadm为GPL-2.0-or-later，最终按实际链接对象核完整许可 |

上游签名tag与提交已核对；未取得可验证的官方源码归档SHA-256，因此不提供猜测的归档哈希。载荷构建必须保留实际取得源码的完整清单与摘要、构建脚本和所有许可。

## 原生环境与依赖

分别使用Linux x86_64和aarch64构建主机。建议拟定Debian12、glibc2.36为最低构建基线；这是建议，尚不是已支持系统承诺。macOS及缺少原生环境时不能以纯Go交叉编译冒充这些原生工具成功。

ddcutil构建机需要C编译器、Autotools、pkg-config、GLib、Jansson、ACL和libdrm开发文件。固定源码可采用：

```sh
NOCONFIGURE=1 ./autogen.sh
./configure --disable-usb --disable-udev --disable-systemd \
  --disable-dbus --disable-x11 --disable-lib --enable-drm
make
make DESTDIR="$stage" install
```

stage须由构建调用者设置为独立暂存目录，不能指向系统安装位置。该配置保留项目现有I2C DDC/CI路径，关闭USB显示器控制；项目USB事件监听仍由独立事件工具负责。DRM不可关闭。

systemd v258使用Meson、Ninja、C编译器、Python/Jinja2、gperf、pkg-config、libcap及libmount等构建依赖。采用上游 `-Dstandalone-binaries=true`，从独立暂存产物选取busctl.standalone和udevadm.standalone并按客户端约定命名。standalone静态纳入systemd私有代码，但不代表完全静态可移植；glibc及其它外部库仍需检查。具体可选依赖须按最终Meson配置和实际产物核验，不能在本机未构建时宣称闭包完整。

## 随包与验收

最终客户端的daemon和CLI所在工具目录应与ddcutil、busctl、udevadm相邻；非基线共享库放相邻lib，采用受控相对RUNPATH，不写入构建机绝对路径。运行时通过统一controltools.Resolver按真实程序位置定位，不搜索用户PATH，也不要求终端用户自行下载工具。

在受信任的本地构建产物上用readelf核架构、动态依赖和RUNPATH，ldd核依赖闭包；除约定的系统加载器/glibc外，依赖均须从包内解析。glibc符号需求不得高于最终声明基线。任何缺依赖、错误架构、构建机绝对路径、缺源码或缺许可均不能产出可分发包。每份工具与随包库均记录实际SHA-256、版本、来源、完整对应源码和构建脚本。

Linux内核、I2C驱动/设备节点、访问权限和宿主D-Bus/logind/udev服务是系统运行前提；工具随包不替代这些设施，不安装systemd daemon、不自动更改设备权限或系统服务。

## 当前缺口

尚缺amd64/arm64原生Linux构建环境、真实工具和共享库闭包、对应源码/许可产物、目标发行版实机与显示器证据。macOS上的解析器临时文件测试只能证明路径逻辑，不能证明上述项目通过。Windows显示器控制后端与macOS Intel支持也不能据核心可编译产物标为完成。

官方材料：https://github.com/rockowitz/ddcutil/tree/v3.0.2 、https://github.com/systemd/systemd/tree/v258 。

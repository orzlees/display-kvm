#!/bin/sh
set -eu
umask 077

die() { printf '%s\n' "$*" >&2; exit 1; }

# 与核心构建一样拒绝每层符号链接，包括尚不存在目标的父目录和断链。
# 检查后仍由mkdir独占新应用目录，不能覆盖旧产物或借链接写到项目外。
no_links() {
  checked_path=$1
  while :; do
    [ ! -L "$checked_path" ] || die "路径包含符号链接：$checked_path"
    [ "$checked_path" != / ] || break
    checked_path=$(dirname "$checked_path")
  done
}

# 固定来源、完整许可及每个编译输入都必须是普通非空原件；仅核对清单自报哈希不足以防来源被替换。
# 该函数也用于核对复制件与签名后的工具，失败保留当前独占输出，不接管或清理旧目录。
pin_file() {
  pin_path=$1
  pin_expected=$2
  no_links "$pin_path"
  [ -f "$pin_path" ] && [ -s "$pin_path" ] || die "固定输入缺失、为空或不是普通文件：$pin_path"
  pin_actual=$(/usr/bin/shasum -a 256 "$pin_path")
  pin_actual=${pin_actual%% *}
  [ "$pin_actual" = "$pin_expected" ] || die "固定文件SHA-256不匹配：$pin_path"
}

desktop_root=$(CDPATH= cd "$(dirname "$0")" && pwd -P)
repo_root=$(CDPATH= cd "$desktop_root/.." && pwd -P)
app_bundle="$desktop_root/bin/DisplayKvm.app"
output_set=0
while [ "$#" -gt 0 ]; do
  case "$1" in
    --output)
      [ "$output_set" = 0 ] || die '--output不能重复'
      [ "$#" -ge 2 ] && [ -n "$2" ] || die '--output必须提供非空绝对路径'
      app_bundle=$2
      output_set=1
      shift 2
      ;;
    *) die "未知参数：$1；用法：build-macos.sh [--output 绝对路径]" ;;
  esac
done

if [ "$output_set" = 1 ]; then
  case "$app_bundle" in
    "$repo_root/build/out/"*/DisplayKvm.app) ;;
    *) die '--output必须为项目build/out/单个编号/DisplayKvm.app的绝对路径' ;;
  esac
  candidate_run=${app_bundle#"$repo_root/build/out/"}
  candidate_run=${candidate_run%/DisplayKvm.app}
  case "$candidate_run" in [A-Za-z0-9]*) ;; *) die '构建编号必须以字母或数字开头' ;; esac
  case "$candidate_run" in *[!A-Za-z0-9._-]*) die '构建编号只能含字母、数字、点、下划线、短横线' ;; esac
  [ "${#candidate_run}" -le 64 ] || die '构建编号不能超过64字符'
fi
no_links "$app_bundle"
[ ! -e "$app_bundle" ] || die "应用目标已存在，拒绝覆盖：$app_bundle"

# 候选编号也不复用未知目录，避免与核心构建、旧包装或失败现场混用。
# 无参入口仍选择desktop/bin，但已有应用同样拒绝覆盖。
app_parent=$(dirname "$app_bundle")
if [ "$output_set" = 1 ]; then
  [ ! -e "$app_parent" ] || die "候选构建编号已存在，请保留现场并使用新编号：$app_parent"
fi

# 构建只允许使用已准备的本机工具与桌面模块缓存，不自动恢复依赖、
# 下载工具链、写入根模块或调用 Wails/Task/Node 的全局安装入口。
if [ "$(uname -s)" != "Darwin" ]; then
  printf '%s\n' '此入口仅用于本机 macOS 定向构建。' >&2
  exit 1
fi

if [ ! -f "$desktop_root/go.sum" ]; then
  printf '%s\n' '缺少 desktop/go.sum；请先在获准的依赖准备阶段恢复模块。' >&2
  exit 1
fi

export GOENV=off GOWORK=off GOFLAGS= GOEXPERIMENT=
export GOTOOLCHAIN=local
export GOMODCACHE="$desktop_root/.cache/gomod"
export GOCACHE="$desktop_root/.cache/build"
export GOTMPDIR="$desktop_root/.cache/tmp"
export TMPDIR="$desktop_root/.cache/tmp"
# 离线构建使缺失模块直接失败，不让独立验收隐式补充依赖或访问代理。
export GOPROXY=off
export GOSUMDB=off
export CGO_ENABLED=1
export GOOS=darwin
export GOAMD64=v1 GOARM64=v8.0
[ "$(go env GOVERSION)" = go1.27.3 ] || die '必须使用实际本机Go1.27.3工具链'
GOARCH=$(go env GOHOSTARCH)
export GOARCH
[ "$GOARCH" = arm64 ] || die '此桌面与m1ddc候选仅支持原生macOS arm64构建'
[ -x /usr/bin/codesign ] || die '缺少本机codesign工具'
[ -x /usr/bin/xcrun ] || die '缺少已准备的Apple构建工具；此脚本不会安装CLT'
# 仅使用构建机已经选择的Apple编译器和SDK；客户端运行随包工具，不依赖CLT或PATH定位。
native_clang=$(/usr/bin/xcrun --sdk macosx --find clang)
native_sdk=$(/usr/bin/xcrun --sdk macosx --show-sdk-path)
case "$native_clang" in /*) ;; *) die 'clang必须来自已准备的Apple工具链绝对路径' ;; esac
case "$native_sdk" in /*) ;; *) die 'macOS SDK必须是已准备的绝对路径' ;; esac
[ -x "$native_clang" ] && [ -d "$native_sdk" ] || die '已选择的clang或macOS SDK不可用'
# 本机产物使用 Go 1.27.3；Go 1.27 官方最低为 macOS 13，Wails 模板的
# 12.0 目标不能降低 Go 对象的实际要求。C、链接和 Info.plist 必须同为 13，
# 避免对象按 13 编译却按 12 链接的警告，以及错误声明可在 macOS 12 运行。
# 此限制针对当前应用包，不改变 go.mod 的最低 Go 版本或承诺其他系统兼容。
# 官方依据：https://go.dev/doc/go1.27 （Ports / Darwin、Linker）。
export CGO_CFLAGS='-mmacosx-version-min=13.0'
export CGO_LDFLAGS='-mmacosx-version-min=13.0'
export MACOSX_DEPLOYMENT_TARGET=13.0

m1ddc_root="$repo_root/third_party/m1ddc"
m1ddc_source_sha=148224cba5b8e6b573db67d62570b24763fc78580a76152413e53d140a750407
m1ddc_license_sha=b01d3d1de4baa142bb3746588e70814d04eae071d104f8aff580f88e30bb4547
pin_file "$m1ddc_root/SOURCE.json" "$m1ddc_source_sha"

for cache_dir in "$GOMODCACHE" "$GOCACHE" "$GOTMPDIR"; do
  no_links "$cache_dir"
done
mkdir -p "$GOMODCACHE" "$GOCACHE" "$GOTMPDIR"
if [ "$output_set" = 1 ]; then
  no_links "$repo_root/build/out"
  mkdir -p "$repo_root/build/out"
  mkdir "$app_parent" || die '候选构建编号已存在，拒绝接管'
  printf '%s\n' displaykvm-desktop-build-v1 > "$app_parent/.displaykvm-run"
else
  mkdir -p "$app_parent"
fi
no_links "$app_parent"
mkdir "$app_bundle" || die '应用目标已存在，拒绝覆盖'
# 失败只报告本次独占的新目标，不删除失败现场或用户已有成果。
trap 'build_status=$?; if [ "$build_status" -ne 0 ]; then printf "构建失败，保留现场：%s\n" "$app_bundle" >&2; fi; exit "$build_status"' 0
mkdir "$app_bundle/Contents" "$app_bundle/Contents/MacOS" "$app_bundle/Contents/Resources"
cp "$desktop_root/build/darwin/Info.plist" "$app_bundle/Contents/Info.plist"
# 登录项登记据此区分具备新启动行为的应用包与旧单GUI包；字符串版本需与核心校验一致。
# 只标记本次生成的副本，且在签名前写入；该能力版本不代表正式签名或分发认证。
/usr/libexec/PlistBuddy -c 'Add :DisplayKVMStartupVersion string 1' "$app_bundle/Contents/Info.plist"

# 固定m1ddc v1.3.1 / 7339ceebb03f691b1eaeb8471dca301d215d19c8的九个官方原件。
# 保留完整源码、原始Makefile、README和MIT许可，以便随包来源清单逐字节追溯；不执行上游安装规则。
native_license="$app_bundle/Contents/Resources/licenses/m1ddc"
native_source="$native_license/upstream"
native_build="$app_parent/.displaykvm-m1ddc-build"
no_links "$native_build"
mkdir "$native_build" || die '原生工具构建目录已存在，拒绝接管'
mkdir "$native_build/module-cache"
mkdir "$app_bundle/Contents/Resources/licenses" "$native_license" "$native_source"
mkdir "$native_source/headers" "$native_source/sources"
cp "$m1ddc_root/SOURCE.json" "$native_license/SOURCE.json"
pin_file "$native_license/SOURCE.json" "$m1ddc_source_sha"
native_source_records=
native_separator=
while read -r expected_sha source_rel; do
  pin_file "$m1ddc_root/upstream/$source_rel" "$expected_sha"
  cp "$m1ddc_root/upstream/$source_rel" "$native_source/$source_rel"
  pin_file "$native_source/$source_rel" "$expected_sha"
  native_source_records="$native_source_records$native_separator
      {\"file\": \"Contents/Resources/licenses/m1ddc/upstream/$source_rel\", \"sha256\": \"$expected_sha\"}"
  native_separator=,
done <<'M1DDC_PINS'
336e0bd09cd6328fd9dfae08a018a9b349cfe25000c6a95f135da92df7a5c01d Makefile
b01d3d1de4baa142bb3746588e70814d04eae071d104f8aff580f88e30bb4547 LICENSE
d24debe644f97bfc92d309a2105e0248d2119359bb31380d7c5b1e7096f1a730 README.md
7e0ae57dccbe53d84ddbc4c10f3efc036a06269583b788a854a89f8de81d326d headers/i2c.h
10a4833144b6168afa978f8ac63da30fe6dc67df24d881f22cb58f8a6e2b5ad2 headers/ioregistry.h
fcd87a2495f5bb7ca1d86253e3bab7ee1758f939c079106bf81c8bcaf6599b64 headers/utils.h
0f21bfb03a9f851f4dec72d6df1eae1809fe8ec49b0b74d3f4b9be601c2902d6 sources/i2c.m
39bbc789169aa1d7132bfc3da6005f8e5aeb41052195992181e7c61addcfe0a8 sources/ioregistry.m
37794b3c9d0831796496a11500f33b12c3c1a05f781d94c90c9762727b50f678 sources/m1ddc.m
M1DDC_PINS
cp "$native_source/LICENSE" "$native_license/LICENSE"
pin_file "$native_license/LICENSE" "$m1ddc_license_sha"

# 沿用原Makefile的编译/链接参数，只将所有输出和模块缓存移出源码树，并明确架构与部署目标。
# 私有CoreDisplay/IOAVService是否兼容具体系统及显示器仍需实机证据，部署标记不代表硬件验收。
for source_name in i2c ioregistry m1ddc; do
  "$native_clang" -arch arm64 -isysroot "$native_sdk" -mmacosx-version-min=13.0 \
    -c "$native_source/sources/$source_name.m" -o "$native_build/$source_name.o" \
    -I "$native_source/headers" -Wall -Werror -Wextra -fmodules \
    "-fmodules-cache-path=$native_build/module-cache" "-ffile-prefix-map=$repo_root=." \
    -MMD -MF "$native_build/$source_name.d"
done
"$native_clang" -arch arm64 -isysroot "$native_sdk" -mmacosx-version-min=13.0 \
  -framework CoreDisplay "$native_build/i2c.o" "$native_build/ioregistry.o" "$native_build/m1ddc.o" \
  -o "$app_bundle/Contents/MacOS/m1ddc"
/usr/bin/codesign --force --sign - "$app_bundle/Contents/MacOS/m1ddc"
/usr/bin/codesign --verify --strict "$app_bundle/Contents/MacOS/m1ddc"
native_sha=$(/usr/bin/shasum -a 256 "$app_bundle/Contents/MacOS/m1ddc")
native_sha=${native_sha%% *}
# 元数据记录的是签名后的实际程序；所有路径相对应用根，不记录构建机或用户业务路径。
printf '%s\n' "{
  \"schema_version\": 1,
  \"tools\": [{
    \"name\": \"m1ddc\", \"file\": \"Contents/MacOS/m1ddc\", \"sha256\": \"$native_sha\",
    \"goos\": \"darwin\", \"goarch\": \"arm64\", \"version\": \"1.3.1\",
    \"repository\": \"https://github.com/waydabber/m1ddc\", \"tag\": \"v1.3.1\",
    \"commit\": \"7339ceebb03f691b1eaeb8471dca301d215d19c8\", \"license\": \"MIT\",
    \"source_manifest\": {\"file\": \"Contents/Resources/licenses/m1ddc/SOURCE.json\", \"sha256\": \"$m1ddc_source_sha\"},
    \"licenses\": [{\"file\": \"Contents/Resources/licenses/m1ddc/LICENSE\", \"sha256\": \"$m1ddc_license_sha\"}],
    \"sources\": [$native_source_records
    ]
  }]
}" > "$app_bundle/Contents/Resources/bundled-tools.json"

# 手写静态资源已由 embed 收入，不存在前端编译或绑定生成步骤。
# readonly 使模块声明缺失时立即失败，不能在验证阶段修补工程输入。
go -C "$desktop_root" build -mod=readonly -tags production \
  -trimpath -buildvcs=false \
  -o "$app_bundle/Contents/MacOS/displaykvm" .

# 两个辅助程序来自根模块的真实入口；后台保持CGO=1以保留macOS原生事件。
# 共用本机离线缓存及13.0部署约束，不借用旧核心构建目录或伪装GUI副本。
go -C "$repo_root" build -mod=readonly -trimpath -buildvcs=false \
  -o "$app_bundle/Contents/MacOS/displaykvmd" ./cmd/displaykvmd
go -C "$repo_root" build -mod=readonly -trimpath -buildvcs=false \
  -o "$app_bundle/Contents/MacOS/displaykvm-cli" ./cmd/displaykvm

# 临时签名只作用于本地 .app，便于本机启动；不选择开发者证书、
# 不访问公证服务，不代表可对外分发或已经通过发布验收。
# 先签嵌套的真实辅助程序，再签应用整体；逐个严格验证以防漏签内层载荷。
for program in displaykvmd displaykvm-cli; do
  /usr/bin/codesign --force --sign - "$app_bundle/Contents/MacOS/$program"
done
/usr/bin/codesign --force --sign - "$app_bundle"
for program in displaykvm displaykvmd displaykvm-cli m1ddc; do
  /usr/bin/codesign --verify --strict "$app_bundle/Contents/MacOS/$program"
done
/usr/bin/codesign --verify --deep --strict "$app_bundle"
pin_file "$app_bundle/Contents/MacOS/m1ddc" "$native_sha"
printf '本地应用包：%s\n' "$app_bundle"

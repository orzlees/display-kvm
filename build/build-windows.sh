#!/bin/sh
set -eu
umask 077
root=$(CDPATH= cd "$(dirname "$0")/.." && pwd -P)
die() { printf '%s\n' "$*" >&2; exit 1; }
target=
version=
run_id=
while [ "$#" -gt 0 ]; do
    [ "$#" -ge 2 ] || die '参数必须提供值'
    case "$1" in
        --target) target=$2 ;;
        --version) version=$2 ;;
        --run-id) run_id=$2 ;;
        *) die "未知参数：$1" ;;
    esac
    shift 2
done
case "$target" in windows-amd64|windows-arm64) ;; *) die '只接受Windows amd64或arm64目标' ;; esac
for value in "$version" "$run_id"; do
    case "$value" in [A-Za-z0-9]*) ;; *) die '标签必须以字母或数字开头' ;; esac
    case "$value" in *[!A-Za-z0-9._-]*) die '标签包含无效字符' ;; esac
    [ "${#value}" -le 64 ] || die '标签不能超过64字符'
done
# 此入口可在已准备桌面模块缓存的POSIX构建机交叉编译；编译不等于Windows运行验证。
# 运行编号独占，失败现场不清理；链接拒绝逻辑及原始字节核验继续由notices统一负责。
export GOENV=off GOWORK=off GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off GOFLAGS= GOEXPERIMENT=
export CGO_ENABLED=0 GOAMD64=v1 GOARM64=v8.0
[ "$(go env GOVERSION)" = go1.27.3 ] || die '必须使用实际Go1.27.3工具链'
[ ! -L "$root/build" ] && [ ! -L "$root/build/out" ] || die '构建输出不得为符号链接'
if [ ! -e "$root/build/out" ]; then mkdir "$root/build/out"; fi
run=$root/build/out/$run_id
mkdir "$run" || die '运行编号已存在，请使用新编号'
printf '%s\n' displaykvm-windows-build-v1 > "$run/.displaykvm-run"
mkdir "$run/tools" "$run/cache" "$run/cache/gomod" "$run/cache/build" "$run/cache/tmp"
export GOMODCACHE="$run/cache/gomod" GOCACHE="$run/cache/build" GOTMPDIR="$run/cache/tmp" TMPDIR="$run/cache/tmp"
host_os=$(go env GOHOSTOS)
host_arch=$(go env GOHOSTARCH)
cd "$root"
GOOS=$host_os GOARCH=$host_arch go build -mod=readonly -trimpath -buildvcs=false -o "$run/tools/notices" ./packaging/notices
# 当前项目没有额外版本ldflags；静态前端已embed。保留调试/构建信息供许可及入口核查，
# 只加Windows GUI子系统标记，不运行Wails模板的tidy、图标生成或安装器任务。
GOMODCACHE="$root/desktop/.cache/gomod" GOOS=windows GOARCH=${target#windows-} \
    go -C "$root/desktop" build -mod=readonly -tags production -trimpath -buildvcs=false \
    -ldflags='-H=windowsgui' -o "$run/displaykvm.exe" .
"$run/tools/notices" record-windows --repo "$root" --run "$run" --version "$version" --target "$target"
printf 'Windows桌面构建候选：%s\n' "$run"

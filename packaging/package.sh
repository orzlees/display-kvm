#!/bin/sh
set -eu
umask 077
root=$(CDPATH= cd "$(dirname "$0")/.." && pwd -P)
die() { printf '%s\n' "$*" >&2; exit 1; }
[ "$#" -gt 0 ] || die '用法：package.sh core|desktop --run-id 编号 [--app 路径]'
kind=$1
shift
run_id=
app=
while [ "$#" -gt 0 ]; do
    [ "$#" -ge 2 ] || die '参数必须提供值'
    case "$1" in
        --run-id) run_id=$2 ;;
        --app) app=$2 ;;
        *) die "未知参数：$1" ;;
    esac
    shift 2
done
case "$run_id" in [A-Za-z0-9]*) ;; *) die '运行编号必须以字母或数字开头' ;; esac
case "$run_id" in *[!A-Za-z0-9._-]*) die '运行编号格式无效' ;; esac
[ "${#run_id}" -le 64 ] || die '运行编号不能超过64字符'
case "$kind" in core) [ -z "$app" ] || die 'core不接受--app' ;; desktop) [ -n "$app" ] || die 'desktop必须提供--app' ;; *) die '只接受core或desktop' ;; esac
export GOENV=off GOWORK=off GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off GOFLAGS= GOEXPERIMENT=
export CGO_ENABLED=0 GOAMD64=v1 GOARM64=v8.0
[ "$(go env GOVERSION)" = go1.27.3 ] || die '包装必须使用实际Go1.27.3工具链'
[ ! -L "$root/build" ] || die 'build不得为符号链接'
if [ ! -e "$root/build/out" ] && [ ! -L "$root/build/out" ]; then mkdir "$root/build/out"; fi
[ -d "$root/build/out" ] && [ ! -L "$root/build/out" ] || die '输出根目录无效'
run=$root/build/out/$run_id
if [ "$kind" = desktop ]; then
    # 桌面成品只读，包装独占新编号；不会调用原构建脚本或改变原应用签名。
    mkdir "$run" || die '运行编号已存在，请使用新编号'
    printf '%s\n' displaykvm-desktop-v1 > "$run/.displaykvm-run"
    mkdir "$run/tools" "$run/cache" "$run/cache/gomod" "$run/cache/build" "$run/cache/tmp"
else
    [ -d "$run" ] && [ ! -L "$run" ] || die '核心运行目录不存在或为链接'
    [ -f "$run/.displaykvm-run" ] && [ ! -L "$run/.displaykvm-run" ] || die '缺少核心运行标记'
    IFS= read -r owner < "$run/.displaykvm-run"
    [ "$owner" = displaykvm-core-v1 ] || die '不是本入口拥有的核心运行目录'
fi
# mkdir dist同时作为包装锁；不覆盖已有归档，也不删除失败现场或用户配置。
mkdir "$run/dist" || die '此编号已包装或留有失败现场，请使用新编号'
for dir in cache cache/gomod cache/build cache/tmp; do
    [ -d "$run/$dir" ] && [ ! -L "$run/$dir" ] || die '隔离缓存无效'
done
export GOMODCACHE="$run/cache/gomod" GOCACHE="$run/cache/build" GOTMPDIR="$run/cache/tmp" TMPDIR="$run/cache/tmp"
host_os=$(go env GOHOSTOS)
host_arch=$(go env GOHOSTARCH)
cd "$root"
GOOS=$host_os GOARCH=$host_arch go build -mod=readonly -trimpath -buildvcs=false -o "$run/dist/notices" ./packaging/notices
if [ "$kind" = desktop ]; then
    "$run/dist/notices" desktop --repo "$root" --run "$run" --app "$app"
else
    "$run/dist/notices" core --repo "$root" --run "$run"
fi
printf '包装候选产物：%s\n' "$run/dist"

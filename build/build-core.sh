#!/bin/sh
set -eu
umask 077
root=$(CDPATH= cd "$(dirname "$0")/.." && pwd -P)
die() { printf '%s\n' "$*" >&2; exit 1; }
target=all
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
for value in "$version" "$run_id"; do
    case "$value" in [A-Za-z0-9]*) ;; *) die '标签必须以字母或数字开头' ;; esac
    case "$value" in ''|.*|*[!A-Za-z0-9._-]*) die '版本标签与运行编号须以字母或数字开头，仅含字母、数字、点、下划线、短横线' ;; esac
    [ "${#value}" -le 64 ] || die '标签不能超过64字符'
done
all_targets='darwin-amd64 darwin-arm64 linux-amd64 linux-arm64 windows-amd64 windows-arm64'
case "$target" in
    all) targets=$all_targets ;;
    darwin-amd64|darwin-arm64|linux-amd64|linux-arm64|windows-amd64|windows-arm64) targets=$target ;;
    *) die '未知核心目标' ;;
esac
# 禁止读取个人Go配置、隐式下载或继承构建标签，确保工件与许可查询使用同一契约。
export GOENV=off GOWORK=off GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off GOFLAGS= GOEXPERIMENT=
export CGO_ENABLED=0 GOAMD64=v1 GOARM64=v8.0
[ "$(go env GOVERSION)" = go1.27.3 ] || die '必须使用实际Go1.27.3工具链'
[ ! -L "$root/build" ] || die 'build不得为符号链接'
if [ ! -e "$root/build/out" ] && [ ! -L "$root/build/out" ]; then mkdir "$root/build/out"; fi
[ -d "$root/build/out" ] && [ ! -L "$root/build/out" ] || die '输出根目录无效'
run=$root/build/out/$run_id
# mkdir独占编号，已有目录、断链或未知内容均不接管；失败现场保留，无自动删除。
mkdir "$run" || die '运行编号已存在，请使用新编号'
printf '%s\n' displaykvm-core-v1 > "$run/.displaykvm-run"
mkdir "$run/bin" "$run/tools" "$run/cache" "$run/cache/gomod" "$run/cache/build" "$run/cache/tmp"
export GOMODCACHE="$run/cache/gomod" GOCACHE="$run/cache/build" GOTMPDIR="$run/cache/tmp" TMPDIR="$run/cache/tmp"
host_os=$(go env GOHOSTOS)
host_arch=$(go env GOHOSTARCH)
cd "$root"
GOOS=$host_os GOARCH=$host_arch go build -mod=readonly -trimpath -buildvcs=false -o "$run/tools/notices" ./packaging/notices
for item in $targets; do
    os=${item%-*}
    arch=${item#*-}
    mkdir "$run/bin/$item"
    suffix=
    [ "$os" != windows ] || suffix=.exe
    for program in displaykvm displaykvmd; do
        GOOS=$os GOARCH=$arch go build -mod=readonly -trimpath -buildvcs=false -o "$run/bin/$item/$program$suffix" "./cmd/$program"
    done
done
"$run/tools/notices" record-core --repo "$root" --run "$run" --version "$version"
printf '核心候选产物：%s\n' "$run"

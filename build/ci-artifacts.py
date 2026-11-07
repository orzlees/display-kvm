#!/usr/bin/env python3
"""CI仅转运既有载荷与归档；产品manifest、许可和归档仍由notices负责。"""
import argparse
import hashlib
import os
from pathlib import Path, PurePosixPath
import re
import shutil
import tarfile

ROOT = Path(__file__).resolve().parent.parent
OWNER = b"displaykvm-linux-tools-v1\n"


def label(value):
    if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9._-]{0,63}", value):
        raise ValueError("运行编号或版本标签无效")
    return value


def regular(path):
    for item in (path, *path.parents):
        if item.is_symlink():
            raise ValueError(f"CI输入不允许符号链接：{item}")
    if not path.is_file():
        raise ValueError(f"缺少普通文件：{path}")


def pack_linux(args):
    run = ROOT / "build/out" / label(args.run_id)
    marker = run / ".displaykvm-run"
    regular(marker)
    if marker.read_bytes() != OWNER:
        raise ValueError("Linux载荷构建身份不符")
    payload = run / "linux" / args.arch
    if not payload.is_dir():
        raise ValueError("原生Linux载荷尚未生成")
    output = Path(args.output).absolute()
    output.parent.mkdir(parents=True, exist_ok=True)
    # 只传身份与该架构载荷；work包含构建中间文件，不能混进发布材料。
    with output.open("xb") as stream, tarfile.open(fileobj=stream, mode="w") as archive:
        archive.add(marker, arcname=".displaykvm-run", recursive=False)
        for path in sorted(payload.rglob("*")):
            if path.is_dir() and not path.is_symlink():
                continue
            regular(path)
            archive.add(path, arcname=path.relative_to(run).as_posix(), recursive=False)


def merge_linux(args):
    run = ROOT / "build/out" / label(args.run_id)
    run.parent.mkdir(parents=True, exist_ok=True)
    run.mkdir()  # 不覆盖上次失败现场或其他编号的材料。
    for arch in ("amd64", "arm64"):
        source = Path(args.artifacts) / f"linux-tools-{arch}.tar"
        regular(source)
        with tarfile.open(source, "r:") as archive:
            seen = set()
            owner = None
            for entry in archive:
                name = PurePosixPath(entry.name)
                if entry.name in seen or name.is_absolute() or ".." in name.parts or "\\" in entry.name:
                    raise ValueError("Linux转运归档路径重复或越界")
                seen.add(entry.name)
                if not entry.isfile():
                    raise ValueError("Linux转运仅允许普通文件，不接受链接或特殊条目")
                data = archive.extractfile(entry)
                if data is None:
                    raise ValueError("Linux转运文件不可读")
                if entry.name == ".displaykvm-run":
                    owner = data.read(128)
                    if owner != OWNER or entry.size != len(OWNER):
                        raise ValueError("Linux转运身份不符")
                    continue
                if len(name.parts) < 3 or name.parts[:2] != ("linux", arch):
                    raise ValueError("Linux转运含非本架构载荷或work目录")
                dest = run.joinpath(*name.parts)
                dest.parent.mkdir(parents=True, exist_ok=True)
                with dest.open("xb") as out:
                    shutil.copyfileobj(data, out)
                # 官方artifact会丢失执行位，tar中显式保留；不继承setuid等特殊位。
                os.chmod(dest, entry.mode & 0o777)
            if owner != OWNER or f"linux/{arch}/bundled-tools.json" not in seen:
                raise ValueError("Linux转运缺少身份或完整清单")
    (run / ".displaykvm-run").write_bytes(OWNER)
    # 此处只检查转运边界；原来源/hash/许可仍由后续record-core及core包装完整复验。


def collect_release(args):
    version = label(args.version)
    artifacts = Path(args.artifacts)
    output = Path(args.output)
    output.mkdir()
    expected = {
        f"displaykvm-{version}-{target}.{extension}"
        for target, extension in (
            ("darwin-amd64", "tar.gz"), ("darwin-arm64", "tar.gz"),
            ("linux-amd64", "tar.gz"), ("linux-arm64", "tar.gz"),
            ("windows-amd64", "zip"), ("windows-arm64", "zip"),
        )
    }
    expected.update(f"displaykvm-desktop-{version}-windows-{arch}.zip" for arch in ("amd64", "arm64"))
    found = set()
    mac = []
    for path in sorted(artifacts.rglob("*")):
        if not (path.name.endswith(".zip") or path.name.endswith(".tar.gz")):
            continue
        regular(path)
        if path.name in found:
            raise ValueError("发布材料归档名称重复")
        if path.name not in expected:
            # 保留既有macOS GUI的真实plist版本；不伪造产品版本或改写签名后的app。
            if not re.fullmatch(r"displaykvm-desktop-[A-Za-z0-9][A-Za-z0-9._-]{0,63}-darwin-arm64\.tar\.gz", path.name):
                raise ValueError(f"发布材料包含意外归档：{path.name}")
            mac.append(path.name)
        sidecar = path.with_name(path.name + ".sha256")
        regular(sidecar)
        digest = hashlib.sha256()
        with path.open("rb") as stream:
            for block in iter(lambda: stream.read(1024 * 1024), b""):
                digest.update(block)
        if sidecar.read_text(encoding="utf-8") != f"{digest.hexdigest()}  {path.name}\n":
            raise ValueError(f"发布归档摘要不匹配：{path.name}")
        for source in (path, sidecar):
            with source.open("rb") as src, (output / source.name).open("xb") as dest:
                shutil.copyfileobj(src, dest)
        found.add(path.name)
    if not expected.issubset(found) or len(mac) != 1 or len(found) != 9:
        raise ValueError("完整发布必须包含六平台核心、macOS arm64 GUI及Windows双架构GUI；不能降级发布")
    notes = (
        f"DisplayKVM {version} 候选版\n\n"
        "各归档内保留manifest.json、完整许可、来源材料与SHA256SUMS；归档旁附.sha256。\n"
        "Windows ZIP含同目录displaykvm.exe、displaykvmd.exe、displaykvm-cli.exe；先启动后台再打开桌面，需WebView2 Runtime。\n"
        "macOS GUI沿用app的真实plist版本；候选包未完成开发者签名/公证。\n"
        "CI通过不替代DDC、USB、登录启动、休眠与多显示器的最终实机验收。\n"
    )
    (output / "RELEASE-NOTES.txt").write_text(notes, encoding="utf-8")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    modes = parser.add_subparsers(dest="mode", required=True)
    pack = modes.add_parser("pack-linux")
    pack.add_argument("--run-id", required=True)
    pack.add_argument("--arch", choices=("amd64", "arm64"), required=True)
    pack.add_argument("--output", required=True)
    pack.set_defaults(action=pack_linux)
    merge = modes.add_parser("merge-linux")
    merge.add_argument("--run-id", required=True)
    merge.add_argument("--artifacts", required=True)
    merge.set_defaults(action=merge_linux)
    collect = modes.add_parser("collect-release")
    collect.add_argument("--version", required=True)
    collect.add_argument("--artifacts", required=True)
    collect.add_argument("--output", required=True)
    collect.set_defaults(action=collect_release)
    args = parser.parse_args()
    args.action(args)


if __name__ == "__main__":
    main()

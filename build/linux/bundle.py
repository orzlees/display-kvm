#!/usr/bin/env python3
"""只生成原生Linux受控工具载荷，不安装软件包，不构建项目Go入口。"""
import argparse
import hashlib
import io
import json
import os
from pathlib import Path, PurePosixPath
import platform
import re
import shlex
import shutil
import stat
import subprocess
import sys
import tarfile
import time
import urllib.parse
import urllib.request

ROOT = Path(__file__).resolve().parents[2]
BASELINE = {"libc.so.6", "libm.so.6", "libdl.so.2", "libpthread.so.0",
            "librt.so.1", "libresolv.so.2", "libutil.so.1",
            "ld-linux-x86-64.so.2", "ld-linux-aarch64.so.1"}
LOADERS = {"amd64": "/lib64/ld-linux-x86-64.so.2", "arm64": "/lib/ld-linux-aarch64.so.1"}
ENV = {"PATH": "/usr/bin:/bin", "LANG": "C", "LC_ALL": "C", "TZ": "UTC",
       "SOURCE_DATE_EPOCH": "1758585600", "PYTHONHASHSEED": "0"}


def fail(message):
    raise ValueError(message)


def regular(path):
    path = Path(path).absolute()
    for parent in (path, *path.parents):
        if parent.is_symlink():
            fail("受控输入或输出路径含符号链接")
    if not path.is_file() or not stat.S_ISREG(path.stat().st_mode):
        fail("受控输入不是普通文件")
    return path


def sha(path):
    with regular(path).open("rb") as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()


def write(path, data, mode=0o644):
    path.parent.mkdir(parents=True, exist_ok=True)
    for parent in (path.parent, *path.parent.parents):
        if parent.is_symlink():
            fail("受控输出目录含符号链接")
    with path.open("xb") as stream:
        stream.write(data)
    path.chmod(mode)


def json_write(path, data):
    write(path, (json.dumps(data, ensure_ascii=False, indent=2) + "\n").encode())


def record(path, payload):
    return {"file": path.relative_to(payload).as_posix(), "sha256": sha(path)}


def command(args, cwd, log, capture=True, extra=None):
    env = dict(ENV)
    if extra:
        env.update(extra)
    result = subprocess.run([str(item) for item in args], cwd=cwd, env=env,
                            stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
    with log.open("ab") as stream:
        stream.write(("\n$ " + shlex.join(str(item) for item in args) + "\n").encode())
        # APT配置可能来自runner；即使随后拒绝非官方来源，也不能先把私人镜像URL写入日志。
        stream.write(re.sub(rb"(?:https?|ftp)://[^\s'\"<>]+", b"<source-url-redacted>", result.stdout))
    if result.returncode:
        # 完整错误只留在本次独占目录，避免下载器把镜像凭据或私人配置输出到控制台。
        fail("命令失败：%s；退出码%d，详见本次work/build.log" % (Path(args[0]).name, result.returncode))
    return result.stdout.decode("utf-8", errors="strict") if capture else result.stdout


def official_url(value):
    url = urllib.parse.urlsplit(value)
    if (url.hostname not in {"codeload.github.com", "deb.debian.org", "security.debian.org"}
            or url.scheme not in {"https", "http"} or url.username or url.password
            or url.query or url.fragment or url.netloc != url.hostname):
        fail("下载只接受无凭据的固定官方来源")
    return urllib.parse.urlunsplit(("https", url.netloc, url.path, "", ""))


def fetch(url, expected, dest):
    url = official_url(url)
    if not re.fullmatch(r"[0-9a-f]{64}", expected):
        fail("下载缺少精确SHA-256")
    for attempt in range(3):
        try:
            with urllib.request.urlopen(url, timeout=45) as response:
                official_url(response.url)
                data = response.read(512 * 1024 * 1024 + 1)
            if len(data) > 512 * 1024 * 1024:
                fail("来源归档超过512MiB上限")
            if hashlib.sha256(data).hexdigest() != expected:
                fail("来源归档SHA-256不符，停止而不接受新摘要")
            write(dest, data)
            return url
        except (OSError, TimeoutError):
            if attempt == 2:
                fail("来源下载失败，已达到三次有限重试上限")
            time.sleep(attempt + 1)


def extract_source(archive, destination, prefix):
    destination.mkdir()
    links = []
    with tarfile.open(archive, "r:gz") as source:
        for member in source.getmembers():
            parts = PurePosixPath(member.name).parts
            if not parts or parts[0] != prefix or ".." in parts or member.name.startswith("/"):
                fail("上游归档路径越界")
            target = destination.joinpath(*parts[1:])
            if member.isdir():
                target.mkdir(parents=True, exist_ok=True)
            elif member.isfile():
                data = source.extractfile(member).read()
                write(target, data, 0o755 if member.mode & 0o111 else 0o644)
            elif member.issym():
                links.append((target, member.linkname))
            else:
                fail("上游归档包含不支持的特殊文件或硬链接")
    # 先写完普通文件再创建链接，防止归档通过先放链接、后写子路径逃逸暂存目录。
    for target, value in links:
        if not target.parent.resolve().is_relative_to(destination) or not (target.parent / value).resolve().is_relative_to(destination):
            fail("上游归档链接越界")
        target.parent.mkdir(parents=True, exist_ok=True)
        target.symlink_to(value)


def control_records(text):
    records = []
    current, key = {}, None
    for line in text.splitlines() + [""]:
        if not line:
            if current:
                records.append(current)
            current, key = {}, None
        elif line[0].isspace() and key:
            current[key] += "\n" + line.strip()
        else:
            key, sep, value = line.partition(":")
            if not sep:
                fail("Debian来源索引格式错误")
            current[key] = value.strip()
    return records


def archive_uris(text):
    values = {}
    for line in text.splitlines():
        if not line.startswith("'"):
            continue
        fields = shlex.split(line)
        if len(fields) < 3 or Path(fields[1]).name != fields[1]:
            fail("Debian下载元数据格式错误")
        values[fields[1]] = official_url(fields[0])
    return values


def upstream(pin, work, payload):
    archive = payload / "sources" / (pin["name"] + "-" + pin["version"] + ".tar.gz")
    url = fetch(pin["url"], pin["sha256"], archive)
    source = work / pin["name"]
    extract_source(archive, source, pin["name"] + "-" + pin["commit"])
    licenses = []
    for path in sorted(source.rglob("*")):
        rel = path.relative_to(source)
        upper = path.name.upper()
        if path.is_file() and (upper.startswith(("LICENSE", "LICENCE", "COPYING", "NOTICE")) or "LICENSES" in rel.parts):
            actual = path.resolve()
            if not actual.is_relative_to(source):
                fail("上游许可路径越界")
            dest = payload / "licenses" / pin["name"] / rel
            write(dest, actual.read_bytes())
            licenses.append(record(dest, payload))
    if not licenses:
        fail("上游缺少完整许可")
    return source, {"id": pin["name"], "name": pin["name"], "version": pin["version"],
                    "archives": [dict(record(archive, payload), url=url)],
                    "binary_archives": [], "licenses": licenses}


class DebianCollector:
    def __init__(self, work, payload, arch, log):
        self.work, self.payload, self.arch, self.log = work, payload, arch, log
        self.sources, self.packages = {}, {}

    def run(self, *args):
        return command(args, self.work, self.log)

    def package(self, library):
        owners = None
        # Debian12合并/usr后真实路径与dpkg文件表可能分别使用/usr/lib和/lib。
        for candidate in (str(library), str(library).removeprefix("/usr")):
            result = subprocess.run(["dpkg-query", "-S", candidate], env=ENV,
                                    stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
            if result.returncode == 0:
                owners = result.stdout.decode().strip().splitlines()
                break
        if not owners or len(owners) != 1:
            fail("运行库无法唯一对应已安装Debian软件包")
        owner = owners[0].split(": ", 1)[0]
        fields = self.run("dpkg-query", "-W", "-f=${binary:Package}\t${Version}\t${Architecture}\t${source:Package}\t${source:Version}", owner).split("\t")
        if len(fields) != 5 or fields[2] != self.arch:
            fail("运行库包架构或来源信息不符")
        package, version, arch, source, source_version = fields
        package = package.split(":")[0]
        key = package + "=" + version
        if key not in self.packages:
            source_id = "debian-" + hashlib.sha256((source + "=" + source_version).encode()).hexdigest()[:16]
            if source_id not in self.sources:
                records = control_records(self.run("apt-cache", "showsrc", "--only-source", source))
                matches = [item for item in records if item.get("Package") == source and item.get("Version") == source_version]
                if not matches or not matches[0].get("Checksums-Sha256"):
                    fail("缺少运行库对应精确版本的Debian源码索引；须预置官方deb-src索引")
                meta = matches[0]
                uris = archive_uris(self.run("apt-get", "--print-uris", "--download-only", "--only-source", "source", source + "=" + source_version))
                archives = []
                for line in meta["Checksums-Sha256"].strip().splitlines():
                    checksum, size, name = line.split()
                    if name not in uris or Path(name).name != name:
                        fail("源码下载清单与已验证索引不符")
                    dest = self.payload / "sources" / source_id / name
                    url = fetch(uris[name], checksum, dest)
                    if dest.stat().st_size != int(size):
                        fail("Debian源码归档长度不符")
                    archives.append(dict(record(dest, self.payload), url=url))
                if not any(item["file"].endswith(".dsc") for item in archives):
                    fail("Debian源码缺少精确.dsc及上游/补丁归档")
                self.sources[source_id] = {"id": source_id, "name": source, "version": source_version,
                                           "archives": archives, "binary_archives": [], "licenses": []}
            records = control_records(self.run("apt-cache", "show", package + ":" + arch + "=" + version))
            matches = [item for item in records if item.get("Package") == package and item.get("Version") == version and item.get("Architecture") == arch and item.get("SHA256")]
            if not matches:
                fail("运行库缺少精确二进制包的SHA-256索引")
            meta = matches[0]
            uris = archive_uris(self.run("apt-get", "--print-uris", "download", package + ":" + arch + "=" + version))
            if len(uris) != 1:
                fail("运行库原始二进制包来源不唯一")
            name, url = next(iter(uris.items()))
            archive = self.payload / "sources" / source_id / "binaries" / name
            url = fetch(url, meta["SHA256"], archive)
            self.sources[source_id]["binary_archives"].append(dict(record(archive, self.payload), url=url))
            copyright_path = Path("/usr/share/doc") / package / "copyright"
            copyright_path = copyright_path.resolve(strict=True)
            if not copyright_path.is_relative_to(Path("/usr/share/doc")):
                fail("Debian版权文件越界")
            dest = self.payload / "licenses" / source_id / (package + "-copyright")
            write(dest, regular(copyright_path).read_bytes())
            self.sources[source_id]["licenses"].append(record(dest, self.payload))
            self.packages[key] = (source_id, archive)
        source_id, archive = self.packages[key]
        # 原始.deb一同保留。只有实际库字节在同一精确包中出现，才允许打补丁设置相对RUNPATH。
        data = command(["dpkg-deb", "--fsys-tarfile", archive], self.work, self.log, capture=False)
        original_hash = sha(library)
        with tarfile.open(fileobj=io.BytesIO(data), mode="r:") as contents:
            found = any(member.isfile() and PurePosixPath(member.name).name == library.name
                        and hashlib.sha256(contents.extractfile(member).read()).hexdigest() == original_hash
                        for member in contents.getmembers())
        if not found:
            fail("运行库字节不属于下载的精确Debian二进制包")
        return {"package": package, "version": version, "architecture": arch,
                "source": source_id, "package_archive": archive.relative_to(self.payload).as_posix(),
                "original_sha256": original_hash}


def resolved_dependencies(binary, work, log, arch):
    output = command(["ldd", binary], work, log)
    dependencies = {}
    for line in output.splitlines():
        line = line.strip()
        if "=> not found" in line:
            fail("ELF运行库无法解析")
        if "=>" in line:
            name, location = line.split("=>", 1)
            name, location = name.strip(), location.strip().split()[0]
            if name in {LOADERS[arch], Path(LOADERS[arch]).name} and Path(location).resolve() == Path(LOADERS[arch]).resolve():
                continue
            if not location.startswith("/") or "/" in name:
                fail("ELF依赖解析不是受控绝对位置")
            dependencies[name] = Path(location).resolve(strict=True)
        elif line.startswith("/"):
            location = line.split()[0]
            if Path(location).resolve() != Path(LOADERS[arch]).resolve():
                fail("ELF使用未知加载器")
        elif not line.startswith("linux-vdso.so."):
            fail("ldd返回无法识别的依赖行")
    return dependencies


def check_elf(binary, arch, program, work, log):
    with binary.open("rb") as stream:
        header = stream.read(20)
    machine = {"amd64": 62, "arm64": 183}[arch]
    if header[:6] != b"\x7fELF\x02\x01" or int.from_bytes(header[18:20], "little") != machine:
        fail("ELF架构不符")
    dynamic = command(["readelf", "-dW", binary], work, log)
    paths = re.findall(r"\((RUNPATH|RPATH)\).*?\[(.*?)\]", dynamic)
    if paths != [("RUNPATH", "$ORIGIN/lib" if program else "$ORIGIN")]:
        fail("ELF必须只保留相对RUNPATH")
    for needed in re.findall(r"\(NEEDED\).*?\[(.*?)\]", dynamic):
        if "/" in needed:
            fail("ELF依赖含构建机目录")
        if needed.startswith("ld-linux-") and needed != Path(LOADERS[arch]).name:
            fail("ELF依赖含错误架构的加载器")
    versions = command(["readelf", "-VW", binary], work, log)
    if "GLIBC_PRIVATE" in versions or any(tuple(map(int, value.split("."))) > (2, 36) for value in re.findall(r"GLIBC_([0-9]+\.[0-9]+(?:\.[0-9]+)?)", versions)):
        fail("ELF需要超过glibc2.36的符号")
    headers = command(["readelf", "-lW", binary], work, log)
    interpreters = re.findall(r"Requesting program interpreter: ([^\]]+)", headers)
    if interpreters != ([LOADERS[arch]] if program else []):
        fail("ELF加载器不符")


def generate(args):
    if sys.version_info < (3, 11) or platform.system() != "Linux":
        fail("须在原生Debian12/Python3.11执行；不接受macOS交叉编译产物")
    native = {"x86_64": "amd64", "aarch64": "arm64"}.get(platform.machine())
    if native != args.arch:
        fail("目标架构与原生runner不符")
    release = dict(line.split("=", 1) for line in Path("/etc/os-release").read_text().splitlines() if "=" in line)
    if release.get("ID", "").strip('"') != "debian" or release.get("VERSION_ID", "").strip('"') != "12":
        fail("构建边界固定为Debian12，禁止在更高glibc环境生成后改清单")
    required = ["gcc", "make", "autoconf", "automake", "autoreconf", "aclocal", "libtoolize",
                "pkg-config", "meson", "ninja", "gperf", "patchelf", "readelf", "ldd", "getconf",
                "dpkg-query", "dpkg-deb", "apt-cache", "apt-get", "m4", "gzip", "xz", "tar"]
    missing = [name for name in required if not shutil.which(name, path=ENV["PATH"])]
    if missing:
        fail("runner须预置工具（脚本不会安装）：" + ", ".join(missing))
    if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9._-]{0,63}", args.run_id):
        fail("run-id格式无效")
    # 每次生成独占编号；失败产物保留供诊断，不能覆盖旧产物或把半成品登记成可打包载荷。
    output = ROOT / "build" / "out"
    for parent in (output, *output.parents):
        if parent.is_symlink():
            fail("输出目录含链接")
    output.mkdir(exist_ok=True)
    run = output / args.run_id
    run.mkdir()
    write(run / ".displaykvm-run", b"displaykvm-linux-tools-v1\n")
    work = run / "work"
    work.mkdir()
    log = work / "build.log"
    log.touch(exist_ok=False)
    payload = run / "linux" / args.arch
    payload.mkdir(parents=True)
    lock_path = regular(ROOT / "build" / "linux" / "source-lock.json")
    lock = json.loads(lock_path.read_text())
    if lock["schema_version"] != 1 or lock["baseline"] != "debian-12-glibc-2.36" or {item["name"] for item in lock["sources"]} != {"ddcutil", "systemd"}:
        fail("来源锁定文件无效")
    libc = command(["getconf", "GNU_LIBC_VERSION"], work, log).strip()
    if libc != "glibc 2.36":
        fail("runner的glibc不是2.36")
    command(["pkg-config", "--exists", "glib-2.0", "libdrm >= 2.4.67", "mount", "libcap", "libxcrypt", "jansson"], work, log)
    command([sys.executable, "-c", "import jinja2"], work, log)
    tools = {}
    for name in required:
        # 全部工具版本和已安装包版本写入配方，供后续按相同原生镜像重建；不改变宿主包状态。
        if name not in {"ldd", "dpkg-query", "dpkg-deb", "apt-cache", "apt-get", "getconf", "m4", "gzip", "xz", "tar", "gperf"}:
            tools[name] = command([name, "--version"], work, log).splitlines()[0]
    installed = command(["dpkg-query", "-W", "-f=${binary:Package}\t${Version}\t${Architecture}\n"], work, log)
    environment = {"distribution": "debian-12", "glibc": libc, "architecture": args.arch,
                   "tools": tools, "installed_packages": installed.splitlines(), "environment": ENV,
                   "compiler_flags": "-O2 -ffile-prefix-map=<work>=/usr/src/displaykvm-linux-tools"}
    sources, trees = [], {}
    for pin in lock["sources"]:
        tree, source = upstream(pin, work, payload)
        trees[pin["name"]] = tree
        sources.append(source)
    flags = "-O2 -ffile-prefix-map=" + str(work) + "=/usr/src/displaykvm-linux-tools"
    build_env = {"CFLAGS": flags, "CXXFLAGS": flags, "LDFLAGS": "-Wl,--as-needed"}
    pins = {item["name"]: item for item in lock["sources"]}
    ddc = trees["ddcutil"]
    # 上游autogen默认会立即以默认能力配置；先跳过该次configure，避免未裁剪功能隐式增加依赖。
    command(["sh", "autogen.sh"], ddc, log, extra=dict(build_env, NOCONFIGURE="1"))
    command(["sh", "configure", *pins["ddcutil"]["configure"]], ddc, log, extra=build_env)
    # ddcutil缺少DRM开发头时会静默关闭该功能；本配方必须拒绝功能缩水的成功构建。
    if not re.search(r"^#define\s+USE_LIBDRM\s+1\s*$", (ddc / "config.h").read_text(), re.MULTILINE):
        fail("ddcutil未启用固定配方的DRM能力")
    command(["make", "-j" + str(args.jobs)], ddc, log, extra=build_env)
    systemd_build = work / "systemd-build"
    command(["meson", "setup", systemd_build, trees["systemd"], *pins["systemd"]["meson"]], work, log, extra=build_env)
    command(["meson", "compile", "-C", systemd_build, "-j", str(args.jobs), "busctl.standalone", "udevadm.standalone"], work, log, extra=build_env)
    binaries = {"ddcutil": ddc / "src" / "ddcutil", "busctl": systemd_build / "busctl.standalone",
                "udevadm": systemd_build / "udevadm.standalone"}
    libraries = {}
    for binary in binaries.values():
        for name, location in resolved_dependencies(regular(binary), work, log, args.arch).items():
            if name not in BASELINE:
                if name in libraries and libraries[name] != location:
                    fail("不同工具解析到同名但不同来源的运行库")
                if location.is_relative_to(work):
                    fail("工具依赖未独立封装的构建树共享库")
                libraries[name] = location
    collector = DebianCollector(work, payload, args.arch, log)
    library_records = []
    for name, source in sorted(libraries.items()):
        metadata = collector.package(source)
        dest = payload / "lib" / name
        write(dest, regular(source).read_bytes(), 0o755)
        command(["patchelf", "--set-rpath", "$ORIGIN", dest], work, log)
        check_elf(dest, args.arch, False, work, log)
        library_records.append(dict(record(dest, payload), **metadata))
    tool_records = []
    for name, source in binaries.items():
        dest = payload / name
        write(dest, regular(source).read_bytes(), 0o755)
        command(["patchelf", "--set-rpath", "$ORIGIN/lib", dest], work, log)
        check_elf(dest, args.arch, True, work, log)
        version = command([dest, "--version"], work, log)
        expected = "3.0.2" if name == "ddcutil" else "258"
        if not re.search(r"(?<![0-9.])" + re.escape(expected) + r"(?![0-9.])", version):
            fail("工具实际版本与固定源码版本不符")
        # 以打包后的RUNPATH再次解析全部传递依赖，非glibc依赖必须落在随包lib中。
        for soname, location in resolved_dependencies(dest, work, log, args.arch).items():
            if soname not in BASELINE and location != payload / "lib" / soname:
                fail("运行库闭包仍依赖runner系统目录")
        tool_records.append(dict(record(dest, payload), name=name, version=expected,
                                 source="ddcutil" if name == "ddcutil" else "systemd"))
    common = Path("/usr/share/common-licenses")
    common_licenses = []
    for source in sorted(common.iterdir()):
        actual = source.resolve(strict=True)
        if not actual.is_relative_to(common):
            fail("Debian公共许可链接越界")
        dest = payload / "licenses" / "debian-common" / source.name
        write(dest, regular(actual).read_bytes())
        common_licenses.append(record(dest, payload))
    for source in collector.sources.values():
        source["licenses"].extend(common_licenses)
        sources.append(source)
    for source in (Path(__file__).resolve(), lock_path):
        write(payload / "recipe" / source.name, regular(source).read_bytes())
    json_write(payload / "recipe" / "environment.json", environment)
    inventory = [record(path, payload) for path in sorted(payload.rglob("*")) if path.is_file()]
    manifest = {"schema_version": 1, "goos": "linux", "goarch": args.arch,
                "baseline": lock["baseline"], "tools": tool_records, "libraries": library_records,
                "sources": sources, "files": inventory}
    # manifest最后以独占写入，缺少此文件的中断目录绝不能进入核心包。
    json_write(payload / "bundled-tools.json", manifest)
    print("Linux工具载荷已生成：build/out/" + args.run_id + "/linux/" + args.arch)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    build = commands.add_parser("generate", help="在原生Debian12生成一个架构的工具和依赖闭包")
    build.add_argument("--arch", choices=("amd64", "arm64"), required=True)
    build.add_argument("--run-id", required=True)
    build.add_argument("--jobs", type=int, default=2)
    args = parser.parse_args()
    if not 1 <= args.jobs <= 64:
        fail("jobs须在1至64之间")
    generate(args)


if __name__ == "__main__":
    try:
        main()
    except (ValueError, OSError, KeyError, tarfile.TarError) as error:
        # 仅记录明确的受控错误，不把urllib异常中的原始URL回显到调用方日志。
        if isinstance(error, ValueError):
            print("Linux载荷生成失败：" + str(error), file=sys.stderr)
        else:
            print("Linux载荷生成失败：输入、文件系统或归档错误；请检查本次独占目录", file=sys.stderr)
        sys.exit(1)

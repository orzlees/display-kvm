package main

import (
	"bytes"
	"debug/elf"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 构造最小ELF用于检查包装边界，不执行它，也不把测试字节当作真实Linux载荷。
// 三个动态节分别保存依赖字符串、动态表和空符号表，覆盖实际ELF读取路径。
func linuxTestELF(machine elf.Machine, needed []string, runpath, interpreter string) []byte {
	strtab := []byte{0}
	var dynamic bytes.Buffer
	for _, name := range append(append([]string{}, needed...), runpath) {
		tag := uint64(elf.DT_NEEDED)
		if len(needed) == 0 {
			tag = uint64(elf.DT_RUNPATH)
		} else {
			needed = needed[1:]
		}
		binary.Write(&dynamic, binary.LittleEndian, tag)
		binary.Write(&dynamic, binary.LittleEndian, uint64(len(strtab)))
		strtab = append(strtab, append([]byte(name), 0)...)
	}
	dynamic.Write(make([]byte, 16))
	const dataStart = 64 + 56
	dynOffset := dataStart + len(strtab)
	symOffset := dynOffset + dynamic.Len()
	interpOffset := symOffset + 24
	sectionOffset := interpOffset + len(interpreter) + 1
	header := elf.Header64{Type: uint16(elf.ET_DYN), Machine: uint16(machine), Version: 1, Ehsize: 64, Phoff: 64, Phentsize: 56, Phnum: 1, Shoff: uint64(sectionOffset), Shentsize: 64, Shnum: 4}
	copy(header.Ident[:], []byte{0x7f, 'E', 'L', 'F', byte(elf.ELFCLASS64), byte(elf.ELFDATA2LSB), 1})
	var out bytes.Buffer
	binary.Write(&out, binary.LittleEndian, header)
	binary.Write(&out, binary.LittleEndian, elf.Prog64{Type: uint32(elf.PT_INTERP), Off: uint64(interpOffset), Filesz: uint64(len(interpreter) + 1)})
	out.Write(strtab)
	out.Write(dynamic.Bytes())
	out.Write(make([]byte, 24))
	out.WriteString(interpreter + "\x00")
	for _, section := range []elf.Section64{
		{},
		{Type: uint32(elf.SHT_STRTAB), Off: dataStart, Size: uint64(len(strtab)), Addralign: 1},
		{Type: uint32(elf.SHT_DYNAMIC), Off: uint64(dynOffset), Size: uint64(dynamic.Len()), Link: 1, Entsize: 16, Addralign: 8},
		{Type: uint32(elf.SHT_DYNSYM), Off: uint64(symOffset), Size: 24, Link: 1, Entsize: 24, Addralign: 8},
	} {
		binary.Write(&out, binary.LittleEndian, section)
	}
	return out.Bytes()
}

func TestLinuxELFRejectsWrongMachineAndMissingClosure(t *testing.T) {
	dir := noticeTempDir(t)
	path := filepath.Join(dir, "ddcutil")
	write := func(data []byte) {
		t.Helper()
		if err := os.WriteFile(path, data, 0755); err != nil {
			t.Fatal(err)
		}
	}
	write(linuxTestELF(elf.EM_AARCH64, []string{"libc.so.6"}, "$ORIGIN/lib", "/lib/ld-linux-aarch64.so.1"))
	if _, err := inspectLinuxELF(path, "amd64", true); err == nil {
		t.Fatal("错误架构必须拒绝")
	}
	write(linuxTestELF(elf.EM_X86_64, []string{"libc.so.6", "libmissing.so.1"}, "$ORIGIN/lib", "/lib64/ld-linux-x86-64.so.2"))
	deps, err := inspectLinuxELF(path, "amd64", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := linuxClosure(map[string][]string{"ddcutil": deps}, map[string]bool{}); err == nil {
		t.Fatal("未随包的非基线依赖必须拒绝")
	}
	if err := linuxClosure(map[string][]string{"ddcutil": deps, "libmissing.so.1": {"libc.so.6"}}, map[string]bool{"libmissing.so.1": true}); err != nil {
		t.Fatal(err)
	}
}

func TestLinuxELFRejectsHostPathsAndNewerGLIBC(t *testing.T) {
	for _, tc := range []struct{ name, runpath, dependency, interpreter string }{
		{"absolute-runpath", "/usr/lib", "libc.so.6", "/lib64/ld-linux-x86-64.so.2"},
		{"fallback-runpath", "$ORIGIN/lib:/usr/lib", "libc.so.6", "/lib64/ld-linux-x86-64.so.2"},
		{"absolute-needed", "$ORIGIN/lib", "/usr/lib/libc.so.6", "/lib64/ld-linux-x86-64.so.2"},
		{"newer-glibc", "$ORIGIN/lib", "GLIBC_2.39", "/lib64/ld-linux-x86-64.so.2"},
		{"wrong-loader", "$ORIGIN/lib", "libc.so.6", "/private/loader"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(noticeTempDir(t), "tool")
			if err := os.WriteFile(path, linuxTestELF(elf.EM_X86_64, []string{tc.dependency}, tc.runpath, tc.interpreter), 0755); err != nil {
				t.Fatal(err)
			}
			if _, err := inspectLinuxELF(path, "amd64", true); err == nil {
				t.Fatal("构建机路径、未知加载器及超出基线的符号必须拒绝")
			}
		})
	}
}

func TestLinuxManifestCannotPackagePlaceholderOrUnlistedFiles(t *testing.T) {
	dir := noticeTempDir(t)
	if err := os.WriteFile(filepath.Join(dir, "bundled-tools.json"), []byte(`{"schema_version":1,"goos":"linux","goarch":"amd64"}`), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := linuxBundle(dir, "amd64"); err == nil {
		t.Fatal("仅有清单不能冒充真实工具、库、源码及许可")
	}
	if err := linuxInventory(dir, []licenseFile{{File: "../escape", SHA256: strings.Repeat("a", 64)}}); err == nil {
		t.Fatal("清单路径不得越界")
	}
	path := filepath.Join(dir, "listed")
	if err := os.WriteFile(path, []byte("source"), 0644); err != nil {
		t.Fatal(err)
	}
	hash, err := digest(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := linuxInventory(dir, []licenseFile{{File: "listed", SHA256: hash}}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "unlisted"), []byte("secret"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := linuxInventory(dir, []licenseFile{{File: "listed", SHA256: hash}}); err == nil {
		t.Fatal("未列入清单的文件不得随包")
	}
}

func TestLinuxInventoryRejectsLinksAndChangedBytes(t *testing.T) {
	dir := noticeTempDir(t)
	path := filepath.Join(dir, "file")
	if err := os.WriteFile(path, []byte("changed"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := linuxInventory(dir, []licenseFile{{File: "file", SHA256: strings.Repeat("a", 64)}}); err == nil {
		t.Fatal("哈希变化必须拒绝")
	}
	if err := os.Symlink(path, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	hash, err := digest(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := linuxInventory(dir, []licenseFile{{File: "file", SHA256: hash}, {File: "link", SHA256: hash}}); err == nil {
		t.Fatal("符号链接不得绕过安全复制")
	}
}

func TestLinuxDebianSourceRequiresOriginalAndPatchArchives(t *testing.T) {
	dir := noticeTempDir(t)
	var archives []linuxArchive
	var sums strings.Builder
	for _, name := range []string{"example_1.orig.tar.xz", "example_1-2.debian.tar.xz"} {
		file := filepath.Join(dir, name)
		if err := os.WriteFile(file, []byte(name), 0644); err != nil {
			t.Fatal(err)
		}
		hash, err := digest(file)
		if err != nil {
			t.Fatal(err)
		}
		archives = append(archives, linuxArchive{File: name, SHA256: hash})
		fmt.Fprintf(&sums, " %s %d %s\n", hash, len(name), name)
	}
	dsc := "Source: example\nVersion: 1-2\nChecksums-Sha256:\n" + sums.String() + "Files:\n"
	if err := os.WriteFile(filepath.Join(dir, "example.dsc"), []byte(dsc), 0644); err != nil {
		t.Fatal(err)
	}
	archives = append(archives, linuxArchive{File: "example.dsc"})
	source := linuxSource{Name: "example", Version: "1-2", Archives: archives, BinaryArchives: []linuxArchive{{File: "example.deb"}}}
	if err := linuxDebianSource(dir, source); err != nil {
		t.Fatal(err)
	}
	source.Archives = []linuxArchive{archives[0], archives[2]}
	if err := linuxDebianSource(dir, source); err == nil {
		t.Fatal("缺少发行版补丁时不得把单个上游归档当作完整对应源码")
	}
	source.Archives = archives
	source.Version = "1-3"
	if err := linuxDebianSource(dir, source); err == nil {
		t.Fatal("源码版本必须与运行库精确对应")
	}
}

func TestLinuxCoreRequiresNativePayload(t *testing.T) {
	dir := noticeTempDir(t)
	binaries := []binaryRecord{{GOOS: "linux", GOARCH: "amd64"}}
	if err := stageLinux(dir, "", binaries); err == nil {
		t.Fatal("纯Go交叉编译不能冒充Linux原生工具载荷")
	}
	if _, err := linuxBuildRecords(dir, binaries); err == nil {
		t.Fatal("包装前必须重新检查真实原生载荷")
	}
	if err := stageLinux(dir, "", []binaryRecord{{GOOS: "darwin", GOARCH: "arm64"}}); err != nil {
		t.Fatal(err)
	}
}

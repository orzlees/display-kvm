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
func linuxTestELF(machine elf.Machine, needed []string, runpath string, interpreters []string, sonames ...string) []byte {
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
	for _, name := range sonames {
		binary.Write(&dynamic, binary.LittleEndian, uint64(elf.DT_SONAME))
		binary.Write(&dynamic, binary.LittleEndian, uint64(len(strtab)))
		strtab = append(strtab, append([]byte(name), 0)...)
	}
	dynamic.Write(make([]byte, 16))
	// 按实际数量写入PT_INTERP；没有段与含空字符串的段不同，后者不能冒充无加载器的共享库。
	var interpreterData bytes.Buffer
	for _, interpreter := range interpreters {
		interpreterData.WriteString(interpreter + "\x00")
	}
	dataStart := 64 + 56*len(interpreters)
	dynOffset := dataStart + len(strtab)
	symOffset := dynOffset + dynamic.Len()
	interpOffset := symOffset + 24
	sectionOffset := interpOffset + interpreterData.Len()
	header := elf.Header64{Type: uint16(elf.ET_DYN), Machine: uint16(machine), Version: 1, Ehsize: 64, Phoff: 64, Phentsize: 56, Phnum: uint16(len(interpreters)), Shoff: uint64(sectionOffset), Shentsize: 64, Shnum: 4}
	copy(header.Ident[:], []byte{0x7f, 'E', 'L', 'F', byte(elf.ELFCLASS64), byte(elf.ELFDATA2LSB), 1})
	var out bytes.Buffer
	binary.Write(&out, binary.LittleEndian, header)
	for _, interpreter := range interpreters {
		binary.Write(&out, binary.LittleEndian, elf.Prog64{Type: uint32(elf.PT_INTERP), Off: uint64(interpOffset), Filesz: uint64(len(interpreter) + 1)})
		interpOffset += len(interpreter) + 1
	}
	out.Write(strtab)
	out.Write(dynamic.Bytes())
	out.Write(make([]byte, 24))
	out.Write(interpreterData.Bytes())
	for _, section := range []elf.Section64{
		{},
		{Type: uint32(elf.SHT_STRTAB), Off: uint64(dataStart), Size: uint64(len(strtab)), Addralign: 1},
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
	write(linuxTestELF(elf.EM_AARCH64, []string{"libc.so.6"}, "$ORIGIN/lib", []string{"/lib/ld-linux-aarch64.so.1"}))
	if _, err := inspectLinuxELF(path, "amd64", true); err == nil {
		t.Fatal("错误架构必须拒绝")
	}
	write(linuxTestELF(elf.EM_X86_64, []string{"libc.so.6", "libmissing.so.1"}, "$ORIGIN/lib", []string{"/lib64/ld-linux-x86-64.so.2"}))
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
			if err := os.WriteFile(path, linuxTestELF(elf.EM_X86_64, []string{tc.dependency}, tc.runpath, []string{tc.interpreter}), 0755); err != nil {
				t.Fatal(err)
			}
			if _, err := inspectLinuxELF(path, "amd64", true); err == nil {
				t.Fatal("构建机路径、未知加载器及超出基线的符号必须拒绝")
			}
		})
	}
}

func TestLinuxELFInterpreterPolicy(t *testing.T) {
	for _, target := range []struct {
		arch, loader, otherLoader string
		machine                   elf.Machine
	}{
		{"amd64", "/lib64/ld-linux-x86-64.so.2", "/lib/ld-linux-aarch64.so.1", elf.EM_X86_64},
		{"arm64", "/lib/ld-linux-aarch64.so.1", "/lib64/ld-linux-x86-64.so.2", elf.EM_AARCH64},
	} {
		t.Run(target.arch, func(t *testing.T) {
			// 可直接执行的共享库允许一个本架构加载器，但仍是ET_DYN、正确SONAME和相对RUNPATH；
			// 空段或重复段不能按“没有解释器”处理，程序也不能借共享库规则省略加载器。
			for _, tc := range []struct {
				name         string
				program      bool
				interpreters []string
				valid        bool
			}{
				{"shared-no-interp", false, nil, true},
				{"shared-native-interp", false, []string{target.loader}, true},
				{"shared-foreign-interp", false, []string{target.otherLoader}, false},
				{"shared-unknown-interp", false, []string{"/private/loader"}, false},
				{"shared-multiple-interps", false, []string{target.loader, target.loader}, false},
				{"shared-empty-interp", false, []string{""}, false},
				{"shared-empty-then-native", false, []string{"", target.loader}, false},
				{"program-native-interp", true, []string{target.loader}, true},
				{"program-foreign-interp", true, []string{target.otherLoader}, false},
				{"program-unknown-interp", true, []string{"/private/loader"}, false},
				{"program-no-interp", true, nil, false},
				{"program-multiple-interps", true, []string{target.loader, target.loader}, false},
				{"program-empty-interp", true, []string{""}, false},
				{"program-empty-then-native", true, []string{"", target.loader}, false},
			} {
				t.Run(tc.name, func(t *testing.T) {
					name, runpath := "libinterpreter-test.so.1", "$ORIGIN"
					var sonames []string
					if tc.program {
						name, runpath = "tool", "$ORIGIN/lib"
					} else {
						sonames = []string{name}
					}
					file := filepath.Join(noticeTempDir(t), name)
					data := linuxTestELF(target.machine, []string{"libc.so.6"}, runpath, tc.interpreters, sonames...)
					if err := os.WriteFile(file, data, 0755); err != nil {
						t.Fatal(err)
					}
					_, err := inspectLinuxELF(file, target.arch, tc.program)
					if (err == nil) != tc.valid {
						t.Fatalf("ELF解释器数量及目标架构校验不符：%v", err)
					}
				})
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

func TestLinuxLibrarySourceBuildProvenance(t *testing.T) {
	// 这里只检查已登记来源与库元数据的关系；真实归档字节、许可摘要仍由linuxBundle统一核验。
	pin := linuxSourcePins["systemd"]
	source := linuxSource{ID: "systemd", Name: "systemd", Version: pin.version,
		Archives: []linuxArchive{{File: "sources/systemd-258.tar.gz", SHA256: pin.sha256}},
		Licenses: []licenseFile{{File: "licenses/systemd/LICENSE.LGPL2.1", SHA256: strings.Repeat("a", 64)}}}
	library := linuxLibrary{File: "lib/libprivate-test.so.258", SHA256: strings.Repeat("b", 64),
		OriginalSHA256: strings.Repeat("c", 64), Architecture: "amd64", Source: "systemd",
		Version: pin.version, BuildPath: "src/shared/libprivate-test.so.258"}
	if err := linuxLibrarySource(library, source); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		change func(*linuxLibrary, *linuxSource)
	}{
		{"absolute-build-path", func(l *linuxLibrary, _ *linuxSource) { l.BuildPath = "/work/libprivate.so" }},
		{"parent-build-path", func(l *linuxLibrary, _ *linuxSource) { l.BuildPath = "../libprivate.so" }},
		{"nested-parent-build-path", func(l *linuxLibrary, _ *linuxSource) { l.BuildPath = "src/../../libprivate.so" }},
		{"windows-build-path", func(l *linuxLibrary, _ *linuxSource) { l.BuildPath = `C:\work\libprivate.so` }},
		{"control-build-path", func(l *linuxLibrary, _ *linuxSource) { l.BuildPath = "src/lib\x1bprivate.so" }},
		{"missing-build-path", func(l *linuxLibrary, _ *linuxSource) { l.BuildPath = "" }},
		{"missing-original-hash", func(l *linuxLibrary, _ *linuxSource) { l.OriginalSHA256 = "" }},
		{"unknown-source", func(l *linuxLibrary, _ *linuxSource) { l.Source = "unregistered" }},
		{"unknown-source-kind", func(l *linuxLibrary, s *linuxSource) { l.Source, s.ID = "unregistered", "unregistered" }},
		{"missing-source", func(_ *linuxLibrary, s *linuxSource) { *s = linuxSource{} }},
		{"missing-source-archive", func(_ *linuxLibrary, s *linuxSource) { s.Archives = nil }},
		{"missing-source-license", func(_ *linuxLibrary, s *linuxSource) { s.Licenses = nil }},
		{"wrong-build-version", func(l *linuxLibrary, _ *linuxSource) { l.Version = "257" }},
		{"wrong-source-version", func(_ *linuxLibrary, s *linuxSource) { s.Version = "257" }},
		{"forged-debian-package", func(l *linuxLibrary, _ *linuxSource) { l.Package = "libprivate" }},
		{"forged-debian-archive", func(l *linuxLibrary, _ *linuxSource) { l.PackageArchive = "sources/libprivate.deb" }},
		{"forged-source-binary-archive", func(_ *linuxLibrary, s *linuxSource) {
			s.BinaryArchives = []linuxArchive{{File: "sources/libprivate.deb"}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l, s := library, source
			tc.change(&l, &s)
			if err := linuxLibrarySource(l, s); err == nil {
				t.Fatal("构建库必须有固定来源、完整许可和受限相对构建路径，不能伪造Debian原包")
			}
		})
	}

	debian := linuxSource{ID: "debian-example", Name: "example", Version: "1-2",
		Archives:       []linuxArchive{{File: "sources/example.dsc"}},
		BinaryArchives: []linuxArchive{{File: "sources/binaries/example.deb"}},
		Licenses:       []licenseFile{{File: "licenses/example-copyright"}}}
	library.Source, library.Version, library.BuildPath = debian.ID, "1-2+b1", ""
	library.Package, library.PackageArchive = "libexample1", debian.BinaryArchives[0].File
	if err := linuxLibrarySource(library, debian); err != nil {
		t.Fatal(err)
	}
	library.BuildPath = "src/libexample.so"
	if err := linuxLibrarySource(library, debian); err == nil {
		t.Fatal("Debian库不能借构建路径规避原包追溯")
	}
	library.BuildPath, library.PackageArchive = "", "sources/unregistered.deb"
	if err := linuxLibrarySource(library, debian); err == nil {
		t.Fatal("Debian库仍须匹配登记的精确原包")
	}
	library.PackageArchive, library.Package = debian.BinaryArchives[0].File, ""
	if err := linuxLibrarySource(library, debian); err == nil {
		t.Fatal("Debian库不能省略对应二进制包名")
	}
}

func TestLinuxLibrarySONAMEMatchesPayloadName(t *testing.T) {
	for _, tc := range []struct {
		name    string
		sonames []string
		valid   bool
	}{
		{"matching", []string{"libprivate-test.so.258"}, true},
		{"missing", nil, false},
		{"mismatched", []string{"libother.so.258"}, false},
		{"duplicate", []string{"libprivate-test.so.258", "libother.so.258"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := filepath.Join(noticeTempDir(t), "libprivate-test.so.258")
			data := linuxTestELF(elf.EM_X86_64, []string{"libc.so.6"}, "$ORIGIN", nil, tc.sonames...)
			if err := os.WriteFile(file, data, 0755); err != nil {
				t.Fatal(err)
			}
			_, err := inspectLinuxELF(file, "amd64", false)
			if (err == nil) != tc.valid {
				t.Fatalf("共享库SONAME与随包文件名校验不符：%v", err)
			}
		})
	}
}

func TestLinuxPrivateLibraryTransitiveClosure(t *testing.T) {
	objects := map[string][]string{"busctl": {"libprivate-test.so.258"},
		"libprivate-test.so.258": {"libtransitive-test.so.1", "libc.so.6"}}
	libraries := map[string]bool{"libprivate-test.so.258": true}
	if err := linuxClosure(objects, libraries); err == nil {
		t.Fatal("随包构建私库不能遗漏它的传递依赖")
	}
	objects["libtransitive-test.so.1"] = []string{"libc.so.6"}
	libraries["libtransitive-test.so.1"] = true
	if err := linuxClosure(objects, libraries); err != nil {
		t.Fatal(err)
	}
}

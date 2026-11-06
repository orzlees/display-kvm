package main

import (
	"archive/tar"
	"compress/gzip"
	"debug/macho"
	"encoding/binary"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func bundledToolsFixture(t *testing.T) (string, nativeToolManifest) {
	t.Helper()
	app := filepath.Join(noticeTempDir(t), "DisplayKvm.app")
	resource := filepath.Join(app, "Contents", "Resources", "licenses", "m1ddc")
	if err := os.MkdirAll(resource, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(app, "Contents", "MacOS"), 0755); err != nil {
		t.Fatal(err)
	}
	// 使用仓库内固定原件作为来源证据，不查询网络、用户工具或真实显示器。
	for _, file := range []string{"SOURCE.json", "upstream/LICENSE"} {
		data, err := os.ReadFile(filepath.Join("..", "..", "third_party", "m1ddc", filepath.FromSlash(file)))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(resource, filepath.Base(file)), data, 0644); err != nil {
			t.Fatal(err)
		}
	}
	var sources []licenseFile
	for _, source := range m1ddcUpstreamFiles {
		rel := "Contents/Resources/licenses/m1ddc/upstream/" + source.File
		data, err := os.ReadFile(filepath.Join("..", "..", "third_party", "m1ddc", "upstream", filepath.FromSlash(source.File)))
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(app, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0644); err != nil {
			t.Fatal(err)
		}
		sources = append(sources, licenseFile{File: rel, SHA256: source.SHA256})
	}
	binaryPath := filepath.Join(app, "Contents", "MacOS", "m1ddc")
	writeBundledMachO(t, binaryPath, macho.CpuArm64)
	hash, err := digest(binaryPath)
	if err != nil {
		t.Fatal(err)
	}
	manifest := nativeToolManifest{Schema: 1, Tools: []nativeToolRecord{{
		Name: "m1ddc", File: "Contents/MacOS/m1ddc", SHA256: hash,
		GOOS: "darwin", GOARCH: "arm64", Version: "1.3.1",
		Repository: "https://github.com/waydabber/m1ddc", Tag: "v1.3.1",
		Commit: "7339ceebb03f691b1eaeb8471dca301d215d19c8", License: "MIT",
		SourceManifest: licenseFile{File: "Contents/Resources/licenses/m1ddc/SOURCE.json", SHA256: m1ddcSourceSHA256},
		Licenses:       []licenseFile{{File: "Contents/Resources/licenses/m1ddc/LICENSE", SHA256: m1ddcLicenseSHA256}},
		Sources:        sources,
	}}}
	writeBundledManifest(t, app, manifest)
	return app, manifest
}

func writeBundledMachO(t *testing.T, path string, cpu macho.Cpu) {
	t.Helper()
	// 仅生成可被解析的Mach-O头；测试从不执行该占位程序，也不假装它能控制硬件。
	words := []uint32{macho.Magic64, uint32(cpu), 0, uint32(macho.TypeExec), 0, 0, 0, 0}
	var data []byte
	for _, word := range words {
		data = binary.LittleEndian.AppendUint32(data, word)
	}
	if err := os.WriteFile(path, data, 0755); err != nil {
		t.Fatal(err)
	}
}

func writeBundledManifest(t *testing.T, app string, manifest nativeToolManifest) {
	t.Helper()
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(app, "Contents", "Resources", "bundled-tools.json"), data, 0644); err != nil {
		t.Fatal(err)
	}
}

func TestBundledToolsRejectMissingTool(t *testing.T) {
	app, _ := bundledToolsFixture(t)
	if err := os.Remove(filepath.Join(app, "Contents", "MacOS", "m1ddc")); err != nil {
		t.Fatal(err)
	}
	if _, err := bundledTools(app); err == nil {
		t.Fatal("缺失的随包工具被接受")
	}
}

func TestBundledToolsRejectReplacedTool(t *testing.T) {
	app, _ := bundledToolsFixture(t)
	path := filepath.Join(app, "Contents", "MacOS", "m1ddc")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := f.Write([]byte("replaced"))
	closeErr := f.Close()
	if writeErr != nil || closeErr != nil {
		t.Fatalf("fixture写入失败：%v %v", writeErr, closeErr)
	}
	if _, err := bundledTools(app); err == nil || !strings.Contains(err.Error(), "哈希") {
		t.Fatalf("被替换工具未被哈希检查拒绝：%v", err)
	}
}

func TestBundledToolsRejectWrongArchitecture(t *testing.T) {
	app, manifest := bundledToolsFixture(t)
	path := filepath.Join(app, "Contents", "MacOS", "m1ddc")
	writeBundledMachO(t, path, macho.CpuAmd64)
	hash, err := digest(path)
	if err != nil {
		t.Fatal(err)
	}
	manifest.Tools[0].SHA256 = hash
	writeBundledManifest(t, app, manifest)
	if _, err := bundledTools(app); err == nil || !strings.Contains(err.Error(), "arm64") {
		t.Fatalf("错误架构未被拒绝：%v", err)
	}
}

func TestBundledToolsRejectUnpinnedSourceOrLicense(t *testing.T) {
	for _, change := range []string{"source", "source-file", "license", "commit", "version", "unknown-tool", "binary-path"} {
		t.Run(change, func(t *testing.T) {
			app, manifest := bundledToolsFixture(t)
			record := &manifest.Tools[0]
			switch change {
			case "source":
				if err := os.WriteFile(filepath.Join(app, filepath.FromSlash(record.SourceManifest.File)), []byte("{}\n"), 0644); err != nil {
					t.Fatal(err)
				}
				hash, err := digest(filepath.Join(app, filepath.FromSlash(record.SourceManifest.File)))
				if err != nil {
					t.Fatal(err)
				}
				record.SourceManifest.SHA256 = hash
			case "source-file":
				if err := os.WriteFile(filepath.Join(app, filepath.FromSlash(record.Sources[0].File)), []byte("changed source\n"), 0644); err != nil {
					t.Fatal(err)
				}
			case "license":
				if err := os.WriteFile(filepath.Join(app, filepath.FromSlash(record.Licenses[0].File)), []byte("other license\n"), 0644); err != nil {
					t.Fatal(err)
				}
				hash, err := digest(filepath.Join(app, filepath.FromSlash(record.Licenses[0].File)))
				if err != nil {
					t.Fatal(err)
				}
				record.Licenses[0].SHA256 = hash
			case "commit":
				record.Commit = strings.Repeat("0", 40)
			case "version":
				record.Version = "9.9.9"
			case "unknown-tool":
				record.Name = "other"
			case "binary-path":
				record.File = "../../private-tool"
			}
			writeBundledManifest(t, app, manifest)
			if _, err := bundledTools(app); err == nil {
				t.Fatal("未锁定的工具、来源或许可被接受")
			}
		})
	}
}

func TestBundledToolsRecordAndArchiveContainPinnedFiles(t *testing.T) {
	app, manifest := bundledToolsFixture(t)
	tools, err := bundledTools(app)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 || tools[0].File != "DisplayKvm.app/Contents/MacOS/m1ddc" || tools[0].SHA256 != manifest.Tools[0].SHA256 {
		t.Fatalf("原生工具记录不符：%+v", tools)
	}
	payload := filepath.Join(noticeTempDir(t), "candidate")
	if err := copyTree(app, filepath.Join(payload, "DisplayKvm.app")); err != nil {
		t.Fatal(err)
	}
	if err := verifyBundledCopies(payload, tools); err != nil {
		t.Fatal(err)
	}
	if err := finishPayload(payload, packageRecord{Schema: 1, Label: "candidate", NativeTools: tools}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(payload, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var packaged packageRecord
	if err := json.Unmarshal(data, &packaged); err != nil || !reflect.DeepEqual(packaged.NativeTools, tools) {
		t.Fatalf("归档原生清单未保留：%v %+v", err, packaged.NativeTools)
	}
	data, err = os.ReadFile(filepath.Join(payload, "SHA256SUMS"))
	if err != nil {
		t.Fatal(err)
	}
	files := []string{tools[0].File, tools[0].SourceManifest.File, tools[0].Licenses[0].File}
	for _, source := range tools[0].Sources {
		files = append(files, source.File)
	}
	for _, file := range files {
		if !strings.Contains(string(data), "  "+file+"\n") {
			t.Fatalf("哈希表缺少%s", file)
		}
	}
	dist := noticeTempDir(t)
	archive, err := makeArchive(payload, dist, false)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(filepath.Join(dist, archive.File))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	reader := tar.NewReader(gz)
	found := false
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Name == "candidate/"+tools[0].File {
			found = true
			data, err := io.ReadAll(reader)
			if err != nil {
				t.Fatal(err)
			}
			extracted := filepath.Join(noticeTempDir(t), "m1ddc")
			if err := os.WriteFile(extracted, data, 0600); err != nil {
				t.Fatal(err)
			}
			hash, err := digest(extracted)
			if err != nil {
				t.Fatal(err)
			}
			if hash != tools[0].SHA256 {
				t.Fatalf("归档内随包工具哈希不匹配：得到%s，预期%s", hash, tools[0].SHA256)
			}
		}
	}
	if !found {
		t.Fatal("归档缺少随包工具")
	}
}

func TestBundledToolsRejectChangedPayloadCopy(t *testing.T) {
	app, _ := bundledToolsFixture(t)
	tools, err := bundledTools(app)
	if err != nil {
		t.Fatal(err)
	}
	payload := noticeTempDir(t)
	if err := copyTree(app, filepath.Join(payload, "DisplayKvm.app")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(payload, filepath.FromSlash(tools[0].File)), []byte("changed"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := verifyBundledCopies(payload, tools); err == nil {
		t.Fatal("复制期间变化的原生工具被接受")
	}
}

func TestBundledToolsKeepGoProgramsStrict(t *testing.T) {
	app, _ := bundledToolsFixture(t)
	for _, name := range []string{"displaykvm", "displaykvmd", "displaykvm-cli"} {
		writeBundledMachO(t, filepath.Join(app, "Contents", "MacOS", name), macho.CpuArm64)
	}
	if _, err := desktopBinaries(app); err == nil || !strings.Contains(err.Error(), "构建信息无效") {
		t.Fatalf("原生工具支持放宽了Go入口校验：%v", err)
	}
}

func TestBundledToolsRejectUserDataAndUnknownExecutables(t *testing.T) {
	for _, rel := range []string{"Contents/Resources/config.json", "Contents/MacOS/other-tool", "Contents/Resources/private/startup.json"} {
		t.Run(rel, func(t *testing.T) {
			app, _ := bundledToolsFixture(t)
			path := filepath.Join(app, filepath.FromSlash(rel))
			if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("private"), 0644); err != nil {
				t.Fatal(err)
			}
			if _, err := bundledTools(app); err == nil {
				t.Fatalf("用户数据或未知载荷被接受：%s", rel)
			}
		})
	}
}

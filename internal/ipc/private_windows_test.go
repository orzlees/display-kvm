//go:build windows

package ipc

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/orzlee/displayKvm/internal/config"
	"github.com/orzlee/displayKvm/internal/model"
)

func TestWindowsPrivateSecurityDescriptors(t *testing.T) {
	api, err := windowsAPI()
	if err != nil {
		t.Fatal(err)
	}
	sid, err := windowsCurrentSID(api)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, sddl string
		private    bool
		allowed    bool
	}{
		{"private", "O:" + sid + "D:P(A;;FA;;;" + sid + ")", true, true},
		{"null-dacl", "O:" + sid + "D:NO_ACCESS_CONTROL", true, false},
		{"foreign-owner", "O:S-1-1-0D:P(A;;FA;;;" + sid + ")", true, false},
		{"extra-reader", "O:" + sid + "D:P(A;;FA;;;" + sid + ")(A;;FR;;;WD)", true, false},
		{"extra-system", "O:" + sid + "D:P(A;;FA;;;" + sid + ")(A;;FA;;;SY)", true, false},
		{"missing-user", "O:" + sid + "D:P", true, false},
		{"ancestor-reader", "O:SYD:P(A;;FA;;;SY)(A;;FRFX;;;WD)", false, true},
		{"ancestor-create-subdirectory", "O:SYD:P(A;;FA;;;SY)(A;;0x4;;;WD)", false, true},
		{"ancestor-writer", "O:SYD:P(A;;FA;;;SY)(A;;GW;;;WD)", false, false},
		{"ancestor-write-attributes", "O:SYD:P(A;;FA;;;SY)(A;;0x100;;;WD)", false, false},
		{"ancestor-delete-child", "O:SYD:P(A;;FA;;;SY)(A;;DC;;;WD)", false, false},
		{"ancestor-inherit-only", "O:SYD:P(A;;FA;;;SY)(A;OICIIO;GA;;;CO)", false, true},
		{"ancestor-object-ace", "O:SYD:P(OA;;FR;00000000-0000-0000-0000-000000000001;;WD)", false, false},
		{"ancestor-foreign-owner", "O:WDD:P(A;;FR;;;WD)", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text, err := syscall.UTF16PtrFromString(tc.sddl)
			if err != nil {
				t.Fatal(err)
			}
			var descriptor uintptr
			ok, _, callErr := api.convertDescriptor.Call(uintptr(unsafe.Pointer(text)), 1, uintptr(unsafe.Pointer(&descriptor)), 0)
			if ok == 0 {
				t.Fatal(callErr)
			}
			checkErr := windowsCheckDescriptor(api, descriptor, sid, tc.private)
			_, freeErr := syscall.LocalFree(syscall.Handle(descriptor))
			if freeErr != nil {
				t.Fatal(freeErr)
			}
			if (checkErr == nil) != tc.allowed {
				t.Fatalf("描述符检查结果与预期不符: allowed=%v err=%v", tc.allowed, checkErr)
			}
		})
	}
}

func TestWindowsPrivateRuntimeRejectsUnsafeExistingDirectory(t *testing.T) {
	root := t.TempDir()
	directory, release, err := privateRuntime(root, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	api, err := windowsAPI()
	if err != nil {
		t.Fatal(err)
	}
	sid, err := windowsCurrentSID(api)
	if err != nil {
		t.Fatal(err)
	}
	windowsTestSetDACL(t, directory, "D:P(A;;FA;;;"+sid+")(A;;FR;;;WD)")
	if instance, err := Acquire(root); model.CodeOf(err) != model.CodePermissionDenied {
		if instance != nil {
			instance.Close()
		}
		t.Fatalf("额外主体可读的私有目录未拒绝: %v", err)
	}
	name, err := syscall.UTF16PtrFromString(directory)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := syscall.CreateFile(name, windowsReadControl|windowsReadAttributes, syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE, nil, syscall.OPEN_EXISTING, syscall.FILE_FLAG_BACKUP_SEMANTICS|syscall.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		t.Fatal(err)
	}
	checkErr := windowsCheckSecurity(api, handle, sid, true)
	if err := syscall.CloseHandle(handle); err != nil {
		t.Fatal(err)
	}
	if model.CodeOf(checkErr) != model.CodePermissionDenied {
		t.Fatal("拒绝启动时不应自动修复既有目录DACL")
	}
}

func TestWindowsPrivateRuntimeRejectsWritableAncestor(t *testing.T) {
	base := filepath.Join(t.TempDir(), "unsafe-parent")
	if err := os.Mkdir(base, 0700); err != nil {
		t.Fatal(err)
	}
	api, err := windowsAPI()
	if err != nil {
		t.Fatal(err)
	}
	sid, err := windowsCurrentSID(api)
	if err != nil {
		t.Fatal(err)
	}
	windowsTestSetDACL(t, base, "D:P(A;;FA;;;"+sid+")(A;;GW;;;WD)")
	if instance, err := Acquire(base); model.CodeOf(err) != model.CodePermissionDenied {
		if instance != nil {
			instance.Close()
		}
		t.Fatalf("可被普通主体修改的祖先未拒绝: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(base, "DisplayKVM")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("祖先未核验就创建私有目录: %v", err)
	}
}

func TestWindowsPrivateDirectoryLeaseAndFailureRelease(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, "new-config")
	directory, release, err := privateRuntime(base, true)
	if err != nil {
		t.Fatal(err)
	}
	if release == nil {
		t.Fatal("成功租约没有释放方法")
	}
	defer release()
	if err := os.Rename(base, base+"-moved"); err == nil {
		t.Fatal("租约未固定配置祖先目录")
	}
	if err := os.Rename(directory, directory+"-moved"); err == nil {
		t.Fatal("租约未固定最终运行目录")
	}
	if err := os.Rename(filepath.Dir(directory), filepath.Dir(directory)+"-moved"); err == nil {
		t.Fatal("租约未固定DisplayKVM祖先目录")
	}
	lockPath := filepath.Join(directory, "instance.lock")
	file, err := openPrivate(lockPath, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	windowsTestSetDACL(t, lockPath, "D:NO_ACCESS_CONTROL")
	if instance, err := Acquire(base); model.CodeOf(err) != model.CodePermissionDenied {
		if instance != nil {
			instance.Close()
		}
		t.Fatalf("不安全锁文件未拒绝: %v", err)
	}
	if err := os.Rename(base, base+"-moved"); err != nil {
		t.Fatalf("Acquire失败遗留目录句柄: %v", err)
	}
}

func TestWindowsPrivateDirectoryLeaseBlocksEmptyDirectoryRemoval(t *testing.T) {
	base := filepath.Join(t.TempDir(), "new-config")
	directory, release, err := privateRuntime(base, true)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	// 此时runtime尚无锁文件或端点，删除失败必须来自句柄固定，
	// 不能把非空目录的普通删除失败当作DELETE共享已生效的证据。
	if err := os.Remove(directory); err == nil {
		t.Fatal("租约持有时可删除空运行目录")
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(directory); err != nil {
		t.Fatalf("租约释放后仍无法删除空运行目录: %v", err)
	}
	parent := filepath.Dir(directory)
	if err := os.Rename(parent, parent+"-released"); err != nil {
		t.Fatalf("租约释放后仍无法改名祖先目录: %v", err)
	}
}

func TestWindowsPrivateFilesRejectDACLAndMultipleLinks(t *testing.T) {
	for _, sddl := range []string{"D:NO_ACCESS_CONTROL", "D:P(A;;FA;;;%s)(A;;FR;;;WD)"} {
		t.Run(sddl, func(t *testing.T) {
			root := t.TempDir()
			instance, err := Acquire(root)
			if err != nil {
				t.Fatal(err)
			}
			defer instance.Close()
			endpoint := instance.Endpoint("127.0.0.1:12345")
			if err := instance.Publish(endpoint); err != nil {
				t.Fatal(err)
			}
			api, err := windowsAPI()
			if err != nil {
				t.Fatal(err)
			}
			sid, err := windowsCurrentSID(api)
			if err != nil {
				t.Fatal(err)
			}
			path := model.EndpointPath(root)
			windowsTestSetDACL(t, path, strings.ReplaceAll(sddl, "%s", sid))
			if _, err := LoadEndpoint(root); model.CodeOf(err) != model.CodePermissionDenied {
				t.Fatalf("不安全DACL未拒绝: %v", err)
			}
			// 只还原本测试私有目录中的夹具，避免清理逻辑的安全拒绝掩盖测试断言。
			windowsTestSetDACL(t, path, "D:P(A;;FA;;;"+sid+")")
		})
	}
	root := t.TempDir()
	instance, err := Acquire(root)
	if err != nil {
		t.Fatal(err)
	}
	defer instance.Close()
	if err := instance.Publish(instance.Endpoint("127.0.0.1:12345")); err != nil {
		t.Fatal(err)
	}
	path := model.EndpointPath(root)
	if err := os.Link(path, filepath.Join(filepath.Dir(path), "second-link")); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadEndpoint(root); model.CodeOf(err) != model.CodePermissionDenied {
		t.Fatalf("多链接私有文件未拒绝: %v", err)
	}
	if err := os.Remove(filepath.Join(filepath.Dir(path), "second-link")); err != nil {
		t.Fatal(err)
	}
}

func TestWindowsPrivateRuntimeRejectsReparseAndNonLocalPaths(t *testing.T) {
	for _, path := range []string{`\\server\share\config`, `\\?\C:\config`, `\\.\C:\config`, `C:relative`, `relative`, `C:\bad:stream`} {
		if _, release, err := privateRuntime(path, false); err == nil {
			if release != nil {
				release()
			}
			t.Fatalf("不支持的路径被放行: %q", path)
		}
	}
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "linked")
	if err := os.Symlink(target, link); err != nil {
		if errors.Is(err, syscall.Errno(1314)) {
			t.Skip("符号链接夹具需要开发者模式或创建符号链接权限；其余Windows权限测试独立执行")
		}
		t.Fatal(err)
	}
	if _, release, err := privateRuntime(link, true); model.CodeOf(err) != model.CodePermissionDenied {
		if release != nil {
			release()
		}
		t.Fatalf("重解析目录未拒绝: %v", err)
	}
}

func TestWindowsPrivateEndpointRejectsReparseFile(t *testing.T) {
	root := t.TempDir()
	instance, err := Acquire(root)
	if err != nil {
		t.Fatal(err)
	}
	defer instance.Close()
	file, err := createPrivateTemp(instance.directory, ".target-*")
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(file.Name(), model.EndpointPath(root)); err != nil {
		if errors.Is(err, syscall.Errno(1314)) {
			t.Skip("符号链接夹具需要开发者模式或创建符号链接权限")
		}
		t.Fatal(err)
	}
	if _, err := LoadEndpoint(root); model.CodeOf(err) != model.CodePermissionDenied {
		t.Fatalf("重解析文件未拒绝: %v", err)
	}
}

func TestWindowsEndpointReadFailureReleasesDirectoryLease(t *testing.T) {
	root := t.TempDir()
	instance, err := Acquire(root)
	if err != nil {
		t.Fatal(err)
	}
	defer instance.Close()
	// 只制造内容无效的私有文件，让读取走到解码失败路径；不登记到实例，
	// 关闭实例后仍可用该夹具检查发现读取自己的租约和文件句柄释放。
	if _, err := publishPrivate(instance.directory, "endpoint.json", []byte("{")); err != nil {
		t.Fatal(err)
	}
	if err := instance.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadEndpoint(root); model.CodeOf(err) != model.CodeInvalidRequest {
		t.Fatalf("未进入预期的内容拒绝路径: %v", err)
	}
	if err := os.Rename(root, root+"-released"); err != nil {
		t.Fatalf("端点读取失败遗留租约或文件句柄: %v", err)
	}
	if err := os.Rename(root+"-released", root); err != nil {
		t.Fatal(err)
	}
}

func TestWindowsPersistentLockIsNotTruncatedOrReplaced(t *testing.T) {
	root := t.TempDir()
	first, err := Acquire(root)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	const marker = "persistent-lock-marker"
	if _, err := first.lock.WriteAt([]byte(marker), 8); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(first.directory, "instance.lock")
	if err := os.Rename(path, path+"-replaced"); err == nil {
		t.Fatal("持锁时允许替换永久锁文件")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := Acquire(root)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	buffer := make([]byte, len(marker))
	if _, err := second.lock.ReadAt(buffer, 8); err != nil || string(buffer) != marker {
		t.Fatalf("重新取得锁截断了永久文件: %v", err)
	}
}

func TestWindowsPrivatePublicationRejectsReplacedSourceIdentity(t *testing.T) {
	root := t.TempDir()
	instance, err := Acquire(root)
	if err != nil {
		t.Fatal(err)
	}
	defer instance.Close()
	first, err := createPrivateTemp(instance.directory, ".first-*")
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := createPrivateTemp(instance.directory, ".second-*")
	if err != nil {
		t.Fatal(err)
	}
	info, statErr := second.Stat()
	if err := errors.Join(statErr, second.Close()); err != nil {
		t.Fatal(err)
	}
	target := model.EndpointPath(root)
	if err := replacePrivate(first.Name(), target, info); model.CodeOf(err) != model.CodePermissionDenied {
		t.Fatalf("未拒绝发布其他文件身份: %v", err)
	}
	if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("身份不匹配仍创建或替换了目标: %v", err)
	}
	if _, err := os.Lstat(first.Name()); err != nil {
		t.Fatalf("身份不匹配仍删除了源文件: %v", err)
	}
}

func TestWindowsRuntimeReaderAllowsRotationAndReleasesLease(t *testing.T) {
	root := t.TempDir()
	instance, err := Acquire(root)
	if err != nil {
		t.Fatal(err)
	}
	defer instance.Close()
	cfg, err := config.Load(strings.NewReader(`{"version":2,"displays":[],"input_schemes":[],"targets":[],"groups":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	first, err := instance.PublishRuntime("127.0.0.1:12345", cfg)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := openPrivate(model.EndpointPath(root), false)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	second, err := instance.PublishRuntime("127.0.0.1:12346", cfg)
	if err != nil {
		t.Fatalf("旧端点读句柄阻止原子换轮: %v", err)
	}
	data, readErr := io.ReadAll(reader)
	var old model.Endpoint
	if readErr != nil || json.Unmarshal(data, &old) != nil || old.Token != first.Token {
		t.Fatal("旧读句柄未保持原文件身份")
	}
	if _, err := LoadConfiguration(root, first); model.CodeOf(err) != model.CodeDaemonUnavailable {
		t.Fatalf("换轮后仍接受旧配置身份: %v", err)
	}
	if _, err := LoadConfiguration(root, second); err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if err := instance.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(root, root+"-released"); err != nil {
		t.Fatalf("读取或关闭遗留目录租约: %v", err)
	}
	if err := os.Rename(root+"-released", root); err != nil {
		t.Fatal(err)
	}
}

func TestWindowsIPCLockAcrossProcesses(t *testing.T) {
	root := t.TempDir()
	instance, err := Acquire(root)
	if err != nil {
		t.Fatal(err)
	}
	defer instance.Close()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"blocked", "available"} {
		if mode == "available" {
			if err := instance.Close(); err != nil {
				t.Fatal(err)
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		command := exec.CommandContext(ctx, executable, "-test.run=^TestWindowsIPCLockChild$")
		command.Env = append(os.Environ(), "DISPLAYKVM_TEST_LOCK_ROOT="+root, "DISPLAYKVM_TEST_LOCK_MODE="+mode)
		output, err := command.CombinedOutput()
		cancel()
		if err != nil {
			t.Fatalf("子进程锁断言失败: mode=%s err=%v output=%s", mode, err, output)
		}
	}
}

func TestWindowsIPCLockChild(t *testing.T) {
	root := os.Getenv("DISPLAYKVM_TEST_LOCK_ROOT")
	if root == "" {
		return
	}
	instance, err := Acquire(root)
	if os.Getenv("DISPLAYKVM_TEST_LOCK_MODE") == "blocked" {
		if instance != nil {
			instance.Close()
		}
		if model.CodeOf(err) != model.CodeDaemonUnavailable {
			t.Fatalf("跨进程独占锁未生效: %v", err)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := instance.Close(); err != nil {
		t.Fatal(err)
	}
}

func windowsTestSetDACL(t *testing.T, path, sddl string) {
	t.Helper()
	api, err := windowsAPI()
	if err != nil {
		t.Fatal(err)
	}
	text, err := syscall.UTF16PtrFromString(sddl)
	if err != nil {
		t.Fatal(err)
	}
	var descriptor uintptr
	ok, _, callErr := api.convertDescriptor.Call(uintptr(unsafe.Pointer(text)), 1, uintptr(unsafe.Pointer(&descriptor)), 0)
	if ok == 0 {
		t.Fatal(callErr)
	}
	defer func() {
		if _, err := syscall.LocalFree(syscall.Handle(descriptor)); err != nil {
			t.Errorf("释放测试描述符失败: %v", err)
		}
	}()
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	// 测试只在t.TempDir内改变自有夹具DACL，不调用命令行权限工具。
	setSecurity, err := api.advapi.FindProc("SetFileSecurityW")
	if err != nil {
		t.Fatal(err)
	}
	ok, _, callErr = setSecurity.Call(uintptr(unsafe.Pointer(name)), 0x80000004, descriptor)
	if ok == 0 {
		t.Fatal(callErr)
	}
}

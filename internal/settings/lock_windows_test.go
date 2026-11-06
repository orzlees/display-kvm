//go:build windows

package settings

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"unsafe"

	"github.com/orzlee/displayKvm/internal/model"
)

func windowsLockFixture(t *testing.T) (*Store, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "controlled")
	sid, err := windowsSettingsSID()
	if err != nil {
		t.Fatal(err)
	}
	if err := createWindowsSettingsDirectory(root, sid); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(Options{Environment: func() (Environment, error) {
		// Windows身份取自真实令牌；故意不用UID模拟当前用户。
		return Environment{ConfigDir: filepath.Join(root, "private"), UID: 123456}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	return store, root
}

func windowsExpectCode(t *testing.T, err error, code model.ErrorCode) {
	t.Helper()
	var problem *model.Error
	if !errors.As(err, &problem) || problem.Code != code {
		t.Fatalf("错误分类应为%s: %v", code, err)
	}
}

func TestWindowsSettingsWritersExcludeAliasesReleaseAndKeepOtherTargetsIndependent(t *testing.T) {
	store, root := windowsLockFixture(t)
	path := filepath.Join(root, "settings.json")
	writer, err := store.OpenWriter(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	otherStore, err := NewStore(Options{Environment: func() (Environment, error) { return store.environment, nil }})
	if err != nil {
		t.Fatal(err)
	}
	for _, alias := range []string{path, filepath.Join(root, ".", "settings.json"), filepath.Join(root, "SETTINGS.JSON"), filepath.Join(filepath.Dir(root), "CONTROLLED", "settings.json")} {
		other, err := otherStore.OpenWriter(context.Background(), alias)
		if other != nil {
			other.Close()
			t.Fatal("同一路径及大小写别名不得获得第二写者")
		}
		windowsExpectCode(t, err, model.CodeInvalidRequest)
	}
	independent, err := store.OpenWriter(context.Background(), filepath.Join(root, "other.json"))
	if err != nil {
		t.Fatalf("不同配置应能同时持锁: %v", err)
	}
	defer independent.Close()
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := otherStore.OpenWriter(context.Background(), path)
	if err != nil {
		t.Fatalf("关闭后未释放系统锁: %v", err)
	}
	defer reopened.Close()
	_, err = writer.Read()
	windowsExpectCode(t, err, model.CodeInvalidRequest)
	result, err := writer.Save(context.Background(), NewDocument(), "")
	windowsExpectCode(t, err, model.CodeInvalidRequest)
	if result.Saved == nil || *result.Saved {
		t.Fatalf("关闭的写者不能保存: %+v", result)
	}
	if err := checkSettingsLock(writer.lock); err == nil {
		t.Fatal("已关闭的句柄不得通过锁身份检查")
	}
}

func TestWindowsSettingsPathLockSurvivesAtomicReplacement(t *testing.T) {
	store, root := windowsLockFixture(t)
	path := filepath.Join(root, "settings.json")
	var original bytes.Buffer
	if err := Encode(&original, NewDocument()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, original.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	beforeFile, err := openManaged(path)
	if err != nil {
		t.Fatal(err)
	}
	// Windows路径Stat会延迟加载fileID；必须在替换前从真实句柄取得身份，
	// 否则事后SameFile可能把两份路径元数据都解析成替换后的新文件。
	before, statErr := beforeFile.Stat()
	if err := errors.Join(statErr, beforeFile.Close()); err != nil {
		t.Fatal(err)
	}
	writer, err := store.OpenWriter(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	snapshot, err := writer.Read()
	if err != nil {
		t.Fatal(err)
	}
	result, saveErr := writer.Save(context.Background(), snapshot.Document, snapshot.Revision)
	// Windows目录Sync可能报错；是否提交仍由既有真实字节三态判定，不要求伪造无错误。
	if result.Saved == nil || !*result.Saved || result.Snapshot == nil {
		t.Fatalf("原子替换后应证明真实字节已保存: %+v %v", result, saveErr)
	}
	after, err := os.Stat(path)
	if err != nil || os.SameFile(before, after) {
		t.Fatalf("测试必须实际替换配置文件身份: %v", err)
	}
	other, err := store.OpenWriter(context.Background(), filepath.Join(root, "SETTINGS.JSON"))
	if other != nil {
		other.Close()
		t.Fatal("替换后的配置身份不得生成另一个路径锁")
	}
	windowsExpectCode(t, err, model.CodeInvalidRequest)
}

func TestWindowsSettingsRejectsCaseSensitiveAncestorWithInsensitiveParents(t *testing.T) {
	store, root := windowsLockFixture(t)
	// kernel32是运行时已加载的KnownDLL；仅临时fixture使用设置标志的API，
	// 不借助命令行，也不向产品API缓存或真实用户目录增加可变状态。
	kernel := syscall.NewLazyDLL("kernel32.dll")
	volumeInfo := kernel.NewProc("GetVolumeInformationW")
	setInfo := kernel.NewProc("SetFileInformationByHandle")
	for _, proc := range []*syscall.LazyProc{volumeInfo, setInfo} {
		if err := proc.Find(); err != nil {
			if errors.Is(err, syscall.ERROR_PROC_NOT_FOUND) {
				t.Skipf("系统缺少构造逐目录大小写fixture的API: %v", err)
			}
			t.Fatal(err)
		}
	}
	volume, err := syscall.UTF16PtrFromString(filepath.VolumeName(root) + `\`)
	if err != nil {
		t.Fatal(err)
	}
	var filesystem [32]uint16
	value, _, callErr := volumeInfo.Call(uintptr(unsafe.Pointer(volume)), 0, 0, 0, 0, 0, uintptr(unsafe.Pointer(&filesystem[0])), uintptr(len(filesystem)))
	if value == 0 {
		t.Fatal(windowsSettingsFailure("GetVolumeInformationW(测试卷)", callErr))
	}
	if name := syscall.UTF16ToString(filesystem[:]); name != "NTFS" {
		t.Skipf("逐目录大小写回归fixture要求临时目录位于NTFS，当前为%s", name)
	}
	setCase := func(path string, flags uint32, allowUnavailable bool) {
		t.Helper()
		name, err := syscall.UTF16PtrFromString(path)
		if err != nil {
			t.Fatal(err)
		}
		// 修改目录标志要求写属性、创建文件/子目录及删除子项权限；所有对象
		// 均在本次私有临时目录中。成功启用祖先后，子目录设置失败必须报错。
		access := uint32(windowsSettingsReadControl | windowsSettingsAttributes | windowsSettingsList | 0x100 | 0x2 | 0x4 | 0x40)
		handle, err := syscall.CreateFile(name, access, syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE, nil, syscall.OPEN_EXISTING, syscall.FILE_FLAG_BACKUP_SEMANTICS|syscall.FILE_FLAG_OPEN_REPARSE_POINT, 0)
		if err != nil {
			if allowUnavailable && (errors.Is(err, syscall.ERROR_ACCESS_DENIED) || errors.Is(err, syscall.ERROR_PRIVILEGE_NOT_HELD)) {
				t.Skipf("当前权限无法打开临时目录以设置大小写标志: %v", err)
			}
			t.Fatal(windowsSettingsFailure("CreateFileW(大小写测试目录)", err))
		}
		file := os.NewFile(uintptr(handle), path)
		defer func() {
			if err := file.Close(); err != nil {
				t.Error(err)
			}
		}()
		if _, err := windowsSettingsHandleInfo(file, true); err != nil {
			t.Fatal(err)
		}
		var actual uint32
		if err := windowsSettingsBool("GetFileInformationByHandleEx", file.Fd(), 23, uintptr(unsafe.Pointer(&actual)), unsafe.Sizeof(actual)); err != nil {
			// 有效目录句柄及固定四字节布局下，这些码表示当前系统不支持类23；
			// 其他查询错误不能当作环境不支持而跳过。
			if allowUnavailable && (errors.Is(err, syscall.Errno(1)) || errors.Is(err, syscall.Errno(50)) || errors.Is(err, syscall.Errno(87))) {
				t.Skipf("当前系统不支持逐目录大小写查询: %v", err)
			}
			t.Fatal(err)
		}
		value, _, callErr := setInfo.Call(file.Fd(), 23, uintptr(unsafe.Pointer(&flags)), unsafe.Sizeof(flags))
		if value == 0 {
			if allowUnavailable && (errors.Is(callErr, syscall.ERROR_ACCESS_DENIED) || errors.Is(callErr, syscall.ERROR_PRIVILEGE_NOT_HELD) || errors.Is(callErr, syscall.Errno(1)) || errors.Is(callErr, syscall.Errno(50))) {
				t.Skipf("当前系统或权限禁止为临时目录启用大小写敏感: %v", windowsSettingsFailure("SetFileInformationByHandle(FileCaseSensitiveInfo)", callErr))
			}
			t.Fatal(windowsSettingsFailure("SetFileInformationByHandle(FileCaseSensitiveInfo)", callErr))
		}
		if err := windowsSettingsBool("GetFileInformationByHandleEx", file.Fd(), 23, uintptr(unsafe.Pointer(&actual)), unsafe.Sizeof(actual)); err != nil {
			t.Fatal(err)
		}
		if actual&1 != flags {
			t.Fatalf("目录%s未实际取得所需大小写标志: got=%#x want=%#x", path, actual, flags)
		}
	}
	ancestor := filepath.Join(root, "s")
	if err := os.Mkdir(ancestor, 0700); err != nil {
		t.Fatal(err)
	}
	setCase(ancestor, 1, true)
	var contents bytes.Buffer
	if err := Encode(&contents, NewDocument()); err != nil {
		t.Fatal(err)
	}
	var paths []string
	var identities []os.FileInfo
	for _, child := range []string{"A", "a"} {
		directory := filepath.Join(ancestor, child)
		if err := os.Mkdir(directory, 0700); err != nil {
			t.Fatal(err)
		}
		// 子目录可能继承敏感标志；在创建配置前明确清除并从真实句柄复查。
		setCase(directory, 0, false)
		path := filepath.Join(directory, "settings.json")
		if err := os.WriteFile(path, contents.Bytes(), 0600); err != nil {
			t.Fatal(err)
		}
		file, err := openManaged(path)
		if err != nil {
			t.Fatal(err)
		}
		info, statErr := file.Stat()
		if err := errors.Join(statErr, file.Close()); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, path)
		identities = append(identities, info)
	}
	if os.SameFile(identities[0], identities[1]) {
		t.Fatal("回归fixture必须含两个真实存在且句柄身份不同的配置")
	}
	// ConfigDir仍在root/private，祖先s只属于配置目标链；仅检查内部锁目录
	// 或直接父目录时，两条路径均会通过预检，完整路径折叠随后却生成同一锁键。
	for _, path := range paths {
		t.Run(filepath.Base(filepath.Dir(path)), func(t *testing.T) {
			writer, err := store.OpenWriter(context.Background(), path)
			if writer != nil {
				if err := writer.Close(); err != nil {
					t.Error(err)
				}
				t.Fatal("祖先大小写敏感时不得创建配置写者")
			}
			windowsExpectCode(t, err, model.CodeUnsupportedOperation)
		})
	}
	if err := os.Rename(ancestor, ancestor+"-released"); err != nil {
		t.Fatalf("拒绝敏感祖先后必须释放目标链的全部短期句柄: %v", err)
	}
}

func TestWindowsSettingsLockReplacementIsBlockedOrStopsSaving(t *testing.T) {
	store, root := windowsLockFixture(t)
	path := filepath.Join(root, "settings.json")
	writer, err := store.OpenWriter(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	for _, original := range []string{writer.lock.Name(), filepath.Dir(writer.lock.Name())} {
		before, err := os.Lstat(original)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(original, original+"-old"); err != nil {
			current, statErr := os.Lstat(original)
			if statErr != nil || !os.SameFile(before, current) {
				t.Fatalf("系统拒绝替换后原对象必须仍受保护: %v", statErr)
			}
			if err := checkSettingsLock(writer.lock); err != nil {
				t.Fatalf("被阻止的替换不应破坏原锁: %v", err)
			}
			continue
		}
		result, err := writer.Save(context.Background(), NewDocument(), "")
		if err == nil || result.Saved == nil || *result.Saved {
			t.Fatalf("锁对象换身后不得继续写: %+v %v", result, err)
		}
		return
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("检查身份不能创建配置: %v", err)
	}
}

type windowsCancelAfterLock struct {
	context.Context
	calls int
}

func (ctx *windowsCancelAfterLock) Err() error {
	ctx.calls++
	if ctx.calls >= 2 {
		return context.Canceled
	}
	return nil
}

func TestWindowsSettingsCanceledAndFailedAcquireReleaseHandlesAndPreserveBytes(t *testing.T) {
	store, root := windowsLockFixture(t)
	path := filepath.Join(root, "settings.json")
	original := []byte("existing bytes")
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	target, _, err := settingsPath(path)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := acquireSettingsLock(&windowsCancelAfterLock{Context: context.Background()}, store.environment, target)
	if lock != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("取得锁后取消必须关闭并失败: %v", err)
	}
	if err := os.Rename(store.environment.ConfigDir, store.environment.ConfigDir+"-released"); err != nil {
		t.Fatalf("取消后短期目录和锁文件句柄必须全部释放: %v", err)
	}
	if err := os.Rename(store.environment.ConfigDir+"-released", store.environment.ConfigDir); err != nil {
		t.Fatal(err)
	}
	writer, err := store.OpenWriter(context.Background(), path)
	if err != nil {
		t.Fatalf("取消后不得残留锁: %v", err)
	}
	defer writer.Close()
	_, conflict := store.OpenWriter(context.Background(), path)
	windowsExpectCode(t, conflict, model.CodeInvalidRequest)
	data, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(data, original) {
		t.Fatalf("锁失败不能改变配置内容: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	err = lockWindowsSettingsFile(writer.lock)
	var problem *model.Error
	if err == nil || errors.As(err, &problem) && problem.Code == model.CodeInvalidRequest {
		t.Fatalf("坏句柄不得冒充锁冲突: %v", err)
	}
}

func TestWindowsSettingsRejectsNonPrivateACLAndHardLinks(t *testing.T) {
	store, root := windowsLockFixture(t)
	sid, err := windowsSettingsSID()
	if err != nil {
		t.Fatal(err)
	}
	// 只为临时fixture新建开放DACL，不修改真实用户目录或已有对象权限。
	attributes, release, err := windowsSettingsSecurity("O:" + sid + "D:P(A;OICI;FA;;;WD)")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err := windowsCreateDirectoryWithSecurity(store.environment.ConfigDir, attributes); err != nil {
		t.Fatal(err)
	}
	if writer, err := store.OpenWriter(context.Background(), filepath.Join(root, "settings.json")); err == nil {
		writer.Close()
		t.Fatal("所有者正确但Everyone可写的目录必须被拒绝")
	} else {
		windowsExpectCode(t, err, model.CodePermissionDenied)
	}
	if err := os.Rename(store.environment.ConfigDir, store.environment.ConfigDir+"-rejected"); err != nil {
		t.Fatalf("ACL拒绝后短期目录句柄必须全部释放: %v", err)
	}
	path := filepath.Join(root, "original.json")
	if err := os.WriteFile(path, []byte("bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "hard.json")
	if err := os.Link(path, alias); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadManagedFile(alias); err == nil {
		t.Fatal("真实句柄的NumberOfLinks必须拒绝硬链接")
	}
}

func TestWindowsSettingsDirectoryPinsBlockRenameAndDeleteUntilReleased(t *testing.T) {
	store, _ := windowsLockFixture(t)
	sid, err := windowsSettingsSID()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(store.environment.ConfigDir, "DisplayKVM", "settings-locks", strings.Repeat("0", 64)+".lock")
	directories, err := pinWindowsSettingsDirectories(path, sid, true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if directories != nil {
			if err := closeWindowsSettingsDirectories(directories); err != nil {
				t.Error(err)
			}
		}
	}()
	for _, file := range directories[len(directories)-3:] {
		if err := os.Rename(file.Name(), file.Name()+"-moved"); err == nil {
			t.Fatal("持有真实数据访问且不共享删除的目录句柄时，rename必须被阻止")
		}
	}
	lockDirectory := filepath.Dir(path)
	// 此时锁目录为空，删除失败必须来自句柄保护，而非目录非空。
	if err := os.Remove(lockDirectory); err == nil {
		t.Fatal("校验期的空锁目录不得被删除")
	}
	if err := closeWindowsSettingsDirectories(directories); err != nil {
		t.Fatal(err)
	}
	directories = nil
	if err := os.Remove(lockDirectory); err != nil {
		t.Fatalf("校验期结束后目录pin必须释放: %v", err)
	}
}

func TestWindowsSettingsLockExcludesAnotherProcess(t *testing.T) {
	store, root := windowsLockFixture(t)
	path := filepath.Join(root, "settings.json")
	writer, err := store.OpenWriter(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(executable, "-test.run=^TestWindowsSettingsLockProcessHelper$", "--", store.environment.ConfigDir, path)
	command.Env = append(os.Environ(), "DISPLAYKVM_SETTINGS_LOCK_HELPER=1")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("跨进程互斥失败: %v\n%s", err, output)
	}
}

func TestWindowsSettingsLockProcessHelper(t *testing.T) {
	if os.Getenv("DISPLAYKVM_SETTINGS_LOCK_HELPER") != "1" {
		return
	}
	arguments := os.Args
	if len(arguments) < 3 || !strings.HasSuffix(arguments[len(arguments)-1], "settings.json") {
		t.Fatal("辅助进程参数缺失")
	}
	store, err := NewStore(Options{Environment: func() (Environment, error) {
		return Environment{ConfigDir: arguments[len(arguments)-2]}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	writer, err := store.OpenWriter(context.Background(), arguments[len(arguments)-1])
	if writer != nil {
		writer.Close()
		t.Fatal("另一进程不得取得已持有的路径锁")
	}
	windowsExpectCode(t, err, model.CodeInvalidRequest)
}

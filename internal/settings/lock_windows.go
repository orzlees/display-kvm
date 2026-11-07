//go:build windows

package settings

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	"github.com/orzlee/displayKvm/internal/model"
)

const (
	windowsSettingsReadControl = 0x00020000
	windowsSettingsAttributes  = 0x00000080
	windowsSettingsList        = 0x00000001
	windowsSettingsReparse     = 0x00000400
	windowsSettingsLockError   = syscall.Errno(33) // ERROR_LOCK_VIOLATION
)

// 这里只缓存系统API入口，不保存写者、文件锁或目录句柄；跨进程互斥由内核承担。
var windowsSettingsAPI struct {
	once  sync.Once
	procs map[string]*syscall.LazyProc
	err   error
}

func windowsSettingsProc(name string) (*syscall.LazyProc, error) {
	windowsSettingsAPI.once.Do(func() {
		// kernel32属于Windows KnownDLL，且已由Go运行时载入；仅用它取得系统目录。
		// 后续DLL全部按该绝对目录加载，不能搜索工作目录或依赖可修改的环境变量。
		bootstrap := syscall.NewLazyDLL("kernel32.dll").NewProc("GetSystemDirectoryW")
		if err := bootstrap.Find(); err != nil {
			windowsSettingsAPI.err = err
			return
		}
		buffer := make([]uint16, 32768)
		n, _, callErr := bootstrap.Call(uintptr(unsafe.Pointer(&buffer[0])), uintptr(len(buffer)))
		if n == 0 || n >= uintptr(len(buffer)) {
			windowsSettingsAPI.err = windowsSettingsFailure("GetSystemDirectoryW", callErr)
			return
		}
		directory := syscall.UTF16ToString(buffer[:n])
		windowsSettingsAPI.procs = make(map[string]*syscall.LazyProc)
		for library, names := range map[string][]string{
			"kernel32.dll": {"LockFileEx", "GetFinalPathNameByHandleW", "GetFileInformationByHandleEx", "GetLongPathNameW", "LCMapStringEx", "GetDriveTypeW"},
			"advapi32.dll": {"GetSecurityInfo", "ConvertStringSecurityDescriptorToSecurityDescriptorW", "GetAce", "IsValidSid"},
		} {
			dll := syscall.NewLazyDLL(filepath.Join(directory, library))
			for _, entry := range names {
				proc := dll.NewProc(entry)
				if err := proc.Find(); err != nil {
					windowsSettingsAPI.err = fmt.Errorf("载入Windows配置锁API %s: %w", entry, err)
					return
				}
				windowsSettingsAPI.procs[entry] = proc
			}
		}
	})
	if windowsSettingsAPI.err != nil {
		return nil, windowsSettingsAPI.err
	}
	proc, ok := windowsSettingsAPI.procs[name]
	if !ok {
		return nil, fmt.Errorf("未知Windows配置锁API: %s", name)
	}
	return proc, nil
}

func windowsSettingsFailure(stage string, err error) error {
	if err == nil || errors.Is(err, syscall.Errno(0)) {
		err = syscall.EINVAL
	}
	return fmt.Errorf("Windows配置锁 %s: %w", stage, err)
}

// args可携带Go对象地址；与syscall.LazyProc.Call一样保留指针逃逸语义，避免
// 查询API或栈增长期间地址失效。该包装仅适用于返回BOOL的同步Windows API。
//
//go:uintptrescapes
func windowsSettingsBool(name string, args ...uintptr) error {
	proc, err := windowsSettingsProc(name)
	if err != nil {
		return err
	}
	value, _, callErr := proc.Call(args...)
	if value == 0 {
		return windowsSettingsFailure(name, callErr)
	}
	return nil
}

func windowsSettingsSID() (sid string, result error) {
	token, err := syscall.OpenCurrentProcessToken()
	if err != nil {
		return "", windowsSettingsFailure("OpenCurrentProcessToken", err)
	}
	defer func() { result = errors.Join(result, token.Close()) }()
	user, err := token.GetTokenUser()
	if err != nil {
		return "", windowsSettingsFailure("GetTokenUser", err)
	}
	return user.User.Sid.String()
}

// Windows的FileInfo不含硬链接数；这里只做元数据预检，不能用Unix UID或权限位
// 冒充安全保证。openManaged在真实句柄上核对NumberOfLinks后才允许读取内容。
func singleLink(info os.FileInfo) bool {
	attributes, ok := info.Sys().(*syscall.Win32FileAttributeData)
	return ok && info.Mode().IsRegular() && attributes.FileAttributes&windowsSettingsReparse == 0
}

func windowsSettingsHandleInfo(file *os.File, directory bool) (syscall.ByHandleFileInformation, error) {
	var information syscall.ByHandleFileInformation
	info, err := file.Stat()
	if err != nil {
		return information, err
	}
	handle := syscall.Handle(file.Fd())
	if err := syscall.GetFileInformationByHandle(handle, &information); err != nil {
		return information, windowsSettingsFailure("GetFileInformationByHandle", err)
	}
	typeOfFile, err := syscall.GetFileType(handle)
	if err != nil {
		return information, windowsSettingsFailure("GetFileType", err)
	}
	isDirectory := information.FileAttributes&syscall.FILE_ATTRIBUTE_DIRECTORY != 0
	if typeOfFile != syscall.FILE_TYPE_DISK || information.FileAttributes&(windowsSettingsReparse|0x40) != 0 || isDirectory != directory || !directory && (!info.Mode().IsRegular() || information.NumberOfLinks != 1) {
		return information, model.NewError(model.CodePermissionDenied, "配置锁路径必须是磁盘上的非重解析目录或单链接普通文件", nil)
	}
	return information, nil
}

func openManaged(path string) (*os.File, error) {
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	// 不跟随末级reparse；读取期间不共享写入/删除，防止内容和链接身份被替换。
	handle, err := syscall.CreateFile(name, syscall.GENERIC_READ, syscall.FILE_SHARE_READ, nil, syscall.OPEN_EXISTING, syscall.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, windowsSettingsFailure("CreateFileW(读取配置)", err)
	}
	file := os.NewFile(uintptr(handle), path)
	if _, err := windowsSettingsHandleInfo(file, false); err != nil {
		return nil, errors.Join(err, file.Close())
	}
	return file, nil
}

func windowsSettingsSecurity(sddl string) (*syscall.SecurityAttributes, func() error, error) {
	text, err := syscall.UTF16PtrFromString(sddl)
	if err != nil {
		return nil, nil, err
	}
	var descriptor uintptr
	if err := windowsSettingsBool("ConvertStringSecurityDescriptorToSecurityDescriptorW", uintptr(unsafe.Pointer(text)), 1, uintptr(unsafe.Pointer(&descriptor)), 0); err != nil {
		return nil, nil, err
	}
	attributes := &syscall.SecurityAttributes{SecurityDescriptor: descriptor}
	attributes.Length = uint32(unsafe.Sizeof(*attributes))
	return attributes, func() error {
		_, err := syscall.LocalFree(syscall.Handle(descriptor))
		return err
	}, nil
}

func windowsCreateDirectoryWithSecurity(path string, attributes *syscall.SecurityAttributes) error {
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	return syscall.CreateDirectory(name, attributes)
}

func createWindowsSettingsDirectory(path, sid string) (result error) {
	// 新对象从创建时即使用受保护DACL，仅当前SID可访问；不先创建再修权限，
	// 也不修改已有目录ACL。Environment.UID在Windows没有可用于授权的Unix语义。
	attributes, release, err := windowsSettingsSecurity("O:" + sid + "D:P(A;OICI;FA;;;" + sid + ")")
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, release()) }()
	err = windowsCreateDirectoryWithSecurity(path, attributes)
	if errors.Is(err, syscall.ERROR_ALREADY_EXISTS) {
		return nil
	}
	return err
}

func openWindowsSettingsDirectory(path string) (*os.File, error) {
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	// FILE_LIST_DIRECTORY是真实数据访问位。仅READ_CONTROL/READ_ATTRIBUTES的
	// metadata句柄不参与完整的删除共享检查，不能靠省略SHARE_DELETE固定目录。
	handle, err := syscall.CreateFile(name, windowsSettingsReadControl|windowsSettingsAttributes|windowsSettingsList, syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE, nil, syscall.OPEN_EXISTING, syscall.FILE_FLAG_BACKUP_SEMANTICS|syscall.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, windowsSettingsFailure("CreateFileW(固定目录)", err)
	}
	return os.NewFile(uintptr(handle), path), nil
}

func windowsSettingsTrustedSID(sid, current string) bool {
	// 只认可明确的本机管理主体；不因所有者字符串看似系统账号就一律信任。
	return sid == current || sid == "S-1-5-18" || sid == "S-1-5-32-544" || sid == "S-1-5-80-956008885-3418522649-1831038044-1853292631-2271478464"
}

func windowsSettingsOwnerAllowed(owner, current string, private, configDirectory bool) bool {
	// 私有锁对象必须属于当前用户；ConfigDir是可由提升令牌创建的既有基目录，
	// 默认owner可能为SYSTEM或Administrators，接受这两个固定主体仍须完整核验DACL和路径。
	// ConfigDir不复用更高祖先的完整集合，以免连带允许TrustedInstaller或任意令牌默认owner。
	if private {
		return owner == current
	}
	if configDirectory {
		return owner == current || owner == "S-1-5-18" || owner == "S-1-5-32-544"
	}
	return windowsSettingsTrustedSID(owner, current)
}

func checkWindowsSettingsACL(file *os.File, sid string, private, configDirectory bool) (result error) {
	proc, err := windowsSettingsProc("GetSecurityInfo")
	if err != nil {
		return err
	}
	var owner *syscall.SID
	var acl, descriptor unsafe.Pointer
	status, _, _ := proc.Call(file.Fd(), 1, 0x1|0x4, uintptr(unsafe.Pointer(&owner)), 0, uintptr(unsafe.Pointer(&acl)), 0, uintptr(unsafe.Pointer(&descriptor)))
	if status != 0 {
		return windowsSettingsFailure("GetSecurityInfo", syscall.Errno(status))
	}
	defer func() {
		_, err := syscall.LocalFree(syscall.Handle(uintptr(descriptor)))
		result = errors.Join(result, err)
	}()
	if owner == nil || acl == nil || descriptor == nil {
		return model.NewError(model.CodePermissionDenied, "配置锁路径缺少可核验的所有者或DACL", nil)
	}
	if err := windowsSettingsBool("IsValidSid", uintptr(unsafe.Pointer(owner))); err != nil {
		return err
	}
	ownerSID, err := owner.String()
	if err != nil {
		return err
	}
	if !windowsSettingsOwnerAllowed(ownerSID, sid, private, configDirectory) {
		return fmt.Errorf("%w\nowner=%s\ncurrentSID=%s", model.NewError(model.CodePermissionDenied, "配置锁路径所有者不安全", nil), ownerSID, sid)
	}
	// ACL头固定8字节，ACE由系统GetAce返回并受AclSize约束；不接受无法解释的
	// 对象/回调ACE，避免漏掉非普通主体的授权。拒绝ACE只会收紧访问，可保留。
	header := (*struct {
		Revision uint8
		Reserved uint8
		Size     uint16
		Count    uint16
		Padding  uint16
	})(acl)
	if header.Size < 8 || header.Revision != 2 && header.Revision != 4 {
		return model.NewError(model.CodePermissionDenied, "配置锁DACL格式无法安全核验", nil)
	}
	getAce, err := windowsSettingsProc("GetAce")
	if err != nil {
		return err
	}
	for index := uint16(0); index < header.Count; index++ {
		var entry unsafe.Pointer
		ok, _, callErr := getAce.Call(uintptr(acl), uintptr(index), uintptr(unsafe.Pointer(&entry)))
		if ok == 0 {
			return windowsSettingsFailure("GetAce", callErr)
		}
		start, limit := uintptr(acl)+8, uintptr(acl)+uintptr(header.Size)
		if entry == nil || uintptr(entry) < start || uintptr(entry)+8 > limit {
			return model.NewError(model.CodePermissionDenied, "配置锁ACE越界", nil)
		}
		ace := (*struct {
			Type  uint8
			Flags uint8
			Size  uint16
			Mask  uint32
		})(entry)
		if ace.Size < 8 || uintptr(entry)+uintptr(ace.Size) > limit {
			return model.NewError(model.CodePermissionDenied, "配置锁ACE长度无效", nil)
		}
		if ace.Flags&0x08 != 0 || ace.Type == 1 { // INHERIT_ONLY_ACE / ACCESS_DENIED_ACE
			continue
		}
		if ace.Type != 0 || ace.Size < 16 { // ACCESS_ALLOWED_ACE，SID至少8字节
			// 此处仅已核验ACE头边界；未知授权类型不能按普通ACE布局读取主体SID。
			return fmt.Errorf("%w\nACE type=%d flags=0x%02x mask=0x%08x", model.NewError(model.CodePermissionDenied, "配置锁DACL包含不支持的授权类型", nil), ace.Type, ace.Flags, ace.Mask)
		}
		principal := (*syscall.SID)(unsafe.Add(entry, 8))
		sidHeader := (*[8]byte)(unsafe.Pointer(principal))
		if sidHeader[0] != 1 || sidHeader[1] > 15 || 8+4*int(sidHeader[1]) > int(ace.Size)-8 {
			return model.NewError(model.CodePermissionDenied, "配置锁ACE的SID边界无效", nil)
		}
		if err := windowsSettingsBool("IsValidSid", uintptr(unsafe.Pointer(principal))); err != nil {
			return err
		}
		if principal.Len() > int(ace.Size)-8 {
			return model.NewError(model.CodePermissionDenied, "配置锁ACE的SID长度无效", nil)
		}
		principalSID, err := principal.String()
		if err != nil {
			return err
		}
		if principalSID == sid || !private && windowsSettingsTrustedSID(principalSID, sid) {
			continue
		}
		// 展开generic写/全权及所有可改写当前对象、ACL、所有者或删除子项的权限。
		// 祖先单独FILE_ADD_SUBDIRECTORY(0x4)不允许替换已有子项或修改reparse，
		// 因而可保留；新选中的子目录仍须逐个核验所有者和ACL，不能信任继承。
		const unsafeAncestor = uint32(0x10000000 | 0x40000000 | 0x000d0000 | 0x2 | 0x10 | 0x40 | 0x100)
		if private && ace.Mask != 0 || !private && ace.Mask&unsafeAncestor != 0 {
			return fmt.Errorf("%w\nACE type=%d flags=0x%02x mask=0x%08x\nprincipalSID=%s", model.NewError(model.CodePermissionDenied, "配置锁路径允许非可信主体访问或改写", nil), ace.Type, ace.Flags, ace.Mask, principalSID)
		}
	}
	runtime.KeepAlive(file)
	return nil
}

func closeWindowsSettingsDirectories(files []*os.File) error {
	var result error
	for index := len(files) - 1; index >= 0; index-- {
		result = errors.Join(result, files[index].Close())
	}
	return result
}

func pinWindowsSettingsDirectories(path, sid string, create bool) (files []*os.File, result error) {
	lockDirectory := filepath.Dir(path)
	if !strings.EqualFold(filepath.Base(lockDirectory), "settings-locks") || !strings.EqualFold(filepath.Base(filepath.Dir(lockDirectory)), "DisplayKVM") {
		return nil, model.NewError(model.CodePermissionDenied, "内部配置锁不在受保护目录中", nil)
	}
	if err := checkWindowsSettingsLocalPath(lockDirectory); err != nil {
		return nil, err
	}
	var directories []string
	for directory := lockDirectory; ; directory = filepath.Dir(directory) {
		directories = append(directories, directory)
		if filepath.Dir(directory) == directory {
			break
		}
	}
	defer func() {
		if result != nil {
			result = errors.Join(result, closeWindowsSettingsDirectories(files))
			files = nil
		}
	}()
	// 从卷根到锁目录依次持有不共享删除的句柄，防止校验后再打开下一级时换身。
	// 这些句柄只覆盖当前校验过程；Writer生命周期由同一锁文件句柄和每次复核保护。
	for index := len(directories) - 1; index >= 0; index-- {
		directory := directories[index]
		file, err := openWindowsSettingsDirectory(directory)
		if create && errors.Is(err, os.ErrNotExist) {
			if err := createWindowsSettingsDirectory(directory, sid); err != nil {
				return files, model.NewError(model.CodePermissionDenied, "创建私有配置锁目录失败", err)
			}
			file, err = openWindowsSettingsDirectory(directory)
		}
		if err != nil {
			return files, err
		}
		files = append(files, file)
		if _, err := windowsSettingsHandleInfo(file, true); err != nil {
			return files, err
		}
		if err := checkWindowsSettingsACL(file, sid, index < 2, index == 2); err != nil {
			// 仅在拒绝时追加目录和模式，区分ConfigDir与更高祖先；层级从settings-locks=0向上递增。
			// 按实际分层模式记录检查条件，错误链保留原分类；原因在前、字段分行，便于截断采集诊断。
			return files, fmt.Errorf("%w\n目录层级=%d private=%t currentOwner=%t configDirectory=%t\n目录=%q", err, index, index < 2, index < 2, index == 2, directory)
		}
		opened, err := file.Stat()
		if err != nil {
			return files, err
		}
		current, err := os.Lstat(directory)
		if err != nil || !current.IsDir() || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(opened, current) {
			return files, model.NewError(model.CodePermissionDenied, "配置锁目录路径身份不一致", err)
		}
	}
	return files, nil
}

func checkWindowsSettingsLocalPath(path string) error {
	volume := filepath.VolumeName(path)
	if !filepath.IsAbs(path) || len(volume) != 2 || volume[1] != ':' || strings.HasPrefix(path, `\\`) {
		return model.NewError(model.CodeUnsupportedOperation, "配置写锁仅支持本地盘绝对路径，不支持UNC或设备路径", nil)
	}
	root, err := syscall.UTF16PtrFromString(volume + `\`)
	if err != nil {
		return err
	}
	proc, err := windowsSettingsProc("GetDriveTypeW")
	if err != nil {
		return err
	}
	typeOfDrive, _, _ := proc.Call(uintptr(unsafe.Pointer(root)))
	if typeOfDrive != 3 { // DRIVE_FIXED；排除映射网络盘及不可核验的卷。
		return model.NewError(model.CodeUnsupportedOperation, "配置写锁要求可核验ACL的本地固定磁盘", nil)
	}
	return nil
}

func windowsSettingsFold(path string) (string, error) {
	proc, err := windowsSettingsProc("LCMapStringEx")
	if err != nil {
		return "", err
	}
	source, err := syscall.UTF16FromString(path)
	if err != nil {
		return "", err
	}
	locale := []uint16{0} // LOCALE_NAME_INVARIANT，不随登录用户语言变动。
	buffer := make([]uint16, 32768)
	n, _, callErr := proc.Call(uintptr(unsafe.Pointer(&locale[0])), 0x200, uintptr(unsafe.Pointer(&source[0])), uintptr(len(source)-1), uintptr(unsafe.Pointer(&buffer[0])), uintptr(len(buffer)), 0, 0, 0)
	if n == 0 || n > uintptr(len(buffer)) {
		return "", windowsSettingsFailure("LCMapStringEx", callErr)
	}
	return syscall.UTF16ToString(buffer[:n]), nil
}

func windowsSettingsTargetKey(target string) (key string, result error) {
	if err := checkWindowsSettingsLocalPath(target); err != nil {
		return "", err
	}
	base := filepath.Base(target)
	if base == "" || strings.TrimRight(base, " .") != base || strings.ContainsAny(base, "<>:\"/\\|?*~") || strings.IndexFunc(base, func(r rune) bool { return r < 32 }) >= 0 {
		return "", model.NewError(model.CodeUnsupportedOperation, "配置锁不支持设备名、短文件名或带尾随点空格的路径", nil)
	}
	stem := strings.ToUpper(strings.SplitN(base, ".", 2)[0])
	if stem == "CON" || stem == "PRN" || stem == "AUX" || stem == "NUL" || len(stem) == 4 && (strings.HasPrefix(stem, "COM") || strings.HasPrefix(stem, "LPT")) && stem[3] >= '1' && stem[3] <= '9' {
		return "", model.NewError(model.CodeUnsupportedOperation, "配置锁不支持Windows设备名", nil)
	}
	parent, err := openWindowsSettingsDirectory(filepath.Dir(target))
	if err != nil {
		return "", err
	}
	defer func() { result = errors.Join(result, parent.Close()) }()
	if _, err := windowsSettingsHandleInfo(parent, true); err != nil {
		return "", err
	}
	proc, err := windowsSettingsProc("GetFinalPathNameByHandleW")
	if err != nil {
		return "", err
	}
	buffer := make([]uint16, 32768)
	// VOLUME_NAME_GUID固定卷名，避免同一卷的盘符别名生成不同的路径锁。
	n, _, callErr := proc.Call(parent.Fd(), uintptr(unsafe.Pointer(&buffer[0])), uintptr(len(buffer)), 1)
	if n == 0 || n >= uintptr(len(buffer)) {
		return "", windowsSettingsFailure("GetFinalPathNameByHandleW", callErr)
	}
	canonicalParent := syscall.UTF16ToString(buffer[:n])
	if !strings.HasPrefix(canonicalParent, `\\?\Volume{`) || !strings.Contains(canonicalParent, `}\`) {
		return "", model.NewError(model.CodeUnsupportedOperation, "无法取得配置父目录的本地规范路径", nil)
	}
	var directories []string
	for directory := canonicalParent; ; directory = filepath.Dir(directory) {
		directories = append(directories, directory)
		if filepath.Dir(directory) == directory {
			break
		}
	}
	var pinned []*os.File
	defer func() { result = errors.Join(result, closeWindowsSettingsDirectories(pinned)) }()
	// 大小写规则逐目录生效：敏感祖先s可以同时含A、a两个不敏感子目录。
	// 仅检查直接父目录会将这两个独立目标的完整路径折叠成同一锁键。必须沿
	// 实际参与GUID锁键的目标目录链从卷根逐级固定并核验，不能用锁目录链代替。
	for index := len(directories) - 1; index >= 0; index-- {
		directory := directories[index]
		file, err := openWindowsSettingsDirectory(directory)
		if err != nil {
			return "", err
		}
		pinned = append(pinned, file)
		if _, err := windowsSettingsHandleInfo(file, true); err != nil {
			return "", err
		}
		opened, err := file.Stat()
		if err != nil {
			return "", err
		}
		current, err := os.Lstat(directory)
		if err != nil || !current.IsDir() || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(opened, current) {
			return "", model.NewError(model.CodePermissionDenied, "配置目标祖先目录路径身份不一致", err)
		}
		var caseFlags uint32
		// FileCaseSensitiveInfo=23，结构为四字节ULONG Flags。查询不支持或失败
		// 时保留阶段及系统码并拒绝，不能把未知目录规则假定为大小写不敏感。
		if err := windowsSettingsBool("GetFileInformationByHandleEx", file.Fd(), 23, uintptr(unsafe.Pointer(&caseFlags)), unsafe.Sizeof(caseFlags)); err != nil {
			return "", model.NewError(model.CodeUnsupportedOperation, fmt.Sprintf("无法核验配置目标祖先目录%s的Windows大小写规则", directory), err)
		}
		if caseFlags&1 != 0 {
			return "", model.NewError(model.CodeUnsupportedOperation, fmt.Sprintf("配置写锁不支持启用大小写敏感的目标祖先目录: %s", directory), nil)
		}
	}
	openedParent, err := parent.Stat()
	if err != nil {
		return "", err
	}
	pinnedParent, err := pinned[len(pinned)-1].Stat()
	if err != nil || !os.SameFile(openedParent, pinnedParent) {
		return "", model.NewError(model.CodePermissionDenied, "配置目标的规范父目录身份不一致", err)
	}
	canonical := filepath.Join(canonicalParent, base)
	name, err := syscall.UTF16PtrFromString(target)
	if err != nil {
		return "", err
	}
	proc, err = windowsSettingsProc("GetLongPathNameW")
	if err != nil {
		return "", err
	}
	n, _, callErr = proc.Call(uintptr(unsafe.Pointer(name)), uintptr(unsafe.Pointer(&buffer[0])), uintptr(len(buffer)))
	if n == 0 {
		if !errors.Is(callErr, syscall.ERROR_FILE_NOT_FOUND) {
			return "", windowsSettingsFailure("GetLongPathNameW", callErr)
		}
	} else {
		if n >= uintptr(len(buffer)) {
			return "", model.NewError(model.CodeUnsupportedOperation, "配置规范路径过长", nil)
		}
		longBase := filepath.Base(syscall.UTF16ToString(buffer[:n]))
		foldedBase, err := windowsSettingsFold(base)
		if err != nil {
			return "", err
		}
		foldedLong, err := windowsSettingsFold(longBase)
		if err != nil {
			return "", err
		}
		if foldedBase != foldedLong {
			return "", model.NewError(model.CodeUnsupportedOperation, "配置锁不支持8.3短文件名别名", nil)
		}
	}
	// 锁键必须在配置尚不存在及原子Rename换身后保持一致。使用系统规范父路径和
	// invariant大小写映射，拒绝特殊别名；绝不能用目标fileID作为生命周期锁键，
	// 否则保存替换目标后第二个Writer将锁住新身份，与旧Writer并发写入。
	return windowsSettingsFold(canonical)
}

func lockWindowsSettingsFile(file *os.File) error {
	var overlapped syscall.Overlapped // offset=0，锁定一字节；文件可仍为空。
	err := windowsSettingsBool("LockFileEx", file.Fd(), 0x2|0x1, 0, 1, 0, uintptr(unsafe.Pointer(&overlapped)))
	if errors.Is(err, windowsSettingsLockError) {
		return model.NewError(model.CodeInvalidRequest, "配置正被另一写者占用，不能保存", err)
	}
	if err != nil {
		return model.NewError(model.CodeIO, "取得Windows配置文件锁失败", err)
	}
	return nil
}

func acquireSettingsLock(ctx context.Context, env Environment, target string) (file *os.File, result error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	sid, err := windowsSettingsSID()
	if err != nil {
		return nil, err
	}
	key, err := windowsSettingsTargetKey(target)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256([]byte(key))
	path := filepath.Join(env.ConfigDir, "DisplayKVM", "settings-locks", hex.EncodeToString(digest[:])+".lock")
	directories, err := pinWindowsSettingsDirectories(path, sid, true)
	if err != nil {
		return nil, err
	}
	defer func() {
		result = errors.Join(result, closeWindowsSettingsDirectories(directories))
		if result != nil && file != nil {
			result = errors.Join(result, file.Close())
			file = nil
		}
	}()
	attributes, release, err := windowsSettingsSecurity("O:" + sid + "D:P(A;;FA;;;" + sid + ")")
	if err != nil {
		return nil, err
	}
	defer func() { result = errors.Join(result, release()) }()
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	// 不共享删除让持有的锁文件不能换身；共享读写使第二个句柄确实走到LockFileEx，
	// 而不是把一般共享错误误报为锁冲突。既有文件只核验，不修复其ACL或内容。
	handle, err := syscall.CreateFile(name, syscall.GENERIC_READ|syscall.GENERIC_WRITE|windowsSettingsReadControl, syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE, attributes, syscall.OPEN_ALWAYS, syscall.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, model.NewError(model.CodePermissionDenied, "无法安全打开Windows内部配置锁", windowsSettingsFailure("CreateFileW(配置锁)", err))
	}
	opened := os.NewFile(uintptr(handle), path)
	if err := checkSettingsLock(opened); err != nil {
		return nil, errors.Join(err, opened.Close())
	}
	if err := lockWindowsSettingsFile(opened); err != nil {
		return nil, errors.Join(err, opened.Close())
	}
	if err := ctx.Err(); err != nil {
		return nil, errors.Join(err, opened.Close())
	}
	if err := checkSettingsLock(opened); err != nil {
		return nil, errors.Join(err, opened.Close())
	}
	return opened, nil
}

func checkSettingsLock(file *os.File) (result error) {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return model.NewError(model.CodePermissionDenied, "内部配置锁必须是普通文件", nil)
	}
	if _, err := windowsSettingsHandleInfo(file, false); err != nil {
		return err
	}
	sid, err := windowsSettingsSID()
	if err != nil {
		return err
	}
	directories, err := pinWindowsSettingsDirectories(file.Name(), sid, false)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, closeWindowsSettingsDirectories(directories)) }()
	if err := checkWindowsSettingsACL(file, sid, true, false); err != nil {
		return err
	}
	current, err := os.Lstat(file.Name())
	if err != nil || !singleLink(current) || !os.SameFile(info, current) {
		return model.NewError(model.CodePermissionDenied, "内部配置锁路径在持有期间已被替换", err)
	}
	return nil
}

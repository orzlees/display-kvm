//go:build windows

package ipc

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
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
	windowsReadControl    = uint32(0x00020000)
	windowsReadAttributes = uint32(0x00000080)
	windowsDelete         = uint32(0x00010000)
	windowsFileAllAccess  = uint32(0x001f01ff)
	windowsInheritOnly    = byte(0x08)
	// TrustedInstaller的服务SID固定，不从目录当前owner或环境变量推导信任主体。
	windowsTrustedInstaller = "S-1-5-80-956008885-3418522649-1831038044-1853292631-2271478464"
)

type windowsIPCAPI struct {
	kernel, advapi    *syscall.DLL
	convertDescriptor *syscall.Proc
	getSecurityInfo   *syscall.Proc
	getOwner          *syscall.Proc
	getDACL           *syscall.Proc
	validDescriptor   *syscall.Proc
	validACL          *syscall.Proc
	validSID          *syscall.Proc
	getDriveType      *syscall.Proc
	getFinalPath      *syscall.Proc
	lockFile          *syscall.Proc
	setFileInfo       *syscall.Proc
}

var windowsIPCProcedures struct {
	once sync.Once
	api  *windowsIPCAPI
	err  error
}

func windowsAPI() (*windowsIPCAPI, error) {
	windowsIPCProcedures.once.Do(func() {
		// kernel32是Windows启动时已经装载的KnownDLL；仅用它取得系统目录，
		// 随后的DLL均用系统目录的绝对路径装载，不搜索应用目录或PATH。
		bootstrap := syscall.NewLazyDLL("kernel32.dll").NewProc("GetSystemDirectoryW")
		if err := bootstrap.Find(); err != nil {
			windowsIPCProcedures.err = err
			return
		}
		buffer := make([]uint16, 32768)
		length, _, callErr := bootstrap.Call(uintptr(unsafe.Pointer(&buffer[0])), uintptr(len(buffer)))
		if length == 0 || length >= uintptr(len(buffer)) {
			windowsIPCProcedures.err = windowsError("取得可信系统目录失败", callErr)
			return
		}
		directory := syscall.UTF16ToString(buffer[:length])
		kernel, err := syscall.LoadDLL(filepath.Join(directory, "kernel32.dll"))
		if err != nil {
			windowsIPCProcedures.err = err
			return
		}
		advapi, err := syscall.LoadDLL(filepath.Join(directory, "advapi32.dll"))
		if err != nil {
			windowsIPCProcedures.err = errors.Join(err, kernel.Release())
			return
		}
		api := &windowsIPCAPI{kernel: kernel, advapi: advapi}
		for _, item := range []struct {
			dll    *syscall.DLL
			name   string
			target **syscall.Proc
		}{
			{advapi, "ConvertStringSecurityDescriptorToSecurityDescriptorW", &api.convertDescriptor},
			{advapi, "GetSecurityInfo", &api.getSecurityInfo},
			{advapi, "GetSecurityDescriptorOwner", &api.getOwner},
			{advapi, "GetSecurityDescriptorDacl", &api.getDACL},
			{advapi, "IsValidSecurityDescriptor", &api.validDescriptor},
			{advapi, "IsValidAcl", &api.validACL},
			{advapi, "IsValidSid", &api.validSID},
			{kernel, "GetDriveTypeW", &api.getDriveType},
			{kernel, "GetFinalPathNameByHandleW", &api.getFinalPath},
			{kernel, "LockFileEx", &api.lockFile},
			{kernel, "SetFileInformationByHandle", &api.setFileInfo},
		} {
			proc, err := item.dll.FindProc(item.name)
			if err != nil {
				windowsIPCProcedures.err = errors.Join(err, advapi.Release(), kernel.Release())
				return
			}
			*item.target = proc
		}
		// 这里只缓存进程生命周期内的系统API地址，不缓存任何路径校验结果。
		windowsIPCProcedures.api = api
	})
	return windowsIPCProcedures.api, windowsIPCProcedures.err
}

func windowsError(stage string, err error) error {
	if err == nil || errors.Is(err, syscall.Errno(0)) {
		err = syscall.EINVAL
	}
	code := model.CodeIO
	if errors.Is(err, syscall.ERROR_ACCESS_DENIED) || errors.Is(err, syscall.Errno(1314)) || errors.Is(err, syscall.Errno(1307)) || errors.Is(err, syscall.Errno(1336)) || errors.Is(err, syscall.Errno(1338)) {
		code = model.CodePermissionDenied
	}
	return model.NewError(code, stage, err)
}

func windowsCurrentSID(api *windowsIPCAPI) (sid string, result error) {
	token, err := syscall.OpenCurrentProcessToken()
	if err != nil {
		return "", windowsError("读取当前进程访问令牌失败", err)
	}
	defer func() { result = errors.Join(result, token.Close()) }()
	user, err := token.GetTokenUser()
	if err != nil {
		return "", windowsError("读取当前进程用户SID失败", err)
	}
	valid, _, _ := api.validSID.Call(uintptr(unsafe.Pointer(user.User.Sid)))
	if valid == 0 {
		return "", model.NewError(model.CodePermissionDenied, "当前进程用户SID无效", nil)
	}
	// TokenUser缓冲区及令牌都有独立生命周期，先复制SID再关闭令牌，
	// 不依赖username、环境变量或Windows无意义的Unix uid结果。
	copySID, err := user.User.Sid.Copy()
	runtime.KeepAlive(user)
	if err != nil {
		return "", windowsError("复制当前用户SID失败", err)
	}
	sid, err = copySID.String()
	if err != nil {
		return "", windowsError("编码当前用户SID失败", err)
	}
	return sid, nil
}

func windowsDescriptor(api *windowsIPCAPI, sid string, directory bool) (uintptr, error) {
	flags := ""
	if directory {
		flags = "OICI"
	}
	// 显式指定owner及受保护DACL，创建之初仅本SID拥有完全控制。
	// 管理员提升令牌的默认owner可能是Administrators，不能依赖默认安全描述符。
	text, err := syscall.UTF16PtrFromString("O:" + sid + "D:P(A;" + flags + ";FA;;;" + sid + ")")
	if err != nil {
		return 0, windowsError("编码私有安全描述符失败", err)
	}
	var descriptor uintptr
	ok, _, callErr := api.convertDescriptor.Call(uintptr(unsafe.Pointer(text)), 1, uintptr(unsafe.Pointer(&descriptor)), 0)
	if ok == 0 {
		var freeErr error
		if descriptor != 0 {
			_, freeErr = syscall.LocalFree(syscall.Handle(descriptor))
		}
		return 0, errors.Join(windowsError("创建私有安全描述符失败", callErr), freeErr)
	}
	if descriptor == 0 {
		return 0, model.NewError(model.CodePermissionDenied, "创建接口未返回私有安全描述符", nil)
	}
	return descriptor, nil
}

func windowsCheckSecurity(api *windowsIPCAPI, handle syscall.Handle, sid string, private bool) (result error) {
	var descriptor uintptr
	// GetSecurityInfo返回DWORD状态而非BOOL，不能用线程的LastError判断成功。
	status, _, _ := api.getSecurityInfo.Call(uintptr(handle), 1, 0x5, 0, 0, 0, 0, uintptr(unsafe.Pointer(&descriptor)))
	if descriptor != 0 {
		defer func() {
			_, err := syscall.LocalFree(syscall.Handle(descriptor))
			result = errors.Join(result, err)
		}()
	}
	if status != 0 {
		return windowsError("查询句柄所有者及DACL失败", syscall.Errno(status))
	}
	return windowsCheckDescriptor(api, descriptor, sid, private)
}

func windowsCheckDescriptor(api *windowsIPCAPI, descriptor uintptr, sid string, private bool) error {
	if descriptor == 0 {
		return model.NewError(model.CodePermissionDenied, "对象安全描述符不可核验", nil)
	}
	valid, _, _ := api.validDescriptor.Call(descriptor)
	if valid == 0 {
		return model.NewError(model.CodePermissionDenied, "对象安全描述符不可核验", nil)
	}
	var owner *syscall.SID
	var defaulted, present uint32
	ok, _, callErr := api.getOwner.Call(descriptor, uintptr(unsafe.Pointer(&owner)), uintptr(unsafe.Pointer(&defaulted)))
	if ok == 0 {
		return windowsError("读取对象所有者失败", callErr)
	}
	if owner == nil {
		return model.NewError(model.CodePermissionDenied, "对象所有者SID无效", nil)
	}
	valid, _, _ = api.validSID.Call(uintptr(unsafe.Pointer(owner)))
	if valid == 0 {
		return model.NewError(model.CodePermissionDenied, "对象所有者SID无效", nil)
	}
	ownerSID, err := owner.String()
	if err != nil {
		return windowsError("读取对象所有者SID失败", err)
	}
	trusted := func(candidate string) bool {
		return candidate == sid || candidate == "S-1-5-18" || candidate == "S-1-5-32-544" || candidate == windowsTrustedInstaller
	}
	if (private && ownerSID != sid) || (!private && !trusted(ownerSID)) {
		return model.NewError(model.CodePermissionDenied, "对象所有者不符合私有目录策略", nil)
	}
	var dacl uintptr
	ok, _, callErr = api.getDACL.Call(descriptor, uintptr(unsafe.Pointer(&present)), uintptr(unsafe.Pointer(&dacl)), uintptr(unsafe.Pointer(&defaulted)))
	if ok == 0 {
		return windowsError("读取对象DACL失败", callErr)
	}
	// 缺失DACL与null DACL都代表不受限制，绝不能当成没有其他授权。
	if present == 0 || dacl == 0 {
		return model.NewError(model.CodePermissionDenied, "对象缺少受限DACL", nil)
	}
	valid, _, _ = api.validACL.Call(dacl)
	if valid == 0 {
		return model.NewError(model.CodePermissionDenied, "对象DACL结构无效", nil)
	}
	// IsValidAcl核验原生分配范围后，再按ACL/ACE长度检查每一步，
	// SID指针仅在描述符释放前使用，不能留下指向LocalFree后内存的引用。
	header := unsafe.Slice((*byte)(unsafe.Pointer(dacl)), 8)
	size := int(binary.LittleEndian.Uint16(header[2:4]))
	count := int(binary.LittleEndian.Uint16(header[4:6]))
	if size < 8 || (private && count != 1) {
		return model.NewError(model.CodePermissionDenied, "私有对象必须仅有当前用户的完整授权", nil)
	}
	acl := unsafe.Slice((*byte)(unsafe.Pointer(dacl)), size)
	offset := 8
	for index := 0; index < count; index++ {
		if offset > len(acl)-4 {
			return model.NewError(model.CodePermissionDenied, "DACL条目越界", nil)
		}
		aceSize := int(binary.LittleEndian.Uint16(acl[offset+2 : offset+4]))
		if aceSize < 4 || aceSize%4 != 0 || aceSize > len(acl)-offset {
			return model.NewError(model.CodePermissionDenied, "DACL条目长度无效", nil)
		}
		ace := acl[offset : offset+aceSize]
		offset += aceSize
		if !private && ace[1]&windowsInheritOnly != 0 {
			// 例如卷根的CREATOR OWNER完全控制只继承给子项，不授予当前目录权限。
			continue
		}
		if len(ace) < 16 || ace[0] > 1 || ace[1]&^byte(0x1f) != 0 || 8+8+4*int(ace[9]) != len(ace) {
			return model.NewError(model.CodePermissionDenied, "DACL包含无法证明安全的条目类型", nil)
		}
		entrySID := (*syscall.SID)(unsafe.Pointer(&ace[8]))
		valid, _, _ := api.validSID.Call(uintptr(unsafe.Pointer(entrySID)))
		if valid == 0 {
			return model.NewError(model.CodePermissionDenied, "DACL条目SID无效", nil)
		}
		entry, err := entrySID.String()
		if err != nil {
			return windowsError("读取DACL条目SID失败", err)
		}
		mask := binary.LittleEndian.Uint32(ace[4:8])
		// 文件与目录使用同一组generic映射；不展开会把GW/GA误当无危险的高位。
		if mask&syscall.GENERIC_ALL != 0 {
			mask |= windowsFileAllAccess
		}
		if mask&syscall.GENERIC_READ != 0 {
			mask |= 0x00120089
		}
		if mask&syscall.GENERIC_WRITE != 0 {
			mask |= 0x00120116
		}
		if mask&syscall.GENERIC_EXECUTE != 0 {
			mask |= 0x001200a0
		}
		mask &^= syscall.GENERIC_ALL | syscall.GENERIC_READ | syscall.GENERIC_WRITE | syscall.GENERIC_EXECUTE
		if mask&^windowsFileAllAccess != 0 {
			return model.NewError(model.CodePermissionDenied, "DACL包含无法解释的文件授权位", nil)
		}
		if private {
			if ace[0] != 0 || ace[1]&windowsInheritOnly != 0 || entry != sid || mask != windowsFileAllAccess {
				return model.NewError(model.CodePermissionDenied, "私有对象DACL必须仅授权当前用户完全控制", nil)
			}
			continue
		}
		// 祖先可由固定可信系统主体管理；与Unix一致，不隔离同SID恶意进程或
		// 可绕过权限的管理员。普通主体的读取/遍历、单纯创建子目录不影响
		// 已固定的选中子项；删除子项、修改DACL/owner及重解析相关写入必须拒绝。
		// 新出现的子目录仍逐句柄核验，不能因祖先允许创建子目录而直接信任它。
		// FILE_ADD_SUBDIRECTORY(0x4)本身不足以设置或删除当前目录重解析点；
		// MS-FSA要求该操作至少持有FILE_WRITE_DATA(0x2)或FILE_WRITE_ATTRIBUTES(0x100)。
		const dangerous = windowsDelete | 0x00040000 | 0x00080000 | 0x40 | 0x2 | 0x10 | 0x100
		if ace[0] == 0 && !trusted(entry) && mask&dangerous != 0 {
			return model.NewError(model.CodePermissionDenied, "祖先目录允许不可信主体更改选中路径", nil)
		}
	}
	return nil
}

func privateRuntime(configDirectory string, create bool) (directory string, release func() error, result error) {
	// 不接受UNC、设备命名空间、驱动器相对路径或备用数据流；不能在无法核验
	// 本地卷根的路径上降级使用POSIX权限位或只检查末级目录。
	volume := filepath.VolumeName(configDirectory)
	if !filepath.IsAbs(configDirectory) || len(volume) != 2 || volume[1] != ':' || strings.ContainsRune(configDirectory, 0) || strings.HasPrefix(configDirectory, `\\`) {
		return "", nil, model.NewError(model.CodePermissionDenied, "后台仅支持可核验的本地磁盘绝对路径", nil)
	}
	base := filepath.Clean(configDirectory)
	root := volume + `\`
	components := []string{root}
	for _, component := range strings.Split(base[len(root):], `\`) {
		if component == "" {
			continue
		}
		name := strings.ToUpper(strings.SplitN(component, ".", 2)[0])
		reserved := name == "CON" || name == "PRN" || name == "AUX" || name == "NUL" || name == "CONIN$" || name == "CONOUT$" || (len(name) == 4 && (strings.HasPrefix(name, "COM") || strings.HasPrefix(name, "LPT")) && name[3] >= '1' && name[3] <= '9')
		if strings.ContainsAny(component, ":/") || strings.HasSuffix(component, ".") || strings.HasSuffix(component, " ") || reserved {
			return "", nil, model.NewError(model.CodePermissionDenied, "后台路径包含不明确的Windows名称", nil)
		}
		components = append(components, component)
	}
	components = append(components, "DisplayKVM", "runtime")
	api, err := windowsAPI()
	if err != nil {
		return "", nil, windowsError("载入Windows私有IPC接口失败", err)
	}
	sid, err := windowsCurrentSID(api)
	if err != nil {
		return "", nil, err
	}
	rootName, err := syscall.UTF16PtrFromString(root)
	if err != nil {
		return "", nil, windowsError("编码本地卷根失败", err)
	}
	driveType, _, _ := api.getDriveType.Call(uintptr(unsafe.Pointer(rootName)))
	if driveType != 2 && driveType != 3 {
		return "", nil, model.NewError(model.CodePermissionDenied, "后台运行目录必须位于本地磁盘卷", nil)
	}
	var handles []syscall.Handle
	var releaseOnce sync.Once
	var releaseError error
	releaseHandles := func() error {
		releaseOnce.Do(func() {
			for index := len(handles) - 1; index >= 0; index-- {
				releaseError = errors.Join(releaseError, syscall.CloseHandle(handles[index]))
			}
		})
		return releaseError
	}
	defer func() {
		if result != nil {
			result = errors.Join(result, releaseHandles())
			directory, release = "", nil
		}
	}()
	path := root
	for index, component := range components {
		if index > 0 {
			path = filepath.Join(path, component)
		}
		name, err := syscall.UTF16PtrFromString(path)
		if err != nil {
			return "", nil, windowsError("编码目录路径失败", err)
		}
		// 所有祖先和最终目录均不共享DELETE；额外申请FILE_LIST_DIRECTORY，
		// 因为Windows仅metadata访问的句柄不参与删除共享冲突检查，不能只用
		// READ_CONTROL|FILE_READ_ATTRIBUTES声称固定目录。缺少列举权限时明确失败。
		// 同一句柄取属性、owner和DACL，并保留到租约释放，避免检查后替换窗口。
		access := windowsReadControl | windowsReadAttributes | syscall.FILE_LIST_DIRECTORY
		handle, err := syscall.CreateFile(name, access, syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE, nil, syscall.OPEN_EXISTING, syscall.FILE_FLAG_OPEN_REPARSE_POINT|syscall.FILE_FLAG_BACKUP_SEMANTICS, 0)
		created := false
		if create && index > 0 && (errors.Is(err, syscall.ERROR_FILE_NOT_FOUND) || errors.Is(err, syscall.ERROR_PATH_NOT_FOUND)) {
			created, err = windowsCreateDirectory(api, path, sid)
			if err == nil {
				handle, err = syscall.CreateFile(name, access, syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE, nil, syscall.OPEN_EXISTING, syscall.FILE_FLAG_OPEN_REPARSE_POINT|syscall.FILE_FLAG_BACKUP_SEMANTICS, 0)
			}
		}
		if err != nil {
			return "", nil, windowsError("安全打开运行目录链失败", err)
		}
		handles = append(handles, handle)
		var information syscall.ByHandleFileInformation
		if err := syscall.GetFileInformationByHandle(handle, &information); err != nil {
			return "", nil, windowsError("读取目录句柄属性失败", err)
		}
		kind, err := syscall.GetFileType(handle)
		if err != nil {
			return "", nil, windowsError("读取目录句柄类型失败", err)
		}
		if kind != syscall.FILE_TYPE_DISK || information.FileAttributes&syscall.FILE_ATTRIBUTE_DIRECTORY == 0 || information.FileAttributes&syscall.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
			return "", nil, model.NewError(model.CodePermissionDenied, "运行目录链含非磁盘目录或重解析点", nil)
		}
		if index == 0 {
			// SUBST等DOS别名可能隐藏尚未固定的祖先；根句柄的最终路径必须
			// 真正落在卷根，而非某个卷内子目录，否则明确拒绝该路径。
			buffer := make([]uint16, 32768)
			length, _, callErr := api.getFinalPath.Call(uintptr(handle), uintptr(unsafe.Pointer(&buffer[0])), uintptr(len(buffer)), 1)
			if length == 0 || length >= uintptr(len(buffer)) {
				return "", nil, windowsError("核验本地卷根句柄失败", callErr)
			}
			final := syscall.UTF16ToString(buffer[:length])
			if !strings.HasPrefix(final, `\\?\Volume{`) || strings.Index(final, `}\`) != len(final)-2 {
				return "", nil, model.NewError(model.CodePermissionDenied, "驱动器路径未直接指向可核验卷根", nil)
			}
		}
		if err := windowsCheckSecurity(api, handle, sid, created || index >= len(components)-2); err != nil {
			return "", nil, err
		}
	}
	return path, releaseHandles, nil
}

func windowsCreateDirectory(api *windowsIPCAPI, path, sid string) (created bool, result error) {
	descriptor, err := windowsDescriptor(api, sid, true)
	if err != nil {
		return false, err
	}
	defer func() {
		_, err := syscall.LocalFree(syscall.Handle(descriptor))
		result = errors.Join(result, err)
	}()
	attributes := syscall.SecurityAttributes{Length: uint32(unsafe.Sizeof(syscall.SecurityAttributes{})), SecurityDescriptor: descriptor}
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return false, windowsError("编码新目录路径失败", err)
	}
	if err := syscall.CreateDirectory(name, &attributes); err != nil {
		if errors.Is(err, syscall.ERROR_ALREADY_EXISTS) {
			// 与其他创建者并发时只重新打开核验，绝不修复已存在对象的ACL。
			return false, nil
		}
		return false, windowsError("创建私有运行目录失败", err)
	}
	return true, nil
}

func windowsOpenPrivate(path string, access, sharing, creation uint32, create bool) (*os.File, error) {
	api, err := windowsAPI()
	if err != nil {
		return nil, windowsError("载入私有文件接口失败", err)
	}
	sid, err := windowsCurrentSID(api)
	if err != nil {
		return nil, err
	}
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, windowsError("编码私有文件路径失败", err)
	}
	var attributes *syscall.SecurityAttributes
	var descriptor uintptr
	if create {
		descriptor, err = windowsDescriptor(api, sid, false)
		if err != nil {
			return nil, err
		}
		attributes = &syscall.SecurityAttributes{Length: uint32(unsafe.Sizeof(syscall.SecurityAttributes{})), SecurityDescriptor: descriptor}
	}
	handle, openErr := syscall.CreateFile(name, access, sharing, attributes, creation, syscall.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	var freeErr error
	if descriptor != 0 {
		_, freeErr = syscall.LocalFree(syscall.Handle(descriptor))
	}
	if openErr != nil {
		return nil, errors.Join(windowsError("安全打开私有文件失败", openErr), freeErr)
	}
	if freeErr != nil {
		return nil, errors.Join(freeErr, syscall.CloseHandle(handle))
	}
	var information syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(handle, &information); err != nil {
		return nil, errors.Join(windowsError("读取私有文件句柄属性失败", err), syscall.CloseHandle(handle))
	}
	kind, err := syscall.GetFileType(handle)
	if err != nil {
		return nil, errors.Join(windowsError("读取私有文件句柄类型失败", err), syscall.CloseHandle(handle))
	}
	if kind != syscall.FILE_TYPE_DISK || information.FileAttributes&(syscall.FILE_ATTRIBUTE_DIRECTORY|syscall.FILE_ATTRIBUTE_REPARSE_POINT) != 0 || information.NumberOfLinks != 1 {
		return nil, errors.Join(model.NewError(model.CodePermissionDenied, "私有文件必须为无重解析点的单链接磁盘普通文件", nil), syscall.CloseHandle(handle))
	}
	if err := windowsCheckSecurity(api, handle, sid, true); err != nil {
		return nil, errors.Join(err, syscall.CloseHandle(handle))
	}
	return os.NewFile(uintptr(handle), path), nil
}

func openPrivate(path string, create bool) (*os.File, error) {
	if create {
		// 仅永久锁文件使用OPEN_ALWAYS；不截断，读写共享但禁止替换，
		// 否则两个实例可能分别锁住不同的文件对象。
		return windowsOpenPrivate(path, syscall.GENERIC_READ|syscall.GENERIC_WRITE|windowsReadControl, syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE, syscall.OPEN_ALWAYS, true)
	}
	// 发布端点/config需要原子换轮；读者保留旧对象但必须允许DELETE共享，
	// 同时不共享WRITE，避免认证内容在读取过程中被原地改写。
	return windowsOpenPrivate(path, syscall.GENERIC_READ|windowsReadControl, syscall.FILE_SHARE_READ|syscall.FILE_SHARE_DELETE, syscall.OPEN_EXISTING, false)
}

func lockExclusive(file *os.File) error {
	api, err := windowsAPI()
	if err != nil {
		return windowsError("载入单实例锁接口失败", err)
	}
	var overlapped syscall.Overlapped
	ok, _, callErr := api.lockFile.Call(file.Fd(), 3, 0, 1, 0, uintptr(unsafe.Pointer(&overlapped)))
	runtime.KeepAlive(file)
	if ok == 0 {
		if errors.Is(callErr, syscall.ERROR_ACCESS_DENIED) || errors.Is(callErr, syscall.Errno(1314)) {
			return windowsError("权限不足，无法取得后台单实例锁", callErr)
		}
		return model.NewError(model.CodeDaemonUnavailable, "DisplayKVM后台已运行或单实例锁不可用", callErr)
	}
	return nil
}

func createPrivateTemp(directory, pattern string) (*os.File, error) {
	if filepath.Base(pattern) != pattern || strings.ContainsAny(pattern, `\/:`) {
		return nil, model.NewError(model.CodeInvalidRequest, "私有临时文件模板不能包含路径", nil)
	}
	index := strings.LastIndex(pattern, "*")
	prefix, suffix := pattern, ""
	if index >= 0 {
		prefix, suffix = pattern[:index], pattern[index+1:]
	}
	for attempt := 0; attempt < 128; attempt++ {
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return nil, model.NewError(model.CodeInternal, "生成私有临时文件名失败", err)
		}
		path := filepath.Join(directory, prefix+hex.EncodeToString(random[:])+suffix)
		// CREATE_NEW与显式受保护DACL一起生效；写入任何令牌之前就已私密，
		// 不能先CreateTemp继承宽松ACL、再Chmod或补写安全描述符。
		file, err := windowsOpenPrivate(path, syscall.GENERIC_READ|syscall.GENERIC_WRITE|windowsReadControl, syscall.FILE_SHARE_READ, syscall.CREATE_NEW, true)
		if errors.Is(err, syscall.ERROR_FILE_EXISTS) || errors.Is(err, syscall.ERROR_ALREADY_EXISTS) {
			continue
		}
		return file, err
	}
	return nil, model.NewError(model.CodeIO, "私有临时文件名冲突次数过多", os.ErrExist)
}

func replacePrivate(source, target string, expected os.FileInfo) (result error) {
	if expected == nil || !strings.EqualFold(filepath.Dir(source), filepath.Dir(target)) {
		return model.NewError(model.CodePermissionDenied, "只能在同一私有目录内发布已核验文件", nil)
	}
	file, err := windowsOpenPrivate(source, windowsDelete|windowsReadControl|windowsReadAttributes, syscall.FILE_SHARE_READ, syscall.OPEN_EXISTING, false)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, file.Close()) }()
	info, err := file.Stat()
	if err != nil {
		return windowsError("核验发布源文件身份失败", err)
	}
	// os.File.Stat在Windows从该句柄保存卷号与文件ID；SameFile不需再按路径打开。
	if !os.SameFile(info, expected) {
		return model.NewError(model.CodePermissionDenied, "发布源文件已被替换", nil)
	}
	name, err := syscall.UTF16FromString(target)
	if err != nil {
		return windowsError("编码原子发布目标失败", err)
	}
	// FILE_RENAME_INFO的开头是DWORD union、按指针宽度对齐的HANDLE、
	// DWORD字节长度与变长WCHAR数组；用Offsetof兼容Win64及32位布局。
	type renameInfo struct {
		Flags  uint32
		Root   syscall.Handle
		Length uint32
		Name   [1]uint16
	}
	nameOffset := int(unsafe.Offsetof(renameInfo{}.Name))
	buffer := make([]byte, nameOffset+2*len(name))
	information := (*renameInfo)(unsafe.Pointer(&buffer[0]))
	information.Flags = 3 // REPLACE_IF_EXISTS(1) | POSIX_SEMANTICS(2)
	information.Length = uint32(2 * (len(name) - 1))
	for index, value := range name {
		binary.LittleEndian.PutUint16(buffer[nameOffset+2*index:], value)
	}
	api, err := windowsAPI()
	if err != nil {
		return windowsError("载入原子发布接口失败", err)
	}
	// 用户态FileRenameInfoEx=22配合上述Flags，才能在旧读者仍持有共享删除句柄时
	// 替换目标路径，同时让旧句柄继续读取原对象；普通FileRenameInfo会拒绝此场景。
	// 仍在校验过且禁止他人删除共享的源句柄上原子替换，不先删除目标，不复制或原地写入。
	// 依赖Windows 10 1709及以后和卷的FILE_SUPPORTS_POSIX_UNLINK_RENAME能力；
	// 不支持时保留原生错误及原端点，禁止回退普通rename而伪装成支持读者换轮。
	ok, _, callErr := api.setFileInfo.Call(file.Fd(), 22, uintptr(unsafe.Pointer(&buffer[0])), uintptr(len(buffer)))
	if ok == 0 {
		return windowsError("句柄原子替换私有文件失败", callErr)
	}
	return nil
}

func removeOwned(path string, published os.FileInfo) (result error) {
	if published == nil {
		return nil
	}
	file, err := windowsOpenPrivate(path, windowsDelete|windowsReadControl|windowsReadAttributes, syscall.FILE_SHARE_READ, syscall.OPEN_EXISTING, false)
	if err != nil {
		if errors.Is(err, syscall.ERROR_FILE_NOT_FOUND) || errors.Is(err, syscall.ERROR_PATH_NOT_FOUND) {
			return nil
		}
		// 共享冲突和权限错误不能伪装成不存在；保留异物并报告失败。
		return err
	}
	defer func() { result = errors.Join(result, file.Close()) }()
	info, err := file.Stat()
	if err != nil {
		return windowsError("核验待清理私有文件身份失败", err)
	}
	if !os.SameFile(info, published) {
		return nil
	}
	api, err := windowsAPI()
	if err != nil {
		return windowsError("载入私有文件清理接口失败", err)
	}
	// FileDispositionInfo=4的DeleteFile为单字节BOOLEAN；绑定到同一校验句柄，
	// 不做Lstat后按文件名删除，避免旧实例误删后来替换的端点或快照。
	deleteFile := byte(1)
	ok, _, callErr := api.setFileInfo.Call(file.Fd(), 4, uintptr(unsafe.Pointer(&deleteFile)), 1)
	if ok == 0 {
		return windowsError("句柄清理私有文件失败", callErr)
	}
	return nil
}

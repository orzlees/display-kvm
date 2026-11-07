//go:build windows

package loginstart

import (
	"encoding/binary"
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

var loginWindowsAPI struct {
	sync.Once
	procedures map[string]*syscall.Proc
	err        error
}

func loginWindowsProc(name string) (*syscall.Proc, error) {
	loginWindowsAPI.Do(func() {
		// 仅用启动时已加载的KnownDLL取得系统目录，其余DLL以系统绝对路径加载，
		// 避免应用目录或PATH中的同名库影响当前用户身份及注册表安全边界。
		bootstrap := syscall.NewLazyDLL("kernel32.dll").NewProc("GetSystemDirectoryW")
		if err := bootstrap.Find(); err != nil {
			loginWindowsAPI.err = err
			return
		}
		buffer := make([]uint16, 32768)
		n, _, callErr := bootstrap.Call(uintptr(unsafe.Pointer(&buffer[0])), uintptr(len(buffer)))
		if n == 0 || n >= uintptr(len(buffer)) {
			loginWindowsAPI.err = loginWindowsFailure("取得Windows系统目录失败", callErr)
			return
		}
		directory := syscall.UTF16ToString(buffer[:n])
		procedures := make(map[string]*syscall.Proc)
		var libraries []*syscall.DLL
		for _, library := range []struct {
			name  string
			procs []string
		}{
			{"kernel32.dll", []string{"GetDriveTypeW"}},
			{"advapi32.dll", []string{"RegCreateKeyExW", "RegSetValueExW", "RegDeleteValueW", "GetSecurityInfo", "GetSecurityDescriptorOwner", "GetSecurityDescriptorDacl", "IsValidSecurityDescriptor", "IsValidAcl", "IsValidSid", "ConvertStringSecurityDescriptorToSecurityDescriptorW"}},
		} {
			dll, err := syscall.LoadDLL(filepath.Join(directory, library.name))
			if err == nil {
				libraries = append(libraries, dll)
				for _, procedure := range library.procs {
					procedures[procedure], err = dll.FindProc(procedure)
					if err != nil {
						break
					}
				}
			}
			if err != nil {
				for _, loaded := range libraries {
					err = errors.Join(err, loaded.Release())
				}
				loginWindowsAPI.err = err
				return
			}
		}
		// 仅缓存进程生命周期的系统过程地址；路径、SID和ACL每次重新核验。
		loginWindowsAPI.procedures = procedures
	})
	if loginWindowsAPI.err != nil {
		return nil, loginWindowsAPI.err
	}
	proc := loginWindowsAPI.procedures[name]
	if proc == nil {
		return nil, model.NewError(model.CodeUnsupportedOperation, "缺少Windows登录启动系统过程："+name, nil)
	}
	return proc, nil
}

func loginWindowsFailure(message string, err error) error {
	if err == nil || errors.Is(err, syscall.Errno(0)) {
		err = syscall.EINVAL
	}
	code := model.CodeIO
	if errors.Is(err, syscall.ERROR_ACCESS_DENIED) {
		code = model.CodePermissionDenied
	}
	return model.NewError(code, message, err)
}

func loginWindowsSID() (sid string, result error) {
	token, err := syscall.OpenCurrentProcessToken()
	if err != nil {
		return "", loginWindowsFailure("读取登录启动当前进程令牌失败", err)
	}
	defer func() { result = errors.Join(result, token.Close()) }()
	user, err := token.GetTokenUser()
	if err != nil {
		return "", loginWindowsFailure("读取登录启动当前用户SID失败", err)
	}
	proc, err := loginWindowsProc("IsValidSid")
	if err != nil {
		return "", err
	}
	valid, _, _ := proc.Call(uintptr(unsafe.Pointer(user.User.Sid)))
	if valid == 0 {
		return "", model.NewError(model.CodePermissionDenied, "当前用户令牌的SID无效", nil)
	}
	sid, err = user.User.Sid.String()
	// SID指针属于TokenUser缓冲区；转换成Go字符串前不能让缓冲区被回收。
	runtime.KeepAlive(user)
	return sid, err
}

func loginWindowsSecurity(sid string, directory bool) (*syscall.SecurityAttributes, func() error, error) {
	inherit := ""
	if directory {
		inherit = "OICI"
	}
	text, err := syscall.UTF16PtrFromString("O:" + sid + "D:P(A;" + inherit + ";FA;;;" + sid + ")")
	if err != nil {
		return nil, nil, err
	}
	proc, err := loginWindowsProc("ConvertStringSecurityDescriptorToSecurityDescriptorW")
	if err != nil {
		return nil, nil, err
	}
	var descriptor uintptr
	ok, _, callErr := proc.Call(uintptr(unsafe.Pointer(text)), 1, uintptr(unsafe.Pointer(&descriptor)), 0)
	if ok == 0 || descriptor == 0 {
		var freeErr error
		if descriptor != 0 {
			_, freeErr = syscall.LocalFree(syscall.Handle(descriptor))
		}
		return nil, nil, errors.Join(loginWindowsFailure("创建登录启动私有安全描述符失败", callErr), freeErr)
	}
	attributes := &syscall.SecurityAttributes{Length: uint32(unsafe.Sizeof(syscall.SecurityAttributes{})), SecurityDescriptor: descriptor}
	return attributes, func() error { _, err := syscall.LocalFree(syscall.Handle(descriptor)); return err }, nil
}

func loginWindowsBool(name string, arguments ...uintptr) error {
	proc, err := loginWindowsProc(name)
	if err != nil {
		return err
	}
	ok, _, callErr := proc.Call(arguments...)
	if ok == 0 {
		return loginWindowsFailure("Windows登录启动安全检查失败："+name, callErr)
	}
	return nil
}

func checkLoginWindowsHandle(file *os.File, sid string, directory, private bool) (result error) {
	var info syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(syscall.Handle(file.Fd()), &info); err != nil {
		return loginWindowsFailure("核实登录启动文件句柄失败", err)
	}
	if info.FileAttributes&syscall.FILE_ATTRIBUTE_REPARSE_POINT != 0 || (info.FileAttributes&syscall.FILE_ATTRIBUTE_DIRECTORY != 0) != directory || !directory && info.NumberOfLinks != 1 {
		return model.NewError(model.CodePermissionDenied, "登录启动路径不能是重解析点、错误类型或多硬链接文件", nil)
	}
	proc, err := loginWindowsProc("GetSecurityInfo")
	if err != nil {
		return err
	}
	var descriptor uintptr
	// GetSecurityInfo返回DWORD错误码，不使用线程LastError；系统分配的描述符
	// 在所有SID和ACL指针使用完成后才释放，失败路径也必须释放已分配内存。
	status, _, _ := proc.Call(file.Fd(), 1, 0x5, 0, 0, 0, 0, uintptr(unsafe.Pointer(&descriptor)))
	if descriptor != 0 {
		defer func() { _, err := syscall.LocalFree(syscall.Handle(descriptor)); result = errors.Join(result, err) }()
	}
	if status != 0 {
		return loginWindowsFailure("读取登录启动路径所有者和DACL失败", syscall.Errno(status))
	}
	if descriptor == 0 {
		return model.NewError(model.CodePermissionDenied, "登录启动路径缺少可核验的安全描述符", nil)
	}
	if err := loginWindowsBool("IsValidSecurityDescriptor", descriptor); err != nil {
		return err
	}
	var owner *syscall.SID
	var defaulted, present uint32
	if err := loginWindowsBool("GetSecurityDescriptorOwner", descriptor, uintptr(unsafe.Pointer(&owner)), uintptr(unsafe.Pointer(&defaulted))); err != nil {
		return err
	}
	if owner == nil {
		return model.NewError(model.CodePermissionDenied, "登录启动路径缺少所有者SID", nil)
	}
	if err := loginWindowsBool("IsValidSid", uintptr(unsafe.Pointer(owner))); err != nil {
		return err
	}
	ownerSID, err := owner.String()
	if err != nil {
		return err
	}
	trusted := func(candidate string) bool {
		return candidate == sid || candidate == "S-1-5-18" || candidate == "S-1-5-32-544" || candidate == "S-1-5-80-956008885-3418522649-1831038044-1853292631-2271478464"
	}
	if private && ownerSID != sid || !private && !trusted(ownerSID) {
		return model.NewError(model.CodePermissionDenied, "登录启动路径所有者不属于当前用户或可信系统主体", nil)
	}
	var dacl uintptr
	if err := loginWindowsBool("GetSecurityDescriptorDacl", descriptor, uintptr(unsafe.Pointer(&present)), uintptr(unsafe.Pointer(&dacl)), uintptr(unsafe.Pointer(&defaulted))); err != nil {
		return err
	}
	// 缺失或null DACL允许任意主体访问，不能误认成“没有其他授权”。
	if present == 0 || dacl == 0 {
		return model.NewError(model.CodePermissionDenied, "登录启动路径缺少受限DACL", nil)
	}
	if err := loginWindowsBool("IsValidAcl", dacl); err != nil {
		return err
	}
	header := unsafe.Slice((*byte)(unsafe.Pointer(dacl)), 8)
	size, count := int(binary.LittleEndian.Uint16(header[2:4])), int(binary.LittleEndian.Uint16(header[4:6]))
	if size < 8 || private && count != 1 {
		return model.NewError(model.CodePermissionDenied, "私有登录启动文件只能包含当前用户的完整授权", nil)
	}
	acl := unsafe.Slice((*byte)(unsafe.Pointer(dacl)), size)
	offset := 8
	for index := 0; index < count; index++ {
		if offset > len(acl)-4 {
			return model.NewError(model.CodePermissionDenied, "登录启动DACL条目越界", nil)
		}
		length := int(binary.LittleEndian.Uint16(acl[offset+2 : offset+4]))
		if length < 4 || length%4 != 0 || length > len(acl)-offset {
			return model.NewError(model.CodePermissionDenied, "登录启动DACL条目长度无效", nil)
		}
		ace := acl[offset : offset+length]
		offset += length
		if !private && ace[1]&0x08 != 0 {
			// 卷根的CREATOR OWNER完整授权只继承给子项，不授予当前目录写权限。
			continue
		}
		if len(ace) < 16 || ace[0] > 1 || ace[1]&^byte(0x1f) != 0 || 16+4*int(ace[9]) != len(ace) {
			return model.NewError(model.CodePermissionDenied, "登录启动DACL包含无法证明安全的条目", nil)
		}
		entrySID := (*syscall.SID)(unsafe.Pointer(&ace[8]))
		if err := loginWindowsBool("IsValidSid", uintptr(unsafe.Pointer(entrySID))); err != nil {
			return err
		}
		entry, err := entrySID.String()
		if err != nil {
			return err
		}
		mask := binary.LittleEndian.Uint32(ace[4:8])
		unsafeWrites := uint32(0x500d0156)
		if directory {
			// Windows卷根常允许普通用户新建子目录(0x4)，这不允许替换既有
			// 子项。逐级核验已选目录后可保留该权限；文件的同一位表示追加，仍拒绝。
			unsafeWrites &^= 0x4
		}
		if private {
			if ace[0] != 0 || ace[1]&0x08 != 0 || entry != sid || mask != 0x001f01ff {
				return model.NewError(model.CodePermissionDenied, "私有登录启动文件必须仅授权当前用户完全控制", nil)
			}
		} else if ace[0] == 0 && !trusted(entry) && mask&unsafeWrites != 0 {
			return model.NewError(model.CodePermissionDenied, "登录启动路径允许不可信主体改写或替换", nil)
		}
	}
	runtime.KeepAlive(file)
	return nil
}

func closeLoginWindowsDirectories(files []*os.File) error {
	var result error
	for index := len(files) - 1; index >= 0; index-- {
		result = errors.Join(result, files[index].Close())
	}
	return result
}

func pinLoginWindowsDirectory(directory string, create, private bool) (files []*os.File, result error) {
	volume := filepath.VolumeName(directory)
	if !absolutePath(directory) || len(volume) != 2 || volume[1] != ':' || strings.HasPrefix(directory, `\\`) || strings.ContainsAny(directory[len(volume):], ":\x00\r\n\"%") {
		return nil, model.NewError(model.CodePermissionDenied, "登录启动路径只支持明确的本地盘绝对目录，不接受UNC、设备或流路径", nil)
	}
	for _, component := range strings.Split(directory[len(volume):], `\`) {
		if component != "" && strings.TrimRight(component, " .") != component {
			return nil, model.NewError(model.CodePermissionDenied, "登录启动路径不接受末尾空格或点的目录别名", nil)
		}
	}
	root, err := syscall.UTF16PtrFromString(volume + `\`)
	if err != nil {
		return nil, err
	}
	proc, err := loginWindowsProc("GetDriveTypeW")
	if err != nil {
		return nil, err
	}
	drive, _, _ := proc.Call(uintptr(unsafe.Pointer(root)))
	if drive != 3 { // DRIVE_FIXED：映射网络盘不能提供这里依赖的本地ACL与替换边界。
		return nil, model.NewError(model.CodeUnsupportedOperation, "登录启动路径要求可核验ACL的本地固定磁盘", nil)
	}
	sid, err := loginWindowsSID()
	if err != nil {
		return nil, err
	}
	var paths []string
	for path := directory; ; path = filepath.Dir(path) {
		paths = append(paths, path)
		if filepath.Dir(path) == path {
			break
		}
	}
	defer func() {
		if result != nil {
			result = errors.Join(result, closeLoginWindowsDirectories(files))
			files = nil
		}
	}()
	for index := len(paths) - 1; index >= 0; index-- {
		path := paths[index]
		name, err := syscall.UTF16PtrFromString(path)
		if err != nil {
			return files, err
		}
		// 从卷根逐级固定不共享删除的目录句柄；重解析点自身被打开并拒绝，
		// 防止检查父目录后被替换成junction，再把子文件读写重定向到别处。
		handle, err := syscall.CreateFile(name, 0x00020000|0x80, syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE, nil, syscall.OPEN_EXISTING, syscall.FILE_FLAG_BACKUP_SEMANTICS|syscall.FILE_FLAG_OPEN_REPARSE_POINT, 0)
		if create && errors.Is(err, os.ErrNotExist) {
			attributes, release, securityErr := loginWindowsSecurity(sid, true)
			if securityErr != nil {
				return files, securityErr
			}
			createErr := syscall.CreateDirectory(name, attributes)
			if errors.Is(createErr, syscall.ERROR_ALREADY_EXISTS) {
				createErr = nil
			}
			if err := errors.Join(createErr, release()); err != nil {
				return files, loginWindowsFailure("创建登录启动私有目录失败", err)
			}
			handle, err = syscall.CreateFile(name, 0x00020000|0x80, syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE, nil, syscall.OPEN_EXISTING, syscall.FILE_FLAG_BACKUP_SEMANTICS|syscall.FILE_FLAG_OPEN_REPARSE_POINT, 0)
		}
		if err != nil {
			return files, loginWindowsFailure("打开登录启动安全目录失败", err)
		}
		file := os.NewFile(uintptr(handle), path)
		files = append(files, file)
		if err := checkLoginWindowsHandle(file, sid, true, private && index == 0); err != nil {
			return files, err
		}
		opened, statErr := file.Stat()
		current, pathErr := os.Lstat(path)
		if statErr != nil || pathErr != nil || !os.SameFile(opened, current) {
			return files, model.NewError(model.CodePermissionDenied, "登录启动目录在核验期间改变身份", errors.Join(statErr, pathErr))
		}
	}
	return files, nil
}

func secureDirectory(directory string, _ int, create, private bool) error {
	files, err := pinLoginWindowsDirectory(directory, create, private)
	return errors.Join(err, closeLoginWindowsDirectories(files))
}

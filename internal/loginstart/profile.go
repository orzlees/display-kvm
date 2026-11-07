package loginstart

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/orzlee/displayKvm/internal/controltools"
	"github.com/orzlee/displayKvm/internal/model"
	"github.com/orzlee/displayKvm/internal/settings"
)

const maxStartupBytes = 1024 * 1024

// LoadServiceProfile只加载私有档案结构；业务文档被移除或损坏不等于档案不存在。
// 文档内容和工具就绪由保存选择及服务启动入口校验，状态查询分别保留已配置和未就绪。
func LoadServiceProfile(path string) (ServiceProfile, error) { return loadProfile(path, os.Getuid()) }

func loadProfile(path string, uid int) (ServiceProfile, error) {
	if !absolutePath(path) || filepath.Base(path) != "startup.json" || filepath.Base(filepath.Dir(path)) != "DisplayKVM" {
		return ServiceProfile{}, model.NewError(model.CodePermissionDenied, "启动档案必须是绝对私有DisplayKVM/startup.json路径", nil)
	}
	if err := secureDirectory(filepath.Dir(path), uid, false, true); err != nil {
		return ServiceProfile{}, err
	}
	data, _, err := readPrivate(path, uid)
	if err != nil {
		return ServiceProfile{}, err
	}
	return decodeProfile(data)
}

func decodeProfile(data []byte) (ServiceProfile, error) {
	var profile ServiceProfile
	if len(data) > maxStartupBytes {
		return profile, model.NewError(model.CodeMessageTooLarge, "启动档案超过1MiB", nil)
	}
	if err := model.DecodeJSON(data, &profile); err != nil {
		return profile, model.NewError(model.CodeInvalidConfig, "启动档案结构无效", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return profile, model.NewError(model.CodeInvalidConfig, "启动档案字段无效", err)
	}
	for name, value := range fields {
		if name != "version" && name != "config_path" {
			err := model.NewError(model.CodeInvalidConfig, "版本2启动档案只接受version和config_path，不能混入旧路径字段", nil)
			err.Field = name
			return ServiceProfile{}, err
		}
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			err := model.NewError(model.CodeInvalidConfig, "启动档案字段不能为null", nil)
			err.Field = name
			return ServiceProfile{}, err
		}
	}
	if err := validateProfile(profile); err != nil {
		return ServiceProfile{}, err
	}
	return profile, nil
}

func validateProfile(profile ServiceProfile) error {
	if profile.Version != 2 {
		return model.NewError(model.CodeUnsupportedVersion, "只接受启动档案版本2", nil)
	}
	if !absolutePath(profile.ConfigPath) || strings.ContainsRune(profile.ConfigPath, 0) {
		err := model.NewError(model.CodeInvalidConfig, "完整配置文档必须使用明确的绝对文件路径", nil)
		err.Field = "config_path"
		return err
	}
	return nil
}

// 显式文件只能引用普通文件，不能依赖PATH或把目录、设备、链接当配置读取。
// 打开后再比较inode，防止检查与打开之间的替换绕过最终文件类型检查。
func openSource(path string, executable bool) (*os.File, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, model.NewError(model.CodeIO, "检查显式启动文件失败", err)
	}
	// Windows普通文件没有Unix执行位；应用登记由平台核实PE及真实Go入口，
	// 不能把合成的0666权限当成不可执行，也不能为兼容Windows给文件补假执行位。
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || (executable && runtime.GOOS != "windows" && info.Mode().Perm()&0111 == 0) {
		return nil, model.NewError(model.CodeInvalidConfig, "启动路径必须引用普通文件，控制工具还必须可执行", nil)
	}
	file, err := openSourceFile(path)
	if err != nil {
		return nil, model.NewError(model.CodeIO, "打开显式启动文件失败", err)
	}
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		file.Close()
		return nil, model.NewError(model.CodePermissionDenied, "启动文件在核验期间发生变化", err)
	}
	return file, nil
}

// 保存文档选择不等于登记服务。语言和独立桌面设置也使用此档案，不能因为控制工具
// 尚未安装或当前应用已移动而拒绝保存；仍须严格读取完整文档，不能把坏文件当作草稿。
func loadProfileDocument(profile ServiceProfile) (settings.Document, error) {
	if err := validateProfile(profile); err != nil {
		return settings.Document{}, err
	}
	file, err := openSource(profile.ConfigPath, false)
	if err != nil {
		return settings.Document{}, err
	}
	document, loadErr := settings.Load(file)
	if err := errors.Join(loadErr, file.Close()); err != nil {
		return settings.Document{}, err
	}
	return document, nil
}

// 服务登记和就绪检查才需要控制工具；每次重读用户文档，不缓存解析出的安装绝对路径。
// resolver必须来自已核实的目标应用内daemon，不能用管理界面自身位置或PATH代替下次登录环境。
func validateServiceReady(profile ServiceProfile, resolver controltools.Resolver) error {
	document, err := loadProfileDocument(profile)
	if err != nil {
		return err
	}
	_, err = document.ValidateForService(resolver)
	return err
}

func readBounded(file io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(file, maxStartupBytes+1))
	if err != nil {
		return nil, model.NewError(model.CodeIO, "读取登录启动文件失败", err)
	}
	if len(data) > maxStartupBytes {
		return nil, model.NewError(model.CodeMessageTooLarge, "登录启动文件超过1MiB", nil)
	}
	return data, nil
}

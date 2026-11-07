//go:build darwin

package loginstart

import (
	"bytes"
	"context"
	"debug/buildinfo"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"

	"github.com/orzlee/displayKvm/internal/controltools"
	"github.com/orzlee/displayKvm/internal/model"
)

const agentMarker = "io.github.orzlee.displaykvm.loginstart.v1"

func platformAvailable() error { return nil }

func platformIdentity() error { return nil }

// 文件检查后仍可能被替换成链接或FIFO；打开本身也必须拒绝链接并保持非阻塞，
// 随后的inode比较才能验证同一普通文件，而不会先卡在打开设备的副作用上。
func openSourceFile(path string) (*os.File, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}

// 私有文件沿用IPC的0700目录、0600单链接文件和不跟随链接的检查边界；
// IPC助手绑定运行目录且未导出，此处不能借后台单实例锁代替短时设置锁。
func secureDirectory(directory string, uid int, create, private bool) error {
	if !absolutePath(directory) {
		return model.NewError(model.CodePermissionDenied, "登录启动目录必须为绝对路径", nil)
	}
	var missing []string
	for path := directory; ; path = filepath.Dir(path) {
		info, err := os.Lstat(path)
		if create && errors.Is(err, os.ErrNotExist) {
			missing = append(missing, path)
			continue
		}
		if err != nil {
			return model.NewError(model.CodeIO, "检查登录启动目录失败", err)
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || (stat.Uid != uint32(uid) && stat.Uid != 0) {
			return model.NewError(model.CodePermissionDenied, "登录启动目录类型、链接或所有者不安全", nil)
		}
		// /Applications是系统管理的稳定安装根，允许管理员组写入；其它祖先
		// 禁止组写入。root拥有的sticky临时根允许隔离测试建立私有子目录。
		writable := info.Mode().Perm()&0022 != 0
		trustedRoot := stat.Uid == 0 && (info.Mode()&os.ModeSticky != 0 || (path == "/Applications" && info.Mode().Perm()&0002 == 0))
		if writable && !trustedRoot {
			return model.NewError(model.CodePermissionDenied, "登录启动父目录允许不可信写入", nil)
		}
		if path == directory && (stat.Uid != uint32(uid) || (private && info.Mode().Perm() != 0700)) {
			return model.NewError(model.CodePermissionDenied, "登录启动私有目录所有者或权限无效", nil)
		}
		if parent := filepath.Dir(path); parent == path {
			break
		}
	}
	for i := len(missing) - 1; i >= 0; i-- {
		if err := os.Mkdir(missing[i], 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return model.NewError(model.CodeIO, "创建登录启动私有目录失败", err)
		}
		// 另一个进程可能在Mkdir前建立目录，必须重新核验，不能chmod掩盖问题。
		if err := secureDirectory(missing[i], uid, false, true); err != nil {
			return err
		}
	}
	return nil
}

func openPrivate(path string, uid int, create bool) (*os.File, error) {
	flags := syscall.O_RDONLY | syscall.O_NOFOLLOW | syscall.O_CLOEXEC | syscall.O_NONBLOCK
	if create {
		flags = syscall.O_RDWR | syscall.O_CREAT | syscall.O_NOFOLLOW | syscall.O_CLOEXEC | syscall.O_NONBLOCK
	}
	fd, err := syscall.Open(path, flags, 0600)
	if err != nil {
		return nil, model.NewError(model.CodePermissionDenied, "无法安全打开登录启动私有文件", err)
	}
	file := os.NewFile(uintptr(fd), path)
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, model.NewError(model.CodeIO, "检查登录启动私有文件失败", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || stat.Uid != uint32(uid) || stat.Nlink != 1 {
		file.Close()
		return nil, model.NewError(model.CodePermissionDenied, "登录启动文件必须属于当前用户、单链接且权限为0600", nil)
	}
	return file, nil
}

func readPrivate(path string, uid int) ([]byte, os.FileInfo, error) {
	file, err := openPrivate(path, uid, false)
	if err != nil {
		return nil, nil, err
	}
	data, readErr := readBounded(file)
	info, statErr := file.Stat()
	if err := errors.Join(readErr, statErr, file.Close()); err != nil {
		return nil, nil, model.NewError(model.CodeIO, "读取登录启动私有文件失败", err)
	}
	return data, info, nil
}

func (m *Manager) settingsLock() (io.Closer, error) {
	directory := filepath.Dir(m.profilePath())
	if err := secureDirectory(directory, m.env.UID, true, true); err != nil {
		return nil, err
	}
	file, err := openPrivate(filepath.Join(directory, "startup.lock"), m.env.UID, true)
	if err != nil {
		return nil, err
	}
	// 持久锁文件绝不删除，避免两个进程各锁住不同inode后并发改写同一设置。
	// 非阻塞锁让调用方显式处理占用；失败不得自动重发保存或注册。
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		return nil, model.NewError(model.CodeIO, "启动设置正被另一进程修改", err)
	}
	return file, nil
}

func sameDestination(path string, expected os.FileInfo) error {
	actual, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) && expected == nil {
		return nil
	}
	if err != nil {
		return model.NewError(model.CodeIO, "重新核验登录启动目标失败", err)
	}
	if expected == nil || !os.SameFile(actual, expected) || actual.Mode() != expected.Mode() || actual.Size() != expected.Size() || !actual.ModTime().Equal(expected.ModTime()) {
		return model.NewError(model.CodePermissionDenied, "登录启动文件已被外部替换或修改，拒绝覆盖或删除", nil)
	}
	oldStat, oldOK := expected.Sys().(*syscall.Stat_t)
	newStat, newOK := actual.Sys().(*syscall.Stat_t)
	if !oldOK || !newOK || oldStat.Uid != newStat.Uid || newStat.Nlink != 1 {
		return model.NewError(model.CodePermissionDenied, "登录启动文件所有者或链接数已改变", nil)
	}
	return nil
}

// 临时文件与目标位于同一目录，先同步再原子替换并读回。替换后读回失败属于
// 保存结果未知，不能报告false诱使上层重发；锁仅覆盖此设置事务，不锁业务文件。
func atomicPrivate(path string, data []byte, uid int, expected os.FileInfo) (*bool, error) {
	file, err := os.CreateTemp(filepath.Dir(path), ".startup-*")
	if err != nil {
		return boolean(false), model.NewError(model.CodeIO, "创建登录启动临时文件失败", err)
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	defer file.Close()
	if _, err := file.Write(data); err != nil {
		return boolean(false), model.NewError(model.CodeIO, "写入登录启动文件失败", err)
	}
	if err := errors.Join(file.Sync(), file.Close()); err != nil {
		return boolean(false), model.NewError(model.CodeIO, "同步登录启动文件失败", err)
	}
	check, _, err := readPrivate(temporary, uid)
	if err != nil || !bytes.Equal(check, data) {
		return boolean(false), model.NewError(model.CodeIO, "核验登录启动临时文件失败", err)
	}
	if err := sameDestination(path, expected); err != nil {
		return boolean(false), err
	}
	if err := os.Rename(temporary, path); err != nil {
		return boolean(false), model.NewError(model.CodeIO, "原子保存登录启动文件失败", err)
	}
	check, _, err = readPrivate(path, uid)
	if err != nil || !bytes.Equal(check, data) {
		return nil, model.NewError(model.CodeIO, "登录启动文件已替换，但读回无法确认", err)
	}
	return boolean(true), nil
}

func (m *Manager) agentPath(target Target) string {
	return filepath.Join(m.env.HomeDir, "Library", "LaunchAgents", label(target)+".plist")
}

func (m *Manager) stableAppPath(path string) error {
	if !absolutePath(path) || (path != "/Applications/DisplayKvm.app" && path != filepath.Join(m.env.HomeDir, "Applications", "DisplayKvm.app")) {
		return model.NewError(model.CodeInvalidRequest, "请将DisplayKvm.app安装到/Applications或当前用户Applications目录", nil)
	}
	return nil
}

func (m *Manager) validateApp(path string, target Target, requireStartup bool) error {
	if err := m.stableAppPath(path); err != nil {
		return err
	}
	// 允许系统或本用户安装的应用，逐级拒绝符号链接和可被普通用户替换的父目录。
	for current := path; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil {
			return model.NewError(model.CodeIO, "稳定应用位置不存在或无法读取", err)
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || (stat.Uid != 0 && stat.Uid != uint32(m.env.UID)) || (info.Mode().Perm()&0022 != 0 && !(stat.Uid == 0 && (info.Mode()&os.ModeSticky != 0 || (current == "/Applications" && info.Mode().Perm()&0002 == 0)))) {
			return model.NewError(model.CodePermissionDenied, "稳定应用路径存在链接、不可信所有者或写权限", nil)
		}
		if parent := filepath.Dir(current); parent == current {
			break
		}
	}
	for _, directory := range []string{filepath.Join(path, "Contents"), filepath.Join(path, "Contents", "MacOS")} {
		info, err := os.Lstat(directory)
		if err != nil {
			return model.NewError(model.CodeIO, "应用辅助程序目录不可用", err)
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || (stat.Uid != 0 && stat.Uid != uint32(m.env.UID)) || info.Mode().Perm()&0022 != 0 {
			return model.NewError(model.CodePermissionDenied, "应用辅助程序目录不可信", nil)
		}
	}
	infoFile, err := openSource(filepath.Join(path, "Contents", "Info.plist"), false)
	if err != nil {
		return err
	}
	data, readErr := readBounded(infoFile)
	metadataInfo, metadataStatErr := infoFile.Stat()
	if err := errors.Join(readErr, metadataStatErr, infoFile.Close()); err != nil {
		return err
	}
	metadataStat, metadataOK := metadataInfo.Sys().(*syscall.Stat_t)
	if !metadataOK || (metadataStat.Uid != 0 && metadataStat.Uid != uint32(m.env.UID)) || metadataInfo.Mode().Perm()&0022 != 0 {
		return model.NewError(model.CodePermissionDenied, "应用身份文件所有者或写权限不可信", nil)
	}
	values, err := parsePlist(data)
	if err != nil || values["CFBundleIdentifier"] != "io.github.orzlee.displaykvm" || values["CFBundleExecutable"] != "displaykvm" || values["CFBundlePackageType"] != "APPL" {
		return model.NewError(model.CodeInvalidConfig, "稳定路径中的应用不是DisplayKVM应用包", err)
	}
	if requireStartup {
		// 旧GUI沿用相同包标识和Go入口，无法仅凭名称判断是否支持静默登录。
		// 功能版本只说明同包能力，不代表分发认证；未知版本不能猜测兼容。
		if values["DisplayKVMStartupVersion"] != "1" {
			return model.NewError(model.CodeInvalidConfig, "应用缺少已知的登录启动功能版本DisplayKVMStartupVersion=1", nil)
		}
		return m.validateStartupPrograms(path)
	}
	name := "displaykvm"
	if target == Service {
		name = "displaykvmd"
	}
	file, err := openSource(filepath.Join(path, "Contents", "MacOS", name), true)
	if err != nil {
		return err
	}
	info, statErr := file.Stat()
	closeErr := file.Close()
	if err := errors.Join(statErr, closeErr); err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || (stat.Uid != 0 && stat.Uid != uint32(m.env.UID)) || info.Mode().Perm()&0022 != 0 {
		return model.NewError(model.CodePermissionDenied, "应用辅助程序所有者或写权限不可信", nil)
	}
	return nil
}

// 登记以后登录安排前，必须核实同包三程序齐备，不能把GUI副本当作服务或CLI。
// 查询仍按各任务已有依赖分别说明就绪状态，关闭仅核实plist归属，避免旧包或
// 已丢失的应用反过来阻止用户关闭登录项；完整包能力检查只用于Enable。
func (m *Manager) validateStartupPrograms(app string) error {
	for _, program := range []struct{ name, entry string }{
		{"displaykvm", "github.com/orzlee/displayKvm/desktop"},
		{"displaykvmd", "github.com/orzlee/displayKvm/cmd/displaykvmd"},
		{"displaykvm-cli", "github.com/orzlee/displayKvm/cmd/displaykvm"},
	} {
		file, err := openSource(filepath.Join(app, "Contents", "MacOS", program.name), true)
		if err != nil {
			return model.NewError(model.CodeInvalidConfig, "应用缺少安全可执行程序"+program.name, err)
		}
		info, statErr := file.Stat()
		if statErr != nil {
			return errors.Join(model.NewError(model.CodeIO, "读取应用程序状态失败"+program.name, statErr), file.Close())
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 || (stat.Uid != 0 && stat.Uid != uint32(m.env.UID)) || info.Mode().Perm()&0022 != 0 || stat.Nlink != 1 {
			return errors.Join(model.NewError(model.CodePermissionDenied, "应用程序类型、执行权限、所有者、写权限或链接不可信"+program.name, nil), file.Close())
		}
		// 从已经拒绝链接并核对inode的文件句柄读取，不能再按路径打开而绕过检查。
		// Path表示实际命令包入口；Main.Path只是模块名，daemon与CLI共用根模块，
		// 按Main.Path比较会错误地接受互相冒充的程序。
		build, buildErr := buildinfo.Read(file)
		if err := errors.Join(buildErr, file.Close()); err != nil {
			return model.NewError(model.CodeInvalidConfig, "无法确认应用程序的真实Go入口"+program.name, err)
		}
		if build.Path != program.entry {
			return model.NewError(model.CodeInvalidConfig, fmt.Sprintf("应用程序%s入口不匹配：实际%s，预期%s", program.name, build.Path, program.entry), nil)
		}
	}
	return nil
}

func (m *Manager) arguments(target Target, app string) []string {
	if target == Service {
		return []string{filepath.Join(app, "Contents", "MacOS", "displaykvmd"), "-startup-profile", m.profilePath()}
	}
	return []string{filepath.Join(app, "Contents", "MacOS", "displaykvm"), "--login-start"}
}

func (m *Manager) encodeAgent(target Target, app string) []byte {
	var buffer bytes.Buffer
	buffer.WriteString(xml.Header + `<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n" + `<plist version="1.0"><dict><key>Label</key><string>`)
	xml.EscapeText(&buffer, []byte(label(target)))
	buffer.WriteString(`</string><key>DisplayKVMLoginStart</key><string>` + agentMarker + `</string><key>RunAtLoad</key><true/><key>ProgramArguments</key><array>`)
	for _, argument := range m.arguments(target, app) {
		buffer.WriteString(`<string>`)
		xml.EscapeText(&buffer, []byte(argument))
		buffer.WriteString(`</string>`)
	}
	buffer.WriteString("</array></dict></plist>\n")
	return buffer.Bytes()
}

// plist采用键值节点解析，不能用contains识别本功能；未知键、重复键、额外参数
// 或KeepAlive均不属于本功能拥有的任务，禁用时必须保留原文件而不是误删。
func (m *Manager) readAgent(target Target) (string, os.FileInfo, error) {
	path := m.agentPath(target)
	if err := secureDirectory(filepath.Dir(path), m.env.UID, false, false); err != nil {
		return "", nil, err
	}
	data, info, err := readPrivate(path, m.env.UID)
	if err != nil {
		return "", nil, err
	}
	values, err := parsePlist(data)
	if err != nil || len(values) != 4 || values["Label"] != label(target) || values["DisplayKVMLoginStart"] != agentMarker || values["RunAtLoad"] != true {
		return "", nil, model.NewError(model.CodePermissionDenied, "同名登录任务不属于本功能，拒绝覆盖或删除", err)
	}
	arguments, ok := values["ProgramArguments"].([]string)
	if !ok || len(arguments) == 0 {
		return "", nil, model.NewError(model.CodePermissionDenied, "登录任务参数无效，不能确认所属功能", nil)
	}
	app := filepath.Dir(filepath.Dir(filepath.Dir(arguments[0])))
	if err := m.stableAppPath(app); err != nil {
		return "", nil, err
	}
	want := m.arguments(target, app)
	if len(arguments) != len(want) {
		return "", nil, model.NewError(model.CodePermissionDenied, "登录任务包含额外或缺少的参数", nil)
	}
	for index := range want {
		if arguments[index] != want[index] {
			return "", nil, model.NewError(model.CodePermissionDenied, "登录任务参数与本功能不匹配", nil)
		}
	}
	return app, info, nil
}

type plistNode struct {
	XMLName  xml.Name
	Text     string      `xml:",chardata"`
	Children []plistNode `xml:",any"`
}

func parsePlist(data []byte) (map[string]any, error) {
	var root plistNode
	decoder := xml.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&root); err != nil {
		return nil, err
	}
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if text, ok := token.(xml.CharData); !ok || strings.TrimSpace(string(text)) != "" {
			return nil, fmt.Errorf("plist后存在额外内容")
		}
	}
	if root.XMLName.Space != "" || root.XMLName.Local != "plist" || strings.TrimSpace(root.Text) != "" || len(root.Children) != 1 || root.Children[0].XMLName.Space != "" || root.Children[0].XMLName.Local != "dict" || strings.TrimSpace(root.Children[0].Text) != "" {
		return nil, fmt.Errorf("plist必须为单个字典")
	}
	children := root.Children[0].Children
	if len(children)%2 != 0 {
		return nil, fmt.Errorf("plist键值不完整")
	}
	values := make(map[string]any)
	for i := 0; i < len(children); i += 2 {
		key, value := children[i], children[i+1]
		if key.XMLName.Space != "" || value.XMLName.Space != "" || key.XMLName.Local != "key" || key.Text == "" || len(key.Children) != 0 {
			return nil, fmt.Errorf("plist键无效")
		}
		if _, exists := values[key.Text]; exists {
			return nil, fmt.Errorf("plist存在重复键")
		}
		switch value.XMLName.Local {
		case "string":
			if len(value.Children) != 0 {
				return nil, fmt.Errorf("plist字符串无效")
			}
			values[key.Text] = value.Text
		case "true", "false":
			if len(value.Children) != 0 || strings.TrimSpace(value.Text) != "" {
				return nil, fmt.Errorf("plist布尔值无效")
			}
			values[key.Text] = value.XMLName.Local == "true"
		case "array":
			if strings.TrimSpace(value.Text) != "" {
				return nil, fmt.Errorf("plist参数数组含额外文本")
			}
			var items []string
			for _, item := range value.Children {
				if item.XMLName.Space != "" || item.XMLName.Local != "string" || len(item.Children) != 0 {
					return nil, fmt.Errorf("plist参数数组必须为字符串")
				}
				items = append(items, item.Text)
			}
			values[key.Text] = items
		default:
			return nil, fmt.Errorf("plist含不支持的节点类型")
		}
	}
	return values, nil
}

var disabledLine = regexp.MustCompile(`^\s*"([^"]+)"\s*=>\s*(true|false|enabled|disabled)\s*;?\s*$`)

func (m *Manager) disabled(ctx context.Context) (map[Target]*bool, error) {
	data, err := m.run(ctx, "/bin/launchctl", "print-disabled", "gui/"+strconv.Itoa(m.env.UID))
	if err != nil {
		return nil, model.NewError(model.CodeIO, "查询登录任务disabled状态失败", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) < 2 || strings.TrimSpace(lines[0]) != "disabled services = {" || strings.TrimSpace(lines[len(lines)-1]) != "}" {
		return nil, model.NewError(model.CodeIO, "登录任务disabled输出结构无法识别", nil)
	}
	values := make(map[Target]*bool)
	for _, line := range lines[1 : len(lines)-1] {
		if strings.TrimSpace(line) == "" {
			continue
		}
		match := disabledLine.FindStringSubmatch(line)
		if match == nil {
			return nil, model.NewError(model.CodeIO, "登录任务disabled输出字段无法识别", nil)
		}
		for _, target := range []Target{Service, Desktop} {
			if match[1] == label(target) {
				if values[target] != nil {
					return nil, model.NewError(model.CodeIO, "登录任务disabled输出存在重复标签", nil)
				}
				// 系统输出可能使用布尔词或enabled/disabled；这里存储“是否禁用”，
				// 因此enabled与false等价，不能按“是否启用”直接转换而倒置开关。
				values[target] = boolean(match[2] == "true" || match[2] == "disabled")
			}
		}
	}
	return values, nil
}

func (m *Manager) loaded(ctx context.Context, target Target) (*bool, error) {
	service := fmt.Sprintf("gui/%d/%s", m.env.UID, label(target))
	data, err := m.run(ctx, "/bin/launchctl", "print", service)
	if err != nil {
		absent := fmt.Sprintf("Could not find service %q in domain for user gui: %d", label(target), m.env.UID)
		lines := strings.Split(strings.TrimSpace(string(data)), "\n")
		if len(lines) == 1 && strings.TrimSpace(lines[0]) == absent || len(lines) == 2 && strings.TrimSpace(lines[0]) == "Bad request." && strings.TrimSpace(lines[1]) == absent {
			return boolean(false), nil
		}
		return nil, model.NewError(model.CodeIO, "查询登录任务加载状态失败", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) < 2 || strings.TrimSpace(lines[0]) != service+" = {" || strings.TrimSpace(lines[len(lines)-1]) != "}" {
		return nil, model.NewError(model.CodeIO, "登录任务加载查询没有返回精确标签", nil)
	}
	return boolean(true), nil
}

func (m *Manager) status(ctx context.Context) (Status, error) {
	status := emptyStatus()
	if err := ctx.Err(); err != nil {
		return status, err
	}
	profile, profileErr := m.Profile(ctx)
	if profileErr == nil {
		status.Profile = &profile
	} else {
		status.ProfileMessage = profileErr.Error()
	}
	var failures []error
	if profileErr != nil && !errors.Is(profileErr, os.ErrNotExist) {
		failures = append(failures, profileErr)
	}
	disabled, disabledErr := m.disabled(ctx)
	if disabledErr != nil {
		failures = append(failures, disabledErr)
	}
	for _, item := range []*ItemStatus{&status.Service, &status.Desktop} {
		var messages []string
		app, _, agentErr := m.readAgent(item.Target)
		switch {
		case errors.Is(agentErr, os.ErrNotExist):
			item.Present = boolean(false)
			item.Enabled = boolean(false)
			item.Ready = boolean(false)
			messages = append(messages, "尚未配置此登录任务")
		case agentErr != nil:
			messages = append(messages, agentErr.Error())
			failures = append(failures, agentErr)
		default:
			item.Present = boolean(true)
			item.AppPath = app
			if disabledErr == nil && disabled[item.Target] != nil {
				item.Enabled = boolean(!*disabled[item.Target])
			} else {
				err := model.NewError(model.CodeIO, "无法确认此标签的disabled状态，启用状态未知", disabledErr)
				messages = append(messages, err.Error())
				failures = append(failures, err)
			}
			readyErr := m.validateApp(app, item.Target, false)
			if item.Target == Service {
				if profileErr != nil {
					readyErr = errors.Join(readyErr, profileErr)
				} else if readyErr == nil {
					// 仅在实际应用路径核实后解析工具，并复用将写入plist的daemon位置。
					// 应用已丢失时不回退到GUI工作目录，否则会把另一份包误报为就绪。
					readyErr = validateServiceReady(profile, controltools.Resolver{GOOS: "darwin", Executable: m.arguments(Service, app)[0]})
				}
			}
			item.Ready = boolean(readyErr == nil)
			if readyErr != nil {
				messages = append(messages, "下次登录所需文件未就绪："+readyErr.Error())
			} else {
				messages = append(messages, "登录安排仅在以后登录生效；不改变当前进程")
			}
		}
		loaded, loadedErr := m.loaded(ctx, item.Target)
		item.Loaded = loaded
		if loadedErr != nil {
			messages = append(messages, loadedErr.Error())
			failures = append(failures, loadedErr)
		}
		item.Message = strings.Join(messages, "；")
	}
	return status, errors.Join(failures...)
}

func (m *Manager) change(ctx context.Context, target Target, app string, enable bool) (Result, error) {
	if err := validTarget(target); err != nil {
		return m.result(ctx, boolean(false), err)
	}
	if err := ctx.Err(); err != nil {
		return m.result(ctx, boolean(false), err)
	}
	if enable {
		if err := m.validateApp(app, target, true); err != nil {
			return m.result(ctx, boolean(false), err)
		}
		if target == Service {
			profile, err := m.Profile(ctx)
			if err == nil {
				err = validateServiceReady(profile, controltools.Resolver{GOOS: "darwin", Executable: m.arguments(Service, app)[0]})
			}
			if err != nil {
				return m.result(ctx, boolean(false), err)
			}
		}
	}
	lock, err := m.settingsLock()
	if err != nil {
		return m.result(ctx, boolean(false), err)
	}
	path := m.agentPath(target)
	if err := secureDirectory(filepath.Dir(path), m.env.UID, enable, false); err != nil {
		if !enable && errors.Is(err, os.ErrNotExist) {
			return m.result(ctx, boolean(true), lock.Close())
		}
		return m.result(ctx, boolean(false), errors.Join(err, lock.Close()))
	}
	_, expected, err := m.readAgent(target)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return m.result(ctx, boolean(false), errors.Join(err, lock.Close()))
	}
	if err := ctx.Err(); err != nil {
		return m.result(ctx, boolean(false), errors.Join(err, lock.Close()))
	}
	if !enable {
		if expected == nil {
			return m.result(ctx, boolean(true), lock.Close())
		}
		if err := sameDestination(path, expected); err != nil {
			return m.result(ctx, boolean(false), errors.Join(err, lock.Close()))
		}
		if err := os.Remove(path); err != nil {
			return m.result(ctx, boolean(false), errors.Join(model.NewError(model.CodeIO, "删除本功能登录任务失败", err), lock.Close()))
		}
		_, checkErr := os.Lstat(path)
		if !errors.Is(checkErr, os.ErrNotExist) {
			return m.result(ctx, nil, errors.Join(model.NewError(model.CodeIO, "任务已删除，但目标读回无法确认", checkErr), lock.Close()))
		}
		return m.result(ctx, boolean(true), lock.Close())
	}
	saved, writeErr := atomicPrivate(path, m.encodeAgent(target, app), m.env.UID, expected)
	if writeErr != nil {
		return m.result(ctx, saved, errors.Join(writeErr, lock.Close()))
	}
	disabled, queryErr := m.disabled(ctx)
	var enableErr error
	// 精确读到disabled=true时清除覆盖；合法查询缺少本标签时显式enable再读回，
	// 不能把缺字段直接当作false。查询失败不发送系统写操作。禁止bootstrap、bootout、
	// kickstart、load/unload；保存以后登录安排不能启动或停止当前服务/桌面。
	if queryErr == nil && (disabled[target] == nil || *disabled[target]) {
		_, enableErr = m.run(ctx, "/bin/launchctl", "enable", fmt.Sprintf("gui/%d/%s", m.env.UID, label(target)))
		if enableErr != nil {
			enableErr = model.NewError(model.CodeIO, "任务文件已保存，但清除系统disabled覆盖失败", enableErr)
		}
	}
	return m.result(ctx, saved, errors.Join(queryErr, enableErr, lock.Close()))
}

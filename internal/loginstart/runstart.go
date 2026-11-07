package loginstart

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf16"

	"github.com/orzlee/displayKvm/internal/model"
)

// 注册表句柄与命令校验隔离，使非Windows宿主也能验证精确覆盖/删除、幂等及
// 写后未知结果；测试只注入内存键，不通过系统命令修改真实用户的登录安排。
type runKey interface {
	Read(string) (string, error)
	Write(string, string) error
	Delete(string) error
	Close() error
}

type runRegistration struct {
	manager  *Manager
	open     func(bool) (runKey, error)
	lock     func() (io.Closer, error)
	profile  func(context.Context) (ServiceProfile, error)
	validate func(string, Target, bool) error
	ready    func(ServiceProfile, string) error
}

type runProgram struct {
	name, entry string
	target      Target
}

var runPrograms = [...]runProgram{
	{"displaykvm.exe", "github.com/orzlee/displayKvm/desktop", Desktop},
	{"displaykvmd.exe", "github.com/orzlee/displayKvm/cmd/displaykvmd", Service},
	{"displaykvm-cli.exe", "github.com/orzlee/displayKvm/cmd/displaykvm", ""},
}

// Go主模块路径在三个程序间可能相同，只有入口Path能区分GUI、daemon与CLI。
// 同时核对构建平台，避免登记同名但来自其它系统或架构的候选文件。
func (program runProgram) validate(entry, goos, goarch, currentArch string) error {
	if entry != program.entry || goos != "windows" || goarch != currentArch {
		return model.NewError(model.CodeInvalidConfig, "同包程序角色或构建平台不匹配："+program.name, nil)
	}
	return nil
}

func runAppPath(app string) (string, error) {
	// Run字符串不是shell脚本，但环境变量、控制字符和引号仍会使持久引用不明确。
	// 包路径只允许明确的本地文件/目录；不接受可执行文件后附加参数的伪路径。
	if !absolutePath(app) || strings.ContainsAny(app, "\x00\r\n\"%") {
		return "", model.NewError(model.CodeInvalidRequest, "Windows登录启动必须引用明确的绝对应用路径", nil)
	}
	if strings.EqualFold(filepath.Ext(app), ".exe") {
		if !strings.EqualFold(filepath.Base(app), "displaykvm.exe") {
			return "", model.NewError(model.CodeInvalidRequest, "应用入口必须为同包displaykvm.exe桌面程序", nil)
		}
		app = filepath.Dir(app)
	}
	return filepath.Join(app, "displaykvm.exe"), nil
}

// Windows命令行对引号前及末尾的反斜杠有不同转义规则；统一引用每个参数，
// 尤其不能留下未引用的Program Files可执行路径而被系统优先解释为Program.exe。
func quoteRunArgument(argument string) string {
	var quoted strings.Builder
	quoted.WriteByte('"')
	slashes := 0
	for _, character := range argument {
		if character == '\\' {
			slashes++
			continue
		}
		if character == '"' {
			quoted.WriteString(strings.Repeat("\\", slashes*2+1))
		} else {
			quoted.WriteString(strings.Repeat("\\", slashes))
		}
		slashes = 0
		quoted.WriteRune(character)
	}
	quoted.WriteString(strings.Repeat("\\", slashes*2))
	quoted.WriteByte('"')
	return quoted.String()
}

func (r *runRegistration) command(target Target, app string) (string, error) {
	if err := validTarget(target); err != nil {
		return "", err
	}
	executable, err := runAppPath(app)
	if err != nil {
		return "", err
	}
	arguments := []string{executable, "--login-start"}
	if target == Service {
		profile := r.manager.profilePath()
		if !absolutePath(profile) || strings.ContainsAny(profile, "\x00\r\n\"%") {
			return "", model.NewError(model.CodeInvalidRequest, "Windows启动档案路径不能包含不明确的命令引用", nil)
		}
		arguments = []string{filepath.Join(filepath.Dir(executable), "displaykvmd.exe"), "-startup-profile", profile}
	}
	for index := range arguments {
		arguments[index] = quoteRunArgument(arguments[index])
	}
	command := strings.Join(arguments, " ")
	// Run文档限制命令为260个字符；按UTF-16代码单元计数，提前拒绝可能被系统
	// 忽略的长路径，不能把注册表成功写入等同于下次登录能执行。
	if len(utf16.Encode([]rune(command))) > 260 {
		return "", model.NewError(model.CodeInvalidRequest, "Windows Run登录命令超过260字符限制", nil)
	}
	return command, nil
}

func (r *runRegistration) owned(target Target, command string) (string, error) {
	if !strings.HasPrefix(command, `"`) {
		return "", model.NewError(model.CodePermissionDenied, "同名Run登录项不属于本功能，拒绝覆盖或删除", nil)
	}
	end := strings.IndexByte(command[1:], '"')
	if end < 0 {
		return "", model.NewError(model.CodePermissionDenied, "Run登录项缺少完整可执行路径引用", nil)
	}
	executable := command[1 : end+1]
	app := filepath.Join(filepath.Dir(executable), "displaykvm.exe")
	want, err := r.command(target, app)
	if err != nil || command != want {
		return "", model.NewError(model.CodePermissionDenied, "同名Run登录项的程序或参数不匹配，拒绝覆盖或删除", err)
	}
	return app, nil
}

func (r *runRegistration) status(ctx context.Context) (status Status, result error) {
	status = emptyStatus()
	if err := ctx.Err(); err != nil {
		return status, err
	}
	profile, profileErr := r.profile(ctx)
	if profileErr == nil {
		status.Profile = &profile
	} else {
		status.ProfileMessage = profileErr.Error()
		if !errors.Is(profileErr, os.ErrNotExist) {
			result = profileErr
		}
	}
	key, keyErr := r.open(false)
	if keyErr == nil {
		defer func() { result = errors.Join(result, key.Close()) }()
	} else if !errors.Is(keyErr, os.ErrNotExist) {
		return status, errors.Join(result, keyErr)
	}
	for _, item := range []*ItemStatus{&status.Service, &status.Desktop} {
		command, err := "", keyErr
		if keyErr == nil {
			command, err = key.Read(label(item.Target))
		}
		if errors.Is(err, os.ErrNotExist) {
			item.Present, item.Enabled, item.Ready = boolean(false), boolean(false), boolean(false)
			item.Message = "尚未配置此登录任务"
			continue
		}
		if err == nil {
			item.AppPath, err = r.owned(item.Target, command)
		}
		if err != nil {
			item.Message = err.Error()
			result = errors.Join(result, err)
			continue
		}
		item.Present, item.Enabled = boolean(true), boolean(true)
		readyErr := r.validate(item.AppPath, item.Target, false)
		if item.Target == Service {
			if profileErr != nil {
				readyErr = errors.Join(readyErr, profileErr)
			} else if readyErr == nil {
				readyErr = r.ready(profile, filepath.Join(filepath.Dir(item.AppPath), "displaykvmd.exe"))
			}
		}
		item.Ready = boolean(readyErr == nil)
		// Run只记录下次登录命令，不提供进程加载事实；Loaded保留未知，不能
		// 把存在注册值当作当前后台正在运行，也不能为查询而启动或停止进程。
		item.Message = "已登记以后登录安排；当前进程状态由后台连接单独确认"
		if readyErr != nil {
			item.Message = "下次登录所需文件未就绪：" + readyErr.Error()
		}
	}
	return status, result
}

func (r *runRegistration) result(ctx context.Context, saved *bool, operationErr error) (Result, error) {
	status, err := r.status(ctx)
	return Result{Saved: saved, Status: status}, errors.Join(operationErr, err)
}

func (r *runRegistration) change(ctx context.Context, target Target, app string, enable bool) (result Result, resultErr error) {
	if err := validTarget(target); err != nil {
		return r.result(ctx, boolean(false), err)
	}
	if err := ctx.Err(); err != nil {
		return r.result(ctx, boolean(false), err)
	}
	want := ""
	if enable {
		var err error
		want, err = r.command(target, app)
		if err == nil {
			app, err = runAppPath(app)
		}
		if err == nil {
			err = r.validate(app, target, true)
		}
		if err != nil {
			return r.result(ctx, boolean(false), err)
		}
	}
	lock, err := r.lock()
	if err != nil {
		return r.result(ctx, boolean(false), err)
	}
	defer func() { resultErr = errors.Join(resultErr, lock.Close()) }()
	if err := ctx.Err(); err != nil {
		return r.result(ctx, boolean(false), err)
	}
	if enable && target == Service {
		// 选择档案也使用同一写锁。取得锁后再读取及检查就绪，防止刚检查旧档案
		// 就被另一个保存操作切换来源，随后却为未经核验的新档案登记登录启动。
		profile, err := r.profile(ctx)
		if err == nil {
			err = r.ready(profile, filepath.Join(filepath.Dir(app), "displaykvmd.exe"))
		}
		if err != nil {
			return r.result(ctx, boolean(false), err)
		}
	}
	key, err := r.open(enable)
	if !enable && errors.Is(err, os.ErrNotExist) {
		return r.result(ctx, boolean(true), nil)
	}
	if err != nil {
		return r.result(ctx, boolean(false), err)
	}
	defer func() {
		if key != nil {
			resultErr = errors.Join(resultErr, key.Close())
		}
	}()
	before, readErr := key.Read(label(target))
	existed := readErr == nil
	if existed {
		_, readErr = r.owned(target, before)
	}
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return r.result(ctx, boolean(false), readErr)
	}
	if enable && existed && before == want || !enable && !existed {
		return r.result(ctx, boolean(true), nil)
	}
	if !enable {
		// 查询句柄只有读权限；仅确认本应用确实有待删除项后再申请写权限，
		// 重读防止权限切换期间的外部改写被误删。缺项关闭不创建Run键。
		if err := key.Close(); err != nil {
			return r.result(ctx, boolean(false), err)
		}
		key, err = r.open(true)
		if err != nil {
			key = nil
			return r.result(ctx, boolean(false), err)
		}
		current, err := key.Read(label(target))
		if err != nil || current != before {
			return r.result(ctx, boolean(false), model.NewError(model.CodePermissionDenied, "Run登录项在删除前已改变", err))
		}
	}
	if err := ctx.Err(); err != nil {
		return r.result(ctx, boolean(false), err)
	}
	var writeErr error
	if enable {
		writeErr = key.Write(label(target), want)
	} else {
		writeErr = key.Delete(label(target))
	}
	// Windows注册表没有跨进程值比较替换；应用写者由档案锁串行化，外部改写
	// 仍须读回辨认。系统调用报错也可能已经提交，不能直接报告false诱发重试。
	after, checkErr := key.Read(label(target))
	saved := (*bool)(nil)
	switch {
	case enable && checkErr == nil && after == want, !enable && errors.Is(checkErr, os.ErrNotExist):
		saved = boolean(true)
	case checkErr == nil && existed && after == before, errors.Is(checkErr, os.ErrNotExist) && !existed:
		saved = boolean(false)
	}
	if saved == nil || !*saved && writeErr == nil {
		writeErr = errors.Join(writeErr, model.NewError(model.CodeIO, "Run登录项提交后无法核实预期结果", checkErr))
	}
	return r.result(ctx, saved, writeErr)
}

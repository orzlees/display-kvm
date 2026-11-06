package startup_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"testing"

	"github.com/orzlee/displayKvm/desktop/views/startup"
	"github.com/orzlee/displayKvm/internal/loginstart"
)

func boolean(value bool) *bool { return &value }

type managerStub struct {
	profile           loginstart.ServiceProfile
	profileErr        error
	status            loginstart.Status
	statusErr         error
	calls             []string
	saveErr           error
	saveProfileResult *loginstart.Result
	desktopErr        error
	lastAppPath       string
}

func newManager() *managerStub {
	return &managerStub{
		profileErr: fmt.Errorf("没有启动参数：%w", os.ErrNotExist),
		status: loginstart.Status{
			Service: loginstart.ItemStatus{Enabled: boolean(false), Ready: boolean(true)},
			Desktop: loginstart.ItemStatus{Enabled: boolean(false), Ready: boolean(true)},
		},
	}
}

func (m *managerStub) Profile(context.Context) (loginstart.ServiceProfile, error) {
	return m.profile, m.profileErr
}

func (m *managerStub) Status(context.Context) (loginstart.Status, error) {
	return m.status, m.statusErr
}

func (m *managerStub) SaveProfile(_ context.Context, profile loginstart.ServiceProfile) (loginstart.Result, error) {
	m.calls = append(m.calls, "profile")
	if m.saveProfileResult != nil {
		return *m.saveProfileResult, m.saveErr
	}
	if m.saveErr != nil {
		return loginstart.Result{Saved: boolean(false), Status: m.status}, m.saveErr
	}
	m.profile, m.profileErr = profile, nil
	return loginstart.Result{Saved: boolean(true), Status: m.status}, m.statusErr
}

func (m *managerStub) Enable(_ context.Context, target loginstart.Target, appPath string) (loginstart.Result, error) {
	m.calls = append(m.calls, "enable:"+string(target))
	m.lastAppPath = appPath
	if target == loginstart.Desktop && m.desktopErr != nil {
		return loginstart.Result{Saved: boolean(false), Status: m.status}, m.desktopErr
	}
	if target == loginstart.Service {
		m.status.Service.Enabled = boolean(true)
	} else {
		m.status.Desktop.Enabled = boolean(true)
	}
	return loginstart.Result{Saved: boolean(true), Status: m.status}, m.statusErr
}

func (m *managerStub) Disable(_ context.Context, target loginstart.Target) (loginstart.Result, error) {
	m.calls = append(m.calls, "disable:"+string(target))
	if target == loginstart.Service {
		m.status.Service.Enabled = boolean(false)
	} else {
		m.status.Desktop.Enabled = boolean(false)
	}
	return loginstart.Result{Saved: boolean(true), Status: m.status}, m.statusErr
}

func TestUnifiedSettingsDialogsUseCanceledWithoutSemanticInversion(t *testing.T) {
	for _, canceled := range []bool{true, false} {
		picker := startup.FileDialogs{Open: func(_ context.Context, kind string) (string, bool, error) {
			if kind != "config" {
				t.Errorf("桥传递了错误的文件种类：%s", kind)
			}
			return "/selected/config.json", canceled, nil
		}}
		_, actual, err := picker.Open(context.Background(), "config")
		if err != nil || actual != canceled {
			t.Fatalf("取消语义不能反转：canceled=%v actual=%v err=%v", canceled, actual, err)
		}
	}
}

func TestAdapterDistinguishesMissingAndUnreadableProfile(t *testing.T) {
	manager := newManager()
	backend := startup.NewBackend(manager, nil, "/Applications/DisplayKvm.app")
	state, err := backend.Read(context.Background())
	if err != nil || !state.ConfigKnown || state.ProfileExists {
		t.Fatalf("首次缺少文件必须保持可配置：state=%+v err=%v", state, err)
	}
	manager.profileErr = errors.New("启动参数文件无权读取")
	state, err = backend.Read(context.Background())
	if err == nil || state.ConfigKnown || state.ConfigError == "" {
		t.Fatalf("读取损坏不能伪装成首次空白设置：state=%+v err=%v", state, err)
	}
}

func TestAdapterDesktopOnlyDoesNotSaveOrRequireProfile(t *testing.T) {
	manager := newManager()
	backend := startup.NewBackend(manager, nil, "/Applications/DisplayKvm.app")
	state, _ := backend.Read(context.Background())
	result, err := backend.Save(context.Background(), startup.SaveRequest{DesktopEnabled: boolean(true), Expected: state})
	if err != nil || !result.Desktop.Succeeded || !reflect.DeepEqual(manager.calls, []string{"enable:desktop"}) {
		t.Fatalf("仅桌面项不应依赖服务参数：calls=%v result=%+v err=%v", manager.calls, result, err)
	}
	if manager.lastAppPath != "/Applications/DisplayKvm.app" {
		t.Fatal("应把宿主提供的安装位置交给核心校验，不自行复制或猜测安装目录")
	}
}

func TestAdapterDisableDoesNotResaveHistoricalProfile(t *testing.T) {
	manager := newManager()
	manager.profileErr = nil
	manager.profile = loginstart.ServiceProfile{Version: 2, ConfigPath: "old-bad-path"}
	manager.status.Service.Enabled = boolean(true)
	backend := startup.NewBackend(manager, nil, "/Applications/DisplayKvm.app")
	state, _ := backend.Read(context.Background())
	result, err := backend.Save(context.Background(), startup.SaveRequest{ServiceEnabled: boolean(false), Expected: state})
	if err != nil || !result.Service.Succeeded || !reflect.DeepEqual(manager.calls, []string{"disable:service"}) {
		t.Fatalf("单项禁用不得校验或重写旧参数：calls=%v result=%+v err=%v", manager.calls, result, err)
	}
}

func TestAdapterSavesProfileBeforeEnablingService(t *testing.T) {
	manager := newManager()
	backend := startup.NewBackend(manager, nil, "/Applications/DisplayKvm.app")
	state, _ := backend.Read(context.Background())
	path := "/isolated/settings.json"
	result, err := backend.Save(context.Background(), startup.SaveRequest{ConfigPath: &path, ServiceEnabled: boolean(true), Expected: state})
	if err != nil || !result.Configuration.Succeeded || !result.Service.Succeeded || !reflect.DeepEqual(manager.calls, []string{"profile", "enable:service"}) {
		t.Fatalf("服务注册必须发生在参数保存成功后：calls=%v result=%+v err=%v", manager.calls, result, err)
	}
	if manager.profile.ConfigPath != path || manager.profile.Version != 2 {
		t.Fatalf("参数映射不完整：%+v", manager.profile)
	}
}

func TestAdapterProfileFailureStillAllowsIndependentDesktopOutcome(t *testing.T) {
	manager := newManager()
	manager.saveErr = errors.New("配置文件无权读取")
	backend := startup.NewBackend(manager, nil, "/Applications/DisplayKvm.app")
	state, _ := backend.Read(context.Background())
	path := "/isolated/settings.json"
	result, err := backend.Save(context.Background(), startup.SaveRequest{ConfigPath: &path, ServiceEnabled: boolean(true), DesktopEnabled: boolean(true), Expected: state})
	if err != nil || result.Configuration.Succeeded || result.Service.Succeeded || !result.Desktop.Succeeded || !reflect.DeepEqual(manager.calls, []string{"profile", "enable:desktop"}) {
		t.Fatalf("参数失败应阻止服务启用但不撤销桌面结果：calls=%v result=%+v err=%v", manager.calls, result, err)
	}
}

func TestUnifiedSettingsConfirmedProfileWithStatusErrorStillReachesServiceEnable(t *testing.T) {
	manager := newManager()
	statusErr := errors.New("桌面登录任务加载状态查询失败")
	manager.statusErr = statusErr
	manager.status.Desktop.Enabled = boolean(true)
	manager.status.Desktop.Loaded = nil
	manager.status.Desktop.Message = statusErr.Error()
	backend := startup.NewBackend(manager, nil, "/Applications/DisplayKvm.app")
	state, err := backend.Read(context.Background())
	if !errors.Is(err, statusErr) || !state.Service.Known || state.Service.Ready == nil || !*state.Service.Ready {
		t.Fatalf("需要真实保留汇总错误与可信服务分项：state=%+v err=%v", state, err)
	}
	path := "/isolated/settings.json"
	// 核心在档案写入并读回后仍可能因另一项状态查询返回错误；不能把该错误
	// 当作档案未保存，或阻断显式服务请求进入核心自己的再次校验。
	result, err := backend.Save(context.Background(), startup.SaveRequest{ConfigPath: &path, ServiceEnabled: boolean(true), Expected: state})
	if err != nil || !result.Configuration.Attempted || !result.Configuration.Succeeded || result.Configuration.Unknown || result.Configuration.Message != statusErr.Error() {
		t.Fatalf("确认写入的证据和无关状态错误都应保留：result=%+v err=%v", result, err)
	}
	if !result.Service.Attempted || !result.Service.Succeeded || result.Service.Unknown || !reflect.DeepEqual(manager.calls, []string{"profile", "enable:service"}) {
		t.Fatalf("可信服务应独立进入Enable，不修改未请求的桌面项：calls=%v result=%+v", manager.calls, result)
	}
	if manager.profile.Version != 2 || manager.profile.ConfigPath != path || manager.status.Desktop.Enabled == nil || !*manager.status.Desktop.Enabled {
		t.Fatal("已确认档案应保留实际选择，未请求的桌面启动项必须保持原样")
	}
}

func TestSettingsVersionTracksEditableValuesRatherThanDocumentReadiness(t *testing.T) {
	manager := newManager()
	backend := startup.NewBackend(manager, nil, "/Applications/DisplayKvm.app")
	before, err := backend.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	manager.status.Service.Ready = boolean(false)
	manager.status.Service.Message = "配置文件刚保存，程序可用性重新确认中"
	after, err := backend.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if before.Version != after.Version {
		t.Fatal("本页保存文档导致的可用性变化不能把同一次整页保存误判成外部设置修改")
	}
	manager.status.Desktop.Enabled = boolean(true)
	changed, err := backend.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if changed.Version == after.Version {
		t.Fatal("外部实际修改开关仍必须改变表单基线")
	}
}

func TestUnifiedSettingsUnconfirmedProfileBlocksServiceButAllowsTrustedDesktopDisable(t *testing.T) {
	for _, test := range []struct {
		name    string
		saved   *bool
		unknown bool
	}{
		{name: "保存未知", saved: nil, unknown: true},
		{name: "确认未保存", saved: boolean(false)},
	} {
		t.Run(test.name, func(t *testing.T) {
			manager := newManager()
			manager.status.Desktop.Enabled = boolean(true)
			manager.saveErr = errors.New("服务启动选择" + test.name)
			manager.saveProfileResult = &loginstart.Result{Saved: test.saved, Status: manager.status}
			backend := startup.NewBackend(manager, nil, "/Applications/DisplayKvm.app")
			state, err := backend.Read(context.Background())
			if err != nil || !state.Desktop.Known || !state.Desktop.Enabled {
				t.Fatalf("独立关闭桌面项的前提必须可信：state=%+v err=%v", state, err)
			}
			path := "/isolated/settings.json"
			// nil表示保存证据未知，false表示确认未保存；二者都不能启用服务，
			// 但取消可信桌面项不依赖服务档案，不能被连带封锁或回滚。
			result, err := backend.Save(context.Background(), startup.SaveRequest{ConfigPath: &path, ServiceEnabled: boolean(true), DesktopEnabled: boolean(false), Expected: state})
			if err != nil || !result.Configuration.Attempted || result.Configuration.Succeeded || result.Configuration.Unknown != test.unknown || result.Configuration.Message != manager.saveErr.Error() {
				t.Fatalf("未知与确认失败必须分别报告且保留原因：result=%+v err=%v", result, err)
			}
			if !result.Service.Attempted || result.Service.Succeeded || result.Service.Message == "" || !result.Desktop.Attempted || !result.Desktop.Succeeded || result.Desktop.Unknown {
				t.Fatalf("服务不得启用，可信桌面关闭应独立成功：result=%+v", result)
			}
			if !reflect.DeepEqual(manager.calls, []string{"profile", "disable:desktop"}) || manager.status.Service.Enabled == nil || *manager.status.Service.Enabled || manager.status.Desktop.Enabled == nil || *manager.status.Desktop.Enabled {
				t.Fatalf("不得调用服务Enable或重发，桌面应实际关闭：calls=%v status=%+v", manager.calls, manager.status)
			}
		})
	}
}

func TestAdapterIncompleteProfileIsNotSubmittedToCore(t *testing.T) {
	manager := newManager()
	backend := startup.NewBackend(manager, nil, "/Applications/DisplayKvm.app")
	state, _ := backend.Read(context.Background())
	path := "relative/settings.json"
	result, err := backend.Save(context.Background(), startup.SaveRequest{ConfigPath: &path, DesktopEnabled: boolean(true), Expected: state})
	if err != nil || len(result.Problems) == 0 || !result.Desktop.Succeeded || !reflect.DeepEqual(manager.calls, []string{"enable:desktop"}) {
		t.Fatalf("不完整服务参数应保留草稿，只处理独立桌面项：calls=%v result=%+v err=%v", manager.calls, result, err)
	}
}

func TestAdapterUnknownStateIsNeverTreatedAsDisabled(t *testing.T) {
	manager := newManager()
	manager.status.Service.Enabled = nil
	manager.status.Service.Message = "无法读取服务启动项"
	backend := startup.NewBackend(manager, nil, "/Applications/DisplayKvm.app")
	state, _ := backend.Read(context.Background())
	if state.Service.Known || !state.Desktop.Known {
		t.Fatalf("nil状态必须分项保留未知：%+v", state)
	}
	result, err := backend.Save(context.Background(), startup.SaveRequest{ServiceEnabled: boolean(false), DesktopEnabled: boolean(true), Expected: state})
	if err != nil || result.Service.Succeeded || !result.Desktop.Succeeded || !reflect.DeepEqual(manager.calls, []string{"enable:desktop"}) {
		t.Fatalf("不能覆盖未知服务项，已知桌面项仍独立保存：calls=%v result=%+v err=%v", manager.calls, result, err)
	}
}

func TestAdapterRejectsStaleSnapshotBeforeFirstWrite(t *testing.T) {
	manager := newManager()
	backend := startup.NewBackend(manager, nil, "/Applications/DisplayKvm.app")
	state, _ := backend.Read(context.Background())
	manager.status.Desktop.Enabled = boolean(true)
	result, err := backend.Save(context.Background(), startup.SaveRequest{DesktopEnabled: boolean(true), Expected: state})
	if err != nil || len(result.Problems) == 0 || len(manager.calls) != 0 {
		t.Fatalf("旧页面不能覆盖刚由CLI修改的设置：calls=%v result=%+v err=%v", manager.calls, result, err)
	}
}

func TestAdapterReadErrorDoesNotWriteOrRetry(t *testing.T) {
	manager := newManager()
	backend := startup.NewBackend(manager, nil, "/Applications/DisplayKvm.app")
	state, _ := backend.Read(context.Background())
	manager.statusErr = errors.New("当前平台不支持登录启动")
	manager.status = loginstart.Status{}
	result, err := backend.Save(context.Background(), startup.SaveRequest{DesktopEnabled: boolean(true), Expected: state})
	if err != nil || result.Desktop.Succeeded || len(result.Problems) == 0 && result.Desktop.Message == "" || len(manager.calls) != 0 {
		t.Fatalf("读取或平台错误必须禁写：calls=%v result=%+v err=%v", manager.calls, result, err)
	}
}

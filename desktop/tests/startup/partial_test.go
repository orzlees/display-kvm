package startup_test

import (
	"context"
	"errors"
	"html"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/orzlee/displayKvm/desktop/views/startup"
	"github.com/orzlee/displayKvm/internal/loginstart"
)

func corruptProfileWithTrustedDesktop() *managerStub {
	manager := newManager()
	manager.profileErr = errors.New("服务启动参数文件已经损坏")
	manager.statusErr = errors.Join(manager.profileErr, errors.New("服务启动项无法读取"))
	manager.status.Service = loginstart.ItemStatus{Message: "服务启动项无法读取"}
	manager.status.Desktop.Enabled = boolean(true)
	return manager
}

func TestAdapterPartialStatusAndCorruptProfileCanDisableDesktop(t *testing.T) {
	manager := corruptProfileWithTrustedDesktop()
	backend := startup.NewBackend(manager, nil, "/Applications/DisplayKvm.app")
	state, readErr := backend.Read(context.Background())
	if readErr == nil || state.ConfigKnown || state.Service.Known || !state.Desktop.Known || !state.Desktop.Enabled {
		t.Fatalf("汇总错误不得丢弃可信桌面状态：state=%+v err=%v", state, readErr)
	}
	result, err := backend.Save(context.Background(), startup.SaveRequest{DesktopEnabled: boolean(false), Expected: state})
	if err != nil || !result.Desktop.Succeeded || result.Configuration.Attempted || result.Service.Attempted || !reflect.DeepEqual(manager.calls, []string{"disable:desktop"}) {
		t.Fatalf("坏服务参数不得阻断关闭桌面：calls=%v result=%+v err=%v", manager.calls, result, err)
	}
}

func TestAdapterForeignServiceItemDoesNotBlockDesktopEnable(t *testing.T) {
	manager := newManager()
	manager.statusErr = errors.New("服务登录任务不属于此应用")
	manager.status.Service = loginstart.ItemStatus{Message: manager.statusErr.Error()}
	backend := startup.NewBackend(manager, nil, "/Applications/DisplayKvm.app")
	state, readErr := backend.Read(context.Background())
	if readErr == nil || state.Service.Known || !state.Desktop.Known {
		t.Fatalf("应保留单项未知和桌面可信状态：state=%+v err=%v", state, readErr)
	}
	result, err := backend.Save(context.Background(), startup.SaveRequest{DesktopEnabled: boolean(true), Expected: state})
	if err != nil || !result.Desktop.Succeeded || !reflect.DeepEqual(manager.calls, []string{"enable:desktop"}) {
		t.Fatalf("坏服务任务不得阻断桌面独立设置：calls=%v result=%+v err=%v", manager.calls, result, err)
	}
}

func TestAdapterLoadedQueryFailureDoesNotEraseKnownEnabledState(t *testing.T) {
	manager := newManager()
	manager.statusErr = errors.New("服务加载状态查询失败")
	manager.status.Service.Enabled = boolean(true)
	manager.status.Service.Ready = boolean(false)
	manager.status.Service.Loaded = nil
	manager.status.Service.Message = manager.statusErr.Error()
	backend := startup.NewBackend(manager, nil, "/Applications/DisplayKvm.app")
	state, readErr := backend.Read(context.Background())
	if readErr == nil || !state.Service.Known || !state.Service.Enabled {
		t.Fatalf("Loaded未知不能抹掉Enabled证据：state=%+v err=%v", state, readErr)
	}
	result, err := backend.Save(context.Background(), startup.SaveRequest{ServiceEnabled: boolean(false), Expected: state})
	if err != nil || !result.Service.Succeeded || !reflect.DeepEqual(manager.calls, []string{"disable:service"}) {
		t.Fatalf("可信启用状态应允许交由核心安全关闭：calls=%v result=%+v err=%v", manager.calls, result, err)
	}
}

func TestAdapterPartialStatusNeverTurnsUnknownItemIntoDisable(t *testing.T) {
	manager := corruptProfileWithTrustedDesktop()
	backend := startup.NewBackend(manager, nil, "/Applications/DisplayKvm.app")
	state, _ := backend.Read(context.Background())
	result, err := backend.Save(context.Background(), startup.SaveRequest{
		ServiceEnabled: boolean(false), DesktopEnabled: boolean(false), Expected: state,
	})
	if err != nil || result.Service.Succeeded || !result.Desktop.Succeeded || !reflect.DeepEqual(manager.calls, []string{"disable:desktop"}) {
		t.Fatalf("未知服务项不得变成关闭指令：calls=%v result=%+v err=%v", manager.calls, result, err)
	}
}

func TestAdapterCorruptProfileCannotBeOverwrittenOrEnableService(t *testing.T) {
	manager := corruptProfileWithTrustedDesktop()
	manager.status.Service.Enabled = boolean(false)
	backend := startup.NewBackend(manager, nil, "/Applications/DisplayKvm.app")
	state, _ := backend.Read(context.Background())
	path := "/draft/settings.json"
	result, err := backend.Save(context.Background(), startup.SaveRequest{ConfigPath: &path, ServiceEnabled: boolean(true), Expected: state})
	if err != nil || result.Configuration.Succeeded || result.Service.Succeeded || len(manager.calls) != 0 || len(result.Problems) == 0 {
		t.Fatalf("参数损坏时必须拒绝覆盖和服务启用：calls=%v result=%+v err=%v", manager.calls, result, err)
	}
}

func TestHandlerPartialReadAllowsTrustedDesktopDisableWithoutProfileWrite(t *testing.T) {
	manager := corruptProfileWithTrustedDesktop()
	handler := newChineseStartupHandler(t, startup.NewBackend(manager, nil, "/Applications/DisplayKvm.app"), startup.FileDialogs{})
	response := get(t, handler)
	requireText(t, response, "服务启动参数文件已经损坏")
	requireStartupItemText(t, response, "desktop_enabled-actual", "开机启动桌面客户端", "已开启")
	requireStartupItemText(t, response, "service_enabled-actual", "开机启动服务", "无法读取状态")
	values := form(t, response)
	values.Del("language")
	response = post(handler, values)
	if response.Code != http.StatusOK || !reflect.DeepEqual(manager.calls, []string{"disable:desktop"}) {
		t.Fatalf("部分读取错误后表单未实际关闭可信桌面项：status=%d calls=%v", response.Code, manager.calls)
	}
	requireStartupItemText(t, response, "desktop_enabled-actual", "开机启动桌面客户端", "已关闭")
	requireStartupItemText(t, response, "startup-results", "开机启动桌面客户端", "已保存")
	requireText(t, response, "服务配置：无法读取")
	response = get(t, handler)
	response = post(handler, form(t, response))
	if response.Code != http.StatusOK || !reflect.DeepEqual(manager.calls, []string{"disable:desktop"}) {
		t.Fatalf("无关读取错误不能导致已完成关闭操作重发：status=%d calls=%v", response.Code, manager.calls)
	}
}

func TestHandlerForeignServiceItemStillAllowsDesktopEnable(t *testing.T) {
	manager := newManager()
	manager.statusErr = errors.New("服务登录任务来源无法确认")
	manager.status.Service = loginstart.ItemStatus{Message: manager.statusErr.Error()}
	handler := newChineseStartupHandler(t, startup.NewBackend(manager, nil, "/Applications/DisplayKvm.app"), startup.FileDialogs{})
	values := form(t, get(t, handler))
	values.Set("desktop_enabled", "1")
	response := post(handler, values)
	if response.Code != http.StatusOK || !reflect.DeepEqual(manager.calls, []string{"enable:desktop"}) {
		t.Fatalf("服务任务错误不得封锁可信桌面开关：status=%d calls=%v", response.Code, manager.calls)
	}
	requireStartupItemText(t, response, "service_enabled-actual", "开机启动服务", "无法读取状态")
	requireStartupItemText(t, response, "desktop_enabled-actual", "开机启动桌面客户端", "已开启")
}

// 原位翻译会用 span 分隔名称与状态，断言可见文字不能依赖这些 DOM 边界或冒号样式。
// 实际状态限定在对应开关节点，保存结果限定在结果列表，并核对完整名称与状态，
// 避免把另一分项的“已开启”或“已保存”误认成当前分项的结果。
func requireStartupItemText(t *testing.T, response *httptest.ResponseRecorder, region, label, status string) {
	t.Helper()
	body := response.Body.String()
	pattern := `(?s)<p\b[^>]*\bid="` + regexp.QuoteMeta(region) + `"[^>]*>(.*?)</p>`
	if region == "startup-results" {
		match := regexp.MustCompile(`(?s)<ul\b[^>]*\bclass="startup-results"[^>]*>(.*?)</ul>`).FindStringSubmatch(body)
		if len(match) != 2 {
			t.Errorf("页面没有分项保存结果列表；状态码 %d", response.Code)
			return
		}
		body = match[1]
		pattern = `(?s)<li\b[^>]*>(.*?)</li>`
	}
	for _, match := range regexp.MustCompile(pattern).FindAllStringSubmatch(body, -1) {
		text := startupVisibleText(match[1])
		item, value, found := strings.Cut(strings.ReplaceAll(text, "：", ":"), ":")
		if !found {
			continue
		}
		if region == "startup-results" {
			value, _, _ = strings.Cut(value, "·")
		}
		if strings.Join(strings.Fields(item), " ") == label && strings.Join(strings.Fields(value), " ") == status {
			return
		}
	}
	t.Errorf("页面 %s 缺少 %q 对应的 %q；状态码 %d", region, label, status, response.Code)
}

// 标题、按钮和分项状态共用可见文字规则，避免原位翻译的包装标记改变断言结果。
func startupVisibleText(markup string) string {
	text := html.UnescapeString(regexp.MustCompile(`<[^>]+>`).ReplaceAllString(markup, ""))
	return strings.Join(strings.Fields(text), " ")
}

func TestHandlerCorruptProfileExplicitEditCannotOverwriteOrEnableService(t *testing.T) {
	manager := corruptProfileWithTrustedDesktop()
	manager.status.Service.Enabled = boolean(false)
	handler := newChineseStartupHandler(t, startup.NewBackend(manager, nil, "/Applications/DisplayKvm.app"), startup.FileDialogs{})
	values := form(t, get(t, handler))
	values.Set("service_enabled", "1")
	values.Set("desktop_enabled", "1")
	response := post(handler, values)
	if response.Code != http.StatusConflict || len(manager.calls) != 0 {
		t.Fatalf("损坏参数不应通过显式提交绕过保护：status=%d calls=%v", response.Code, manager.calls)
	}
	requireText(t, response, "草稿已保留")
}

func TestHandlerUnknownPartialItemIsNotSubmittedWhenDesktopChanges(t *testing.T) {
	state := initialState()
	state.ConfigKnown = false
	state.ConfigError = "启动参数无法读取"
	state.Service.Known = false
	state.Service.Error = "服务状态未知"
	state.Desktop.Enabled = true
	backend := &memoryBackend{state: state, readErr: errors.New("部分启动设置未能读取")}
	handler := newChineseStartupHandler(t, backend, startup.FileDialogs{})
	response := post(handler, form(t, get(t, handler)))
	if response.Code != http.StatusOK || len(backend.saves) != 1 {
		t.Fatalf("应允许可信桌面项关闭：status=%d saves=%d", response.Code, len(backend.saves))
	}
	request := backend.saves[0]
	if request.ConfigPath != nil || request.ServiceEnabled != nil || request.DesktopEnabled == nil || *request.DesktopEnabled {
		t.Fatalf("未知项不得被当作关闭或空白参数提交：%+v", request)
	}
}

func TestDirtyRefreshDoesNotSubmitTemporarilyUnknownLoginItem(t *testing.T) {
	for _, frozen := range []loginstart.Target{loginstart.Service, loginstart.Desktop} {
		t.Run(string(frozen), func(t *testing.T) {
			manager := newManager()
			manager.profileErr = nil
			manager.profile = loginstart.ServiceProfile{Version: 2, ConfigPath: "/saved/config.json"}
			originalProfile := manager.profile
			frozenItem, edited := &manager.status.Service, loginstart.Desktop
			if frozen == loginstart.Desktop {
				frozenItem, edited = &manager.status.Desktop, loginstart.Service
			}
			frozenItem.Enabled = boolean(true)
			originalItem := *frozenItem
			frozenField, editedField := string(frozen)+"_enabled", string(edited)+"_enabled"
			handler := newChineseStartupHandler(t, startup.NewBackend(manager, nil, "/Applications/DisplayKvm.app"), startup.FileDialogs{Open: func(context.Context, string) (string, bool, error) {
				return "", true, nil
			}})

			// 用正常选文件后取消的页面动作保留另一项开关草稿，实际参数和系统
			// 都不写入；随后GET必须保留草稿和旧版本基准，才覆盖真实误禁路径。
			response := get(t, handler)
			values := form(t, response)
			values.Set("action", "choose_config")
			values.Set(frozenField, "1")
			values.Set(editedField, "1")
			response = post(handler, values)
			if response.Code != http.StatusOK || len(manager.calls) != 0 {
				t.Fatalf("准备草稿不得保存启动设置：status=%d calls=%v", response.Code, manager.calls)
			}
			requireText(t, response, `data-dirty="true"`)

			manager.statusErr = errors.New("此启动项状态暂时无法确认")
			*frozenItem = loginstart.ItemStatus{Message: manager.statusErr.Error()}
			response = get(t, handler)
			requireText(t, response, manager.statusErr.Error())
			requireText(t, response, `data-dirty="true"`)
			requireText(t, response, `name="`+frozenField+`" value="1" checked disabled`)
			requireText(t, response, `name="`+editedField+`" value="1" checked`)

			// 在POST前恢复完整原状态，让核心版本再次等于旧基准。浏览器不会
			// 提交已禁用的开关，服务端必须沿已呈现状态忽略该项，不能将省略
			// 字段解释为false；只靠保存前重新查询会重现错误的Disable操作。
			*frozenItem, manager.statusErr = originalItem, nil
			values = form(t, response)
			values.Set(editedField, "1")
			response = post(handler, values)
			if response.Code != http.StatusOK || !reflect.DeepEqual(manager.calls, []string{"enable:" + string(edited)}) {
				t.Fatalf("只应保存呈现时可信的另一项：status=%d calls=%v", response.Code, manager.calls)
			}
			if frozenItem.Enabled == nil || !*frozenItem.Enabled || !reflect.DeepEqual(manager.profile, originalProfile) {
				t.Fatalf("未知期间的原启动项和服务参数必须保持原样：item=%+v profile=%+v", *frozenItem, manager.profile)
			}
			requireText(t, response, `name="`+frozenField+`" value="1" checked`)
			requireText(t, response, `name="`+editedField+`" value="1" checked`)
		})
	}
}

package startup_test

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/orzlee/displayKvm/desktop/views/startup"
	"github.com/orzlee/displayKvm/internal/settings"
)

func unifiedSnapshot(path, revision string) settings.Snapshot {
	return settings.Snapshot{Path: path, Document: settings.NewDocument(), Revision: settings.Revision(revision)}
}

func TestUnifiedSettingsNewCanceledOrFailedDoesNotChangePosition(t *testing.T) {
	for _, canceled := range []bool{true, false} {
		t.Run(map[bool]string{true: "取消", false: "失败"}[canceled], func(t *testing.T) {
			backend := &memoryBackend{state: initialState(), write: func(documentWrite) (settings.SaveResult, error) {
				return settings.SaveResult{Saved: boolean(false)}, errors.New("无法保存文件")
			}}
			handler := newChineseStartupHandler(t, backend, startup.FileDialogs{Save: func(_ context.Context, suggested string) (string, bool, error) {
				if !strings.HasSuffix(suggested, "settings.json") {
					t.Errorf("缺少建议文件名：%s", suggested)
				}
				return "/isolated/new.json", canceled, nil
			}})
			values := form(t, get(t, handler))
			values.Set("action", "new_config")
			response := post(handler, values)
			requireText(t, response, "尚未选择配置文件")
			if len(backend.saves) != 0 || (canceled && len(backend.writes) != 0) {
				t.Fatal("取消或文档写失败不能保存启动选择")
			}
		})
	}
}

func TestUnifiedSettingsNewAllowsBundledToolAndSavesSelectionOnlyOnExplicitSave(t *testing.T) {
	backend := &memoryBackend{state: initialState()}
	handler := newChineseStartupHandler(t, backend, startup.FileDialogs{Save: func(context.Context, string) (string, bool, error) { return "/isolated/new.json", false, nil }})
	values := form(t, get(t, handler))
	values.Set("action", "new_config")
	response := post(handler, values)
	if response.Code != http.StatusOK || len(backend.writes) != 1 || len(backend.saves) != 0 {
		t.Fatal("新建仅保存合法文档，不自动选择启动或开启服务")
	}
	if err := backend.writes[0].document.Validate(); err != nil {
		t.Fatal(err)
	}
	if backend.writes[0].document.Runtime.ToolPath != "" {
		t.Fatal("包内工具位置不能作为安装绝对路径写入完整文档")
	}
	requireText(t, response, "/isolated/new.json")
	requireText(t, response, `name="language"`)
	values = form(t, response)
	values.Set("service_enabled", "1")
	response = post(handler, values)
	if response.Code != http.StatusOK || len(backend.writes) != 1 || len(backend.saves) != 1 || !reflect.DeepEqual(backend.events, []string{"document", "settings"}) {
		t.Fatal("空工具路径的完整文档合法，显式保存必须先确认文档再保存选择和开关")
	}
	request := backend.saves[0]
	if request.ConfigPath == nil || *request.ConfigPath != "/isolated/new.json" || request.ServiceEnabled == nil || !*request.ServiceEnabled || request.DesktopEnabled != nil {
		t.Fatalf("包内工具的运行就绪校验应交给后台启用操作：%+v", request)
	}
}

func TestUnifiedSettingsLoadIsReadOnlyAndBadLoadKeepsDraft(t *testing.T) {
	backend := &memoryBackend{state: initialState(), documents: map[string]settings.Snapshot{"/isolated/good.json": unifiedSnapshot("/isolated/good.json", "r1")}, documentErrors: map[string]error{"/isolated/bad.json": errors.New("配置文件格式无法识别")}}
	paths := []string{"/isolated/good.json", "/isolated/bad.json", ""}
	calls := 0
	handler := newChineseStartupHandler(t, backend, startup.FileDialogs{Open: func(context.Context, string) (string, bool, error) {
		p := paths[calls]
		calls++
		return p, p == "", nil
	}})
	response := get(t, handler)
	for index := 0; index < 3; index++ {
		values := form(t, response)
		values.Set("action", "choose_config")
		values.Set("language", "zh-CN")
		response = post(handler, values)
		if response.Header().Get("X-Settings-Request-Revision") != values.Get("revision") || form(t, response).Get("revision") == values.Get("revision") {
			t.Fatal("加载、失败和取消均应消费本次表单版本，防止旧动作重复派发")
		}
	}
	if len(backend.writes) != 0 || len(backend.saves) != 0 || calls != 3 {
		t.Fatal("加载、失败和取消均不得写文件或启动项")
	}
	requireText(t, response, "/isolated/good.json")
	if form(t, response).Get("language") != "zh-CN" {
		t.Fatal("加载失败或取消不能丢弃当前语言草稿")
	}
}

func TestUnifiedSettingsSaveAsUsesDraftAndRequiresExplicitValidOverwrite(t *testing.T) {
	backend := &memoryBackend{state: initialState(), documents: map[string]settings.Snapshot{"/isolated/existing.json": unifiedSnapshot("/isolated/existing.json", "original")}}
	handler := newChineseStartupHandler(t, backend, startup.FileDialogs{Save: func(context.Context, string) (string, bool, error) { return "/isolated/existing.json", false, nil }})
	values := form(t, get(t, handler))
	values.Set("action", "save_as")
	values.Set("language", "zh-CN")
	response := post(handler, values)
	if len(backend.writes) != 0 {
		t.Fatal("发现已存在目标不能直接覆盖")
	}
	requireText(t, response, "确认覆盖")
	values = form(t, response)
	values.Set("action", "confirm_overwrite")
	values.Set("language", "zh-CN")
	response = post(handler, values)
	if len(backend.writes) != 1 || backend.writes[0].revision != "original" || backend.writes[0].document.Desktop.Language != "zh-CN" {
		t.Fatalf("覆盖必须基于已确认目标版本且保存当前草稿：%+v", backend.writes)
	}
	if len(backend.saves) != 0 {
		t.Fatal("文档另存不能混同启动选择")
	}
}

func TestUnifiedSettingsInvalidDestinationCannotBeOverwritten(t *testing.T) {
	for _, reason := range []string{"未知版本", "缺少完整配置分区"} {
		t.Run(reason, func(t *testing.T) {
			backend := &memoryBackend{state: initialState(), documentErrors: map[string]error{"/isolated/target.json": errors.New(reason)}}
			handler := newChineseStartupHandler(t, backend, startup.FileDialogs{Save: func(context.Context, string) (string, bool, error) { return "/isolated/target.json", false, nil }})
			values := form(t, get(t, handler))
			values.Set("action", "save_as")
			response := post(handler, values)
			if len(backend.writes) != 0 || strings.Contains(response.Body.String(), `value="confirm_overwrite"`) {
				t.Fatal("无法读取的完整配置不得提供覆盖入口")
			}
			requireText(t, response, reason)
		})
	}
}

func TestUnifiedSettingsUnknownFileWriteRequiresExplicitReadWithoutRetry(t *testing.T) {
	backend := &memoryBackend{state: initialState(), write: func(documentWrite) (settings.SaveResult, error) {
		return settings.SaveResult{}, errors.New("写入结果未知")
	}}
	handler := newChineseStartupHandler(t, backend, startup.FileDialogs{Save: func(context.Context, string) (string, bool, error) { return "/isolated/new.json", false, nil }})
	values := form(t, get(t, handler))
	values.Set("action", "save_as")
	response := post(handler, values)
	values = form(t, response)
	values.Set("action", "save_as")
	response = post(handler, values)
	if len(backend.writes) != 1 {
		t.Fatal("文件结果未知不能重发写入")
	}
	requireText(t, response, "重新读取文件")
	values = form(t, response)
	values.Set("action", "reload_file")
	response = post(handler, values)
	requireText(t, response, "已确认目标文件不存在")
	values = form(t, response)
	values.Set("action", "save_as")
	post(handler, values)
	if len(backend.writes) != 2 {
		t.Fatal("只有明确读回目标不存在后，才允许再次尝试保存")
	}
}

func TestUnifiedSettingsPageUsesPlainSaveAndLanguagePreference(t *testing.T) {
	backend := &memoryBackend{state: initialState()}
	response := get(t, newChineseStartupHandler(t, backend, startup.FileDialogs{}))
	for _, node := range []struct{ pattern, wanted string }{
		{`(?s)<h1\b[^>]*>(.*?)</h1>`, "设置"},
		{`(?s)<button\b[^>]*\bvalue="save"[^>]*>(.*?)</button>`, "保存"},
	} {
		matches := regexp.MustCompile(node.pattern).FindAllStringSubmatch(response.Body.String(), -1)
		if len(matches) != 1 || startupVisibleText(matches[0][1]) != node.wanted {
			t.Errorf("页面应有唯一且可见文字为 %q 的对应节点", node.wanted)
		}
	}
	for _, text := range []string{`name="language"`, `value="system"`, `value="zh-CN"`, `value="en"`, "加载不会启动、停止或重启当前服务"} {
		requireText(t, response, text)
	}
	if strings.Count(response.Body.String(), `value="save"`) != 1 {
		t.Fatal("设置页必须只有一个提交整页改动的保存按钮")
	}
	// head 内的完整词典包含原生文件对话框标题，不能把它当作设置页已显示的文案。
	// 限定 main 后分别检查控件标记和可见文字，既排除词典，也覆盖按钮以外的文案。
	mainMatch := regexp.MustCompile(`(?s)<main\b[^>]*>(.*?)</main>`).FindStringSubmatch(response.Body.String())
	if len(mainMatch) != 2 {
		t.Fatal("页面没有设置主内容区块")
	}
	mainHTML := mainMatch[1]
	for _, removed := range []string{`name="tool_path"`, `value="choose_tool"`, "import_legacy", "choose_legacy_", `name="legacy_`, `value="save_document"`} {
		if strings.Contains(mainHTML, removed) {
			t.Errorf("页面仍暴露已删除入口或内部措辞：%s", removed)
		}
	}
	visibleText := startupVisibleText(mainHTML)
	for _, removed := range []string{"导入旧配置", "保存下次启动安排", "运行工具", "启动档案", "启动安排", "保存配置文件", "启动设置"} {
		if strings.Contains(visibleText, removed) {
			t.Errorf("页面仍暴露已删除入口或内部措辞：%s", removed)
		}
	}
}

func TestUnifiedSettingsRemovedActionsAndFieldsAreRejected(t *testing.T) {
	for _, action := range []string{"import_legacy", "choose_legacy_config", "choose_legacy_tool", "choose_legacy_rules", "choose_legacy_schedules", "choose_tool", "legacy_field", "tool_field"} {
		t.Run(action, func(t *testing.T) {
			calls := 0
			backend := &memoryBackend{state: initialState()}
			handler := newChineseStartupHandler(t, backend, startup.FileDialogs{
				Open: func(context.Context, string) (string, bool, error) { calls++; return "", true, nil },
				Save: func(context.Context, string) (string, bool, error) { calls++; return "", true, nil },
			})
			values := form(t, get(t, handler))
			values.Set("language", "zh-CN")
			if action == "legacy_field" {
				values.Set("legacy_config", "/old/config.json")
			} else if action == "tool_field" {
				values.Set("tool_path", "/draft/m1ddc")
			} else {
				values.Set("action", action)
			}
			response := post(handler, values)
			if response.Code != http.StatusBadRequest || calls != 0 || len(backend.writes) != 0 || len(backend.saves) != 0 {
				t.Fatalf("已删除入口和字段必须拒绝，不能触发对话框或写入：code=%d calls=%d writes=%d saves=%d", response.Code, calls, len(backend.writes), len(backend.saves))
			}
			if action != "legacy_field" && action != "tool_field" && form(t, response).Get("language") != "zh-CN" {
				t.Fatal("拒绝旧动作时应保留合法的语言草稿")
			}
		})
	}
}

func TestUnifiedSettingsCurrentDocumentSavePreservesAllOtherSections(t *testing.T) {
	snapshot := unifiedSnapshot("/isolated/current.json", "loaded")
	snapshot.Document.Runtime.ToolPath = "/preserved/tool"
	snapshot.Document.Runtime.BusctlPath = "/preserved/busctl"
	expected := snapshot.Document
	expected.Desktop.Language = "zh-CN"
	state := initialState()
	state.ConfigPath = snapshot.Path
	state.ProfileExists = true
	backend := &memoryBackend{state: state, documents: map[string]settings.Snapshot{snapshot.Path: snapshot}}
	handler := newChineseStartupHandler(t, backend, startup.FileDialogs{})
	values := form(t, get(t, handler))
	values.Set("language", "zh-CN")
	response := post(handler, values)
	if len(backend.writes) != 1 || !reflect.DeepEqual(backend.writes[0].document, expected) || backend.writes[0].revision != "loaded" || len(backend.saves) != 0 {
		t.Fatal("修改语言必须保留高级工具路径和其余分区并使用读入版本，不改启动安排")
	}
	requireStartupItemText(t, response, "startup-results", "配置文件", "已保存")
}

func TestWholePageSaveWritesDocumentBeforeSelectingItAndChangingBothItems(t *testing.T) {
	snapshot := unifiedSnapshot("/isolated/whole.json", "r1")
	backend := &memoryBackend{state: initialState(), documents: map[string]settings.Snapshot{snapshot.Path: snapshot}}
	handler := newChineseStartupHandler(t, backend, startup.FileDialogs{Open: func(context.Context, string) (string, bool, error) { return snapshot.Path, false, nil }})
	values := form(t, get(t, handler))
	values.Set("action", "choose_config")
	response := post(handler, values)
	values = form(t, response)
	values.Set("language", "zh-CN")
	values.Set("service_enabled", "1")
	values.Set("desktop_enabled", "1")
	response = post(handler, values)
	if response.Code != http.StatusOK || !reflect.DeepEqual(backend.events, []string{"document", "settings"}) || len(backend.saves) != 1 {
		t.Fatalf("一次保存必须按文档后设置顺序完成：status=%d events=%v", response.Code, backend.events)
	}
	request := backend.saves[0]
	if request.ConfigPath == nil || *request.ConfigPath != snapshot.Path || request.ServiceEnabled == nil || !*request.ServiceEnabled || request.DesktopEnabled == nil || !*request.DesktopEnabled || backend.writes[0].revision != "r1" || backend.writes[0].document.Desktop.Language != "zh-CN" || backend.writes[0].document.Runtime.ToolPath != "" {
		t.Fatalf("整页改动未全部提交：request=%+v writes=%+v", request, backend.writes)
	}
	values = form(t, response)
	values.Set("language", "zh-CN")
	values.Set("service_enabled", "1")
	values.Set("desktop_enabled", "1")
	post(handler, values)
	if len(backend.saves) != 1 || len(backend.writes) != 1 {
		t.Fatal("已保存项目不得在无改动时重发")
	}
}

func TestWholePageSaveChoosesFirstLocationAndCanceledSaveKeepsAllDrafts(t *testing.T) {
	for _, canceled := range []bool{true, false} {
		t.Run(map[bool]string{true: "取消", false: "保存"}[canceled], func(t *testing.T) {
			backend := &memoryBackend{state: initialState()}
			handler := newChineseStartupHandler(t, backend, startup.FileDialogs{Save: func(context.Context, string) (string, bool, error) { return "/isolated/first.json", canceled, nil }})
			values := form(t, get(t, handler))
			values.Set("language", "zh-CN")
			values.Set("service_enabled", "1")
			values.Set("desktop_enabled", "1")
			response := post(handler, values)
			if canceled {
				if len(backend.events) != 0 {
					t.Fatal("取消保存位置不能写文件或系统项")
				}
				if form(t, response).Get("language") != "zh-CN" {
					t.Fatal("取消保存位置不能丢弃语言草稿")
				}
				requireText(t, response, `name="service_enabled" value="1" checked`)
				requireText(t, response, `name="desktop_enabled" value="1" checked`)
			} else if response.Code != http.StatusOK || !reflect.DeepEqual(backend.events, []string{"document", "settings"}) {
				t.Fatalf("首次保存应完成整个页面：%d %v", response.Code, backend.events)
			}
		})
	}
}

func TestWholePageOverwriteConfirmationContinuesTheSameSave(t *testing.T) {
	snapshot := unifiedSnapshot("/isolated/existing.json", "before")
	backend := &memoryBackend{state: initialState(), documents: map[string]settings.Snapshot{snapshot.Path: snapshot}}
	handler := newChineseStartupHandler(t, backend, startup.FileDialogs{Save: func(context.Context, string) (string, bool, error) { return snapshot.Path, false, nil }})
	values := form(t, get(t, handler))
	values.Set("language", "zh-CN")
	values.Set("desktop_enabled", "1")
	response := post(handler, values)
	if len(backend.events) != 0 {
		t.Fatal("覆盖确认前不能写入任一部分")
	}
	values = form(t, response)
	values.Set("action", "confirm_overwrite")
	values.Set("language", "zh-CN")
	values.Set("desktop_enabled", "1")
	response = post(handler, values)
	if response.Code != http.StatusOK || !reflect.DeepEqual(backend.events, []string{"document", "settings"}) || !backend.state.Desktop.Enabled || backend.state.ConfigPath != snapshot.Path {
		t.Fatalf("确认应继续同一次整页保存：status=%d events=%v", response.Code, backend.events)
	}
}

func TestWholePageFileFailureDoesNotSelectOrEnableServiceButKeepsIndependentDesktop(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		t.Run(map[bool]string{false: "失败", true: "未知"}[unknown], func(t *testing.T) {
			snapshot := unifiedSnapshot("/isolated/failure.json", "before")
			state := initialState()
			state.ConfigPath = snapshot.Path
			state.ProfileExists = true
			backend := &memoryBackend{state: state, documents: map[string]settings.Snapshot{snapshot.Path: snapshot}, write: func(documentWrite) (settings.SaveResult, error) {
				if unknown {
					return settings.SaveResult{}, errors.New("结果未知")
				}
				return settings.SaveResult{Saved: boolean(false)}, errors.New("写入被拒绝")
			}}
			handler := newChineseStartupHandler(t, backend, startup.FileDialogs{})
			values := form(t, get(t, handler))
			values.Set("language", "zh-CN")
			values.Set("service_enabled", "1")
			values.Set("desktop_enabled", "1")
			response := post(handler, values)
			if response.Header().Get("X-Settings-Request-Revision") != values.Get("revision") {
				t.Fatal("已受理的部分结果必须与本次请求版本绑定，供原位呈现而非导入旧页面")
			}
			if response.Code != http.StatusConflict || len(backend.saves) != 1 || backend.saves[0].ConfigPath != nil || backend.saves[0].ServiceEnabled != nil || !backend.state.Desktop.Enabled {
				t.Fatalf("文件失败应只完成可信独立项：%d %+v", response.Code, backend.saves)
			}
			if form(t, response).Get("language") != "zh-CN" {
				t.Fatal("文档保存失败或未知不能丢弃语言草稿")
			}
			requireStartupItemText(t, response, "startup-results", "开机启动桌面客户端", "已保存")
			if unknown {
				values = form(t, response)
				values.Set("language", "zh-CN")
				values.Set("service_enabled", "1")
				values.Set("desktop_enabled", "1")
				post(handler, values)
				if len(backend.writes) != 1 || len(backend.saves) != 1 {
					t.Fatal("文件未知不能重发或重复已完成独立项")
				}
			}
		})
	}
}

func TestWholePagePartialSystemFailureDoesNotRewriteConfirmedDocumentOrDesktop(t *testing.T) {
	snapshot := unifiedSnapshot("/isolated/partial.json", "before")
	backend := &memoryBackend{state: initialState(), documents: map[string]settings.Snapshot{snapshot.Path: snapshot}}
	backend.save = func(request startup.SaveRequest) (startup.SaveResult, error) {
		backend.state.ConfigPath, backend.state.ProfileExists = *request.ConfigPath, true
		backend.state.Desktop.Enabled = true
		return startup.SaveResult{
			Configuration: startup.Outcome{Attempted: true, Succeeded: true},
			Service:       startup.Outcome{Attempted: true, Message: "系统拒绝开启服务"},
			Desktop:       startup.Outcome{Attempted: true, Succeeded: true},
		}, nil
	}
	handler := newChineseStartupHandler(t, backend, startup.FileDialogs{Open: func(context.Context, string) (string, bool, error) { return snapshot.Path, false, nil }})
	values := form(t, get(t, handler))
	values.Set("action", "choose_config")
	response := post(handler, values)
	values = form(t, response)
	values.Set("language", "zh-CN")
	values.Set("service_enabled", "1")
	values.Set("desktop_enabled", "1")
	response = post(handler, values)
	if response.Code != http.StatusConflict || len(backend.writes) != 1 {
		t.Fatal("系统部分失败必须保留已写成文件")
	}
	requireStartupItemText(t, response, "startup-results", "配置文件", "已保存")
	requireText(t, response, "系统拒绝开启服务")
	requireText(t, response, `name="service_enabled" value="1" checked`)
	backend.save = nil
	values = form(t, response)
	values.Set("language", "zh-CN")
	values.Set("service_enabled", "1")
	values.Set("desktop_enabled", "1")
	response = post(handler, values)
	if response.Code != http.StatusOK || len(backend.writes) != 1 || len(backend.saves) != 2 || backend.saves[1].ConfigPath != nil || backend.saves[1].DesktopEnabled != nil || backend.saves[1].ServiceEnabled == nil {
		t.Fatal("明确再次保存只能处理未完成服务项，不能重写文件或重发桌面项")
	}
}

func TestWholePageDocumentFailureStillAllowsBothItemsToBeDisabled(t *testing.T) {
	snapshot := unifiedSnapshot("/isolated/disable.json", "before")
	state := initialState()
	state.ConfigPath, state.ProfileExists = snapshot.Path, true
	state.Service.Enabled, state.Desktop.Enabled = true, true
	backend := &memoryBackend{state: state, documents: map[string]settings.Snapshot{snapshot.Path: snapshot}, write: func(documentWrite) (settings.SaveResult, error) {
		return settings.SaveResult{Saved: boolean(false)}, errors.New("文件只读")
	}}
	handler := newChineseStartupHandler(t, backend, startup.FileDialogs{})
	values := form(t, get(t, handler))
	values.Set("language", "zh-CN")
	response := post(handler, values)
	if response.Code != http.StatusConflict || len(backend.saves) != 1 || backend.state.Service.Enabled || backend.state.Desktop.Enabled || backend.saves[0].ConfigPath != nil {
		t.Fatal("文件失败不能阻断两个已确认自动启动项的安全关闭")
	}
	if form(t, response).Get("language") != "zh-CN" {
		t.Fatal("文档保存失败不能丢弃语言草稿")
	}
	requireStartupItemText(t, response, "startup-results", "开机启动服务", "已保存")
	requireStartupItemText(t, response, "startup-results", "开机启动桌面客户端", "已保存")
}

func TestUnifiedSettingsCanceledNewOrSaveAsKeepsPositionLanguageAndRevision(t *testing.T) {
	for _, action := range []string{"new_config", "save_as"} {
		t.Run(action, func(t *testing.T) {
			snapshot := unifiedSnapshot("/isolated/original.json", "original")
			state := initialState()
			state.ConfigPath = snapshot.Path
			state.ProfileExists = true
			backend := &memoryBackend{state: state, documents: map[string]settings.Snapshot{snapshot.Path: snapshot}}
			calls := 0
			handler := newChineseStartupHandler(t, backend, startup.FileDialogs{Save: func(context.Context, string) (string, bool, error) {
				calls++
				return "", true, nil
			}})
			values := form(t, get(t, handler))
			values.Set("action", action)
			values.Set("language", "zh-CN")
			values.Set("desktop_enabled", "1")
			response := post(handler, values)
			requireText(t, response, snapshot.Path)
			requireText(t, response, `name="desktop_enabled" value="1" checked`)
			draft := form(t, response)
			if response.Code != http.StatusOK || draft.Get("language") != "zh-CN" || draft.Get("revision") == values.Get("revision") || response.Header().Get("X-Settings-Request-Revision") != values.Get("revision") {
				t.Fatal("取消对话框应保留语言草稿并消费本次动作版本")
			}
			response = post(handler, values)
			if response.Code != http.StatusConflict || calls != 1 || len(backend.writes) != 0 || len(backend.saves) != 0 || !reflect.DeepEqual(backend.documents[snapshot.Path], snapshot) {
				t.Fatal("取消新建或另存不能改文件、切换启动选择或重复派发旧对话框动作")
			}
		})
	}
}

func TestUnifiedSettingsLoadedReadyDocumentCanBeSelectedWithoutRewritingIt(t *testing.T) {
	snapshot := unifiedSnapshot("/isolated/ready.json", "ready")
	snapshot.Document.Runtime.ToolPath = "/ready/tool"
	backend := &memoryBackend{state: initialState(), documents: map[string]settings.Snapshot{snapshot.Path: snapshot}}
	handler := newChineseStartupHandler(t, backend, startup.FileDialogs{Open: func(context.Context, string) (string, bool, error) { return snapshot.Path, false, nil }})
	values := form(t, get(t, handler))
	values.Set("action", "choose_config")
	response := post(handler, values)
	values = form(t, response)
	values.Set("service_enabled", "1")
	response = post(handler, values)
	if response.Code != http.StatusOK || len(backend.saves) != 1 || len(backend.writes) != 0 {
		t.Fatalf("选择下次启动应只存选择和开关，不重写已加载文档：code=%d saves=%d writes=%d", response.Code, len(backend.saves), len(backend.writes))
	}
	request := backend.saves[0]
	if request.ConfigPath == nil || *request.ConfigPath != snapshot.Path || request.ServiceEnabled == nil || !*request.ServiceEnabled || request.DesktopEnabled != nil || !reflect.DeepEqual(backend.documents[snapshot.Path], snapshot) {
		t.Fatalf("未准确选择单文件启动参数：%+v", request)
	}
}

func TestUnifiedSettingsFourLoginCombinationsRemainIndependent(t *testing.T) {
	for _, service := range []bool{false, true} {
		for _, desktop := range []bool{false, true} {
			name := map[bool]string{false: "服务关", true: "服务开"}[service] + map[bool]string{false: "桌面关", true: "桌面开"}[desktop]
			t.Run(name, func(t *testing.T) {
				snapshot := unifiedSnapshot("/isolated/current.json", "current")
				snapshot.Document.Runtime.ToolPath = "/ready/tool"
				state := initialState()
				state.ConfigPath = snapshot.Path
				state.ProfileExists = true
				backend := &memoryBackend{state: state, documents: map[string]settings.Snapshot{snapshot.Path: snapshot}}
				handler := newChineseStartupHandler(t, backend, startup.FileDialogs{})
				values := form(t, get(t, handler))
				if service {
					values.Set("service_enabled", "1")
				}
				if desktop {
					values.Set("desktop_enabled", "1")
				}
				response := post(handler, values)
				if response.Code != http.StatusOK || backend.state.Service.Enabled != service || backend.state.Desktop.Enabled != desktop || len(backend.writes) != 0 {
					t.Fatal("四种组合均须保留文档且两个开关独立")
				}
				for _, request := range backend.saves {
					if request.ConfigPath != nil {
						t.Fatal("未改配置位置不应重存选择")
					}
				}
			})
		}
	}
}

func TestUnifiedSettingsBundledToolSelectionSurvivesServiceFailureAndIndependentDesktopSave(t *testing.T) {
	snapshot := unifiedSnapshot("/isolated/empty-tool.json", "empty-tool")
	snapshot.Document.Desktop.Language = "zh-CN"
	backend := &memoryBackend{state: initialState(), documents: map[string]settings.Snapshot{snapshot.Path: snapshot}}
	// 空工具路径由核心解析包内工具；页面只确认完整文档和选择，不能代替
	// Enable的实际就绪校验，也不能因该项失败撤销已确认选择或桌面项。
	backend.save = func(request startup.SaveRequest) (startup.SaveResult, error) {
		if request.ConfigPath == nil {
			t.Fatal("启用前必须将合法完整文档的选择交给后台保存")
		}
		backend.state.ConfigPath, backend.state.ProfileExists = *request.ConfigPath, true
		backend.state.Desktop.Enabled = true
		return startup.SaveResult{
			Configuration: startup.Outcome{Attempted: true, Succeeded: true},
			Service:       startup.Outcome{Attempted: true, Message: "随包控制程序不可执行"},
			Desktop:       startup.Outcome{Attempted: true, Succeeded: true},
		}, nil
	}
	handler := newChineseStartupHandler(t, backend, startup.FileDialogs{Open: func(context.Context, string) (string, bool, error) { return snapshot.Path, false, nil }})
	values := form(t, get(t, handler))
	values.Set("action", "choose_config")
	response := post(handler, values)
	values = form(t, response)
	values.Set("service_enabled", "1")
	values.Set("desktop_enabled", "1")
	response = post(handler, values)
	if response.Code != http.StatusConflict || len(backend.saves) != 1 || len(backend.writes) != 0 || !backend.state.Desktop.Enabled || backend.state.Service.Enabled || backend.state.ConfigPath != snapshot.Path {
		t.Fatal("核心启用失败仍应保留合法文档选择和独立桌面结果并显示部分完成")
	}
	request := backend.saves[0]
	if request.ConfigPath == nil || *request.ConfigPath != snapshot.Path || request.ServiceEnabled == nil || !*request.ServiceEnabled || request.DesktopEnabled == nil || !*request.DesktopEnabled || !reflect.DeepEqual(backend.documents[snapshot.Path], snapshot) {
		t.Fatal("页面应提交选择及两个独立开关，保留包内工具的空路径供核心校验")
	}
	requireText(t, response, "草稿已保留")
	requireText(t, response, "随包控制程序不可执行")
}

func TestUnifiedSettingsCancelRestoresStartupSelectionWithoutTouchingFiles(t *testing.T) {
	original := unifiedSnapshot("/isolated/original.json", "original")
	other := unifiedSnapshot("/isolated/other.json", "other")
	state := initialState()
	state.ConfigPath = original.Path
	state.ProfileExists = true
	backend := &memoryBackend{state: state, documents: map[string]settings.Snapshot{original.Path: original, other.Path: other}}
	handler := newChineseStartupHandler(t, backend, startup.FileDialogs{Open: func(context.Context, string) (string, bool, error) { return other.Path, false, nil }})
	values := form(t, get(t, handler))
	values.Set("action", "choose_config")
	response := post(handler, values)
	values = form(t, response)
	values.Set("action", "cancel")
	response = post(handler, values)
	requireText(t, response, "当前选定文件")
	requireText(t, response, original.Path)
	if len(backend.writes) != 0 || len(backend.saves) != 0 || !reflect.DeepEqual(backend.documents[other.Path], other) {
		t.Fatal("取消只恢复草稿，不改动加载文件或系统项")
	}
}

func TestUnifiedSettingsRejectsUnknownPickerKindBeforeNativeDialog(t *testing.T) {
	calls := 0
	backend := &memoryBackend{state: initialState()}
	handler := newChineseStartupHandler(t, backend, startup.FileDialogs{Open: func(context.Context, string) (string, bool, error) { calls++; return "", true, nil }})
	values := form(t, get(t, handler))
	values.Set("action", "choose_daemon")
	response := post(handler, values)
	if response.Code != http.StatusBadRequest || calls != 0 || len(backend.writes) != 0 || len(backend.saves) != 0 {
		t.Fatal("任意选择种类不得送入原生桥")
	}
}

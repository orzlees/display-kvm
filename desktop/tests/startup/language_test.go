package startup_test

import (
	"context"
	"errors"
	"html"
	"net/http"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/orzlee/displayKvm/desktop/i18n"
	"github.com/orzlee/displayKvm/desktop/views/startup"
	"github.com/orzlee/displayKvm/internal/settings"
)

func TestSettingsLanguageChangesOnlyAfterDocumentAndSelectionAreConfirmed(t *testing.T) {
	for _, failure := range []string{"", "file", "unknown", "selection", "login"} {
		t.Run(failure, func(t *testing.T) {
			language, err := i18n.New("zh-CN", nil)
			if err != nil {
				t.Fatal(err)
			}
			snapshot := unifiedSnapshot(filepath.Join(t.TempDir(), "language.json"), "before")
			backend := &memoryBackend{state: initialState(), documents: map[string]settings.Snapshot{snapshot.Path: snapshot}}
			if failure == "file" || failure == "unknown" {
				backend.write = func(documentWrite) (settings.SaveResult, error) {
					if failure == "unknown" {
						return settings.SaveResult{}, errors.New("unknown")
					}
					return settings.SaveResult{Saved: boolean(false)}, errors.New("read only")
				}
			}
			if failure == "selection" || failure == "login" {
				backend.save = func(request startup.SaveRequest) (startup.SaveResult, error) {
					result := startup.SaveResult{Configuration: startup.Outcome{Attempted: true}}
					if failure == "login" {
						backend.state.ConfigPath, backend.state.ProfileExists = *request.ConfigPath, true
						result.Configuration.Succeeded = true
						result.Desktop = startup.Outcome{Attempted: true, Message: "system denied"}
					}
					return result, nil
				}
			}
			handler := startup.NewHandler(backend, startup.FileDialogs{Open: func(context.Context, string) (string, bool, error) { return snapshot.Path, false, nil }}, language)
			values := form(t, get(t, handler))
			values.Set("action", "choose_config")
			response := post(handler, values)
			values = form(t, response)
			values.Set("language", "en")
			values.Set("desktop_enabled", "1")
			response = post(handler, values)
			want := "zh-CN"
			if failure == "" || failure == "login" {
				want = "en"
			}
			if language.Snapshot().Preference != want {
				t.Fatalf("未确认文档/选择不能切语言，独立登录项失败不能回滚语言：%s", language.Snapshot().Preference)
			}
			if len(backend.writes) != 1 || backend.writes[0].document.Desktop.Language != "en" || backend.writes[0].document.Runtime.ToolPath != "" {
				t.Fatal("语言必须保存在同一完整文档，不能要求普通用户填写程序路径")
			}
			if want == "en" && !strings.Contains(response.Body.String(), "Settings") {
				t.Fatal("同次保存响应必须立即使用已确认语言")
			}
		})
	}
}

func TestSettingsLanguageDraftDoesNotApplyOnChangeOrCanceledSave(t *testing.T) {
	language, err := i18n.New("zh-CN", nil)
	if err != nil {
		t.Fatal(err)
	}
	backend := &memoryBackend{state: initialState()}
	handler := startup.NewHandler(backend, startup.FileDialogs{Save: func(context.Context, string) (string, bool, error) { return "", true, nil }}, language)
	values := form(t, get(t, handler))
	values.Set("language", "en")
	values.Set("desktop_enabled", "1")
	response := post(handler, values)
	if response.Code != http.StatusOK || language.Snapshot().Preference != "zh-CN" || len(backend.writes) != 0 || len(backend.saves) != 0 {
		t.Fatal("取消整页保存必须保留语言草稿，不提交文件/登录项或更改当前语言")
	}
	selected := selectedStartupLanguagePattern.FindStringSubmatch(response.Body.String())
	if len(selected) != 2 || selected[1] != "en" {
		t.Fatal("取消保存后必须仍选中英文草稿，不能因翻译属性的位置误判选中状态")
	}
}

func TestSettingsOrdinaryPageDoesNotExposeRuntimeToolPaths(t *testing.T) {
	language, _ := i18n.New("en", nil)
	response := get(t, startup.NewHandler(&memoryBackend{state: initialState()}, startup.FileDialogs{}, language))
	// Catalog包含所有业务页的消息，但脚本数据不等于普通设置的可见内容。
	// 仅移除script；真实诊断仍参与可见文本检查，不能借此隐藏用户看到的内部术语。
	markup := regexp.MustCompile(`(?is)<script\b[^>]*>.*?</script>`).ReplaceAllString(response.Body.String(), "")
	for _, removed := range []string{`name="tool_path"`, `name="busctl_path"`, `name="udevadm_path"`, `value="choose_tool"`} {
		if strings.Contains(markup, removed) {
			t.Errorf("普通设置页不应要求用户管理随包工具：%s", removed)
		}
	}
	visible := strings.ToLower(html.UnescapeString(regexp.MustCompile(`<[^>]+>`).ReplaceAllString(markup, "")))
	for _, removed := range []string{"m1ddc", "runtime", "profile", "download"} {
		if strings.Contains(visible, removed) {
			t.Errorf("普通设置页不应显示工具内部术语或要求手工下载：%s", removed)
		}
	}
	for _, key := range []string{"settings.language", "settings.language.system", "settings.language.zh", "settings.language.en"} {
		requireText(t, response, key)
	}
}

func TestSettingsLanguageSavePreservesAdvancedRuntimeAndDocumentSections(t *testing.T) {
	language, err := i18n.New("zh-CN", nil)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	snapshot := unifiedSnapshot("/isolated/advanced.json", "before")
	snapshot.Document.Runtime = settings.RuntimeSettings{ToolPath: filepath.Join(root, "control"), BusctlPath: filepath.Join(root, "busctl"), UdevadmPath: filepath.Join(root, "udevadm")}
	backend := &memoryBackend{state: initialState(), documents: map[string]settings.Snapshot{snapshot.Path: snapshot}}
	backend.state.ConfigPath, backend.state.ProfileExists = snapshot.Path, true
	handler := startup.NewHandler(backend, startup.FileDialogs{}, language)
	values := form(t, get(t, handler))
	values.Set("language", "en")
	response := post(handler, values)
	expected := snapshot.Document
	expected.Desktop.Language = "en"
	if response.Code != http.StatusOK || len(backend.writes) != 1 || !reflect.DeepEqual(backend.writes[0].document, expected) || len(backend.saves) != 0 || language.Snapshot().Preference != "en" {
		t.Fatal("普通语言保存只能修改desktop.language，必须保留高级程序覆盖及所有业务分区，配置选择未改时不重存系统项")
	}
}

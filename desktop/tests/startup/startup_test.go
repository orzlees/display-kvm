package startup_test

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/orzlee/displayKvm/desktop/views/startup"
	"github.com/orzlee/displayKvm/internal/settings"
)

type documentWrite struct {
	path     string
	document settings.Document
	revision settings.Revision
}

type memoryBackend struct {
	state          startup.State
	readErr        error
	reads          int
	saves          []startup.SaveRequest
	save           func(startup.SaveRequest) (startup.SaveResult, error)
	documents      map[string]settings.Snapshot
	documentErrors map[string]error
	writes         []documentWrite
	write          func(documentWrite) (settings.SaveResult, error)
	events         []string
}

func (b *memoryBackend) Read(context.Context) (startup.State, error) {
	b.reads++
	return b.state, b.readErr
}

func (b *memoryBackend) Save(_ context.Context, request startup.SaveRequest) (startup.SaveResult, error) {
	b.events = append(b.events, "settings")
	b.saves = append(b.saves, request)
	if b.save != nil {
		return b.save(request)
	}
	result := startup.SaveResult{}
	if request.ConfigPath != nil {
		b.state.ConfigPath, b.state.ProfileExists = *request.ConfigPath, true
		result.Configuration = startup.Outcome{Attempted: true, Succeeded: true}
	}
	if request.ServiceEnabled != nil {
		b.state.Service.Enabled = *request.ServiceEnabled
		result.Service = startup.Outcome{Attempted: true, Succeeded: true}
	}
	if request.DesktopEnabled != nil {
		b.state.Desktop.Enabled = *request.DesktopEnabled
		result.Desktop = startup.Outcome{Attempted: true, Succeeded: true}
	}
	return result, nil
}

func (b *memoryBackend) ReadDocument(_ context.Context, path string) (settings.Snapshot, error) {
	if err := b.documentErrors[path]; err != nil {
		return settings.Snapshot{}, err
	}
	if snapshot, ok := b.documents[path]; ok {
		return snapshot, nil
	}
	return settings.Snapshot{}, os.ErrNotExist
}

func (b *memoryBackend) SaveDocument(_ context.Context, path string, document settings.Document, revision settings.Revision) (settings.SaveResult, error) {
	b.events = append(b.events, "document")
	write := documentWrite{path, document, revision}
	b.writes = append(b.writes, write)
	if b.write != nil {
		return b.write(write)
	}
	if b.documents == nil {
		b.documents = map[string]settings.Snapshot{}
	}
	snapshot := settings.Snapshot{Path: path, Document: document, Revision: settings.Revision("saved")}
	b.documents[path] = snapshot
	return settings.SaveResult{Saved: boolean(true), Snapshot: &snapshot}, nil
}

func initialState() startup.State {
	return startup.State{ConfigKnown: true, Service: startup.LoginState{Supported: true, Known: true}, Desktop: startup.LoginState{Supported: true, Known: true}}
}

func get(t *testing.T, handler http.Handler) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/startup/", nil))
	return recorder
}

var revisionPattern = regexp.MustCompile(`name="revision" value="([0-9]+)"`)

func form(t *testing.T, response *httptest.ResponseRecorder) url.Values {
	t.Helper()
	match := revisionPattern.FindStringSubmatch(response.Body.String())
	if len(match) != 2 {
		t.Fatal("页面没有表单版本，无法防止旧表单重复保存")
	}
	language := selectedStartupLanguagePattern.FindStringSubmatch(response.Body.String())
	if len(language) != 2 {
		t.Fatal("页面没有明确选中的语言草稿")
	}
	return url.Values{"revision": {match[1]}, "action": {"save"}, "language": {language[1]}}
}

func post(handler http.Handler, values url.Values) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/startup/", strings.NewReader(values.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	handler.ServeHTTP(recorder, request)
	return recorder
}

func requireText(t *testing.T, response *httptest.ResponseRecorder, wanted string) {
	t.Helper()
	if !strings.Contains(response.Body.String(), wanted) {
		t.Errorf("页面缺少 %q；状态码 %d", wanted, response.Code)
	}
}

func TestUnifiedSettingsFirstVisitOffersCreationAndTwoIndependentItems(t *testing.T) {
	backend := &memoryBackend{state: initialState()}
	response := get(t, newChineseStartupHandler(t, backend, startup.FileDialogs{}))
	for _, text := range []string{"新建配置", "加载已有配置", "尚未选择配置文件", "开机启动服务", "开机启动桌面客户端", "当前服务使用的配置：未知"} {
		requireText(t, response, text)
	}
	for _, old := range []string{`name="config"`, `name="rules"`, `name="schedules"`} {
		if strings.Contains(response.Body.String(), old) {
			t.Fatalf("主界面仍暴露旧多文件字段：%s", old)
		}
	}
	if response.Code != http.StatusOK || len(backend.saves) != 0 || len(backend.writes) != 0 {
		t.Fatal("首次读取不得创建文件或开启启动项")
	}
}

func TestReadFailureCannotOverwriteUnreadableSettings(t *testing.T) {
	backend := &memoryBackend{readErr: errors.New("无法读取启动设置")}
	handler := newChineseStartupHandler(t, backend, startup.FileDialogs{})
	values := form(t, get(t, handler))
	values.Set("desktop_enabled", "1")
	post(handler, values)
	if len(backend.saves) != 0 {
		t.Fatal("读取失败不得以空白表单覆盖启动设置")
	}
}

func TestDesktopOnlyDoesNotRequireServiceFilesOrChangeService(t *testing.T) {
	backend := &memoryBackend{state: initialState()}
	handler := newChineseStartupHandler(t, backend, startup.FileDialogs{})
	values := form(t, get(t, handler))
	values.Set("desktop_enabled", "1")
	response := post(handler, values)
	if response.Code != http.StatusOK || len(backend.saves) != 1 {
		t.Fatal("桌面启动必须可单独设置")
	}
	request := backend.saves[0]
	if request.ConfigPath != nil || request.ServiceEnabled != nil || request.DesktopEnabled == nil || !*request.DesktopEnabled {
		t.Fatalf("请求混入无关操作：%+v", request)
	}
	requireText(t, response, "不会立即启动、停止或重启当前服务")
}

func TestDuplicateOldFormCannotWriteTwice(t *testing.T) {
	backend := &memoryBackend{state: initialState()}
	handler := newChineseStartupHandler(t, backend, startup.FileDialogs{})
	values := form(t, get(t, handler))
	values.Set("desktop_enabled", "1")
	post(handler, values)
	response := post(handler, values)
	if response.Code != http.StatusConflict || len(backend.saves) != 1 || response.Header().Get("X-Settings-Request-Revision") != "" {
		t.Fatal("旧表单不能写入两次")
	}
}

func TestUnknownSaveResultBlocksWritesUntilExplicitReload(t *testing.T) {
	backend := &memoryBackend{state: initialState(), save: func(startup.SaveRequest) (startup.SaveResult, error) {
		return startup.SaveResult{}, errors.New("保存结果暂时无法确认")
	}}
	handler := newChineseStartupHandler(t, backend, startup.FileDialogs{})
	values := form(t, get(t, handler))
	values.Set("desktop_enabled", "1")
	response := post(handler, values)
	values = form(t, response)
	values.Set("desktop_enabled", "1")
	response = post(handler, values)
	if len(backend.saves) != 1 {
		t.Fatal("未知结果不得重发")
	}
	values = form(t, response)
	values.Set("action", "reload")
	response = post(handler, values)
	backend.save = nil
	values = form(t, response)
	values.Set("desktop_enabled", "1")
	response = post(handler, values)
	if response.Code != http.StatusOK || len(backend.saves) != 2 {
		t.Fatal("显式重新读取后应恢复已确认项操作")
	}
}

func TestPageRegistersEmbeddedAssetsAndDoesNotNeedDaemon(t *testing.T) {
	page := startup.Page(&memoryBackend{state: initialState()}, startup.FileDialogs{})
	if page.Route != "/startup/" || page.ResourcePath != "/startup-assets/" || page.Handler == nil {
		t.Fatal("注册契约缺失")
	}
	for _, asset := range []string{"startup.css", "startup.js"} {
		if _, err := fs.ReadFile(page.Resources, asset); err != nil {
			t.Fatal(err)
		}
	}
	if get(t, page.Handler(nil)).Code != http.StatusOK {
		t.Fatal("页面不得依赖daemon")
	}
}

func TestWrongMethodCannotInvokeNativePicker(t *testing.T) {
	backend := &memoryBackend{state: initialState()}
	handler := newChineseStartupHandler(t, backend, startup.FileDialogs{})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPut, "/startup/", nil))
	if response.Code != http.StatusMethodNotAllowed || backend.reads != 0 {
		t.Fatal("错误方法必须先拒绝")
	}
}

func TestUnsupportedLoginItemIsExplainedAndCannotBeSubmitted(t *testing.T) {
	state := initialState()
	state.Service.Supported = false
	backend := &memoryBackend{state: state}
	handler := newChineseStartupHandler(t, backend, startup.FileDialogs{})
	response := get(t, handler)
	requireStartupItemText(t, response, "service_enabled-actual", "开机启动服务", "当前系统暂不支持")
	values := form(t, response)
	values.Set("service_enabled", "1")
	values.Set("desktop_enabled", "1")
	response = post(handler, values)
	if response.Code != http.StatusOK || len(backend.saves) != 1 || backend.saves[0].ServiceEnabled != nil || !backend.state.Desktop.Enabled {
		t.Fatal("不支持的项必须禁用且不阻断独立桌面项")
	}
}

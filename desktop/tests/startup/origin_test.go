package startup_test

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/orzlee/displayKvm/desktop/views/startup"
	"github.com/orzlee/displayKvm/internal/settings"
)

func nativeFormRequest(values url.Values) *http.Request {
	request := httptest.NewRequest(http.MethodPost, "wails://localhost/startup/", strings.NewReader(values.Encode()))
	// 模拟Wails资源服务器交给页面处理器的真实请求形态：保留Request.Host，
	// 清空URL中的来源字段，而不是直接用未经过框架转换的wails绝对URL。
	request.URL.Scheme, request.URL.Host = "", ""
	request.RequestURI = request.URL.RequestURI()
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", "wails://localhost")
	request.Header.Set("Referer", "wails://localhost/startup/")
	return request
}

func TestNativeOriginSaveReachesBackendAfterWailsURLNormalization(t *testing.T) {
	backend := &memoryBackend{state: initialState()}
	handler := newChineseStartupHandler(t, backend, startup.FileDialogs{})
	values := form(t, get(t, handler))
	values.Set("desktop_enabled", "1")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, nativeFormRequest(values))
	if response.Code != http.StatusOK || len(backend.saves) != 1 || !backend.state.Desktop.Enabled {
		t.Fatalf("Wails原生表单没有实际保存：status=%d saves=%d state=%+v", response.Code, len(backend.saves), backend.state)
	}
}

func TestNativeOriginAndRefererFilePickerReachesPickerWithoutSaving(t *testing.T) {
	for _, source := range []string{"Origin与Referer", "仅Referer"} {
		t.Run(source, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			backend := &memoryBackend{state: initialState()}
			picks := 0
			backend.documents = map[string]settings.Snapshot{path: unifiedSnapshot(path, "read")}
			handler := newChineseStartupHandler(t, backend, startup.FileDialogs{Open: func(_ context.Context, field string) (string, bool, error) {
				picks++
				if field != "config" {
					t.Errorf("选择目标错误：%s", field)
				}
				return path, false, nil
			}})
			values := form(t, get(t, handler))
			values.Set("action", "choose_config")
			values.Set("desktop_enabled", "1")
			request := nativeFormRequest(values)
			if source == "仅Referer" {
				request.Header.Del("Origin")
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusOK || picks != 1 || len(backend.saves) != 0 {
				t.Fatalf("原生来源选择器未到达或意外保存：status=%d picks=%d saves=%d", response.Code, picks, len(backend.saves))
			}
			requireText(t, response, path)
			requireText(t, response, `name="desktop_enabled" value="1" checked`)
		})
	}
}

func TestNativeOriginPathHeaderStillReachesBackend(t *testing.T) {
	backend := &memoryBackend{state: initialState()}
	handler := newChineseStartupHandler(t, backend, startup.FileDialogs{})
	values := form(t, get(t, handler))
	values.Set("desktop_enabled", "1")
	request := nativeFormRequest(values)
	request.Header.Set("Origin", "wails://localhost/startup/")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || len(backend.saves) != 1 {
		t.Fatal("来源带当前页面路径时也应按准确scheme和host判断，不能拒绝正常原生请求")
	}
}

func TestNativeOriginRejectsUntrustedRequestsBeforeSaveAndPicker(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		mutate func(*http.Request)
	}{
		{"跨请求主机", func(r *http.Request) { r.Host = "other.example" }},
		{"伪造另一原生主机", func(r *http.Request) {
			r.Host = "other.example"
			r.Header.Set("Origin", "wails://other.example")
			r.Header.Set("Referer", "wails://other.example/startup/")
		}},
		{"原生端口", func(r *http.Request) {
			r.Host = "localhost:8080"
			r.Header.Set("Origin", "wails://localhost:8080")
			r.Header.Set("Referer", "wails://localhost:8080/startup/")
		}},
		{"跨来源主机", func(r *http.Request) { r.Header.Set("Origin", "wails://other.example") }},
		{"跨引用主机", func(r *http.Request) { r.Header.Set("Referer", "wails://other.example/startup/") }},
		{"来源主机前缀", func(r *http.Request) { r.Header.Set("Origin", "wails://localhost.other.example") }},
		{"来源端口", func(r *http.Request) { r.Header.Set("Origin", "wails://localhost:80") }},
		{"引用端口", func(r *http.Request) { r.Header.Set("Referer", "wails://localhost:8080/startup/") }},
		{"空来源", func(r *http.Request) { r.Header.Set("Origin", "null") }},
		{"空引用", func(r *http.Request) { r.Header.Set("Referer", "null") }},
		{"任意协议", func(r *http.Request) {
			r.Header.Set("Origin", "other://localhost")
			r.Header.Set("Referer", "other://localhost/startup/")
		}},
		{"混合协议", func(r *http.Request) { r.Header.Set("Origin", "http://localhost") }},
		{"请求仍有HTTP协议", func(r *http.Request) { r.URL.Scheme = "http" }},
		{"请求仍有URL主机", func(r *http.Request) { r.URL.Host = "localhost" }},
		{"HTTPS请求冒充原生", func(r *http.Request) { r.TLS = &tls.ConnectionState{} }},
		{"跨站标记", func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") }},
		{"来源含用户信息", func(r *http.Request) { r.Header.Set("Origin", "wails://someone@localhost") }},
	} {
		for _, action := range []string{"save", "choose_config"} {
			t.Run(scenario.name+"/"+action, func(t *testing.T) {
				backend := &memoryBackend{state: initialState()}
				picks := 0
				handler := newChineseStartupHandler(t, backend, startup.FileDialogs{Open: func(context.Context, string) (string, bool, error) {
					picks++
					return "/chosen/config.json", false, nil
				}})
				values := form(t, get(t, handler))
				values.Set("action", action)
				values.Set("desktop_enabled", "1")
				request := nativeFormRequest(values)
				scenario.mutate(request)
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, request)
				if response.Code != http.StatusForbidden || picks != 0 || len(backend.saves) != 0 {
					t.Fatalf("不可信来源触发了动作：status=%d picks=%d saves=%d", response.Code, picks, len(backend.saves))
				}
			})
		}
	}
}

func TestHTTPAndHTTPSExactOriginStillReachBackend(t *testing.T) {
	for _, scheme := range []string{"http", "https"} {
		t.Run(scheme, func(t *testing.T) {
			backend := &memoryBackend{state: initialState()}
			handler := newChineseStartupHandler(t, backend, startup.FileDialogs{})
			values := form(t, get(t, handler))
			values.Set("desktop_enabled", "1")
			origin := scheme + "://desktop.local:38473"
			request := httptest.NewRequest(http.MethodPost, origin+"/startup/", strings.NewReader(values.Encode()))
			request.URL.Scheme, request.URL.Host = "", ""
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			request.Header.Set("Origin", origin)
			request.Header.Set("Referer", origin+"/startup/")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusOK || len(backend.saves) != 1 {
				t.Fatalf("正常%s同源请求被拒：status=%d saves=%d", scheme, response.Code, len(backend.saves))
			}
		})
	}
}

func TestHTTPOriginCannotCrossPortOrUseUnknownScheme(t *testing.T) {
	for _, scenario := range []struct{ target, origin, referer string }{
		{"http://desktop.local:38473/startup/", "http://desktop.local:38474", "http://desktop.local:38474/startup/"},
		{"https://desktop.local:38473/startup/", "https://desktop.local:38474", "https://desktop.local:38474/startup/"},
		{"other://localhost/startup/", "other://localhost", "other://localhost/startup/"},
	} {
		t.Run(scenario.target+"/"+scenario.origin, func(t *testing.T) {
			backend := &memoryBackend{state: initialState()}
			handler := newChineseStartupHandler(t, backend, startup.FileDialogs{})
			values := form(t, get(t, handler))
			values.Set("desktop_enabled", "1")
			request := httptest.NewRequest(http.MethodPost, scenario.target, strings.NewReader(values.Encode()))
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			request.Header.Set("Origin", scenario.origin)
			request.Header.Set("Referer", scenario.referer)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusForbidden || len(backend.saves) != 0 {
				t.Fatal("跨端口或任意协议请求不能修改本地设置")
			}
		})
	}
}

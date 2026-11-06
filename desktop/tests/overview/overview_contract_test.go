package overview_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/orzlee/displayKvm/desktop/app"
	"github.com/orzlee/displayKvm/desktop/i18n"
	"github.com/orzlee/displayKvm/desktop/tray"
	"github.com/orzlee/displayKvm/desktop/views/overview"
	"github.com/orzlee/displayKvm/internal/config"
	"github.com/orzlee/displayKvm/internal/model"
)

type fakeClient struct {
	mu                 sync.Mutex
	cfg                config.Config
	devices            []model.Device
	configurationError error
	enumerationError   error
	requests           []model.SwitchRequest
	enumerations       int
	configurations     int
	called             chan model.SwitchRequest
	release            chan struct{}
	result             func(model.SwitchRequest) (model.SwitchResult, error)
}

func (f *fakeClient) Configuration() (config.Config, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.configurations++
	return f.cfg, f.configurationError
}

func (f *fakeClient) Enumerate(context.Context) ([]model.Device, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enumerations++
	return f.devices, f.enumerationError
}

func (f *fakeClient) Switch(ctx context.Context, request model.SwitchRequest) (model.SwitchResult, error) {
	f.mu.Lock()
	f.requests = append(f.requests, request)
	f.mu.Unlock()
	f.called <- request
	if f.release != nil {
		select {
		case <-f.release:
		case <-ctx.Done():
			return model.SwitchResult{}, ctx.Err()
		}
	}
	return f.result(request)
}

func fixture() *fakeClient {
	cfg := config.Config{Version: config.CurrentVersion,
		Displays: []model.Display{
			{ID: "left", Name: "主显示器", Binding: &model.Binding{Path: "left-path"}},
			{ID: "right", Name: "副显示器", Binding: &model.Binding{Path: "right-path"}},
		},
		InputSchemes: []model.InputScheme{{ID: "input", Protocol: model.ProtocolVCP, VCPCode: 0x60, Value: 17}},
		Targets:      []model.Target{{ID: "work", Name: "工作电脑", Inputs: []model.TargetInput{{DisplayID: "left", SchemeID: "input"}, {DisplayID: "right", SchemeID: "input"}}}},
		Groups:       []model.Group{{ID: "desk", Name: "整个桌面", DisplayIDs: []model.DisplayID{"left", "right"}}},
	}
	f := &fakeClient{cfg: cfg, devices: []model.Device{
		{BackendID: "temporary-left", Identity: model.IdentityHints{Path: "left-path"}},
		{BackendID: "temporary-right", Identity: model.IdentityHints{Path: "right-path"}},
	}, called: make(chan model.SwitchRequest, 8)}
	f.result = func(r model.SwitchRequest) (model.SwitchResult, error) {
		ids := r.DisplayIDs
		if r.GroupID != "" {
			ids = []model.DisplayID{"left", "right"}
		}
		result := model.SwitchResult{RequestID: r.ID}
		for _, id := range ids {
			sent := time.Now()
			value := uint16(17)
			result.Displays = append(result.Displays, model.DisplayResult{DisplayID: id, State: model.StateConfirmed, DesiredValue: value, SentAt: &sent, Observation: &model.InputObservation{Value: &value, ObservedAt: sent.Add(time.Millisecond)}})
		}
		return result, nil
	}
	return f
}

func connected(t *testing.T, f *fakeClient) (*app.Service, http.Handler) {
	t.Helper()
	s := chineseService(t, func() (app.Client, error) { return f, nil })
	t.Cleanup(s.Close)
	if err := s.Reconnect(context.Background()); err != nil {
		t.Fatal(err)
	}
	return s, overview.NewHandler(s)
}

func chineseService(t *testing.T, connect app.Connector) *app.Service {
	t.Helper()
	language, err := i18n.New("zh-CN", nil)
	if err != nil {
		t.Fatal(err)
	}
	return app.NewWithLanguage(connect, language)
}

func request(t *testing.T, h http.Handler, action string, body any, want int) app.View {
	t.Helper()
	var data []byte
	if body != nil {
		var err error
		data, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	method := http.MethodPost
	if action == "status" {
		method = http.MethodGet
	}
	r := httptest.NewRequest(method, "/client/"+action, bytes.NewReader(data))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != want {
		t.Fatalf("%s: status=%d body=%s", action, w.Code, w.Body.String())
	}
	var response struct {
		View  app.View `json:"view"`
		Error string   `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("客户端状态不得缓存")
	}
	return response.View
}

func selection(s *app.Service, group bool) app.Selection {
	v := s.Snapshot()
	choice := app.Selection{Kind: "display", DisplayID: "left", TargetID: "work", Generation: v.Generation}
	if group {
		choice.Kind, choice.DisplayID, choice.GroupID = "group", "", "desk"
	}
	return choice
}

func completed(t *testing.T, s *app.Service) app.View {
	t.Helper()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		v := s.Snapshot()
		if !v.Busy {
			return v
		}
		select {
		case <-deadline.C:
			t.Fatal("等待真实service完成超时")
		case <-tick.C:
		}
	}
}

func TestOverviewContract(t *testing.T) {
	t.Run("单屏与分组均只发一次真实动作", func(t *testing.T) {
		for _, group := range []bool{false, true} {
			f := fixture()
			f.release = make(chan struct{})
			s, h := connected(t, f)
			choice := selection(s, group)
			view := request(t, h, "selection", choice, http.StatusOK)
			if !view.CanSwitch {
				t.Fatalf("合法选择不可执行: %s", view.BlockedReason)
			}
			view = request(t, h, "switch", choice, http.StatusAccepted)
			if !view.Busy || len(view.Results) == 0 || view.Results[0].Label != "等待结果" {
				t.Fatal("等待状态必须来自共享service")
			}
			select {
			case sent := <-f.called:
				if sent.TargetID != "work" || sent.ID == "" {
					t.Fatal(sent)
				}
				if group && (sent.GroupID != "desk" || len(sent.DisplayIDs) != 0) {
					t.Fatal("分组不能展开成多个请求")
				}
				if !group && (sent.GroupID != "" || len(sent.DisplayIDs) != 1 || sent.DisplayIDs[0] != "left") {
					t.Fatal("单屏不能用临时后端ID")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("切换未到达共享client")
			}
			request(t, h, "switch", choice, http.StatusConflict)
			close(f.release)
			view = completed(t, s)
			if view.Results[0].Label != "已读回目标输入" || view.Results[0].ManualConfirmed {
				t.Fatal("读回不等于人工看到画面")
			}
			request(t, h, "confirm", map[string]any{"requestID": view.RequestID, "displayID": "left"}, http.StatusOK)
			view = s.Snapshot()
			if !view.Results[0].ManualConfirmed || view.Results[0].State != "confirmed" {
				t.Fatal("人工确认只能独立标记，不能篡改读回")
			}
			request(t, h, "confirm", map[string]any{"requestID": "old-request", "displayID": "left"}, http.StatusConflict)
			f.mu.Lock()
			count := len(f.requests)
			f.mu.Unlock()
			if count != 1 {
				t.Fatalf("重复发送%d次", count)
			}
		}
	})

	t.Run("缺映射缺席与歧义仍可见且阻止发送", func(t *testing.T) {
		for _, kind := range []string{"mapping", "missing", "ambiguous"} {
			f := fixture()
			switch kind {
			case "mapping":
				f.cfg.Targets[0].Inputs = f.cfg.Targets[0].Inputs[:1]
			case "missing":
				f.devices = f.devices[:1]
			case "ambiguous":
				f.devices = append(f.devices, model.Device{BackendID: "another", Identity: model.IdentityHints{Path: "right-path"}})
			}
			s, h := connected(t, f)
			v := request(t, h, "selection", selection(s, true), http.StatusOK)
			if len(v.Displays) != 2 || v.CanSwitch || v.BlockedReason == "" {
				t.Fatal("不能隐藏不可用屏或允许猜测映射")
			}
			request(t, h, "switch", selection(s, true), http.StatusConflict)
			if len(f.requests) != 0 {
				t.Fatal("预检失败仍发送了请求")
			}
		}
	})

	t.Run("逐屏发送确认未知失败与部分结果保留", func(t *testing.T) {
		for _, state := range []model.ExecutionState{model.StateSent, model.StateUnknown, model.StateFailed} {
			f := fixture()
			base := f.result
			failure := model.NewError(model.CodeUnreachable, "副显示器无法确认", errors.New("backend readback timed out"))
			// 使用非空字段和可重试标记，避免零值相等掩盖原始诊断字段丢失。
			failure.Field, failure.Retryable = "displays.right", true
			f.result = func(r model.SwitchRequest) (model.SwitchResult, error) {
				result, _ := base(r)
				row := &result.Displays[1]
				row.State, row.Observation = state, nil
				if state != model.StateSent {
					row.Error = failure
				}
				return result, nil
			}
			s, h := connected(t, f)
			request(t, h, "switch", selection(s, true), http.StatusAccepted)
			v := completed(t, s)
			if len(v.Results) != 2 || v.Results[0].State != "confirmed" || v.Results[1].State != string(state) {
				t.Fatal("逐屏结果丢失")
			}
			if state == model.StateSent && v.Results[1].Label != "已发送切换指令" {
				t.Fatal("发送不能冒称画面成功")
			}
			if state != model.StateSent {
				if v.Summary != s.Language().Snapshot().Text("app.summary.partial", nil) {
					t.Fatal("部分结果摘要必须使用当前语言，不能冒称全部完成")
				}
				diagnostic := v.Results[1].Diagnostic
				if diagnostic == nil || diagnostic.Code != failure.Code || diagnostic.Message != failure.Message || diagnostic.Field != failure.Field || diagnostic.Retryable != failure.Retryable || diagnostic.Detail != failure.Error() {
					t.Fatal("部分失败必须保留原诊断的代码、消息、字段、可重试性和完整详情")
				}
			}
		}
		f := fixture()
		base := f.result
		f.result = func(r model.SwitchRequest) (model.SwitchResult, error) {
			result, _ := base(r)
			result.Displays = result.Displays[:1]
			return result, errors.New("连接中断")
		}
		s, h := connected(t, f)
		request(t, h, "switch", selection(s, true), http.StatusAccepted)
		v := completed(t, s)
		if v.Results[0].State != "confirmed" || v.Results[1].State != "unknown" {
			t.Fatal("部分证据不能被整体错误覆盖")
		}
	})

	t.Run("重连刷新快照拒绝旧选择且从不重放", func(t *testing.T) {
		first, next := fixture(), fixture()
		first.result = func(model.SwitchRequest) (model.SwitchResult, error) {
			return model.SwitchResult{}, errors.New("响应中断")
		}
		next.cfg.Targets[0].Name = "更新后的工作电脑"
		connections := 0
		s := chineseService(t, func() (app.Client, error) {
			connections++
			if connections == 1 {
				return first, nil
			}
			return next, nil
		})
		defer s.Close()
		h := overview.NewHandler(s)
		request(t, h, "connect", struct{}{}, http.StatusOK)
		old := selection(s, false)
		request(t, h, "switch", old, http.StatusAccepted)
		v := completed(t, s)
		if v.Results[0].State != "unknown" {
			t.Fatal("断线后不能断言未发送")
		}
		request(t, h, "connect", struct{}{}, http.StatusOK)
		v = s.Snapshot()
		if v.Selection.TargetID != "" || v.Targets[0].Name != "更新后的工作电脑" || v.Generation == old.Generation {
			t.Fatal("重连必须刷新配置和选择")
		}
		request(t, h, "switch", old, http.StatusConflict)
		if len(first.requests) != 1 || len(next.requests) != 0 {
			t.Fatal("重连重放了切换")
		}
	})

	t.Run("离线和配置读取失败不回退空配置", func(t *testing.T) {
		s := chineseService(t, func() (app.Client, error) { return nil, errors.New("后台未启动") })
		defer s.Close()
		v := request(t, overview.NewHandler(s), "connect", struct{}{}, http.StatusServiceUnavailable)
		if v.Connected || v.CanSwitch || v.ConnectionMessage == "" {
			t.Fatal("离线状态不明确")
		}
		f := fixture()
		f.configurationError = errors.New("本轮快照不可用")
		other := chineseService(t, func() (app.Client, error) { return f, nil })
		defer other.Close()
		v = request(t, overview.NewHandler(other), "connect", struct{}{}, http.StatusServiceUnavailable)
		if v.Connected || len(v.Targets) != 0 || f.enumerations != 0 {
			t.Fatal("缺失快照不能拼出可操作列表")
		}
	})

	t.Run("切换前发现过期配置不发请求", func(t *testing.T) {
		f := fixture()
		s, h := connected(t, f)
		f.mu.Lock()
		f.configurationError = errors.New("后台已重启")
		f.mu.Unlock()
		request(t, h, "switch", selection(s, false), http.StatusAccepted)
		v := completed(t, s)
		if v.Connected || v.Results[0].State != "failed" || len(f.requests) != 0 {
			t.Fatal("旧快照不得发送至重启后台")
		}
	})

	t.Run("窗口托盘与外观复用真实service", func(t *testing.T) {
		f := fixture()
		f.release = make(chan struct{})
		s, h := connected(t, f)
		beforeEnums, beforeConfig := f.enumerations, f.configurations
		v := request(t, h, "appearance", map[string]string{"mode": "light"}, http.StatusOK)
		if v.Appearance != "light" || f.enumerations != beforeEnums || f.configurations != beforeConfig || len(f.requests) != 0 {
			t.Fatal("外观动作触发后台调用")
		}
		shown, quit := false, false
		controller := tray.New(s, func() { shown = true }, func() { quit = true })
		var action tray.Entry
		for _, entry := range controller.Entries() {
			if entry.Kind == "switch" && entry.Selection.Kind == "group" && entry.Enabled {
				action = entry
				break
			}
		}
		if action.Kind == "" {
			t.Fatal("缺少明确分组和目标的托盘入口")
		}
		if err := controller.Activate(context.Background(), action); err != nil {
			t.Fatal(err)
		}
		request(t, h, "switch", selection(s, false), http.StatusConflict)
		close(f.release)
		completed(t, s)
		if len(f.requests) != 1 {
			t.Fatal("托盘与窗口未共用请求门禁")
		}
		if err := controller.Activate(context.Background(), tray.Entry{Kind: "show"}); err != nil || !shown {
			t.Fatal("打开窗口入口未接通")
		}
		if err := controller.Activate(context.Background(), tray.Entry{Kind: "quit"}); err != nil || !quit {
			t.Fatal("退出桌面入口未接通")
		}
		if !strings.Contains(s.Snapshot().TrayMessage, "主窗口") {
			t.Fatal("托盘未确认可用时须提供窗口退化提示")
		}
	})

	t.Run("取消窗口请求不会撤销或重发已启动动作", func(t *testing.T) {
		f := fixture()
		f.release = make(chan struct{})
		s, h := connected(t, f)
		data, _ := json.Marshal(selection(s, false))
		ctx, cancel := context.WithCancel(context.Background())
		r := httptest.NewRequest(http.MethodPost, "/client/switch", bytes.NewReader(data)).WithContext(ctx)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		cancel()
		close(f.release)
		v := completed(t, s)
		if w.Code != http.StatusAccepted || v.Results[0].State != "confirmed" || len(f.requests) != 1 {
			t.Fatal("本地读取生命周期影响了后台动作")
		}
	})

	t.Run("生产壳实际接线且原生宿主隔离", func(t *testing.T) {
		checks := map[string][]string{
			"../../main.go":             {"host.Run(assets,", "//go:embed all:frontend"},
			"../../app/host/host.go":    {"coreclient.Connect(\"\")", "overview.NewHandler(service)", "tray.NewWithLanguage(service", "SystemTray.New()", "SetMenu(menu)", "BundledAssetFileServer(assets)", "OnShutdown", "ApplicationStarted"},
			"../../frontend/app.js":     {"/client/", "\"switch\"", "\"selection\"", "\"appearance\"", "\"confirm\"", "\"connect\""},
			"../../frontend/index.html": {"switch-form", "scope-kind", "target-choice", "display-list", "result-list", "aria-live"},
			"../../go.mod":              {"require github.com/orzlee/displayKvm v0.0.0", "replace github.com/orzlee/displayKvm => ..", "github.com/wailsapp/wails/v3 v3.1.0"},
		}
		for path, snippets := range checks {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			for _, snippet := range snippets {
				if !strings.Contains(string(data), snippet) {
					t.Errorf("生产接线缺失 %s: %s", path, snippet)
				}
			}
		}
		for _, path := range []string{"../../app/service.go", "../../app/client/client.go", "../../app/presentation/view.go", "../../views/overview/overview.go", "../../tray/tray.go"} {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data), "wailsapp") {
				t.Errorf("可测业务层混入原生宿主: %s", path)
			}
		}
	})
}

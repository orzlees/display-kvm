package rules_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/orzlee/displayKvm/desktop/app"
	"github.com/orzlee/displayKvm/desktop/i18n"
	"github.com/orzlee/displayKvm/desktop/views/rules"
	"github.com/orzlee/displayKvm/internal/automation"
	"github.com/orzlee/displayKvm/internal/config"
	"github.com/orzlee/displayKvm/internal/daemon"
	"github.com/orzlee/displayKvm/internal/model"
)

// fakeClient替换网络和持久化边界，预览与记录仍委托同一个真实Engine，
// 避免在测试里复制匹配算法并让页面与核心一起产生相同错误。
type fakeClient struct {
	cfg             config.Config
	rules           []automation.Rule
	engine          *automation.Engine
	generation      model.RuntimeGeneration
	capabilities    []model.EventCapability
	writable        bool
	engineAvailable bool
	management      []model.ManagementRequest
	commits         map[model.RequestID]model.CommitStatus
	prepared        map[model.RequestID][]automation.Rule
	switches        int
	emits           int
	rescans         int
	applyLost       bool
	prepareLost     bool
	staleResponse   bool
	onScan          func()
}

func encode(t *testing.T, value any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func ruleDocument(t *testing.T, entries []automation.Rule) json.RawMessage {
	t.Helper()
	return encode(t, struct {
		Version int               `json:"version"`
		Rules   []automation.Rule `json:"rules"`
	}{1, entries})
}

func (f *fakeClient) Configuration() (config.Config, error) { return f.cfg, nil }
func (f *fakeClient) Enumerate(context.Context) ([]model.Device, error) {
	return []model.Device{{BackendID: "left-backend", Identity: model.IdentityHints{Path: "left-path"}}}, nil
}
func (f *fakeClient) Switch(context.Context, model.SwitchRequest) (model.SwitchResult, error) {
	f.switches++
	return model.SwitchResult{}, errors.New("本测试禁止硬件写入")
}
func (f *fakeClient) EmitEvent(context.Context, model.Event) error {
	f.emits++
	return errors.New("本测试禁止事件提交")
}

func (f *fakeClient) newEngine() error {
	engine, err := automation.New(daemon.AutomationRuntime{Events: make(chan model.Event),
		Switch: f.Switch,
		Rescan: func(context.Context) (daemon.Baseline, error) {
			f.rescans++
			if f.onScan != nil {
				f.onScan()
			}
			return daemon.Baseline{}, errors.New("本测试禁止补扫")
		},
	}, f.cfg, f.rules)
	if err == nil {
		f.engine = engine
	}
	return err
}

func (f *fakeClient) snapshot() (model.ManagementSnapshot, error) {
	cfg, err := json.Marshal(f.cfg)
	if err != nil {
		return model.ManagementSnapshot{}, err
	}
	document, err := json.Marshal(struct {
		Version int               `json:"version"`
		Rules   []automation.Rule `json:"rules"`
	}{1, f.rules})
	if err != nil {
		return model.ManagementSnapshot{}, err
	}
	return model.ManagementSnapshot{Generation: f.generation, Configuration: cfg, Rules: document, RulesWritable: f.writable, EngineAvailable: f.engineAvailable, Capabilities: f.capabilities}, nil
}

func (f *fakeClient) Management(_ context.Context, request model.ManagementRequest) (model.ManagementResponse, error) {
	f.management = append(f.management, request)
	if err := request.Validate(); err != nil {
		return model.ManagementResponse{}, err
	}
	response := model.ManagementResponse{Generation: f.generation}
	if request.Action != model.ManagementSnapshotAction && request.Action != model.ManagementCommitStatus && request.ExpectedGeneration != f.generation {
		return response, errors.New("运行代已过期")
	}
	switch request.Action {
	case model.ManagementSnapshotAction:
		snapshot, err := f.snapshot()
		response.Snapshot = &snapshot
		return response, err
	case model.ManagementPrepareRules:
		entries, err := automation.LoadRules(bytes.NewReader(request.Data))
		if err == nil {
			err = automation.ValidateRules(f.cfg, entries)
		}
		if err != nil {
			return response, err
		}
		no := false
		commit := model.CommitStatus{ID: request.CommitID, Kind: model.ManagementPrepareRules, Phase: model.CommitAccepted, PreparedGeneration: f.generation, Saved: &no, Effective: &no}
		f.commits[request.CommitID], f.prepared[request.CommitID] = commit, entries
		response.Commit = &commit
		if f.prepareLost {
			return response, errors.New("模拟Prepare应答中断")
		}
	case model.ManagementApplyCommit:
		commit, ok := f.commits[request.CommitID]
		if !ok || !request.Confirmed {
			return response, errors.New("缺少已接收申请或明确确认")
		}
		if f.applyLost {
			commit.Phase, commit.Saved, commit.Effective = model.CommitUnknown, nil, nil
			f.commits[request.CommitID] = commit
			return response, errors.New("模拟Apply应答中断")
		}
		f.rules = f.prepared[request.CommitID]
		if err := f.newEngine(); err != nil {
			return response, err
		}
		yes := true
		f.generation = "generation-2"
		commit.Phase, commit.Saved, commit.Effective, commit.EffectiveGeneration = model.CommitApplied, &yes, &yes, f.generation
		f.commits[request.CommitID] = commit
		response.Generation, response.Commit = f.generation, &commit
	case model.ManagementCommitStatus:
		commit, ok := f.commits[request.CommitID]
		if !ok {
			return response, errors.New("原提交不存在")
		}
		response.Commit = &commit
	case model.ManagementPreviewRules:
		var data model.ManagementPreviewRulesData
		if err := model.DecodeJSON(request.Data, &data); err != nil {
			return response, err
		}
		decisions, err := f.engine.Preview(data.Event)
		if err != nil {
			return response, err
		}
		response.Data, err = json.Marshal(decisions)
		if err != nil {
			return response, err
		}
		if f.staleResponse {
			response.Generation = "generation-2"
		}
	case model.ManagementRuleRecords:
		var err error
		response.Data, err = json.Marshal(f.engine.Records())
		if err != nil {
			return response, err
		}
	default:
		return response, errors.New("不支持的测试管理操作")
	}
	return response, nil
}

func fixture(t *testing.T) (*fakeClient, *app.Service, http.Handler) {
	t.Helper()
	f := &fakeClient{generation: "generation-1", writable: true, engineAvailable: true,
		commits: make(map[model.RequestID]model.CommitStatus), prepared: make(map[model.RequestID][]automation.Rule),
		cfg: config.Config{Version: config.CurrentVersion,
			Displays:     []model.Display{{ID: "left", Name: "主显示器", Binding: &model.Binding{Path: "left-path"}}},
			InputSchemes: []model.InputScheme{{ID: "input", Protocol: model.ProtocolVCP, VCPCode: 0x60, Value: 17}},
			Targets:      []model.Target{{ID: "work", Name: "工作电脑", Inputs: []model.TargetInput{{DisplayID: "left", SchemeID: "input"}}}},
			Groups:       []model.Group{{ID: "desk", Name: "桌面", DisplayIDs: []model.DisplayID{"left"}}}},
		capabilities: []model.EventCapability{
			{Kind: model.EventCustom, Available: true, Reason: "仅接收自定义事件，接收不代表切换", FilterFields: []string{"name", "source"}},
			{Kind: model.EventUSBAdded, Available: false, Reason: "本机USB来源未启动", FilterFields: []string{"usb.vendor_id"}},
			{Kind: model.EventResume, Available: false, Reason: "本平台未提供唤醒来源"},
		},
		rules: []automation.Rule{
			{ID: "work-rule", Enabled: true, Priority: 10, When: automation.Condition{Kind: model.EventCustom, Name: "work"}, Action: model.SwitchRequest{TargetID: "work", GroupID: "desk"}},
			{ID: "disabled-rule", Enabled: false, When: automation.Condition{Kind: model.EventCustom, Name: "work"}, Action: model.SwitchRequest{TargetID: "work"}},
		},
	}
	if err := f.newEngine(); err != nil {
		t.Fatal(err)
	}
	language, err := i18n.New("zh-CN", nil)
	if err != nil {
		t.Fatal(err)
	}
	service := app.NewWithLanguage(func() (app.Client, error) { return f, nil }, language)
	t.Cleanup(service.Close)
	if err := service.Reconnect(context.Background()); err != nil {
		t.Fatal(err)
	}
	return f, service, rules.NewHandler(service)
}

func TestSettingsProductRulesLanguageProjection(t *testing.T) {
	f, service, handler := fixture(t)
	if err := service.Language().SetPreference("en"); err != nil {
		t.Fatal(err)
	}
	html := string(request(t, handler, "", nil, http.StatusOK))
	for _, text := range []string{`lang="en"`, `data-i18n="rules.title"`, `id="desktop-catalog"`, `/desktop-i18n.js`, "Event rules"} {
		if !strings.Contains(html, text) {
			t.Errorf("语言投影缺少%s", text)
		}
	}
	file, err := rules.Page().Resources.Open("assets/rules.js")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	script, err := io.ReadAll(file)
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"desktopI18n.text", "dataset.i18n", `window.addEventListener("desktop:translated"`} {
		if !bytes.Contains(script, []byte(text)) {
			t.Errorf("动态文案缺少原位翻译标记%s", text)
		}
	}
	if bytes.Contains(script, []byte("location.reload")) || bytes.Contains(script, []byte("navigator.language")) {
		t.Fatal("语言更新不得重载草稿或另行判断系统语言")
	}
	start := bytes.Index(script, []byte(`window.addEventListener("desktop:translated"`))
	if start >= 0 {
		end := bytes.Index(script[start:], []byte("\n  });"))
		if end < 0 {
			t.Fatal("原位语言事件处理未闭合")
		}
		for _, mutation := range []string{"replaceChildren", "openEditor(", "renderCapabilities(", "loadState(", ".value =", ".selected ="} {
			if bytes.Contains(script[start:start+end], []byte(mutation)) {
				t.Errorf("语言事件不应重建编辑器或改写草稿：%s", mutation)
			}
		}
	}
	if len(f.management) != 0 {
		t.Fatal("页面语言投影不应调用后台管理")
	}
	noWrites(t, f)
}

func request(t *testing.T, handler http.Handler, path string, body any, want int) []byte {
	t.Helper()
	var data []byte
	method := http.MethodGet
	if body != nil {
		data = encode(t, body)
		method = http.MethodPost
	}
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(method, "/rules/"+path, bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(recorder, req)
	if recorder.Code != want {
		t.Fatalf("%s: status=%d want=%d body=%s", path, recorder.Code, want, recorder.Body.String())
	}
	if recorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("规则状态及编辑响应不得缓存")
	}
	return recorder.Body.Bytes()
}

func decode[T any](t *testing.T, data []byte) T {
	t.Helper()
	var value T
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func draft(t *testing.T, f *fakeClient, entries []automation.Rule) map[string]any {
	t.Helper()
	return map[string]any{"generation": f.generation, "document": ruleDocument(t, entries)}
}

func count(f *fakeClient, action model.ManagementAction) int {
	n := 0
	for _, request := range f.management {
		if request.Action == action {
			n++
		}
	}
	return n
}

func noWrites(t *testing.T, f *fakeClient) {
	t.Helper()
	if f.switches != 0 || f.emits != 0 || f.rescans != 0 {
		t.Fatalf("出现副作用：Switch=%d EmitEvent=%d Rescan=%d", f.switches, f.emits, f.rescans)
	}
}

func TestRulesContract(t *testing.T) {
	t.Run("页面通过纯Go注册且资源独立", func(t *testing.T) {
		_, service, _ := fixture(t)
		page := rules.Page()
		if page.Route != "/rules/" || page.ResourcePath != "/rules-assets/" {
			t.Fatalf("页面装配错误：%+v", page)
		}
		html := string(request(t, page.Handler(service), "", nil, http.StatusOK))
		for _, text := range []string{"当什么发生", "满足哪些条件", "执行什么动作", "只检查，不切屏", "最近运行结果", "/rules-assets/assets/rules.js"} {
			if !strings.Contains(html, text) {
				t.Fatalf("页面缺少%s", text)
			}
		}
		for _, path := range []string{"assets/rules.js", "assets/rules.css"} {
			file, err := page.Resources.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			content, readErr := io.ReadAll(file)
			closeErr := file.Close()
			if readErr != nil || closeErr != nil || len(content) == 0 {
				t.Fatalf("资源%s不可读：%v %v", path, readErr, closeErr)
			}
		}
		if err := service.SetAppearance("light"); err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(request(t, page.Handler(service), "", nil, http.StatusOK), []byte(`data-theme="light"`)) {
			t.Fatal("页面没有继承共享Service的外观选择")
		}
	})
	t.Run("能力按Available判断且保留原因和字段", func(t *testing.T) {
		f, _, handler := fixture(t)
		view := decode[struct {
			Capabilities []model.EventCapability `json:"capabilities"`
			Rules        []automation.Rule       `json:"rules"`
			Records      []automation.Record     `json:"records"`
		}](t, request(t, handler, "state", nil, http.StatusOK))
		if !reflect.DeepEqual(view.Capabilities, f.capabilities) || !view.Capabilities[0].Available || view.Capabilities[0].Reason == "" {
			t.Fatal("能力、原因或筛选字段未完整投影")
		}
		if !reflect.DeepEqual(view.Rules, f.rules) || !reflect.DeepEqual(view.Records, f.engine.Records()) {
			t.Fatal("规则或记录未来自后台")
		}
		entries := decode[[]automation.Rule](t, encode(t, f.rules))
		entries[0].When.Source = "desktop"
		request(t, handler, "validate", draft(t, f, entries), http.StatusOK)
		entries[0].When.Attributes = map[string]string{"mode": "work"}
		if !bytes.Contains(request(t, handler, "validate", draft(t, f, entries), http.StatusBadRequest), []byte("attributes.mode")) {
			t.Fatal("不支持的筛选字段应给出原因")
		}
		vendor := uint16(1234)
		entries[0].When = automation.Condition{Kind: model.EventUSBAdded, USB: &automation.USBMatch{VendorID: &vendor}}
		if !bytes.Contains(request(t, handler, "validate", draft(t, f, entries), http.StatusBadRequest), []byte("本机USB来源未启动")) {
			t.Fatal("不可用能力原因不可见")
		}
		if count(f, model.ManagementPrepareRules) != 0 {
			t.Fatal("校验不得创建保存申请")
		}
		noWrites(t, f)
	})
	t.Run("有效保存先接收再明确应用且保持v1", func(t *testing.T) {
		f, _, handler := fixture(t)
		entries := decode[[]automation.Rule](t, encode(t, f.rules))
		entries[0].CooldownMillis = 500
		body := draft(t, f, entries)
		generated := decode[struct {
			ID model.RequestID `json:"commit_id"`
		}](t, request(t, handler, "commit-id", map[string]any{"generation": f.generation}, http.StatusOK))
		body["commit_id"] = generated.ID
		before := ruleDocument(t, f.rules)
		response := decode[model.ManagementResponse](t, request(t, handler, "prepare", body, http.StatusOK))
		if response.Commit == nil || response.Commit.Phase != model.CommitAccepted || *response.Commit.Saved || *response.Commit.Effective {
			t.Fatal("Prepare不能显示已保存或已生效")
		}
		if !bytes.Equal(before, ruleDocument(t, f.rules)) || count(f, model.ManagementApplyCommit) != 0 {
			t.Fatal("Prepare修改了运行规则或自动Apply")
		}
		var sent model.ManagementRequest
		for _, entry := range f.management {
			if entry.Action == model.ManagementPrepareRules {
				sent = entry
			}
		}
		if !bytes.Equal(sent.Data, ruleDocument(t, entries)) {
			t.Fatal("规则保存没有沿用v1原文结构")
		}
		request(t, handler, "apply", map[string]any{"generation": f.generation, "commit_id": generated.ID}, http.StatusBadRequest)
		if count(f, model.ManagementApplyCommit) != 0 {
			t.Fatal("没有明确确认不能Apply")
		}
		response = decode[model.ManagementResponse](t, request(t, handler, "apply", map[string]any{"generation": f.generation, "commit_id": generated.ID, "confirmed": true}, http.StatusOK))
		if response.Commit == nil || response.Commit.Phase != model.CommitApplied || !*response.Commit.Saved || !*response.Commit.Effective || !reflect.DeepEqual(f.rules, entries) {
			t.Fatal("保存与生效结果不正确")
		}
		if count(f, model.ManagementPrepareRules) != 1 || count(f, model.ManagementApplyCommit) != 1 {
			t.Fatal("每阶段应只发送一次")
		}
		noWrites(t, f)
	})
	t.Run("非法规则只读规则和过期运行代禁止提交", func(t *testing.T) {
		f, _, handler := fixture(t)
		entries := decode[[]automation.Rule](t, encode(t, f.rules))
		entries[0].Action.TargetID = "missing"
		request(t, handler, "prepare", draft(t, f, entries), http.StatusBadRequest)
		body := draft(t, f, f.rules)
		body["generation"] = "old-generation"
		request(t, handler, "prepare", body, http.StatusBadRequest)
		f.writable = false
		request(t, handler, "prepare", draft(t, f, f.rules), http.StatusBadRequest)
		f.writable = true
		body = draft(t, f, f.rules)
		body["commit_id"] = "unbound-id"
		request(t, handler, "prepare", body, http.StatusBadRequest)
		body["document"] = json.RawMessage(`{"version":2,"rules":[]}`)
		request(t, handler, "validate", body, http.StatusBadRequest)
		if count(f, model.ManagementPrepareRules) != 0 || count(f, model.ManagementApplyCommit) != 0 {
			t.Fatal("非法草稿不得进入保存链")
		}
		noWrites(t, f)
	})
	t.Run("取消草稿不提交且启停复用同一保存链", func(t *testing.T) {
		f, _, handler := fixture(t)
		before := ruleDocument(t, f.rules)
		entries := decode[[]automation.Rule](t, encode(t, f.rules))
		entries[0].Enabled = false
		request(t, handler, "validate", draft(t, f, entries), http.StatusOK)
		calls := len(f.management)
		request(t, handler, "cancel", struct{}{}, http.StatusOK)
		if len(f.management) != calls || !bytes.Equal(before, ruleDocument(t, f.rules)) {
			t.Fatal("取消草稿不应请求后台或改变规则")
		}
		body := draft(t, f, entries)
		body["commit_id"] = "commit-generation-1-toggle"
		request(t, handler, "prepare", body, http.StatusOK)
		if !f.rules[0].Enabled {
			t.Fatal("启停不应在确认前生效")
		}
		request(t, handler, "apply", map[string]any{"generation": f.generation, "commit_id": body["commit_id"], "confirmed": true}, http.StatusOK)
		if f.rules[0].Enabled || count(f, model.ManagementPrepareRules) != 1 || count(f, model.ManagementApplyCommit) != 1 {
			t.Fatal("启停未通过同一保存链")
		}
		noWrites(t, f)
	})
	t.Run("已有状态规则保留且不可用事件不得重新启用", func(t *testing.T) {
		f, _, handler := fixture(t)
		present, vendor := true, uint16(1234)
		f.rules = append(f.rules,
			automation.Rule{ID: "usb-state", Enabled: false, When: automation.Condition{USBPresent: &present, USB: &automation.USBMatch{VendorID: &vendor}}, Action: model.SwitchRequest{TargetID: "work"}},
			automation.Rule{ID: "old-resume", Enabled: false, When: automation.Condition{Kind: model.EventResume}, Action: model.SwitchRequest{TargetID: "work"}})
		entries := decode[[]automation.Rule](t, encode(t, f.rules))
		entries[2].Enabled = true
		entries[2].Action.GroupID = "desk"
		request(t, handler, "validate", draft(t, f, entries), http.StatusOK)
		entries[3].Enabled = true
		request(t, handler, "validate", draft(t, f, entries), http.StatusBadRequest)
		entries[3].Enabled = false
		entries = append(entries, automation.Rule{ID: "new-state", When: entries[2].When, Action: model.SwitchRequest{TargetID: "work"}})
		request(t, handler, "validate", draft(t, f, entries), http.StatusBadRequest)
		if count(f, model.ManagementPrepareRules) != 0 {
			t.Fatal("能力检查不得提交")
		}
		noWrites(t, f)
	})
	t.Run("其他页面提交及过期申请不得由规则页应用", func(t *testing.T) {
		f, _, handler := fixture(t)
		foreignID := model.RequestID("commit-generation-1-foreign")
		f.commits[foreignID] = model.CommitStatus{ID: foreignID, Kind: model.ManagementPrepareConfig, Phase: model.CommitAccepted, PreparedGeneration: f.generation}
		request(t, handler, "apply", map[string]any{"generation": f.generation, "commit_id": foreignID, "confirmed": true}, http.StatusServiceUnavailable)
		id := model.RequestID("commit-generation-1-old")
		f.commits[id] = model.CommitStatus{ID: id, Kind: model.ManagementPrepareRules, Phase: model.CommitAccepted, PreparedGeneration: f.generation}
		f.generation = "generation-2"
		request(t, handler, "apply", map[string]any{"generation": "generation-1", "commit_id": id, "confirmed": true}, http.StatusConflict)
		if count(f, model.ManagementApplyCommit) != 0 {
			t.Fatal("跨页面或过期申请进入了Apply")
		}
		noWrites(t, f)
	})
	t.Run("预览与真实Engine完整一致且三类副作用为零", func(t *testing.T) {
		f, _, handler := fixture(t)
		beforeRules := ruleDocument(t, f.rules)
		beforeRecords := encode(t, f.engine.Records())
		for _, name := range []string{"work", "unmatched"} {
			event := model.Event{ID: "preview-" + name, Kind: model.EventCustom, Source: "desktop-check", Name: name, ObservedAt: time.Now()}
			want, err := f.engine.Preview(event)
			if err != nil {
				t.Fatal(err)
			}
			response := decode[struct {
				Decisions []automation.Decision `json:"decisions"`
			}](t, request(t, handler, "preview", map[string]any{"generation": f.generation, "event": event}, http.StatusOK))
			if !reflect.DeepEqual(response.Decisions, want) {
				t.Fatalf("页面裁决与真实Engine不同：got=%+v want=%+v", response.Decisions, want)
			}
			for _, decision := range response.Decisions {
				if decision.Request.ID != "" {
					t.Fatal("预览不应产生执行请求ID")
				}
			}
		}
		if !bytes.Equal(beforeRules, ruleDocument(t, f.rules)) || !bytes.Equal(beforeRecords, encode(t, f.engine.Records())) {
			t.Fatal("预览改变了规则或运行记录")
		}
		if count(f, model.ManagementPrepareRules) != 0 || count(f, model.ManagementApplyCommit) != 0 || count(f, model.ManagementPreviewRules) != 2 {
			t.Fatal("预览必须只委托真实规则预览入口")
		}
		noWrites(t, f)
	})
	t.Run("最近结果逐项来自真实Engine已有记录", func(t *testing.T) {
		f, _, handler := fixture(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		// 显式运行核心以生成真实补扫失败记录；替身不会访问设备。完成后清零准备阶段计数，
		// 后续断言只衡量页面读取是否重新触发补扫、事件或切换，避免用空记录作一致性证据。
		f.onScan = cancel
		if err := f.engine.Run(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("核心停止结果=%v", err)
		}
		f.onScan = nil
		if f.rescans != 1 {
			t.Fatalf("测试准备应补扫一次，实际%d", f.rescans)
		}
		f.rescans = 0
		before := f.engine.Records()
		if len(before) == 0 {
			t.Fatal("没有生成真实运行记录")
		}
		view := decode[struct {
			Records []automation.Record `json:"records"`
		}](t, request(t, handler, "state", nil, http.StatusOK))
		if !bytes.Equal(encode(t, view.Records), encode(t, before)) || !reflect.DeepEqual(f.engine.Records(), before) {
			t.Fatal("页面读取改变或丢失了核心记录")
		}
		if count(f, model.ManagementRuleRecords) != 1 {
			t.Fatal("应从后台读取一次记录")
		}
		noWrites(t, f)
	})
	t.Run("预览拒绝非法事件不可用引擎及过期结果", func(t *testing.T) {
		f, _, handler := fixture(t)
		event := model.Event{ID: "preview", Kind: model.EventCustom, Source: "desktop-check", Name: "work", ObservedAt: time.Now()}
		request(t, handler, "preview", map[string]any{"generation": f.generation, "event": model.Event{}}, http.StatusBadRequest)
		request(t, handler, "preview", map[string]any{"generation": "old", "event": event}, http.StatusConflict)
		f.engineAvailable = false
		request(t, handler, "preview", map[string]any{"generation": f.generation, "event": event}, http.StatusConflict)
		f.engineAvailable = true
		f.staleResponse = true
		request(t, handler, "preview", map[string]any{"generation": f.generation, "event": event}, http.StatusServiceUnavailable)
		noWrites(t, f)
	})
	t.Run("Prepare与Apply应答中断只查询原ID", func(t *testing.T) {
		for _, phase := range []string{"prepare", "apply"} {
			t.Run(phase, func(t *testing.T) {
				f, service, handler := fixture(t)
				f.prepareLost = phase == "prepare"
				f.applyLost = phase == "apply"
				id := model.RequestID("commit-generation-1-lost-" + phase)
				body := draft(t, f, f.rules)
				body["commit_id"] = id
				want := http.StatusOK
				if f.prepareLost {
					want = http.StatusServiceUnavailable
				}
				request(t, handler, "prepare", body, want)
				if !f.prepareLost {
					request(t, handler, "apply", map[string]any{"generation": f.generation, "commit_id": id, "confirmed": true}, http.StatusServiceUnavailable)
				}
				if err := service.Reconnect(context.Background()); err != nil {
					t.Fatal(err)
				}
				response := decode[model.ManagementResponse](t, request(t, handler, "commit", map[string]any{"commit_id": id}, http.StatusOK))
				if response.Commit == nil || response.Commit.ID != id {
					t.Fatal("没有查询原提交ID")
				}
				if f.applyLost && (response.Commit.Phase != model.CommitUnknown || response.Commit.Saved != nil || response.Commit.Effective != nil) {
					t.Fatal("未知结果不能改写为保存成功或失败")
				}
				wantQueries := 1
				if f.applyLost {
					wantQueries++
				}
				if count(f, model.ManagementPrepareRules) != 1 || count(f, model.ManagementCommitStatus) != wantQueries {
					t.Fatal("中断后发生Prepare重放")
				}
				wantApply := 0
				if f.applyLost {
					wantApply = 1
				}
				if count(f, model.ManagementApplyCommit) != wantApply {
					t.Fatal("中断后发生Apply重放")
				}
				noWrites(t, f)
			})
		}
	})
	t.Run("未知字段重复键和超限请求均不进入管理操作", func(t *testing.T) {
		f, _, handler := fixture(t)
		for _, body := range []string{`{"generation":"generation-1","document":{"version":1,"rules":[]},"extra":true}`, `{"generation":"generation-1","generation":"other","document":{"version":1,"rules":[]}}`, strings.Repeat(" ", model.MaxMessageBytes+1)} {
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/rules/prepare", strings.NewReader(body)))
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("非法请求状态=%d", recorder.Code)
			}
		}
		if len(f.management) != 0 {
			t.Fatal("非法JSON不应调用管理服务")
		}
		noWrites(t, f)
	})
}

package schedules_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/orzlee/displayKvm/desktop/app"
	"github.com/orzlee/displayKvm/desktop/i18n"
	"github.com/orzlee/displayKvm/desktop/views/schedules"
	"github.com/orzlee/displayKvm/internal/config"
	"github.com/orzlee/displayKvm/internal/model"
	"github.com/orzlee/displayKvm/internal/scheduler"
)

// 只替换公共客户端边界；草稿、核心校验、表单防重和保存状态均运行生产处理器。
// 核心下次触发使用独立给定值，不能由页面按当前时间算出另一份“期望”。
type scheduleClient struct {
	cfg            config.Config
	generation     model.RuntimeGeneration
	document       json.RawMessage
	statuses       []scheduler.ScheduleStatus
	writable       bool
	mode           string
	requests       []model.ManagementRequest
	prepared       []scheduler.Schedule
	commit         *model.CommitStatus
	hardwareWrites int
}

func (f *scheduleClient) Configuration() (config.Config, error)             { return f.cfg, nil }
func (f *scheduleClient) Enumerate(context.Context) ([]model.Device, error) { return nil, nil }
func (f *scheduleClient) Switch(context.Context, model.SwitchRequest) (model.SwitchResult, error) {
	f.hardwareWrites++
	return model.SwitchResult{}, model.NewError(model.CodeUnsupportedOperation, "定时编辑不直接切换硬件", nil)
}
func scheduleBool(v bool) *bool { return &v }
func (f *scheduleClient) Management(_ context.Context, request model.ManagementRequest) (model.ManagementResponse, error) {
	f.requests = append(f.requests, request)
	response := model.ManagementResponse{Generation: f.generation}
	if err := request.Validate(); err != nil {
		return response, err
	}
	if request.Action != model.ManagementSnapshotAction && request.Action != model.ManagementCommitStatus && request.ExpectedGeneration != f.generation {
		return response, model.NewError(model.CodeInvalidRequest, "草稿运行代已过期", nil)
	}
	switch request.Action {
	case model.ManagementSnapshotAction:
		cfg, err := json.Marshal(f.cfg)
		if err != nil {
			return response, err
		}
		response.Snapshot = &model.ManagementSnapshot{Generation: f.generation, Configuration: cfg, Rules: json.RawMessage(`{"version":1,"rules":[]}`)}
	case model.ManagementSchedulesState:
		if f.mode == "legacy" {
			err := model.NewError(model.CodeUnsupportedOperation, "后台不支持定时编辑 <backend>&", nil)
			err.Field, err.Retryable = "schedules.<status>&", true
			return response, err
		}
		statuses, err := json.Marshal(struct {
			Items []scheduler.ScheduleStatus `json:"items"`
		}{f.statuses})
		if err != nil {
			return response, err
		}
		switch f.mode {
		case "statuses-array":
			statuses = []byte(`[]`)
		case "statuses-null":
			statuses = []byte(`{"items":null}`)
		case "statuses-missing":
			statuses = []byte(`{}`)
		case "statuses-unknown":
			statuses = []byte(`{"items":[],"typo":true}`)
		case "statuses-duplicate":
			statuses = []byte(`{"items":[],"items":[]}`)
		case "statuses-item-unknown":
			statuses = []byte(`{"items":[{"schedule":{"id":"work","enabled":true,"time":"01:30","timezone":"America/New_York"},"missed_policy":"skip","typo":true}]}`)
		}
		response.Data, err = json.Marshal(model.ManagementSchedulesData{Document: f.document, Writable: f.writable, Statuses: statuses, Timer: model.TimerRuntimeState{Configured: len(f.statuses) > 0, Reason: "恢复来源不可用"}})
		if err != nil {
			return response, err
		}
		if f.mode == "mixed-generation" {
			response.Generation = "run-other"
		}
	case model.ManagementPrepareSchedules:
		if !f.writable {
			return response, model.NewError(model.CodeInvalidRequest, "未指定日程保存位置", nil)
		}
		var err error
		f.prepared, err = scheduler.LoadSchedules(bytes.NewReader(request.Data))
		if err != nil {
			return response, err
		}
		f.commit = &model.CommitStatus{ID: request.CommitID, Kind: model.ManagementPrepareSchedules, Phase: model.CommitAccepted, PreparedGeneration: f.generation, Saved: scheduleBool(false), Effective: scheduleBool(false)}
		if f.mode == "prepare-lost" {
			return response, model.NewError(model.CodeTransport, "预检应答丢失", nil)
		}
		response.Commit = f.commit
	case model.ManagementApplyCommit:
		if f.commit == nil || request.CommitID != f.commit.ID {
			return response, model.NewError(model.CodeInvalidRequest, "提交ID不匹配", nil)
		}
		f.commit.Saved, f.commit.Effective = scheduleBool(true), scheduleBool(true)
		f.commit.Phase, f.commit.EffectiveGeneration = model.CommitApplied, "run-next"
		f.generation, response.Generation = "run-next", "run-next"
		if f.mode == "saved-only" {
			f.commit.Phase, f.commit.Effective = model.CommitFailed, scheduleBool(false)
			f.commit.Error = model.NewError(model.CodeIO, "已保存但新运行代启动失败", nil)
		}
		if f.mode == "apply-lost" {
			return response, model.NewError(model.CodeTransport, "提交应答丢失", nil)
		}
		response.Commit = f.commit
	case model.ManagementCommitStatus:
		if f.commit == nil || request.CommitID != f.commit.ID {
			return response, model.NewError(model.CodeNotFound, "原提交不存在", nil)
		}
		response.Commit = f.commit
	default:
		return response, model.NewError(model.CodeUnsupportedOperation, "不允许其他管理动作", nil)
	}
	return response, nil
}

type schedulePage struct {
	t       *testing.T
	client  *scheduleClient
	service *app.Service
	handler http.Handler
	body    string
}

var scheduleToken = regexp.MustCompile(`name="_token" value="([^"]+)"`)
var scheduleRevision = regexp.MustCompile(`name="revision" value="([0-9]+)"`)

func openSchedulePage(t *testing.T, mode string, writable bool, empty bool) *schedulePage {
	t.Helper()
	next := time.Date(2031, 11, 2, 5, 30, 0, 0, time.UTC)
	f := &scheduleClient{generation: "run-initial", writable: writable, mode: mode, cfg: config.Config{Version: config.CurrentVersion}, document: json.RawMessage(`{"version":1,"schedules":[{"id":"work","enabled":true,"time":"01:30","timezone":"America/New_York"}]}`), statuses: []scheduler.ScheduleStatus{{Schedule: scheduler.Schedule{ID: "work", Enabled: true, Time: "01:30", Timezone: "America/New_York"}, MissedPolicy: "skip", NextTrigger: &next}}}
	if empty {
		f.document = json.RawMessage(`{"version":1,"schedules":[]}`)
		f.statuses = []scheduler.ScheduleStatus{}
	}
	language, err := i18n.New("zh-CN", nil)
	if err != nil {
		t.Fatal(err)
	}
	service := app.NewWithLanguage(func() (app.Client, error) { return f, nil }, language)
	t.Cleanup(service.Close)
	p := schedules.Page()
	if p.Route != "/schedules/" || p.ResourcePath != "/schedules-assets/" {
		t.Fatalf("页面未通过公共注入注册: %+v", p)
	}
	h := &schedulePage{t: t, client: f, service: service, handler: p.Handler(service)}
	h.request(http.MethodGet, nil, http.StatusOK)
	return h
}

func TestSettingsProductSchedulesLanguageProjection(t *testing.T) {
	h := openSchedulePage(t, "", true, false)
	h.post("disable", scheduleFields("22:45", "Asia/Shanghai", true), http.StatusOK)
	before := len(h.client.requests)
	if err := h.service.Language().SetPreference("en"); err != nil {
		t.Fatal(err)
	}
	h.request(http.MethodGet, nil, http.StatusOK)
	for _, text := range []string{`lang="en"`, `data-i18n="schedules.title"`, `id="desktop-catalog"`, `/desktop-i18n.js`, `value="22:45"`, `value="Asia/Shanghai"`, "Schedules"} {
		if !strings.Contains(h.body, text) {
			t.Errorf("语言投影缺少%s", text)
		}
	}
	if len(h.client.requests) != before || h.client.hardwareWrites != 0 {
		t.Fatal("切换呈现语言不应重读后台、提交草稿或切屏")
	}
}
func (h *schedulePage) request(method string, form url.Values, status int) {
	h.t.Helper()
	r := httptest.NewRequest(method, "/schedules/", strings.NewReader(form.Encode()))
	if method == http.MethodPost {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	w := httptest.NewRecorder()
	h.handler.ServeHTTP(w, r)
	h.body = w.Body.String()
	if w.Code != status {
		h.t.Fatalf("动作%s响应%d，期望%d: %s", form.Get("action"), w.Code, status, h.body)
	}
}
func (h *schedulePage) post(action string, fields url.Values, status int) {
	h.t.Helper()
	if fields == nil {
		fields = url.Values{}
	}
	for name, pattern := range map[string]*regexp.Regexp{"_token": scheduleToken, "revision": scheduleRevision} {
		match := pattern.FindStringSubmatch(h.body)
		if len(match) != 2 {
			h.t.Fatalf("缺少真实表单字段%s", name)
		}
		fields.Set(name, match[1])
	}
	fields.Set("action", action)
	h.request(http.MethodPost, fields, status)
}
func (h *schedulePage) count(action model.ManagementAction) int {
	n := 0
	for _, request := range h.client.requests {
		if request.Action == action {
			n++
		}
	}
	return n
}
func scheduleFields(clock, zone string, enabled bool) url.Values {
	v := url.Values{"editing": {"1"}, "index": {"0"}, "id": {"work"}, "time": {clock}, "timezone": {zone}}
	if enabled {
		v.Set("enabled", "1")
	}
	return v
}

func TestSchedulesContract(t *testing.T) {
	t.Run("非法时间时区和ID均不发送预检", func(t *testing.T) {
		for _, sample := range []struct{ clock, zone, id string }{{"24:00", "UTC", "work"}, {"9:00", "UTC", "work"}, {"09:60", "UTC", "work"}, {"09:00", "Local", "work"}, {"09:00", "", "work"}, {"09:00", "Not/AZone", "work"}, {"09:00", "UTC", "bad id"}} {
			h := openSchedulePage(t, "", true, false)
			fields := scheduleFields(sample.clock, sample.zone, true)
			fields.Set("id", sample.id)
			h.post("save", fields, http.StatusBadRequest)
			if h.count(model.ManagementPrepareSchedules) != 0 || h.client.hardwareWrites != 0 {
				t.Fatal("非法草稿越过核心校验")
			}
		}
	})
	t.Run("取消草稿和取消预检均不保存", func(t *testing.T) {
		h := openSchedulePage(t, "", true, false)
		h.post("disable", scheduleFields("09:00", "UTC", true), http.StatusOK)
		h.post("cancel", nil, http.StatusOK)
		h.post("save", nil, http.StatusOK)
		if len(h.client.prepared) != 1 || !h.client.prepared[0].Enabled || h.client.prepared[0].Time != "01:30" || h.client.prepared[0].Timezone != "America/New_York" {
			t.Fatalf("取消未恢复已保存文档: %+v", h.client.prepared)
		}
		h.post("cancel", nil, http.StatusOK)
		if h.count(model.ManagementApplyCommit) != 0 {
			t.Fatal("取消仍提交了后台保存")
		}
	})
	t.Run("停用保存需要确认且仅提交一次", func(t *testing.T) {
		h := openSchedulePage(t, "", true, false)
		h.post("disable", scheduleFields("09:15", "Asia/Shanghai", true), http.StatusOK)
		h.post("save", nil, http.StatusOK)
		if len(h.client.prepared) != 1 || h.client.prepared[0].Enabled || h.client.prepared[0].Time != "09:15" {
			t.Fatalf("停用未进入整个文档: %+v", h.client.prepared)
		}
		h.post("apply", nil, http.StatusBadRequest)
		if h.count(model.ManagementApplyCommit) != 0 {
			t.Fatal("未经确认发送Apply")
		}
		h.post("apply", url.Values{"confirmed": {"1"}}, http.StatusOK)
		h.post("apply", url.Values{"confirmed": {"1"}}, http.StatusConflict)
		if h.count(model.ManagementPrepareSchedules) != 1 || h.count(model.ManagementApplyCommit) != 1 || h.client.hardwareWrites != 0 {
			t.Fatal("重复提交或直接写硬件")
		}
	})
	t.Run("空列表可以创建首个任务并启用", func(t *testing.T) {
		h := openSchedulePage(t, "", true, true)
		h.post("add", nil, http.StatusOK)
		h.post("save", scheduleFields("23:59", "UTC", true), http.StatusOK)
		if len(h.client.prepared) != 1 || !h.client.prepared[0].Enabled || h.client.prepared[0].ID != "work" {
			t.Fatalf("首个日程未进入预检: %+v", h.client.prepared)
		}
	})
	t.Run("切换列表保留当前编辑值而不提前保存", func(t *testing.T) {
		h := openSchedulePage(t, "", true, false)
		h.post("add", scheduleFields("08:20", "Asia/Shanghai", true), http.StatusOK)
		fields := scheduleFields("17:40", "UTC", false)
		fields.Set("index", "1")
		fields.Set("id", "evening")
		fields.Set("selection", "0")
		h.post("", fields, http.StatusOK)
		h.post("save", nil, http.StatusOK)
		if len(h.client.prepared) != 2 || h.client.prepared[0].Time != "08:20" || h.client.prepared[0].Timezone != "Asia/Shanghai" || h.client.prepared[1].ID != "evening" || h.client.prepared[1].Time != "17:40" {
			t.Fatalf("列表切换丢失草稿: %+v", h.client.prepared)
		}
		if h.count(model.ManagementApplyCommit) != 0 {
			t.Fatal("切换列表提前保存")
		}
	})
	t.Run("只读旧后台跨代状态均不可保存", func(t *testing.T) {
		for _, mode := range []string{"readonly", "legacy", "mixed-generation"} {
			h := openSchedulePage(t, mode, mode != "readonly", false)
			if mode == "legacy" {
				// 嵌入模板保留检出文件的换行；仅跨行诊断比较统一CRLF/LF，
				// 避免格式差异遮蔽字段顺序与HTML转义校验。
				diagnosticBody := strings.ReplaceAll(h.body, "\r\n", "\n")
				for _, detail := range []string{`data-diagnostic-code="unsupported_operation"`, "message: 后台不支持定时编辑 &lt;backend&gt;&amp;"} {
					if !strings.Contains(h.body, detail) {
						t.Fatalf("后台结构化诊断未完整转义保留：%s", detail)
					}
				}
				if detail := "field: schedules.&lt;status&gt;&amp;\nretryable: true\ndetail: unsupported_operation: 后台不支持定时编辑 &lt;backend&gt;&amp;"; !strings.Contains(diagnosticBody, detail) {
					t.Fatalf("后台结构化诊断未完整转义保留：%s", detail)
				}
				if strings.Contains(h.body, "<backend>") || strings.Contains(h.body, "schedules.<status>") {
					t.Fatal("后台诊断被当作HTML插入")
				}
			}
			h.post("save", scheduleFields("09:00", "UTC", true), http.StatusConflict)
			if h.count(model.ManagementPrepareSchedules) != 0 {
				t.Fatalf("%s页面仍发预检", mode)
			}
		}
	})
	t.Run("状态对象严格拒绝数组null缺失列表及异常字段", func(t *testing.T) {
		for _, sample := range []struct{ mode, message string }{
			{"statuses-array", `data-i18n="schedules.error.missing_statuses"`},
			{"statuses-null", `data-i18n="schedules.error.missing_statuses"`},
			{"statuses-missing", `data-i18n="schedules.error.missing_statuses"`},
			{"statuses-unknown", `data-i18n="schedules.error.missing_statuses"`},
			{"statuses-duplicate", `data-i18n="schedules.error.incomplete"`},
			{"statuses-item-unknown", `data-i18n="schedules.error.missing_statuses"`},
		} {
			h := openSchedulePage(t, sample.mode, true, false)
			if !strings.Contains(h.body, sample.message) || h.count(model.ManagementSchedulesState) != 1 {
				t.Fatalf("%s未显示真实状态解码错误", sample.mode)
			}
			if sample.mode == "statuses-duplicate" {
				diagnosticBody := strings.ReplaceAll(h.body, "\r\n", "\n")
				for _, detail := range []string{`data-diagnostic-code="internal"`, "message: JSON对象含重复或非法字段"} {
					if !strings.Contains(h.body, detail) {
						t.Fatalf("重复字段的原始解码诊断被覆盖：%s", detail)
					}
				}
				if detail := "field: \nretryable: false\ndetail: JSON对象含重复或非法字段"; !strings.Contains(diagnosticBody, detail) {
					t.Fatalf("重复字段的原始解码诊断被覆盖：%s", detail)
				}
				if err := h.service.Language().SetPreference("en"); err != nil {
					t.Fatal(err)
				}
				h.request(http.MethodGet, nil, http.StatusOK)
				for _, detail := range []string{`data-i18n="schedules.error.incomplete" data-i18n-values="{}">The backend returned incomplete schedule status</span>`, "JSON对象含重复或非法字段"} {
					if !strings.Contains(h.body, detail) {
						t.Fatalf("英文摘要未保留原始解码诊断：%s", detail)
					}
				}
				if h.count(model.ManagementPrepareSchedules) != 0 || h.count(model.ManagementApplyCommit) != 0 {
					t.Fatal("错误页面的语言投影提交了配置")
				}
			}
			h.post("save", scheduleFields("09:00", "UTC", true), http.StatusConflict)
			if h.count(model.ManagementPrepareSchedules) != 0 || h.count(model.ManagementApplyCommit) != 0 || h.client.hardwareWrites != 0 {
				t.Fatalf("%s异常状态仍允许保存", sample.mode)
			}
		}
	})
	t.Run("下次触发和错过策略忠实呈现核心值", func(t *testing.T) {
		h := openSchedulePage(t, "", true, false)
		for _, text := range []string{"2031-11-02T05:30:00Z", "America/New_York", "跳过", "恢复来源不可用"} {
			if !strings.Contains(h.body, text) {
				t.Fatalf("缺少核心结果%s", text)
			}
		}
		h.post("disable", scheduleFields("22:45", "UTC", true), http.StatusOK)
		if !strings.Contains(h.body, "2031-11-02T05:30:00Z") || !strings.Contains(h.body, "已保存") {
			t.Fatal("草稿变化覆盖了真实下次触发")
		}
		if h.count(model.ManagementPrepareSchedules) != 0 {
			t.Fatal("呈现或编辑触发了保存")
		}
	})
	t.Run("预检和Apply丢应答只查询原ID", func(t *testing.T) {
		for _, mode := range []string{"prepare-lost", "apply-lost"} {
			h := openSchedulePage(t, mode, true, false)
			status := http.StatusOK
			if mode == "prepare-lost" {
				status = http.StatusBadGateway
			}
			h.post("save", scheduleFields("09:00", "UTC", true), status)
			id := h.client.commit.ID
			if mode == "apply-lost" {
				h.post("apply", url.Values{"confirmed": {"1"}}, http.StatusBadGateway)
			}
			h.post("save", nil, http.StatusConflict)
			h.post("cancel", nil, http.StatusConflict)
			h.post("query", nil, http.StatusOK)
			if h.count(model.ManagementPrepareSchedules) != 1 {
				t.Fatal("未知结果重新预检")
			}
			for _, request := range h.client.requests {
				if request.Action == model.ManagementCommitStatus && request.CommitID != id {
					t.Fatal("查询丢失原提交ID")
				}
			}
			if mode == "prepare-lost" && h.count(model.ManagementApplyCommit) != 0 || mode == "apply-lost" && h.count(model.ManagementApplyCommit) != 1 {
				t.Fatal("未知结果重放Apply")
			}
		}
	})
	t.Run("保存和生效证据分开显示", func(t *testing.T) {
		h := openSchedulePage(t, "saved-only", true, false)
		h.post("save", scheduleFields("09:00", "UTC", true), http.StatusOK)
		h.post("apply", url.Values{"confirmed": {"1"}}, http.StatusOK)
		for _, text := range []string{"已保存", "未生效", "新运行代启动失败"} {
			if !strings.Contains(h.body, text) {
				t.Fatalf("缺少独立证据%s", text)
			}
		}
	})
	t.Run("过期表单与跨站表单不能修改状态", func(t *testing.T) {
		h := openSchedulePage(t, "", true, false)
		token := scheduleToken.FindStringSubmatch(h.body)[1]
		revision := scheduleRevision.FindStringSubmatch(h.body)[1]
		h.post("add", nil, http.StatusOK)
		h.request(http.MethodPost, url.Values{"action": {"save"}, "_token": {token}, "revision": {revision}}, http.StatusConflict)
		h.request(http.MethodPost, url.Values{"action": {"save"}, "_token": {"wrong"}, "revision": {revision}}, http.StatusForbidden)
		if h.count(model.ManagementPrepareSchedules) != 0 {
			t.Fatal("失效表单发送了保存")
		}
	})
}

package targets_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/orzlee/displayKvm/desktop/app"
	"github.com/orzlee/displayKvm/desktop/i18n"
	"github.com/orzlee/displayKvm/desktop/views/targets"
	"github.com/orzlee/displayKvm/internal/config"
	"github.com/orzlee/displayKvm/internal/model"
)

type fakeClient struct {
	cfg         config.Config
	prepared    config.Config
	generation  model.RuntimeGeneration
	devices     []model.Device
	requests    []model.ManagementRequest
	commit      *model.CommitStatus
	mode        string
	writes      int
	preview     *model.CalibrationSession
	previewData json.RawMessage
}

func copyConfig(t *testing.T, cfg config.Config) config.Config {
	t.Helper()
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var out config.Config
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func (f *fakeClient) Configuration() (config.Config, error) {
	data, err := json.Marshal(f.cfg)
	if err != nil {
		return config.Config{}, err
	}
	var out config.Config
	err = json.Unmarshal(data, &out)
	return out, err
}

func (f *fakeClient) Enumerate(context.Context) ([]model.Device, error) {
	return append([]model.Device(nil), f.devices...), nil
}

func (f *fakeClient) Switch(context.Context, model.SwitchRequest) (model.SwitchResult, error) {
	f.writes++
	return model.SwitchResult{}, model.NewError(model.CodeUnsupportedOperation, "编辑页面不得写入硬件", nil)
}

func boolValue(value bool) *bool { return &value }

// 替身只表达后台管理契约：Prepare登记、Apply决定有效状态，页面自身不得修改
// cfg。断连场景保留同一回执，借实际HTTP处理器的后续查询核对是否错误重放。
func (f *fakeClient) Management(_ context.Context, request model.ManagementRequest) (model.ManagementResponse, error) {
	f.requests = append(f.requests, request)
	if err := request.Validate(); err != nil {
		return model.ManagementResponse{}, err
	}
	response := model.ManagementResponse{Generation: f.generation}
	switch request.Action {
	case model.ManagementSnapshotAction:
		data, err := json.Marshal(f.cfg)
		if err != nil {
			return response, err
		}
		response.Snapshot = &model.ManagementSnapshot{Generation: f.generation, Configuration: data, Rules: json.RawMessage(`{"version":1,"rules":[]}`)}
	case model.ManagementPrepareConfig:
		if request.ExpectedGeneration != f.generation {
			err := model.NewError(model.CodeInvalidRequest, "后台拒绝过期运行代", nil)
			err.Field = "expected_generation"
			return response, err
		}
		cfg, err := config.Load(bytes.NewReader(request.Data))
		if err != nil {
			return response, err
		}
		f.prepared = cfg
		f.commit = &model.CommitStatus{ID: request.CommitID, Kind: model.ManagementPrepareConfig, Phase: model.CommitAccepted, PreparedGeneration: f.generation, Saved: boolValue(false), Effective: boolValue(false)}
		if f.mode == "prepare-lost" {
			return response, model.NewError(model.CodeTransport, "准备应答丢失", nil)
		}
		response.Commit = f.commit
	case model.ManagementApplyCommit:
		if f.commit == nil || request.CommitID != f.commit.ID || request.ExpectedGeneration != f.generation {
			return response, model.NewError(model.CodeInvalidRequest, "提交标识或运行代不匹配", nil)
		}
		switch f.mode {
		case "save-failed":
			f.commit = &model.CommitStatus{ID: request.CommitID, Kind: model.ManagementPrepareConfig, Phase: model.CommitFailed, PreparedGeneration: f.generation, Saved: boolValue(false), Effective: boolValue(false), Error: model.NewError(model.CodeIO, "无法原子保存配置", nil)}
		case "apply-lost":
			f.commit = &model.CommitStatus{ID: request.CommitID, Kind: model.ManagementPrepareConfig, Phase: model.CommitUnknown, PreparedGeneration: f.generation, Saved: boolValue(true)}
			return response, model.NewError(model.CodeTransport, "提交后连接断开", nil)
		default:
			f.cfg = f.prepared
			f.generation = "runtime-b"
			f.commit = &model.CommitStatus{ID: request.CommitID, Kind: model.ManagementPrepareConfig, Phase: model.CommitApplied, PreparedGeneration: request.ExpectedGeneration, EffectiveGeneration: f.generation, Saved: boolValue(true), Effective: boolValue(true)}
		}
		response.Generation, response.Commit = f.generation, f.commit
	case model.ManagementCommitStatus:
		if f.commit == nil || request.CommitID != f.commit.ID {
			return response, model.NewError(model.CodeNotFound, "未找到本次提交", nil)
		}
		response.Commit = f.commit
	case model.ManagementAction("preview_start"):
		f.preview = &model.CalibrationSession{ID: "preview-runtime-b-1", Kind: "input_preview", Generation: f.generation, ExpiresAt: time.Now().Add(time.Minute)}
		if f.mode == "preview-results" {
			f.previewData = json.RawMessage(`{"state":"completed","phase":"finished","target_id":"work","started_at":"2026-11-07T00:00:00Z","finished_at":"2026-11-07T00:00:05Z","displays":[{"display_id":"screen-a","original_input":0,"target_input":15,"observed_input":15,"switch_status":"verified","restore_status":"conflict","detail":"输入在恢复前被外部更改"},{"display_id":"screen-b","target_input":144,"switch_status":"skipped","restore_status":"not_needed"}]}`)
		} else {
			f.previewData = json.RawMessage(`{"state":"running","phase":"holding","target_id":"work","started_at":"2026-11-07T00:00:00Z","hold_until":"2026-11-07T00:00:05Z","displays":[{"display_id":"screen-a","original_input":1,"target_input":15,"observed_input":15,"switch_status":"verified","restore_status":"pending"},{"display_id":"screen-b","target_input":144,"switch_status":"pending","restore_status":"pending"}]}`)
		}
		if f.mode == "preview-start-lost" {
			return response, model.NewError(model.CodeTransport, "预览启动应答丢失", nil)
		}
		response.Session, response.Data = f.preview, f.previewData
	case model.ManagementAction("preview_status"):
		if f.mode == "preview-status-lost" || f.mode == "preview-both-lost" {
			return response, model.NewError(model.CodeTransport, "预览状态连接中断", nil)
		}
		response.Session, response.Data = f.preview, f.previewData
	default:
		return response, model.NewError(model.CodeUnsupportedOperation, "19不使用其他管理动作", nil)
	}
	return response, nil
}

func fixture(t *testing.T) *fakeClient {
	t.Helper()
	standard := model.InputScheme{ID: "dp-confirmed", Protocol: model.ProtocolVCP, VCPCode: 0x60, Value: 15}
	alternate := model.InputScheme{ID: "lg-confirmed", Protocol: model.ProtocolLGInputAlt, Value: 144}
	confirmed := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	f := &fakeClient{generation: "runtime-a", cfg: config.Config{
		Version: config.CurrentVersion,
		Displays: []model.Display{
			{ID: "screen-a", Name: "左屏", Binding: &model.Binding{Path: "/a"}},
			{ID: "screen-b", Name: "右屏", Binding: &model.Binding{Path: "/b"}},
			{ID: "screen-c", Name: "缺席屏", Binding: &model.Binding{Path: "/c"}},
			{ID: "screen-d", Name: "待确认屏", Identity: model.IdentityHints{Manufacturer: "M", ProductCode: "D", Serial: "same"}},
		},
		InputSchemes: []model.InputScheme{standard, alternate},
		Targets:      []model.Target{{ID: "work", Name: "工作电脑", Inputs: []model.TargetInput{{DisplayID: "screen-b", SchemeID: alternate.ID}, {DisplayID: "screen-a", SchemeID: standard.ID}}}},
		Groups:       []model.Group{{ID: "desk", Name: "整个桌面", DisplayIDs: []model.DisplayID{"screen-a", "screen-b", "screen-c", "screen-d"}}, {ID: "focus", Name: "专注", DisplayIDs: []model.DisplayID{"screen-a"}}},
		Calibrations: []config.CalibrationMetadata{{DisplayID: "screen-a", TargetID: "work", PortName: "已确认的DP输入", Scheme: standard, Basis: "manual", ConfirmedAt: &confirmed}},
	}, devices: []model.Device{
		{BackendID: "device-a", Identity: model.IdentityHints{Path: "/a"}},
		{BackendID: "device-b", Identity: model.IdentityHints{Path: "/b"}},
		{BackendID: "device-d1", Identity: model.IdentityHints{Manufacturer: "M", ProductCode: "D", Serial: "same", Path: "/d1"}},
		{BackendID: "device-d2", Identity: model.IdentityHints{Manufacturer: "M", ProductCode: "D", Serial: "same", Path: "/d2"}},
	}}
	if err := f.cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	return f
}

type pageHarness struct {
	t       *testing.T
	client  *fakeClient
	service *app.Service
	handler http.Handler
	body    string
}

func openPage(t *testing.T, client *fakeClient) *pageHarness {
	t.Helper()
	language, err := i18n.New("zh-CN", nil)
	if err != nil {
		t.Fatal(err)
	}
	service := app.NewWithLanguage(func() (app.Client, error) { return client, nil }, language)
	t.Cleanup(service.Close)
	p := targets.Page()
	if p.Route != "/targets/" || p.ResourcePath != "/targets-assets/" {
		t.Fatalf("页面注册不符合约定：%+v", p)
	}
	for _, name := range []string{"targets.css", "targets.js"} {
		data, err := fs.ReadFile(p.Resources, name)
		if err != nil || len(data) == 0 {
			t.Fatalf("页面资源未注册：%s，%v", name, err)
		}
	}
	h := &pageHarness{t: t, client: client, service: service, handler: p.Handler(service)}
	if status := h.request(http.MethodGet, nil); status != http.StatusOK {
		t.Fatalf("读取页面失败：%d %s", status, h.body)
	}
	return h
}

var revisionPattern = regexp.MustCompile(`name="revision" value="([0-9]+)"`)
var commitPattern = regexp.MustCompile(`name="commit_id" value="([^"]+)"`)

func (h *pageHarness) request(method string, form url.Values) int {
	return h.requestAt(method, "/targets/", form)
}

func (h *pageHarness) requestAt(method, path string, form url.Values) int {
	h.t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	r := httptest.NewRequest(method, path, body)
	if method == http.MethodPost {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	w := httptest.NewRecorder()
	h.handler.ServeHTTP(w, r)
	if w.Code == http.StatusSeeOther {
		return h.requestAt(http.MethodGet, w.Header().Get("Location"), nil)
	}
	h.body = w.Body.String()
	if w.Code >= 500 {
		h.t.Fatalf("真实处理器返回服务器错误：%d %s", w.Code, h.body)
	}
	return w.Code
}

func (h *pageHarness) post(form url.Values, status int) {
	h.t.Helper()
	match := revisionPattern.FindStringSubmatch(h.body)
	if len(match) != 2 {
		h.t.Fatal("真实页面缺少表单版本")
	}
	form.Set("revision", match[1])
	if got := h.request(http.MethodPost, form); got != status {
		h.t.Fatalf("动作%s状态=%d，期望%d；%s", form.Get("action"), got, status, h.body)
	}
}

func (h *pageHarness) choose(kind, id string) {
	h.t.Helper()
	h.post(url.Values{"action": {"select"}, "kind": {kind}, "id": {id}}, http.StatusOK)
}

func (h *pageHarness) query() {
	h.t.Helper()
	match := commitPattern.FindStringSubmatch(h.body)
	if len(match) != 2 {
		h.t.Fatal("真实页面未保留可查询提交ID")
	}
	h.post(url.Values{"action": {"query"}, "commit_id": {match[1]}}, http.StatusOK)
}

func count(f *fakeClient, action model.ManagementAction) int {
	n := 0
	for _, request := range f.requests {
		if request.Action == action {
			n++
		}
	}
	return n
}

func targetForm(action, name string) url.Values {
	return url.Values{"action": {action}, "kind": {"target"}, "id": {"work"}, "name": {name}, "mapping.screen-a": {"dp-confirmed"}, "mapping.screen-b": {"lg-confirmed"}}
}

func contains(t *testing.T, body string, fragments ...string) {
	t.Helper()
	for _, fragment := range fragments {
		if !strings.Contains(body, fragment) {
			t.Errorf("实际呈现缺少：%s", fragment)
		}
	}
}

func assertNoHardware(t *testing.T, f *fakeClient) {
	t.Helper()
	if f.writes != 0 {
		t.Fatalf("编辑触发了%d次硬件切换", f.writes)
	}
}

func TestTargetsContract(t *testing.T) {
	t.Run("混合映射多组与缺席歧义独立呈现", func(t *testing.T) {
		f := fixture(t)
		h := openPage(t, f)
		contains(t, h.body, "也属于：专注", "人工绑定路径当前缺席", "多个兼容候选")
		h.choose("target", "work")
		contains(t, h.body, `value="dp-confirmed" selected`, `value="lg-confirmed" selected`, "已确认的DP输入", `data-display-id="screen-c" data-mapped="false"`, "未映射 ·", "人工绑定路径当前缺席", "多个兼容候选")
		h.post(targetForm("check", "工作电脑"), http.StatusOK)
		contains(t, h.body, "配置校验通过，尚未保存", "未映射 ·")
		h.post(url.Values{"action": {"add"}, "kind": {"target"}}, http.StatusOK)
		if strings.Count(h.body, `<option value="" selected data-i18n="targets.unmapped_option">`) != len(f.cfg.Displays) {
			t.Fatal("新增目标为部分屏幕猜测了默认输入")
		}
		if count(f, model.ManagementPrepareConfig) != 0 {
			t.Fatal("读取及草稿新增不应提交")
		}
		assertNoHardware(t, f)
	})

	t.Run("取消恢复基线且保留全部未编辑字段", func(t *testing.T) {
		f := fixture(t)
		before := copyConfig(t, f.cfg)
		h := openPage(t, f)
		h.choose("target", "work")
		edited := targetForm("update", "草稿电脑")
		edited.Set("mapping.screen-a", "lg-confirmed")
		h.post(edited, http.StatusOK)
		contains(t, h.body, "草稿电脑", "有未保存更改")
		h.post(url.Values{"action": {"cancel"}}, http.StatusOK)
		contains(t, h.body, `name="name" value="工作电脑"`, "已取消全部更改")
		if !reflect.DeepEqual(before, f.cfg) || count(f, model.ManagementPrepareConfig) != 0 || count(f, model.ManagementApplyCommit) != 0 {
			t.Fatal("取消污染有效配置或发送了保存操作")
		}
		assertNoHardware(t, f)
	})

	t.Run("核心非法字段错误零保存", func(t *testing.T) {
		for _, which := range []string{"empty-target", "unknown-scheme", "empty-group"} {
			t.Run(which, func(t *testing.T) {
				f := fixture(t)
				before := copyConfig(t, f.cfg)
				h := openPage(t, f)
				var form url.Values
				var field string
				if which == "empty-group" {
					form = url.Values{"action": {"save"}, "kind": {"group"}, "id": {"desk"}, "name": {"空屏组"}}
					field = "groups[0].display_ids"
				} else {
					h.choose("target", "work")
					form = url.Values{"action": {"save"}, "kind": {"target"}, "id": {"work"}, "name": {"坏目标"}}
					field = "targets[0].inputs"
					if which == "unknown-scheme" {
						form.Set("mapping.screen-a", "made-up")
						field = "targets[0].inputs[0]"
					}
				}
				h.post(form, http.StatusConflict)
				contains(t, h.body, field, "更改仍保留在草稿中")
				if which == "unknown-scheme" {
					contains(t, h.body, `value="made-up" selected`, "无法使用的输入方案")
				}
				if count(f, model.ManagementPrepareConfig) != 0 || count(f, model.ManagementApplyCommit) != 0 || !reflect.DeepEqual(before, f.cfg) {
					t.Fatal("核心拒绝的草稿进入保存或有效配置")
				}
				assertNoHardware(t, f)
			})
		}
	})

	t.Run("合法保存完整配置并明确保存生效", func(t *testing.T) {
		f := fixture(t)
		before := copyConfig(t, f.cfg)
		h := openPage(t, f)
		h.choose("target", "work")
		form := targetForm("save", "开发电脑")
		h.post(form, http.StatusOK)
		contains(t, h.body, "已保存", "已生效", "旧版程序无法读取")
		if count(f, model.ManagementPrepareConfig) != 1 || count(f, model.ManagementApplyCommit) != 1 {
			t.Fatal("合法保存没有恰好执行一次Prepare与Apply")
		}
		expected := copyConfig(t, before)
		expected.Targets[0].Name = "开发电脑"
		if !reflect.DeepEqual(expected, f.cfg) || !reflect.DeepEqual(expected, f.prepared) {
			t.Fatal("保存丢失了身份、输入方案、多组或v3校准元数据")
		}
		view := h.service.Snapshot()
		if len(view.Targets) != len(before.Targets) || view.Targets[0].Name != before.Targets[0].Name {
			// 概览仍持有上次只读快照，页面不得通过改本地cfg宣称运行生效。
			t.Fatal("页面自行篡改了共享服务的只读配置列表")
		}
		assertNoHardware(t, f)
	})

	t.Run("一屏多组成员保存不互相排斥", func(t *testing.T) {
		f := fixture(t)
		h := openPage(t, f)
		h.post(url.Values{"action": {"save"}, "kind": {"group"}, "id": {"desk"}, "name": {"双屏桌面"}, "display_id": {"screen-a", "screen-b"}}, http.StatusOK)
		if !reflect.DeepEqual(f.cfg.Groups[0].DisplayIDs, []model.DisplayID{"screen-a", "screen-b"}) || !reflect.DeepEqual(f.cfg.Groups[1].DisplayIDs, []model.DisplayID{"screen-a"}) {
			t.Fatal("保存一组成员改变了另一组的共享屏幕")
		}
		assertNoHardware(t, f)
	})

	t.Run("保存失败保留草稿且有效配置不变", func(t *testing.T) {
		f := fixture(t)
		f.mode = "save-failed"
		before := copyConfig(t, f.cfg)
		h := openPage(t, f)
		h.choose("target", "work")
		h.post(targetForm("save", "失败时保留"), http.StatusConflict)
		contains(t, h.body, "无法原子保存配置", "未保存", "未生效", `value="失败时保留"`)
		if !reflect.DeepEqual(before, f.cfg) || count(f, model.ManagementApplyCommit) != 1 {
			t.Fatal("失败保存污染有效状态或被自动重放")
		}
		assertNoHardware(t, f)
	})

	t.Run("过期运行代草稿拒绝且零提交", func(t *testing.T) {
		f := fixture(t)
		before := copyConfig(t, f.cfg)
		h := openPage(t, f)
		h.choose("target", "work")
		f.generation = "runtime-new"
		h.post(targetForm("save", "旧草稿"), http.StatusConflict)
		contains(t, h.body, "expected_generation", "草稿已过期", `value="旧草稿"`)
		if count(f, model.ManagementPrepareConfig) != 0 || !reflect.DeepEqual(before, f.cfg) {
			t.Fatal("过期草稿被提交")
		}
		assertNoHardware(t, f)
	})

	t.Run("Prepare丢应答只查询原ID不Apply", func(t *testing.T) {
		f := fixture(t)
		f.mode = "prepare-lost"
		before := copyConfig(t, f.cfg)
		h := openPage(t, f)
		h.choose("target", "work")
		h.post(targetForm("save", "准备应答丢失"), http.StatusConflict)
		contains(t, h.body, "未知", "查询这次保存结果")
		h.query()
		contains(t, h.body, "仅接受了准备请求", "未保存", "未生效")
		if count(f, model.ManagementPrepareConfig) != 1 || count(f, model.ManagementApplyCommit) != 0 || count(f, model.ManagementCommitStatus) != 1 || !reflect.DeepEqual(before, f.cfg) {
			t.Fatal("Prepare丢应答后发生了自动重放或Apply")
		}
		assertNoHardware(t, f)
	})

	t.Run("Apply断连只查询同ID并区分已保存与生效未知", func(t *testing.T) {
		f := fixture(t)
		f.mode = "apply-lost"
		before := copyConfig(t, f.cfg)
		h := openPage(t, f)
		h.choose("target", "work")
		h.post(targetForm("save", "提交应答丢失"), http.StatusConflict)
		contains(t, h.body, "未知", `value="提交应答丢失"`)
		h.query()
		h.query()
		contains(t, h.body, "已保存", "未知", "不会自动重复保存")
		if count(f, model.ManagementPrepareConfig) != 1 || count(f, model.ManagementApplyCommit) != 1 || count(f, model.ManagementCommitStatus) != 2 || !reflect.DeepEqual(before, f.cfg) {
			t.Fatal("Apply断连后重复提交或污染有效配置")
		}
		for _, request := range f.requests {
			if request.Action == model.ManagementCommitStatus && request.CommitID != f.commit.ID {
				t.Fatal("断连后查询了不同的提交ID")
			}
		}
		assertNoHardware(t, f)
	})

	t.Run("旧表单与重复保存不产生第二次提交", func(t *testing.T) {
		f := fixture(t)
		h := openPage(t, f)
		h.choose("target", "work")
		form := targetForm("save", "一次提交")
		h.post(form, http.StatusOK)
		if status := h.request(http.MethodPost, form); status != http.StatusConflict {
			t.Fatalf("旧POST没有被拒绝：%d", status)
		}
		contains(t, h.body, "表单已过期")
		if count(f, model.ManagementPrepareConfig) != 1 || count(f, model.ManagementApplyCommit) != 1 {
			t.Fatal("旧表单重复提交")
		}
		assertNoHardware(t, f)
	})
}

func TestTargetsSavedTargetPreviewBypassesSaveLockWithoutApplyingDraft(t *testing.T) {
	f := fixture(t)
	h := openPage(t, f)
	h.choose("target", "work")
	h.post(targetForm("save", "已保存工作电脑"), http.StatusOK)
	if f.commit == nil || f.commit.Phase != model.CommitApplied {
		t.Fatal("测试前提不成立：目标尚未保存并生效")
	}

	// 保存回执会锁定编辑草稿，但测试已保存目标仍须可用。混入不同名称和映射，
	// 用真实请求证明预览不会把页面上尚未保存的编辑当成后台有效配置。
	form := targetForm("preview_start", "不应吸收的未保存名称")
	form.Set("target_id", "work")
	form.Set("mapping.screen-a", "lg-confirmed")
	form.Del("mapping.screen-b")
	match := revisionPattern.FindStringSubmatch(h.body)
	if len(match) != 2 {
		t.Fatal("保存后的真实页面缺少表单版本")
	}
	form.Set("revision", match[1])
	status := h.request(http.MethodPost, form)
	if got := count(f, model.ManagementAction("preview_start")); got != 1 {
		t.Fatalf("保存锁定后预览应恰好转发一次preview_start，实际%d次，HTTP状态%d", got, status)
	}
	if status != http.StatusOK {
		t.Fatalf("后台已接受预览，真实处理器HTTP状态=%d，期望%d", status, http.StatusOK)
	}

	var forwarded model.ManagementRequest
	for _, request := range f.requests {
		if request.Action == model.ManagementAction("preview_start") {
			forwarded = request
		}
	}
	if forwarded.ExpectedGeneration != "runtime-b" {
		t.Fatalf("预览未绑定保存生效后的后台运行代：%q", forwarded.ExpectedGeneration)
	}
	var data struct {
		Request model.SwitchRequest `json:"request"`
	}
	if err := json.Unmarshal(forwarded.Data, &data); err != nil {
		t.Fatalf("预览没有发送管理契约要求的request负载：%v", err)
	}
	if data.Request.TargetID != "work" || !model.ValidID(string(data.Request.ID)) {
		t.Fatalf("预览应引用已保存目标和有效请求编号：%+v", data.Request)
	}
	displays := map[model.DisplayID]bool{}
	for _, id := range data.Request.DisplayIDs {
		displays[id] = true
	}
	if len(data.Request.DisplayIDs) != 2 || !displays["screen-a"] || !displays["screen-b"] {
		t.Fatalf("预览应使用已保存目标的两屏映射，不能吸收未保存表单：%v", data.Request.DisplayIDs)
	}
	if count(f, model.ManagementPrepareConfig) != 1 || count(f, model.ManagementApplyCommit) != 1 {
		t.Fatal("测试已保存目标额外提交了未保存草稿")
	}
	contains(t, h.body, `name="name" value="已保存工作电脑"`, `value="dp-confirmed" selected`, "preview-runtime-b-1")
	assertNoHardware(t, f)
}

func TestTargetsPreviewStatusSurvivesReloadWithoutRestart(t *testing.T) {
	f := fixture(t)
	h := openPage(t, f)
	h.choose("target", "work")
	form := url.Values{"action": {"preview_start"}, "kind": {"target"}, "id": {"work"}, "target_id": {"work"}}
	h.post(form, http.StatusOK)
	if count(f, model.ManagementAction("preview_start")) != 1 {
		t.Fatal("首次测试没有恰好启动一个后台预览")
	}
	if !strings.Contains(h.body, "preview-runtime-b-1") {
		t.Fatal("首次测试未展示可用于刷新查询的后台会话编号")
	}

	// 启动前允许用空会话查询current/latest；只有重载GET产生的查询必须绑定已知会话。
	beforeReload := len(f.requests)
	status := h.requestAt(http.MethodGet, "/targets/?target=work&preview_session=preview-runtime-b-1", nil)
	if status != http.StatusOK {
		t.Fatalf("重载应查询已有会话，不启动新预览：HTTP=%d", status)
	}
	if count(f, model.ManagementAction("preview_start")) != 1 {
		t.Fatal("重载预览页面时再次发送了preview_start")
	}
	reloadQueries := 0
	for _, request := range f.requests[beforeReload:] {
		switch request.Action {
		case model.ManagementAction("preview_status"):
			reloadQueries++
			if request.SessionID != "preview-runtime-b-1" {
				t.Fatalf("重载查询未绑定原后台会话：%q", request.SessionID)
			}
		case model.ManagementAction("preview_start"):
			t.Fatal("重载GET不应发送preview_start")
		}
	}
	if reloadQueries == 0 {
		t.Fatal("重载GET没有查询原后台会话")
	}
}

func TestTargetsPreviewDuplicateClickQueriesInsteadOfRestarting(t *testing.T) {
	f := fixture(t)
	h := openPage(t, f)
	h.choose("target", "work")
	h.post(url.Values{"action": {"preview_start"}, "kind": {"target"}, "id": {"work"}, "target_id": {"work"}}, http.StatusOK)
	h.post(url.Values{"action": {"preview_start"}, "kind": {"target"}, "id": {"work"}, "target_id": {"work"}}, http.StatusOK)
	if count(f, model.ManagementAction("preview_start")) != 1 || count(f, model.ManagementAction("preview_status")) < 1 {
		t.Fatalf("进行中的重复点击应只查询：start=%d status=%d", count(f, model.ManagementAction("preview_start")), count(f, model.ManagementAction("preview_status")))
	}
}

func TestTargetsPreviewRendersPerDisplayValuesIncludingZeroAndConflict(t *testing.T) {
	f := fixture(t)
	f.mode = "preview-results"
	h := openPage(t, f)
	h.choose("target", "work")
	h.post(url.Values{"action": {"preview_start"}, "kind": {"target"}, "id": {"work"}, "target_id": {"work"}}, http.StatusOK)
	contains(t, h.body, "preview-runtime-b-1", "screen-a", "screen-b", "0", "15", "144", "外部更改")
	if strings.Contains(h.body, "未知原输入：0") {
		t.Fatal("已知的输入值0被错误投影为未知")
	}
}

func TestTargetsPreviewConnectionFailureKeepsSessionAndDoesNotClaimRestore(t *testing.T) {
	f := fixture(t)
	f.mode = "preview-status-lost"
	f.preview = &model.CalibrationSession{ID: "preview-runtime-b-1", Kind: "input_preview", Generation: f.generation, ExpiresAt: time.Now().Add(time.Minute)}
	f.previewData = json.RawMessage(`{"state":"running","phase":"restoring","target_id":"work","started_at":"2026-11-07T00:00:00Z","displays":[{"display_id":"screen-a","original_input":0,"target_input":15,"switch_status":"verified","restore_status":"pending"}]}`)
	h := openPage(t, f)
	status := h.requestAt(http.MethodGet, "/targets/?target=work&preview_session=preview-runtime-b-1", nil)
	if status != http.StatusOK {
		t.Fatalf("连接失败应保留可读状态页，HTTP=%d", status)
	}
	preview := regexp.MustCompile(`(?s)<section id="input-preview"[^>]*>.*?</section>`).FindString(h.body)
	if preview == "" {
		t.Fatal("连接失败页面缺少预览状态区域")
	}
	contains(t, preview, `data-session="preview-runtime-b-1"`, `data-unknown="true"`, `data-preview-state`, `data-i18n="targets.preview.state.unknown"`, "测试状态未知", "不能证明已恢复")
	// 查询失败未返回可采信的逐屏快照，不应生成恢复结果行；警示和共享词典包含
	// “已恢复”的否定句，因此只在预览区域检查结果节点和恢复成功标记。
	if strings.Contains(preview, `class="targets-preview-results"`) || strings.Contains(preview, `data-preview-display=`) || strings.Contains(preview, `data-i18n="targets.preview.restore.restored"`) {
		t.Fatal("状态查询连接失败时页面生成了未经确认的恢复结果行或恢复成功标记")
	}
	if count(f, model.ManagementAction("preview_start")) != 0 || count(f, model.ManagementAction("preview_status")) != 1 {
		t.Fatalf("连接失败路径必须只查询而不启动：start=%d status=%d", count(f, model.ManagementAction("preview_start")), count(f, model.ManagementAction("preview_status")))
	}
}

func TestTargetsPreviewLostStartResponseQueriesLatestOnly(t *testing.T) {
	f := fixture(t)
	f.mode = "preview-start-lost"
	h := openPage(t, f)
	h.choose("target", "work")
	h.post(url.Values{"action": {"preview_start"}, "kind": {"target"}, "id": {"work"}, "target_id": {"work"}}, http.StatusOK)
	if count(f, model.ManagementAction("preview_start")) != 1 || count(f, model.ManagementAction("preview_status")) < 1 {
		t.Fatalf("启动应答丢失后应查询最新状态且不重发：start=%d status=%d", count(f, model.ManagementAction("preview_start")), count(f, model.ManagementAction("preview_status")))
	}
	contains(t, h.body, "preview-runtime-b-1")
}

func TestSettingsProductTargetsLanguageProjection(t *testing.T) {
	f := fixture(t)
	h := openPage(t, f)
	h.choose("target", "work")
	h.post(targetForm("update", "My 中文 draft <screen>"), http.StatusOK)
	contains(t, h.body,
		`data-i18n="error.not_found" data-i18n-values="{}">没有找到所需项目。</span>`,
		`data-i18n="error.identity_ambiguous" data-i18n-values="{}">显示器身份不明确，请人工核对。</span>`)
	before := len(f.requests)
	if err := h.service.Language().SetPreference("en"); err != nil {
		t.Fatal(err)
	}
	h.request(http.MethodGet, nil)
	contains(t, h.body, `lang="en"`, `data-i18n="targets.title"`, `id="desktop-catalog"`, `/desktop-i18n.js`, `value="My 中文 draft &lt;screen&gt;"`, "Targets &amp; groups")
	contains(t, h.body,
		`data-i18n="error.not_found" data-i18n-values="{}">The requested item was not found.</span>`,
		`data-i18n="error.identity_ambiguous" data-i18n-values="{}">The display identity is ambiguous. Check it manually.</span>`,
		`data-diagnostic-code="not_found"`, `data-diagnostic-code="identity_ambiguous"`,
		"message: 人工绑定路径当前缺席，须确认设备并重新绑定", "message: 显示器存在多个兼容候选，须人工绑定",
		"field: \nretryable: false\ndetail: not_found: 人工绑定路径当前缺席，须确认设备并重新绑定",
		"field: \nretryable: false\ndetail: identity_ambiguous: 显示器存在多个兼容候选，须人工绑定")
	if len(f.requests) != before {
		t.Fatal("切换呈现语言不应重读后台或提交草稿")
	}
	data, err := fs.ReadFile(targets.Page().Resources, "targets.js")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte("desktopI18n.text")) || bytes.Contains(data, []byte("location.reload")) || bytes.Contains(data, []byte("navigator.language")) {
		t.Fatal("确认文案必须消费当前词典，不得重载页面或另行判断语言")
	}
	// 设备页也引用预览词条，仅检查目标页会漏掉导航及旧版人工试切的边界说明。
	for _, locale := range []string{"zh-CN", "en"} {
		language, err := i18n.New(locale, nil)
		if err != nil {
			t.Fatal(err)
		}
		catalog := language.Snapshot()
		for _, key := range []string{"targets.preview.device_navigation", "targets.preview.open", "targets.preview.legacy_note"} {
			if text, ok := catalog.Messages[key]; !ok || strings.TrimSpace(text) == "" {
				t.Errorf("%s词典缺少设备页预览词条：%s", locale, key)
			}
		}
	}
	assertNoHardware(t, f)
}

package devices_test

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/orzlee/displayKvm/desktop/app"
	"github.com/orzlee/displayKvm/desktop/i18n"
	devicepage "github.com/orzlee/displayKvm/desktop/views/devices"
	"github.com/orzlee/displayKvm/internal/config"
	"github.com/orzlee/displayKvm/internal/diagnostics"
	"github.com/orzlee/displayKvm/internal/model"
)

const generation model.RuntimeGeneration = "generation-18"

// 替身只模拟公共客户端边界与后台应答，不重写页面的绑定、状态投影或保存门禁。
// hardware与saved是副作用计数，必须由真实HTTP处理器实际派发的动作才能改变。
type fakeClient struct {
	mu             sync.Mutex
	cfg            config.Config
	devices        []model.Device
	requests       []model.ManagementRequest
	manual         bool
	ambiguous      bool
	unsupported    bool
	prepareLost    bool
	applyLost      bool
	applyFailed    bool
	queryPhase     model.CommitPhase
	trialState     model.ExecutionState
	trialError     *model.Error
	trialRequest   model.SwitchRequest
	progress       diagnostics.TrialProgress
	commit         *model.CommitStatus
	hardware       int
	saved          int
	directSwitches int
	enumerations   int
	stepEntered    chan struct{}
	stepRelease    chan struct{}
}

func fixtureConfig() config.Config {
	return config.Config{Version: config.CurrentVersion,
		Displays:     []model.Display{{ID: "left", Name: "左侧工作屏", Identity: model.IdentityHints{Serial: "screen-left", Path: "path-left"}, Binding: &model.Binding{Path: "path-left"}}},
		InputSchemes: []model.InputScheme{{ID: "left-work", Protocol: model.ProtocolVCP, VCPCode: 0x60, Value: 17}},
		Targets:      []model.Target{{ID: "work", Name: "工作电脑", Inputs: []model.TargetInput{{DisplayID: "left", SchemeID: "left-work"}}}},
	}
}

func newFake() *fakeClient {
	return &fakeClient{cfg: fixtureConfig(), devices: []model.Device{{BackendID: "native-left", Name: "工作屏", Identity: model.IdentityHints{Serial: "screen-left", Path: "path-left"}}}, trialState: model.StateConfirmed}
}

func (f *fakeClient) Configuration() (config.Config, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cfg, nil
}

func (f *fakeClient) Enumerate(context.Context) ([]model.Device, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enumerations++
	return append([]model.Device(nil), f.devices...), nil
}

func (f *fakeClient) Switch(context.Context, model.SwitchRequest) (model.SwitchResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.directSwitches++
	return model.SwitchResult{}, errors.New("设备页不得绕过管理试切调用概览Switch")
}

func raw(value any) json.RawMessage {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return encoded
}

func session(kind string) *model.CalibrationSession {
	return &model.CalibrationSession{ID: kind + "-18", Kind: kind, Generation: generation, ExpiresAt: time.Now().Add(time.Hour)}
}

func (f *fakeClient) Management(ctx context.Context, request model.ManagementRequest) (model.ManagementResponse, error) {
	if err := request.Validate(); err != nil {
		return model.ManagementResponse{}, err
	}
	f.mu.Lock()
	f.requests = append(f.requests, request)
	if request.Action == model.ManagementTrialStep {
		f.hardware++
	}
	entered, release := f.stepEntered, f.stepRelease
	if request.Action == model.ManagementTrialStep && entered != nil {
		f.progress.Running = true
		f.mu.Unlock()
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
			return model.ManagementResponse{}, ctx.Err()
		}
		f.mu.Lock()
	}
	defer f.mu.Unlock()
	response := model.ManagementResponse{Generation: generation}
	switch request.Action {
	case model.ManagementSnapshotAction:
		response.Snapshot = &model.ManagementSnapshot{Generation: generation, Configuration: raw(f.cfg), Rules: json.RawMessage(`{"version":1,"rules":[]}`)}
	case model.ManagementDiagnose:
		var data model.ManagementDiagnoseData
		if err := json.Unmarshal(request.Data, &data); err != nil {
			return response, err
		}
		if data.DisplayID == "" {
			response.Data = raw(diagnostics.BackendDiagnostic{Available: true, Devices: f.devices, Note: "扫描不证明输入支持"})
		} else {
			diagnostic := diagnostics.Diagnostic{Display: f.cfg.Displays[0], Candidates: f.devices, CanCalibrate: true,
				Capabilities: model.DeviceCapabilities{Protocols: []model.InputProtocol{model.ProtocolVCP}, CanReadInput: true}, InputConfidence: diagnostics.BasisUnknown}
			if f.manual {
				diagnostic.Capabilities = model.DeviceCapabilities{}
				diagnostic.CapabilityIssue = model.NewError(model.CodeUnreachable, "能力读取失败，可人工核对", nil)
				diagnostic.ObservationIssue = model.NewError(model.CodeUnreachable, "未取得输入读回", nil)
			}
			if f.ambiguous {
				diagnostic.IdentityIssue = model.NewError(model.CodeIdentityAmbiguous, "多个身份候选，必须人工绑定", nil)
				diagnostic.CanCalibrate = false
			}
			response.Data = raw(diagnostic)
		}
	case model.ManagementCalibrationBegin:
		if f.unsupported {
			return response, model.NewError(model.CodeUnsupportedProtocol, "当前后端不支持该输入协议", nil)
		}
		var data model.ManagementCalibrationBeginData
		if err := json.Unmarshal(request.Data, &data); err != nil {
			return response, err
		}
		basis := diagnostics.BasisReadback
		var observation *model.InputObservation
		if f.manual {
			basis = diagnostics.BasisManual
		} else {
			value := data.Scheme.Value
			observation = &model.InputObservation{Value: &value, ObservedAt: time.Now()}
		}
		response.Session = session("calibration")
		response.Session.Evidence = raw(diagnostics.CalibrationEvidence{DisplayID: data.DisplayID, TargetID: data.TargetID, PortName: data.PortName, Scheme: data.Scheme, Basis: basis, Observation: observation})
	case model.ManagementPrepareConfig:
		var proposed config.Config
		if err := json.Unmarshal(request.Data, &proposed); err != nil {
			return response, err
		}
		if err := proposed.Validate(); err != nil {
			return response, err
		}
		fallthrough
	case model.ManagementPrepareCalibration:
		f.commit = &model.CommitStatus{ID: request.CommitID, Kind: request.Action, Phase: model.CommitAccepted, PreparedGeneration: generation}
		if f.prepareLost {
			return response, errors.New("Prepare应答丢失")
		}
		value := *f.commit
		response.Commit = &value
	case model.ManagementApplyCommit:
		f.saved++
		if f.applyLost {
			return response, errors.New("Apply应答丢失")
		}
		saved, effective := true, !f.applyFailed
		phase := model.CommitApplied
		if f.applyFailed {
			phase = model.CommitFailed
		}
		f.commit = &model.CommitStatus{ID: request.CommitID, Phase: phase, PreparedGeneration: generation, Saved: &saved, Effective: &effective}
		if f.applyFailed {
			f.commit.Error = model.NewError(model.CodeIO, "已保存但尚未生效，请核对后台", nil)
		}
		value := *f.commit
		response.Commit = &value
	case model.ManagementCommitStatus:
		if f.commit == nil || request.CommitID != f.commit.ID {
			return response, errors.New("没有该提交")
		}
		value := *f.commit
		if f.queryPhase != "" {
			value.Phase = f.queryPhase
		}
		response.Commit = &value
	case model.ManagementTrialBegin:
		var data model.ManagementTrialBeginData
		if err := json.Unmarshal(request.Data, &data); err != nil || len(data.Requests) != 1 || len(data.Requests[0].DisplayIDs) != 1 || data.Requests[0].GroupID != "" {
			return response, errors.New("试切必须是单屏明确目标")
		}
		f.trialRequest = data.Requests[0]
		f.progress = diagnostics.TrialProgress{Total: 1}
		response.Session = session("trial")
		response.Session.Progress = raw(f.progress)
	case model.ManagementTrialStep:
		if !request.Confirmed {
			return response, errors.New("未明确确认")
		}
		now, input := time.Now(), uint16(17)
		result := model.DisplayResult{DisplayID: "left", DesiredValue: 17, State: f.trialState, SentAt: &now}
		if f.trialState == model.StateConfirmed {
			result.Observation = &model.InputObservation{Value: &input, ObservedAt: now}
		} else if f.trialState == model.StateUnknown {
			result.Error = model.NewError(model.CodeUnreachable, "发送后失联，结果未知", nil)
		}
		if f.trialError != nil {
			result.Error = f.trialError
		}
		f.progress.Next, f.progress.Running = 1, false
		f.progress.Results = []model.SwitchResult{{RequestID: f.trialRequest.ID, Displays: []model.DisplayResult{result}}}
		response.Session = session("trial")
		response.Session.Progress = raw(f.progress)
	case model.ManagementTrialStop:
		f.progress.Stopped = true
		response.Session = session("trial")
		response.Session.Progress = raw(f.progress)
	default:
		return response, errors.New("测试边界不允许未声明动作")
	}
	return response, nil
}

type browser struct {
	t        *testing.T
	handler  http.Handler
	fake     *fakeClient
	token    string
	gen      string
	body     string
	language *i18n.Manager
}

var tokenPattern = regexp.MustCompile(`name="_token" value="([^"]+)"`)
var generationPattern = regexp.MustCompile(`name="generation" value="([^"]+)"`)
var commitPattern = regexp.MustCompile(`name="commit_id" value="([^"]+)"`)
var requestPattern = regexp.MustCompile(`name="request_id" value="([^"]+)"`)
var catalogPattern = regexp.MustCompile(`(?s)(<script type="application/json" id="desktop-catalog">)(.*?)(</script>)`)

func newBrowser(t *testing.T, fake *fakeClient) *browser {
	t.Helper()
	return newBrowserWithLanguage(t, fake, "zh-CN")
}

func newBrowserWithLanguage(t *testing.T, fake *fakeClient, preference string) *browser {
	t.Helper()
	language, err := i18n.New(preference, nil)
	if err != nil {
		t.Fatal(err)
	}
	service := app.NewWithLanguage(func() (app.Client, error) { return fake, nil }, language)
	t.Cleanup(service.Close)
	if err := service.Reconnect(context.Background()); err != nil {
		t.Fatal(err)
	}
	b := &browser{t: t, handler: devicepage.Page().Handler(service), fake: fake, language: language}
	b.get("/devices/")
	return b
}

func (b *browser) absorb(recorder *httptest.ResponseRecorder) {
	// 词典包含未呈现分支的全部文字；业务断言只核页面实际节点，避免把嵌入
	// JSON中的“成功”或已取消预览文案误当作当前可见状态。
	b.body = catalogPattern.ReplaceAllString(recorder.Body.String(), `${1}${3}`)
	if match := tokenPattern.FindStringSubmatch(b.body); len(match) > 1 {
		b.token = match[1]
	}
	if match := generationPattern.FindStringSubmatch(b.body); len(match) > 1 {
		b.gen = match[1]
	}
}

func (b *browser) get(path string) *httptest.ResponseRecorder {
	b.t.Helper()
	r := httptest.NewRecorder()
	b.handler.ServeHTTP(r, httptest.NewRequest(http.MethodGet, path, nil))
	if r.Code != http.StatusOK {
		b.t.Fatalf("GET %s: %d %s", path, r.Code, r.Body.String())
	}
	b.absorb(r)
	return r
}

func (b *browser) post(path string, fields url.Values, want int) *httptest.ResponseRecorder {
	b.t.Helper()
	if fields == nil {
		fields = make(url.Values)
	}
	if !fields.Has("_token") {
		fields.Set("_token", b.token)
	}
	if !fields.Has("generation") {
		fields.Set("generation", b.gen)
	}
	request := httptest.NewRequest(http.MethodPost, "/devices/"+path, strings.NewReader(fields.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recorder := httptest.NewRecorder()
	b.handler.ServeHTTP(recorder, request)
	if recorder.Code != want {
		b.t.Fatalf("POST %s: got %d want %d: %s", path, recorder.Code, want, recorder.Body.String())
	}
	if recorder.Code == http.StatusSeeOther {
		b.get(recorder.Header().Get("Location"))
	} else {
		b.absorb(recorder)
	}
	return recorder
}

func fields(values ...string) url.Values {
	result := make(url.Values)
	for i := 0; i < len(values); i += 2 {
		result.Set(values[i], values[i+1])
	}
	return result
}

func calibrationFields() url.Values {
	return fields("display_id", "left", "target_id", "work", "port_name", "工作电脑 · USB-C", "protocol", "vcp", "value", "0x11")
}

func (f *fakeClient) count(action model.ManagementAction) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	total := 0
	for _, request := range f.requests {
		if request.Action == action {
			total++
		}
	}
	return total
}

func (b *browser) zeroEffects() {
	b.t.Helper()
	b.fake.mu.Lock()
	defer b.fake.mu.Unlock()
	if b.fake.hardware != 0 || b.fake.saved != 0 || b.fake.directSwitches != 0 {
		b.t.Fatalf("意外副作用: hardware=%d saved=%d directSwitch=%d", b.fake.hardware, b.fake.saved, b.fake.directSwitches)
	}
}

func (b *browser) inspect() {
	b.post("scan", nil, http.StatusSeeOther)
	b.post("inspect", fields("display_id", "left"), http.StatusSeeOther)
}

func (b *browser) prepareCalibration() string {
	b.t.Helper()
	b.inspect()
	b.post("calibration/prepare", calibrationFields(), http.StatusSeeOther)
	b.post("calibration/save", fields("confirmed", "true"), http.StatusSeeOther)
	match := commitPattern.FindStringSubmatch(b.body)
	if len(match) != 2 {
		b.t.Fatal("真实页面未提供保留的提交编号")
	}
	return match[1]
}

func TestDevicesContract(t *testing.T) {
	t.Run("真实页面和资源可由纯Go壳注册", func(t *testing.T) {
		page := devicepage.Page()
		if page.Route != "/devices/" || page.ResourcePath != "/devices-assets/" || page.Handler == nil {
			t.Fatalf("页面装配不完整: %+v", page)
		}
		for _, path := range []string{"devices.css", "devices.js"} {
			if _, err := fs.ReadFile(page.Resources, path); err != nil {
				t.Fatal(err)
			}
		}
		b := newBrowser(t, newFake())
		for _, expected := range []string{"设备配置", "人工绑定", "逐屏试切", "实体输入键", "高级诊断", "事件规则", "/devices-assets/devices.css"} {
			if !strings.Contains(b.body, expected) {
				t.Fatalf("真实页面缺少 %q", expected)
			}
		}
		if b.token == "" || b.gen != string(generation) {
			t.Fatal("真实表单缺少凭据或运行代")
		}
		b.zeroEffects()
	})

	t.Run("完整扫描保留歧义且不自动合并", func(t *testing.T) {
		f := newFake()
		f.ambiguous = true
		f.devices = append(f.devices, model.Device{BackendID: "native-copy", Name: "同型号工作屏", Identity: model.IdentityHints{Serial: "screen-left", Path: "path-copy"}})
		b := newBrowser(t, f)
		b.inspect()
		if !strings.Contains(b.body, "同型号工作屏") || !strings.Contains(b.body, "多个身份候选") {
			t.Fatal("歧义候选或明确阻止原因未保留")
		}
		b.post("calibration/prepare", calibrationFields(), http.StatusBadRequest)
		if f.count(model.ManagementCalibrationBegin) != 0 || f.count(model.ManagementPrepareConfig) != 0 {
			t.Fatal("身份歧义仍进入校准或自动保存绑定")
		}
		b.zeroEffects()
	})

	t.Run("首次候选人工绑定保留独立逻辑身份", func(t *testing.T) {
		f := newFake()
		f.cfg.Displays, f.cfg.Targets, f.cfg.InputSchemes = nil, nil, nil
		f.devices = append(f.devices, model.Device{BackendID: "second-native", Name: "另一块屏", Identity: model.IdentityHints{Path: "second-path"}})
		b := newBrowser(t, f)
		b.post("scan", nil, http.StatusSeeOther)
		b.get("/devices/?candidate=native-left")
		b.post("bind", fields("backend_id", "native-left", "name", "左边屏幕", "confirmed", "true"), http.StatusSeeOther)
		f.mu.Lock()
		var proposed config.Config
		for _, request := range f.requests {
			if request.Action == model.ManagementPrepareConfig {
				if err := json.Unmarshal(request.Data, &proposed); err != nil {
					t.Fatal(err)
				}
			}
		}
		f.mu.Unlock()
		if len(proposed.Displays) != 1 || proposed.Displays[0].ID == "native-left" || proposed.Displays[0].Binding == nil || proposed.Displays[0].Binding.Path != "path-left" || proposed.Displays[0].Name != "左边屏幕" {
			t.Fatalf("人工绑定未保留独立逻辑ID/名称/路径: %+v", proposed.Displays)
		}
		if !strings.Contains(b.body, "尚未保存") || f.count(model.ManagementApplyCommit) != 0 {
			t.Fatal("预览被误当成保存")
		}
		b.zeroEffects()
	})

	t.Run("空能力和读回失败仍允许人工校准且取消零副作用", func(t *testing.T) {
		f := newFake()
		f.manual = true
		b := newBrowser(t, f)
		b.inspect()
		b.post("calibration/prepare", calibrationFields(), http.StatusSeeOther)
		if !strings.Contains(b.body, "人工核对；未补造读回依据") || f.count(model.ManagementCalibrationBegin) != 1 {
			t.Fatal("空能力或读回失败阻断了合法人工校准")
		}
		b.post("calibration/cancel", nil, http.StatusSeeOther)
		b.post("calibration/save", fields("confirmed", "true"), http.StatusBadRequest)
		if f.count(model.ManagementPrepareCalibration) != 0 || strings.Contains(b.body, "请核对本次校准") {
			t.Fatal("取消后仍可保存旧预览")
		}
		if !strings.Contains(b.body, "工作电脑 · USB-C") {
			t.Fatal("取消错误地丢失人工草稿")
		}
		b.zeroEffects()
	})

	t.Run("明确不支持保留原因且不生成保存", func(t *testing.T) {
		f := newFake()
		f.unsupported = true
		b := newBrowser(t, f)
		b.inspect()
		b.post("calibration/prepare", calibrationFields(), http.StatusBadRequest)
		if !strings.Contains(b.body, "当前后端不支持") || f.count(model.ManagementPrepareCalibration) != 0 {
			t.Fatal("不支持被吞掉或仍生成保存")
		}
		b.zeroEffects()
	})

	t.Run("新目标和输入方案自动编号且不修改已有共用参数", func(t *testing.T) {
		f := newFake()
		f.manual = true
		b := newBrowser(t, f)
		b.inspect()
		input := calibrationFields()
		input.Set("target_id", "")
		input.Set("port_name", "娱乐电脑 · HDMI")
		b.post("calibration/prepare", input, http.StatusSeeOther)
		var data model.ManagementCalibrationBeginData
		f.mu.Lock()
		for _, request := range f.requests {
			if request.Action == model.ManagementCalibrationBegin {
				if err := json.Unmarshal(request.Data, &data); err != nil {
					t.Fatal(err)
				}
			}
		}
		f.mu.Unlock()
		if !model.ValidID(string(data.TargetID)) || data.TargetID == "work" || data.Scheme.ID == f.cfg.InputSchemes[0].ID || !strings.Contains(b.body, "娱乐电脑 · HDMI") || f.cfg.InputSchemes[0].Value != 17 {
			t.Fatal("新校准没有独立编号或改动了既有共用参数")
		}
		b.zeroEffects()
	})

	t.Run("非法表单和缺确认不调用保存或硬件", func(t *testing.T) {
		b := newBrowser(t, newFake())
		b.inspect()
		for _, value := range []string{"0", "65536", "0xGG", "-1"} {
			input := calibrationFields()
			input.Set("value", value)
			b.post("calibration/prepare", input, http.StatusBadRequest)
		}
		input := calibrationFields()
		input.Set("protocol", "guessed")
		b.post("calibration/prepare", input, http.StatusBadRequest)
		b.post("bind", fields("backend_id", "native-left", "name", "错误未确认"), http.StatusBadRequest)
		b.post("trial/start", fields("display_id", "left", "target_id", "work"), http.StatusBadRequest)
		b.post("calibration/save", nil, http.StatusBadRequest)
		b.post("calibration/prepare", calibrationFields(), http.StatusSeeOther)
		b.post("calibration/save", nil, http.StatusBadRequest)
		before := len(b.fake.requests)
		request := httptest.NewRequest(http.MethodPost, "/devices/calibration/save", strings.NewReader("confirmed=true"))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		recorder := httptest.NewRecorder()
		b.handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusForbidden || len(b.fake.requests) != before {
			t.Fatal("无表单凭据仍触发了后台动作")
		}
		oversized := httptest.NewRequest(http.MethodPost, "/devices/calibration/save", strings.NewReader("name="+strings.Repeat("x", 17<<10)))
		oversized.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		recorder = httptest.NewRecorder()
		b.handler.ServeHTTP(recorder, oversized)
		if recorder.Code != http.StatusBadRequest || len(b.fake.requests) != before {
			t.Fatal("超大表单仍触发了后台动作")
		}
		duplicate := fields("confirmed", "true")
		duplicate["confirmed"] = []string{"true", "false"}
		b.post("calibration/save", duplicate, http.StatusBadRequest)
		b.post("calibration/save", fields("generation", "stale", "confirmed", "true"), http.StatusBadRequest)
		if b.fake.count(model.ManagementPrepareConfig) != 0 || b.fake.count(model.ManagementPrepareCalibration) != 0 || b.fake.count(model.ManagementTrialBegin) != 0 {
			t.Fatal("非法/未确认输入进入保存或试切")
		}
		b.zeroEffects()
	})

	t.Run("Prepare仅接收显式Apply才保存且证据分开", func(t *testing.T) {
		b := newBrowser(t, newFake())
		id := b.prepareCalibration()
		b.zeroEffects()
		if !strings.HasPrefix(id, "commit-"+string(generation)+"-") || !strings.Contains(b.body, "已接收预览，尚未保存") {
			t.Fatal("提交编号未绑定运行代或状态错误")
		}
		b.post("save/apply", fields("commit_id", id), http.StatusBadRequest)
		b.zeroEffects()
		b.post("save/apply", fields("commit_id", id, "confirmed", "true"), http.StatusSeeOther)
		if b.fake.saved != 1 || b.fake.hardware != 0 || b.fake.count(model.ManagementApplyCommit) != 1 {
			t.Fatal("Apply保存次数或硬件副作用不符合边界")
		}
		b.post("save/apply", fields("commit_id", id, "confirmed", "true"), http.StatusBadRequest)
		b.post("reconnect", nil, http.StatusSeeOther)
		b.post("save/query", fields("commit_id", id), http.StatusSeeOther)
		if !strings.Contains(regexp.MustCompile(`<[^>]+>`).ReplaceAllString(b.body, ""), "保存状态：已确认；生效状态：已确认") || b.fake.saved != 1 {
			t.Fatal("保存/生效依据未分开或被重新保存")
		}
	})

	t.Run("Prepare应答丢失保留原ID和草稿只查询", func(t *testing.T) {
		f := newFake()
		f.prepareLost = true
		b := newBrowser(t, f)
		b.inspect()
		b.post("calibration/prepare", calibrationFields(), http.StatusSeeOther)
		b.post("calibration/save", fields("confirmed", "true"), http.StatusBadRequest)
		f.mu.Lock()
		id := string(f.commit.ID)
		f.mu.Unlock()
		b.post("calibration/save", fields("confirmed", "true"), http.StatusBadRequest)
		b.post("save/query", fields("commit_id", id), http.StatusSeeOther)
		if f.count(model.ManagementPrepareCalibration) != 1 || f.count(model.ManagementCommitStatus) != 1 || !strings.Contains(b.body, "工作电脑 · USB-C") || !strings.Contains(b.body, id) {
			t.Fatal("未知Prepare未保留原ID/草稿或发生重提")
		}
		b.zeroEffects()
	})

	t.Run("Apply未知只查询原ID不重发", func(t *testing.T) {
		f := newFake()
		f.applyLost = true
		f.queryPhase = model.CommitUnknown
		b := newBrowser(t, f)
		id := b.prepareCalibration()
		b.post("save/apply", fields("commit_id", id, "confirmed", "true"), http.StatusBadRequest)
		b.post("reconnect", nil, http.StatusSeeOther)
		b.post("save/query", fields("commit_id", id), http.StatusSeeOther)
		b.post("save/apply", fields("commit_id", id, "confirmed", "true"), http.StatusBadRequest)
		b.post("calibration/save", fields("confirmed", "true"), http.StatusBadRequest)
		if f.count(model.ManagementApplyCommit) != 1 || f.count(model.ManagementPrepareCalibration) != 1 || f.count(model.ManagementCommitStatus) != 1 || f.saved != 1 || !strings.Contains(b.body, "结果未知") || !strings.Contains(b.body, id) {
			t.Fatal("未知Apply被重发或丢失提交依据")
		}
	})

	t.Run("保存失败不自动重试且不声称生效", func(t *testing.T) {
		f := newFake()
		f.applyFailed = true
		b := newBrowser(t, f)
		id := b.prepareCalibration()
		b.post("save/apply", fields("commit_id", id, "confirmed", "true"), http.StatusSeeOther)
		b.post("reconnect", nil, http.StatusSeeOther)
		b.post("save/query", fields("commit_id", id), http.StatusSeeOther)
		if f.saved != 1 || !strings.Contains(b.body, "已保存但尚未生效") || !strings.Contains(regexp.MustCompile(`<[^>]+>`).ReplaceAllString(b.body, ""), "生效状态：已确认尚未完成") {
			t.Fatal("保存失败被自动重试或提升为生效")
		}
	})

	t.Run("单屏试切逐步确认且取消未发为零硬件", func(t *testing.T) {
		b := newBrowser(t, newFake())
		b.inspect()
		b.post("trial/start", fields("display_id", "left", "target_id", "work", "confirmed", "true"), http.StatusSeeOther)
		b.zeroEffects()
		b.post("bind", fields("backend_id", "native-left", "name", "不能覆盖待发流程", "confirmed", "true"), http.StatusBadRequest)
		b.post("trial/step", nil, http.StatusBadRequest)
		b.post("trial/stop", nil, http.StatusSeeOther)
		if !strings.Contains(b.body, "已要求停止未发送步骤；已发结果继续保留，不自动回切。") || !strings.Contains(b.body, "已停止未发送步骤；已发送证据仍保留") {
			t.Fatal("停止响应未显示停止状态或不自动回切的即时提示")
		}
		rejected := b.post("trial/step", fields("confirmed", "true"), http.StatusBadRequest)
		if !strings.Contains(rejected.Body.String(), "本次步骤须明确确认，且不能重发在途、未知或已停止的步骤") {
			t.Fatal("停止后再次发送未返回明确的拒绝原因")
		}
		if steps := b.fake.count(model.ManagementTrialStep); steps != 0 {
			t.Fatalf("取消未发步骤后仍派发试切: got %d want 0", steps)
		}
		if stops := b.fake.count(model.ManagementTrialStop); stops != 1 {
			t.Fatalf("停止请求次数错误: got %d want 1", stops)
		}
		b.zeroEffects()
		if !strings.Contains(b.body, "试切后可能看不到当前电脑，请用显示器实体键切回原输入。取消只停止尚未发送的步骤，不保证回切，也没有倒计时自动恢复。") || !strings.Contains(b.body, "先找到显示器的实体输入键或菜单。") {
			t.Fatal("拒绝后的页面缺少完整取消边界或实体键返回指南")
		}
	})

	t.Run("发送读回和人工画面确认分离", func(t *testing.T) {
		b := newBrowser(t, newFake())
		b.inspect()
		b.post("trial/start", fields("display_id", "left", "target_id", "work", "confirmed", "true"), http.StatusSeeOther)
		b.post("trial/step", fields("confirmed", "true"), http.StatusSeeOther)
		for _, phrase := range []string{"发送结果", "输入读回", "人工画面确认", "已读回目标输入", "尚未人工确认画面"} {
			if !strings.Contains(b.body, phrase) {
				t.Fatalf("缺少分离证据: %s", phrase)
			}
		}
		match := requestPattern.FindStringSubmatch(b.body)
		if len(match) != 2 {
			t.Fatal("没有关联人工画面确认的请求编号")
		}
		b.post("trial/confirm", fields("index", "0", "request_id", match[1], "confirmed", "true"), http.StatusSeeOther)
		b.post("trial/step", fields("confirmed", "true"), http.StatusBadRequest)
		if !strings.Contains(b.body, "本次已人工确认看到目标画面") || b.fake.hardware != 1 || b.fake.saved != 0 || b.fake.directSwitches != 0 {
			t.Fatal("画面确认改写硬件结果或产生额外副作用")
		}
	})

	t.Run("切走后未知保留发送证据且不提升成功", func(t *testing.T) {
		f := newFake()
		f.trialState = model.StateUnknown
		b := newBrowser(t, f)
		b.inspect()
		b.post("trial/start", fields("display_id", "left", "target_id", "work", "confirmed", "true"), http.StatusSeeOther)
		b.post("trial/step", fields("confirmed", "true"), http.StatusSeeOther)
		if !strings.Contains(b.body, "发送后失联，结果未知") || !strings.Contains(b.body, "已发送 ·") || strings.Contains(b.body, "已读回目标输入") || f.hardware != 1 {
			t.Fatal("未知结果丢失发送证据或被提升为成功")
		}
	})

	t.Run("在途停止保留最终发送证据且没有回切", func(t *testing.T) {
		f := newFake()
		f.stepEntered, f.stepRelease = make(chan struct{}), make(chan struct{})
		b := newBrowser(t, f)
		b.inspect()
		b.post("trial/start", fields("display_id", "left", "target_id", "work", "confirmed", "true"), http.StatusSeeOther)
		values := fields("_token", b.token, "generation", b.gen, "confirmed", "true")
		request := httptest.NewRequest(http.MethodPost, "/devices/trial/step", strings.NewReader(values.Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		done := make(chan *httptest.ResponseRecorder, 1)
		go func() {
			recorder := httptest.NewRecorder()
			b.handler.ServeHTTP(recorder, request)
			done <- recorder
		}()
		select {
		case <-f.stepEntered:
		case <-time.After(2 * time.Second):
			t.Fatal("真实处理器未派发在途步骤")
		}
		b.post("trial/stop", nil, http.StatusSeeOther)
		close(f.stepRelease)
		select {
		case recorder := <-done:
			if recorder.Code != http.StatusSeeOther {
				t.Fatalf("在途步骤回收失败: %d %s", recorder.Code, recorder.Body.String())
			}
		case <-time.After(2 * time.Second):
			t.Fatal("停止后未能回收原步骤")
		}
		b.get("/devices/")
		if !strings.Contains(b.body, "已停止未发送步骤") || !strings.Contains(b.body, "已发送 ·") || f.hardware != 1 || f.saved != 0 || f.count(model.ManagementTrialStep) != 1 {
			t.Fatal("在途停止丢失结果、重发或产生回切")
		}
	})

	t.Run("人工名称按HTML转义而非执行", func(t *testing.T) {
		f := newFake()
		f.cfg.Displays[0].Name = `<img src=x onerror=alert(1)>`
		b := newBrowser(t, f)
		if strings.Contains(b.body, `<img src=x`) || !strings.Contains(b.body, "&lt;img") {
			t.Fatal("配置中的名称未经转义")
		}
		b.zeroEffects()
	})

	t.Run("已保存名称保留人工依据且不补造读回", func(t *testing.T) {
		f := newFake()
		confirmed := time.Now().Add(-time.Hour)
		f.cfg.Calibrations = []config.CalibrationMetadata{{DisplayID: "left", TargetID: "work", PortName: "工作电脑 · 扩展坞", Scheme: f.cfg.InputSchemes[0], Basis: "manual", ConfirmedAt: &confirmed}}
		b := newBrowser(t, f)
		for _, phrase := range []string{"已保存的输入名称", "工作电脑 · 扩展坞", "人工核对；未补造读回依据", "人工确认时间"} {
			if !strings.Contains(b.body, phrase) {
				t.Fatalf("已保存名称依据缺少 %q", phrase)
			}
		}
		b.zeroEffects()
	})
}

func TestSettingsProductDevicesLanguage(t *testing.T) {
	t.Run("系统缺省英文且显式中文实例隔离", func(t *testing.T) {
		english := newBrowserWithLanguage(t, newFake(), "system")
		chinese := newBrowser(t, newFake())
		response := english.get("/devices/")
		match := catalogPattern.FindStringSubmatch(response.Body.String())
		var catalog i18n.Catalog
		if len(match) != 4 || json.Unmarshal([]byte(match[2]), &catalog) != nil || catalog.Locale != "en" || catalog.Preference != "system" || catalog.Messages["devices.scan"] != english.language.Snapshot().Messages["devices.scan"] {
			t.Fatal("模板和浏览器没有使用服务的同一词典快照")
		}
		for _, expected := range []string{`lang="en"`, "Scan displays", "Prepare a way back", `id="desktop-catalog"`, `/desktop-i18n.js`, `data-i18n-placeholder="devices.display_name_placeholder"`, `data-i18n-aria-label="nav.brand"`} {
			if !strings.Contains(english.body, expected) {
				t.Fatalf("英文页面缺少 %q", expected)
			}
		}
		if !strings.Contains(chinese.body, `lang="zh-CN"`) || !strings.Contains(chinese.body, "扫描显示器") || strings.Contains(english.body, "先找到显示器的实体输入键") {
			t.Fatal("页面语言未遵循各自服务的同一语言快照")
		}
		english.zeroEffects()
		chinese.zeroEffects()
	})

	t.Run("切换语言保留草稿提交编号和未知试切诊断", func(t *testing.T) {
		fake := newFake()
		fake.trialState = model.StateUnknown
		fake.trialError = model.NewError(model.CodeUnreachable, "原始发送诊断 <device>", nil)
		fake.trialError.Field, fake.trialError.Retryable = "display.left", true
		b := newBrowserWithLanguage(t, fake, "en")
		b.inspect()
		b.post("calibration/prepare", calibrationFields(), http.StatusSeeOther)
		if err := b.language.SetPreference("zh-CN"); err != nil {
			t.Fatal(err)
		}
		b.get("/devices/")
		if !strings.Contains(b.body, "工作电脑 · USB-C") || !strings.Contains(b.body, "请核对本次校准") {
			t.Fatal("语言变化丢失校准草稿或预览")
		}
		b.post("calibration/cancel", nil, http.StatusSeeOther)
		b.post("trial/start", fields("display_id", "left", "target_id", "work", "confirmed", "true"), http.StatusSeeOther)
		b.post("trial/step", fields("confirmed", "true"), http.StatusSeeOther)
		if err := b.language.SetPreference("en"); err != nil {
			t.Fatal(err)
		}
		b.get("/devices/")
		for _, expected := range []string{"Result unknown", "The device could not be reached.", "Original diagnostic details", "unreachable", "原始发送诊断 &lt;device&gt;", "display.left", "true", "unreachable: 原始发送诊断", "Command sent", "Picture not confirmed yet"} {
			if !strings.Contains(b.body, expected) {
				t.Fatalf("语言变化后丢失结果状态或诊断 %q", expected)
			}
		}
		if strings.Contains(b.body, "Input readback matches") || fake.hardware != 1 || fake.saved != 0 {
			t.Fatal("翻译把未知结果提升为成功或引入副作用")
		}
		id := b.prepareCalibration()
		if err := b.language.SetPreference("zh-CN"); err != nil {
			t.Fatal(err)
		}
		b.get("/devices/")
		if !strings.Contains(b.body, id) || !strings.Contains(b.body, "工作电脑 · USB-C") || fake.count(model.ManagementApplyCommit) != 0 {
			t.Fatal("语言变化丢失原提交或自动应用")
		}
	})

	t.Run("表单门禁和结构化失败使用当前语言且保原诊断", func(t *testing.T) {
		fake := newFake()
		fake.unsupported = true
		b := newBrowserWithLanguage(t, fake, "en")
		b.inspect()
		b.post("calibration/prepare", calibrationFields(), http.StatusBadRequest)
		if !strings.Contains(b.body, "This protocol version is not supported.") || !strings.Contains(b.body, "当前后端不支持该输入协议") || !strings.Contains(b.body, "unsupported_protocol") {
			t.Fatal("结构化错误没有翻译摘要或丢失原证据")
		}
		request := httptest.NewRequest(http.MethodPost, "/devices/bind", strings.NewReader("confirmed=true"))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		recorder := httptest.NewRecorder()
		b.handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusForbidden || !strings.Contains(recorder.Body.String(), "This form has expired. Reopen Devices.") {
			t.Fatal("安全门禁未使用当前词典或改变了拒绝状态")
		}
		b.zeroEffects()
	})

	t.Run("等待文案复用共享事件而不重载语言草稿", func(t *testing.T) {
		source, err := fs.ReadFile(devicepage.Page().Resources, "devices.js")
		if err != nil {
			t.Fatal(err)
		}
		js := string(source)
		for _, expected := range []string{"desktopI18n.text", "desktop:translated", "devices.operation_wait", "devices.operation_stop", "devices.operation_interrupted", "data-i18n"} {
			if !strings.Contains(js, expected) {
				t.Fatalf("动态文案缺少共享语言接线 %q", expected)
			}
		}
		if strings.Contains(js, "navigator.language") || strings.Contains(js, "location.reload") || strings.Contains(js, "正在等待本次试切") {
			t.Fatal("动态文案另作语言决策或保留硬编码")
		}
	})
}

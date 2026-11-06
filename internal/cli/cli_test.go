package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/orzlee/displayKvm/internal/config"
	"github.com/orzlee/displayKvm/internal/model"
)

type fakeClient struct {
	cfg                config.Config
	configErr          error
	enumerateErr       error
	capErr             error
	emitErr            error
	devices            []model.Device
	capabilities       []model.EventCapability
	switchFn           func(context.Context, model.SwitchRequest) (model.SwitchResult, error)
	requests           []model.SwitchRequest
	events             []model.Event
	configCalls        int
	enumerations       int
	capCalls           int
	managementFn       func(context.Context, model.ManagementRequest) (model.ManagementResponse, error)
	managementRequests []model.ManagementRequest
}

func (f *fakeClient) Configuration() (config.Config, error) {
	f.configCalls++
	return f.cfg, f.configErr
}

func (f *fakeClient) Enumerate(context.Context) ([]model.Device, error) {
	f.enumerations++
	return f.devices, f.enumerateErr
}

func (f *fakeClient) EventCapabilities(context.Context) ([]model.EventCapability, error) {
	f.capCalls++
	return f.capabilities, f.capErr
}

func (f *fakeClient) Switch(ctx context.Context, request model.SwitchRequest) (model.SwitchResult, error) {
	f.requests = append(f.requests, request)
	if f.switchFn != nil {
		return f.switchFn(ctx, request)
	}
	return successfulSwitch(request, "left", "right"), nil
}

func (f *fakeClient) EmitEvent(_ context.Context, event model.Event) error {
	f.events = append(f.events, event)
	return f.emitErr
}

func (f *fakeClient) Management(ctx context.Context, request model.ManagementRequest) (model.ManagementResponse, error) {
	f.managementRequests = append(f.managementRequests, request)
	if f.managementFn != nil {
		return f.managementFn(ctx, request)
	}
	return model.ManagementResponse{}, model.NewError(model.CodeUnsupportedOperation, "旧后台不支持管理操作", nil)
}

func fixtureConfig() config.Config {
	return config.Config{
		Version:      config.CurrentVersion,
		Displays:     []model.Display{{ID: "left", Name: "左屏"}, {ID: "right", Name: "右屏"}, {ID: "spare", Name: "备用"}},
		InputSchemes: []model.InputScheme{{ID: "hdmi", Protocol: model.ProtocolVCP, VCPCode: 0x60, Value: 17}},
		Targets:      []model.Target{{ID: "work", Inputs: []model.TargetInput{{DisplayID: "left", SchemeID: "hdmi"}, {DisplayID: "right", SchemeID: "hdmi"}}}},
		Groups:       []model.Group{{ID: "desk", DisplayIDs: []model.DisplayID{"right", "left"}}, {ID: "all", DisplayIDs: []model.DisplayID{"left", "spare"}}},
	}
}

var fixedTime = time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)

func successfulSwitch(request model.SwitchRequest, ids ...model.DisplayID) model.SwitchResult {
	result := model.SwitchResult{RequestID: request.ID, Displays: []model.DisplayResult{}}
	for _, id := range ids {
		sent := fixedTime
		result.Displays = append(result.Displays, model.DisplayResult{DisplayID: id, State: model.StateSent, DesiredValue: 17, SentAt: &sent})
	}
	return result
}

type captured struct {
	code  int
	out   string
	err   string
	dials int
}

func invoke(ctx context.Context, f *fakeClient, args ...string) captured {
	var stdout, stderr bytes.Buffer
	dials := 0
	ids := 0
	code := Run(ctx, args, Options{
		Stdout: &stdout, Stderr: &stderr,
		Connect: func(string) (Client, error) { dials++; return f, nil },
		NewID: func() (string, error) {
			ids++
			if ids == 1 {
				return "cli-test", nil
			}
			return "cli-test-" + strconv.Itoa(ids), nil
		},
		NewCommitID: func(model.RuntimeGeneration) (model.RequestID, error) { return testCommitID, nil },
		Now:         func() time.Time { return fixedTime },
	})
	return captured{code, stdout.String(), stderr.String(), dials}
}

func decodeEnvelope(t *testing.T, output string) map[string]json.RawMessage {
	t.Helper()
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal([]byte(output), &envelope); err != nil {
		t.Fatalf("输出不是单个JSON对象: %v; %q", err, output)
	}
	for _, field := range []string{"version", "command", "ok", "data", "error"} {
		if _, found := envelope[field]; !found {
			t.Fatalf("外壳缺少字段%s: %s", field, output)
		}
	}
	if len(envelope) != 5 || string(envelope["version"]) != "1" {
		t.Fatalf("外壳格式变化: %s", output)
	}
	return envelope
}

func TestInvalidArgumentsNeverCallClient(t *testing.T) {
	cases := [][]string{
		{"switch"},
		{"switch", "--target", "work", "--display", "left", "--group", "desk"},
		{"switch", "--target", "work", "--display", "left", "--display", "left"},
		{"switch", "--target", "work", "--display", "bad/id"},
		{"switch", "--target", "work", "--timeout-ms", "-1"},
		{"switch", "--target", "work", "--timeout-ms", "30001"},
		{"switch", "--target", "work", "--timeout-ms", "abc"},
		{"switch", "--target", "work", "unexpected"},
		{"switch", "--target", "work", "--force"},
		{"event", "emit"},
		{"event", "emit", "--name", "ready", "--kind", "usb_added"},
		{"event", "emit", "--name", "ready", "--attribute", "x"},
		{"event", "emit", "--name", "ready", "--attribute", "x=1", "--attribute", "x=2"},
		{"config", "check"},
		{"status", "extra"},
		{"unknown"},
	}
	for _, args := range cases {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			f := &fakeClient{cfg: fixtureConfig()}
			got := invoke(context.Background(), f, append([]string{"--json"}, args...)...)
			if got.code != ExitUsage || got.dials != 0 || len(f.requests)+len(f.events) != 0 {
				t.Fatalf("参数未被无调用拒绝: %+v", got)
			}
			if string(decodeEnvelope(t, got.out)["ok"]) != "false" {
				t.Fatal("错误外壳声称成功")
			}
		})
	}
}

func TestDryRunSelectionAndNoDeviceOperations(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want []model.DisplayID
	}{
		{"目标全部映射", nil, []model.DisplayID{"left", "right"}},
		{"分组顺序", []string{"--group", "desk"}, []model.DisplayID{"right", "left"}},
		{"逐屏顺序", []string{"--display", "right", "--display", "left"}, []model.DisplayID{"right", "left"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeClient{cfg: fixtureConfig()}
			args := append([]string{"switch", "--json", "--target", "work", "--dry-run"}, tc.args...)
			got := invoke(context.Background(), f, args...)
			if got.code != ExitOK || got.dials != 1 || f.configCalls != 1 || f.enumerations+f.capCalls+len(f.requests)+len(f.events) != 0 {
				t.Fatalf("dry-run调用了设备操作: %+v %+v", got, f)
			}
			var data struct {
				DryRun        bool             `json:"dry_run"`
				HardwareState string           `json:"hardware_state"`
				Displays      []plannedDisplay `json:"displays"`
			}
			if err := json.Unmarshal(decodeEnvelope(t, got.out)["data"], &data); err != nil {
				t.Fatal(err)
			}
			var ids []model.DisplayID
			for _, display := range data.Displays {
				ids = append(ids, display.DisplayID)
			}
			if !data.DryRun || data.HardwareState != "unknown" || !reflect.DeepEqual(ids, tc.want) {
				t.Fatalf("只读投影不符: %+v", data)
			}
		})
	}
}

func TestMissingMappingRejectsEntireSwitch(t *testing.T) {
	for _, selection := range [][]string{{"--display", "spare"}, {"--group", "all"}, {"--group", "missing"}, {"--display", "missing"}} {
		f := &fakeClient{cfg: fixtureConfig()}
		got := invoke(context.Background(), f, append([]string{"--json", "switch", "--target", "work"}, selection...)...)
		if got.code != ExitUsage || len(f.requests) != 0 || len(f.events) != 0 {
			t.Fatalf("缺映射仍然发送: %v %+v", selection, got)
		}
	}
}

func TestSwitchPreservesSelectionTimeoutAndEvidence(t *testing.T) {
	f := &fakeClient{cfg: fixtureConfig()}
	got := invoke(context.Background(), f, "--json", "switch", "--target", "work", "--group", "desk", "--timeout-ms", "30000")
	if got.code != ExitOK || len(f.requests) != 1 {
		t.Fatalf("切换未成功: %+v", got)
	}
	request := f.requests[0]
	if request.GroupID != "desk" || len(request.DisplayIDs) != 0 || request.TimeoutMillis != 30000 || request.ID != "cli-test" {
		t.Fatalf("请求被改写: %+v", request)
	}
	envelope := decodeEnvelope(t, got.out)
	var data model.SwitchResult
	if err := json.Unmarshal(envelope["data"], &data); err != nil {
		t.Fatal(err)
	}
	if data.RequestID != request.ID || len(data.Displays) != 2 || data.Displays[0].SentAt == nil || data.Displays[0].DesiredValue != 17 || data.Displays[0].State != model.StateSent {
		t.Fatalf("逐屏证据丢失或类型改变: %+v", data)
	}
}

func TestSwitchNeverTreatsPartialOrMismatchedResultsAsSuccess(t *testing.T) {
	cases := []struct {
		name   string
		change func(*model.SwitchResult)
	}{
		{"缺一屏", func(r *model.SwitchResult) { r.Displays = r.Displays[:1] }},
		{"关联ID不符", func(r *model.SwitchResult) { r.RequestID = "other" }},
		{"额外屏", func(r *model.SwitchResult) { r.Displays[1].DisplayID = "spare" }},
		{"重复屏", func(r *model.SwitchResult) { r.Displays[1].DisplayID = "left" }},
		{"错误目标输入", func(r *model.SwitchResult) { r.Displays[1].DesiredValue = 18 }},
		{"仅已受理", func(r *model.SwitchResult) { r.Displays[1].State = model.StateAccepted; r.Displays[1].SentAt = nil }},
		{"失败", func(r *model.SwitchResult) {
			r.Displays[1].State = model.StateFailed
			r.Displays[1].Error = model.NewError(model.CodeUnreachable, "设备不可达", nil)
		}},
		{"未知", func(r *model.SwitchResult) {
			r.Displays[1].State = model.StateUnknown
			r.Displays[1].Error = model.NewError(model.CodeTransport, "发送后失联", nil)
		}},
		{"无确认观测", func(r *model.SwitchResult) { r.Displays[1].State = model.StateConfirmed }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeClient{cfg: fixtureConfig(), switchFn: func(_ context.Context, request model.SwitchRequest) (model.SwitchResult, error) {
				result := successfulSwitch(request, "left", "right")
				tc.change(&result)
				return result, nil
			}}
			got := invoke(context.Background(), f, "switch", "--json", "--target", "work")
			if got.code != ExitFailure || len(f.requests) != 1 || string(decodeEnvelope(t, got.out)["ok"]) != "false" {
				t.Fatalf("失败误报或重发: %+v", got)
			}
		})
	}
}

func TestResultSurvivesCancellationAfterResponse(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := &fakeClient{cfg: fixtureConfig(), switchFn: func(_ context.Context, request model.SwitchRequest) (model.SwitchResult, error) {
		cancel()
		return successfulSwitch(request, "left", "right"), nil
	}}
	got := invoke(ctx, f, "--json", "switch", "--target", "work")
	if got.code != ExitOK || len(f.requests) != 1 {
		t.Fatalf("真实证据被取消覆盖: %+v", got)
	}
}

func TestSwitchTransportErrorPreservesReceivedEvidenceWithoutRetry(t *testing.T) {
	f := &fakeClient{cfg: fixtureConfig(), switchFn: func(_ context.Context, request model.SwitchRequest) (model.SwitchResult, error) {
		return successfulSwitch(request, "left"), context.DeadlineExceeded
	}}
	got := invoke(context.Background(), f, "--json", "switch", "--target", "work")
	if got.code != ExitTimeout || len(f.requests) != 1 || !strings.Contains(got.out, `"sent_at"`) {
		t.Fatalf("超时证据丢失或重发: %+v", got)
	}
}

func TestConnectionErrorsAndGlobalFlags(t *testing.T) {
	for _, tc := range []struct {
		err  error
		code int
	}{
		{model.NewError(model.CodeDaemonUnavailable, "请手动启动后台", nil), ExitUnavailable},
		{model.NewError(model.CodeAuthenticationFailed, "认证失败", nil), ExitFailure},
		{model.NewError(model.CodePermissionDenied, "权限不足", nil), ExitFailure},
		{model.NewError(model.CodeUnsupportedVersion, "版本不兼容", nil), ExitFailure},
		{context.Canceled, ExitCanceled},
		{context.DeadlineExceeded, ExitTimeout},
		{errors.New("private-token-and-path"), ExitFailure},
	} {
		var out, stderr bytes.Buffer
		directory := ""
		code := Run(context.Background(), []string{"list", "--config-dir", "private-directory", "--json"}, Options{Stdout: &out, Stderr: &stderr, Connect: func(dir string) (Client, error) { directory = dir; return nil, tc.err }})
		if code != tc.code || directory != "private-directory" || strings.Contains(out.String(), "private-token-and-path") {
			t.Fatalf("分类或隐私错误: %d %s", code, out.String())
		}
		decodeEnvelope(t, out.String())
	}
}

func TestListAndStatusDoNotInferCurrentInput(t *testing.T) {
	f := &fakeClient{cfg: fixtureConfig()}
	list := invoke(context.Background(), f, "list", "--json")
	if list.code != ExitOK || !strings.Contains(list.out, `"devices":[]`) {
		t.Fatalf("空枚举格式不稳定: %+v", list)
	}
	status := invoke(context.Background(), f, "--json", "status")
	if status.code != ExitOK || !strings.Contains(status.out, `"current_input":"unknown"`) || !strings.Contains(status.out, `"last_switch":"unknown"`) {
		t.Fatalf("状态推断了未知画面: %+v", status)
	}
	if len(f.requests)+len(f.events) != 0 || f.capCalls != 1 {
		t.Fatal("查询产生写操作或遗漏能力查询")
	}
}

func TestConfigCheckReadsRealLoaderAndNeverWritesOrConnects(t *testing.T) {
	cases := []struct {
		name, content string
		code          int
	}{
		{"完整文档", `{"kind":"displaykvm-settings","version":1,"configuration":{"version":3,"displays":[],"input_schemes":[],"targets":[],"groups":[]},"rules":[],"schedules":[],"runtime":{"tool_path":"","busctl_path":"","udevadm_path":""}}`, ExitOK},
		{"裸v1", `{"version":1,"displays":[{"id":"left","name":"左屏","identity":{}}],"targets":[{"id":"work","name":"工作","inputs":[{"display_id":"left","protocol":"vcp","vcp_code":96,"value":17}]}],"groups":[]}`, ExitUsage},
		{"裸v2", `{"version":2,"displays":[],"input_schemes":[],"targets":[],"groups":[]}`, ExitUsage},
		{"裸v3", `{"version":3,"displays":[],"input_schemes":[],"targets":[],"groups":[]}`, ExitUsage},
		{"未知版本", `{"version":999}`, ExitUsage},
		{"未知字段", `{"version":2,"unexpected":true}`, ExitUsage},
		{"重复字段", `{"version":2,"version":2}`, ExitUsage},
		{"额外文档", `{"version":2} {}`, ExitUsage},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(tc.content), 0600); err != nil {
				t.Fatal(err)
			}
			got := invoke(context.Background(), &fakeClient{}, "config", "check", "--json", "--file", path)
			if got.code != tc.code || got.dials != 0 {
				t.Fatalf("配置检查结果或调用错误: %+v", got)
			}
			data, err := os.ReadFile(path)
			if err != nil || string(data) != tc.content {
				t.Fatal("检查修改了源配置")
			}
			if tc.code == ExitOK && !strings.Contains(got.out, `"saved":false`) {
				t.Fatal("只读检查被误报为保存")
			}
		})
	}
}

func TestEmitUsesUnifiedClientAndReportsAcceptanceOnly(t *testing.T) {
	f := &fakeClient{}
	got := invoke(context.Background(), f, "event", "emit", "--json", "--name", "ready", "--attribute", "source=a=b")
	if got.code != ExitOK || len(f.events) != 1 || len(f.requests) != 0 {
		t.Fatalf("事件未通过唯一入口: %+v", got)
	}
	event := f.events[0]
	if event.Kind != model.EventCustom || event.Source != "cli" || event.Name != "ready" || event.ID != "cli-test" || event.Attributes["source"] != "a=b" || !event.ObservedAt.Equal(fixedTime) {
		t.Fatalf("事件事实被改写: %+v", event)
	}
	if !strings.Contains(got.out, `"accepted":true`) || !strings.Contains(got.out, "不代表") {
		t.Fatal("受理被误报为切换成功")
	}
}

type brokenOutput struct{}

func (brokenOutput) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }

func TestOutputFailureDoesNotRetryAction(t *testing.T) {
	f := &fakeClient{cfg: fixtureConfig()}
	var stderr bytes.Buffer
	code := Run(context.Background(), []string{"--json", "switch", "--target", "work"}, Options{Stdout: brokenOutput{}, Stderr: &stderr, Connect: func(string) (Client, error) { return f, nil }, NewID: func() (string, error) { return "test", nil }})
	if code != ExitFailure || len(f.requests) != 1 || !strings.Contains(stderr.String(), "可能已经执行") {
		t.Fatalf("输出失败发生重发或误报: %d %s", code, stderr.String())
	}
}

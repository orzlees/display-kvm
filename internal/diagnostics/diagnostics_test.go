package diagnostics

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/orzlee/displayKvm/internal/config"
	"github.com/orzlee/displayKvm/internal/model"
	"github.com/orzlee/displayKvm/internal/switcher"
)

// 测试只把注入后端交给真实08队列，诊断代码必须沿同一队列执行；
// 不接触平台后端或真实硬件，避免用伪造的诊断队列掩盖所有权问题。
type testBackend struct {
	devices   []model.Device
	enumerate func(context.Context) ([]model.Device, error)
	caps      model.DeviceCapabilities
	capsErr   error
	read      func(context.Context) (model.InputObservation, error)
	write     func(context.Context) (model.WriteReceipt, error)
	mu        sync.Mutex
	writes    int
}

func (b *testBackend) Name() string { return "diagnostics-test" }

func (b *testBackend) Enumerate(ctx context.Context) ([]model.Device, error) {
	if b.enumerate != nil {
		return b.enumerate(ctx)
	}
	return append([]model.Device(nil), b.devices...), nil
}

func (b *testBackend) Capabilities(context.Context, model.Device) (model.DeviceCapabilities, error) {
	return b.caps, b.capsErr
}

func (b *testBackend) ReadInput(ctx context.Context, _ model.Device, _ model.InputScheme) (model.InputObservation, error) {
	if b.read != nil {
		return b.read(ctx)
	}
	return model.InputObservation{}, model.NewError(model.CodeUnsupportedOperation, "不可读取", nil)
}

func (b *testBackend) SetInput(ctx context.Context, _ model.Device, _ model.InputScheme) (model.WriteReceipt, error) {
	b.mu.Lock()
	b.writes++
	b.mu.Unlock()
	if b.write != nil {
		return b.write(ctx)
	}
	return model.WriteReceipt{SentAt: time.Now()}, nil
}

func fixture(t *testing.T, change func(*config.Config, *testBackend)) (*Service, *switcher.Queue, *testBackend, config.Config) {
	t.Helper()
	hints := model.IdentityHints{Manufacturer: "maker", ProductCode: "product", Serial: "secret-serial", Path: "secret-path"}
	cfg := config.Config{
		Version:      config.CurrentVersion,
		Displays:     []model.Display{{ID: "screen", Name: "secret-display", Identity: hints, Binding: &model.Binding{Path: hints.Path}}},
		InputSchemes: []model.InputScheme{{ID: "hdmi", Protocol: model.ProtocolVCP, VCPCode: 0x60, Value: 17}},
		Targets:      []model.Target{{ID: "host", Name: "secret-host", Inputs: []model.TargetInput{{DisplayID: "screen", SchemeID: "hdmi"}}}},
	}
	b := &testBackend{devices: []model.Device{{BackendID: "secret-backend", Name: "secret-device", Identity: hints}}, caps: model.DeviceCapabilities{Protocols: []model.InputProtocol{model.ProtocolVCP}}}
	if change != nil {
		change(&cfg, b)
	}
	q, err := switcher.New(b, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = q.Close() })
	s, err := New(q, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return s, q, b, cfg
}

func calibrationRequest() CalibrationRequest {
	return CalibrationRequest{DisplayID: "screen", TargetID: "other-host", PortName: "桌面HDMI口", Scheme: model.InputScheme{ID: "calibrated", Protocol: model.ProtocolVCP, VCPCode: 0x60, Value: 18}}
}

func TestBackendDiagnosisWorksBeforeAnyDisplayIsConfigured(t *testing.T) {
	for _, available := range []bool{true, false} {
		b := &testBackend{devices: []model.Device{{BackendID: "secret-backend"}}}
		if !available {
			b.enumerate = func(context.Context) ([]model.Device, error) {
				return nil, model.NewError(model.CodePermissionDenied, "secret-permission", nil)
			}
		}
		cfg := config.Config{Version: config.CurrentVersion}
		queue, err := switcher.New(b, cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = queue.Close() })
		service, err := New(queue, cfg)
		if err != nil {
			t.Fatal(err)
		}
		diagnostic, err := service.InspectBackend(context.Background())
		if err != nil || diagnostic.Available != available {
			t.Fatalf("未配置屏幕也要能解释后端枚举状态: %+v %v", diagnostic, err)
		}
		if !available && (diagnostic.Issue == nil || diagnostic.Issue.Code != model.CodePermissionDenied) {
			t.Fatalf("后端失败必须保留具体类别: %+v", diagnostic)
		}
		var output bytes.Buffer
		if err := EncodeReport(&output, Report{Backend: &diagnostic}); err != nil || strings.Contains(output.String(), "secret") {
			t.Fatalf("后端诊断也必须脱敏: %s %v", output.String(), err)
		}
	}
}

func TestA01IncompleteCapabilitiesAllowExplicitManualCalibration(t *testing.T) {
	s, _, b, original := fixture(t, func(_ *config.Config, b *testBackend) { b.caps = model.DeviceCapabilities{} })
	diagnostic, err := s.Inspect(context.Background(), "screen", "hdmi")
	if err != nil || !diagnostic.CanCalibrate || diagnostic.CapabilityIssue == nil || diagnostic.ObservationIssue == nil {
		t.Fatalf("空能力必须保留未知并允许人工校准: %+v, %v", diagnostic, err)
	}
	draft, err := s.BeginCalibration(context.Background(), calibrationRequest())
	if err != nil {
		t.Fatal(err)
	}
	evidence := draft.Evidence()
	if evidence.Basis != BasisManual || evidence.Observation != nil || evidence.ConfirmedAt != nil {
		t.Fatalf("没有读回不能伪造确认依据: %+v", evidence)
	}
	saves := 0
	persist := func(ctx context.Context, cfg config.Config, evidence CalibrationEvidence) error {
		saves++
		if _, err := s.queue.Enumerate(ctx); model.CodeOf(err) != model.CodeBackendUnavailable {
			t.Fatalf("持久化前旧队列必须已经关闭: %v", err)
		}
		if evidence.ConfirmedAt == nil || evidence.PortName != "桌面HDMI口" || cfg.Targets[1].Name != evidence.PortName {
			t.Fatalf("必须保存人工命名和明确确认依据: %+v, %+v", cfg, evidence)
		}
		if err := cfg.Validate(); err != nil {
			t.Fatal(err)
		}
		cfg.Displays[0].Binding.Path = "changed-by-writer"
		return nil
	}
	if _, err := s.SaveCalibration(context.Background(), draft, false, persist); model.CodeOf(err) != model.CodeInvalidRequest || saves != 0 {
		t.Fatalf("未确认不能保存: %v, %d", err, saves)
	}
	if _, err := s.queue.Enumerate(context.Background()); err != nil {
		t.Fatalf("未确认不能关闭队列: %v", err)
	}
	updated, err := s.SaveCalibration(context.Background(), draft, true, persist)
	if err != nil || saves != 1 || len(updated.InputSchemes) != 2 || updated.InputSchemes[1].Value != 18 {
		t.Fatalf("人工确认后应保存确切输入值: %+v, %v", updated, err)
	}
	if original.Displays[0].Binding.Path != "secret-path" || len(original.Targets) != 1 || updated.Displays[0].Binding.Path != "secret-path" || b.writes != 0 {
		t.Fatal("校准预览和保存不得改写源配置、共享存储或试切")
	}
	if _, err := s.SaveCalibration(context.Background(), draft, true, persist); err == nil || saves != 1 {
		t.Fatal("同一确认不得重复保存")
	}
}

func TestA01AmbiguousAbsentAndChangedBindingsCannotPlanOrCalibrate(t *testing.T) {
	for _, name := range []string{"ambiguous", "absent", "changed-binding"} {
		t.Run(name, func(t *testing.T) {
			s, _, b, _ := fixture(t, func(cfg *config.Config, b *testBackend) {
				switch name {
				case "ambiguous":
					cfg.Displays[0].Binding = nil
					other := b.devices[0]
					other.BackendID = "other"
					other.Identity.Path = "other-path"
					b.devices = append(b.devices, other)
				case "absent":
					b.devices = nil
				case "changed-binding":
					b.devices[0].Identity.Path = "new-path"
				}
			})
			if _, err := s.PlanTrial(context.Background(), []model.SwitchRequest{{ID: "trial", TargetID: "host"}}); err == nil {
				t.Fatal("身份不确定时不能建立可执行试切")
			}
			if _, err := s.BeginCalibration(context.Background(), calibrationRequest()); err == nil || b.writes != 0 {
				t.Fatalf("身份不确定时不能校准或写设备: %v", err)
			}
		})
	}
}

func TestCalibrationReadbackAndManualEvidenceAreDistinct(t *testing.T) {
	for _, scenario := range []string{"fresh", "old", "failed-read", "mismatch"} {
		t.Run(scenario, func(t *testing.T) {
			s, _, _, _ := fixture(t, func(_ *config.Config, b *testBackend) {
				b.read = func(context.Context) (model.InputObservation, error) {
					value, at := uint16(18), time.Now()
					if scenario == "old" {
						at = at.Add(-time.Hour)
					}
					if scenario == "mismatch" {
						value = 17
					}
					observation := model.InputObservation{Value: &value, ObservedAt: at}
					if scenario == "failed-read" {
						return observation, model.NewError(model.CodeUnreachable, "切走后失联", nil)
					}
					return observation, nil
				}
			})
			draft, err := s.BeginCalibration(context.Background(), calibrationRequest())
			if scenario == "mismatch" {
				if model.CodeOf(err) != model.CodeInvalidRequest {
					t.Fatalf("新读回冲突不能静默覆盖: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			evidence := draft.Evidence()
			want := BasisManual
			if scenario == "fresh" {
				want = BasisReadback
			}
			if evidence.Basis != want || evidence.Observation == nil || evidence.ConfirmedAt != nil {
				t.Fatalf("读回、历史值和人工依据须分开: %+v", evidence)
			}
			*evidence.Observation.Value = 99
			if *draft.Evidence().Observation.Value != 18 {
				t.Fatal("依据必须是独立副本")
			}
		})
	}
}

func TestCalibrationRejectsSharedSchemeRewriteAndPreservesSaveFailure(t *testing.T) {
	s, _, _, _ := fixture(t, nil)
	request := calibrationRequest()
	request.Scheme.ID = "hdmi"
	if _, err := s.BeginCalibration(context.Background(), request); model.CodeOf(err) != model.CodeInvalidConfig {
		t.Fatalf("不能通过校准改变其他目标共用的方案: %v", err)
	}
	request = calibrationRequest()
	draft, err := s.BeginCalibration(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("保存阶段失败，可能已部分持久化")
	_, err = s.SaveCalibration(context.Background(), draft, true, func(context.Context, config.Config, CalibrationEvidence) error { return sentinel })
	if !errors.Is(err, sentinel) {
		t.Fatalf("必须保留保存失败根因: %v", err)
	}
	if _, err := s.Inspect(context.Background(), "screen", ""); model.CodeOf(err) != model.CodeBackendUnavailable {
		t.Fatalf("失败后不能继续用旧快照或自动重试保存: %v", err)
	}
}

func TestA02TrialIsFiniteExplicitAndPreservesInterruptedUnknownResult(t *testing.T) {
	started := make(chan struct{})
	s, _, b, _ := fixture(t, func(_ *config.Config, b *testBackend) {
		b.write = func(ctx context.Context) (model.WriteReceipt, error) {
			close(started)
			<-ctx.Done()
			return model.WriteReceipt{}, ctx.Err()
		}
	})
	requests := []model.SwitchRequest{{ID: "step-1", TargetID: "host"}, {ID: "step-2", TargetID: "host"}}
	trial, err := s.PlanTrial(context.Background(), requests)
	if err != nil || b.writes != 0 {
		t.Fatalf("计划不能写硬件: %v", err)
	}
	requests[0].TargetID = "invented"
	if _, err := trial.Step(context.Background(), false); model.CodeOf(err) != model.CodeInvalidRequest || b.writes != 0 {
		t.Fatal("每步必须明确确认，计划还须持有独立快照")
	}
	done := make(chan TrialProgress, 1)
	go func() { progress, _ := trial.Step(context.Background(), true); done <- progress }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("注入写入未启动")
	}
	trial.Stop()
	var progress TrialProgress
	select {
	case progress = <-done:
	case <-time.After(time.Second):
		t.Fatal("取消未返回")
	}
	if !progress.Stopped || progress.Next != 1 || len(progress.Results) != 1 || progress.Results[0].Displays[0].State != model.StateUnknown || b.writes != 1 {
		t.Fatalf("中断须保留可能已发的未知结果且不回切: %+v", progress)
	}
	progress.Results[0].Displays[0].State = model.StateConfirmed
	if trial.Progress().Results[0].Displays[0].State != model.StateUnknown {
		t.Fatal("调用方不能改写历史结果")
	}
	if _, err := trial.Step(context.Background(), true); err == nil || b.writes != 1 {
		t.Fatal("停止后不能自动继续或重发")
	}
}

func TestTrialRejectsUnknownTargetsDuplicatesAndUnmappedDisplays(t *testing.T) {
	s, _, b, _ := fixture(t, nil)
	cases := [][]model.SwitchRequest{
		nil,
		{{ID: "x", TargetID: "missing"}},
		{{ID: "x", TargetID: "host"}, {ID: "x", TargetID: "host"}},
		{{ID: "x", TargetID: "host", DisplayIDs: []model.DisplayID{"missing"}}},
	}
	tooMany := make([]model.SwitchRequest, MaxTrialSteps+1)
	cases = append(cases, tooMany)
	for _, requests := range cases {
		if _, err := s.PlanTrial(context.Background(), requests); err == nil {
			t.Fatalf("应拒绝计划: %+v", requests)
		}
	}
	if b.writes != 0 {
		t.Fatal("不能探测或轮询未知输入码")
	}
}

func TestTrialRevalidatesIdentityAndExecutesOnlyOneConfirmedStep(t *testing.T) {
	s, _, b, _ := fixture(t, func(_ *config.Config, b *testBackend) {
		b.caps.CanReadInput = true
		b.read = func(context.Context) (model.InputObservation, error) {
			value := uint16(17)
			return model.InputObservation{Value: &value, ObservedAt: time.Now()}, nil
		}
	})
	trial, err := s.PlanTrial(context.Background(), []model.SwitchRequest{{ID: "one", TargetID: "host"}, {ID: "two", TargetID: "host"}})
	if err != nil {
		t.Fatal(err)
	}
	progress, err := trial.Step(context.Background(), true)
	if err != nil || progress.Next != 1 || progress.Stopped || b.writes != 1 || progress.Results[0].Displays[0].State != model.StateConfirmed {
		t.Fatalf("一次确认只能执行一步: %+v %v", progress, err)
	}
	// 计划快照不能为后来缺席的设备授权写入，实际执行须由08再次验证身份。
	b.devices = nil
	progress, err = trial.Step(context.Background(), true)
	if err != nil || !progress.Stopped || progress.Next != 2 || b.writes != 1 || len(progress.Results) != 2 || progress.Results[1].Displays[0].State != model.StateFailed {
		t.Fatalf("设备变化必须阻止后续写入并保留前步证据: %+v %v", progress, err)
	}
}

func TestCalibrationRejectsIdentityLossBetweenDiagnosticReads(t *testing.T) {
	s, _, _, _ := fixture(t, func(_ *config.Config, b *testBackend) {
		devices := append([]model.Device(nil), b.devices...)
		calls := 0
		b.enumerate = func(context.Context) ([]model.Device, error) {
			calls++
			if calls == 1 {
				return devices, nil
			}
			return nil, nil
		}
	})
	if _, err := s.BeginCalibration(context.Background(), calibrationRequest()); model.CodeOf(err) != model.CodeNotFound {
		t.Fatalf("初始快照可匹配不能覆盖后续读取时身份已缺席的事实: %v", err)
	}
}

func TestTrialRejectsConfiguredButUnmappedGroupMember(t *testing.T) {
	s, _, b, _ := fixture(t, func(cfg *config.Config, b *testBackend) {
		other := cfg.Displays[0]
		other.ID, other.Identity.Serial, other.Identity.Path = "other-screen", "other-serial", "other-path"
		other.Binding = &model.Binding{Path: "other-path"}
		cfg.Displays = append(cfg.Displays, other)
		cfg.Groups = []model.Group{{ID: "both", DisplayIDs: []model.DisplayID{"screen", "other-screen"}}}
		b.devices = append(b.devices, model.Device{BackendID: "other", Identity: other.Identity})
	})
	if _, err := s.PlanTrial(context.Background(), []model.SwitchRequest{{ID: "group", TargetID: "host", GroupID: "both"}}); model.CodeOf(err) != model.CodeInvalidRequest || b.writes != 0 {
		t.Fatalf("整组须先检查目标映射，不能跳过未映射成员: %v", err)
	}
}

func TestA02LostReadbackKeepsOriginalObservationAndNeverClaimsPictureSuccess(t *testing.T) {
	oldAt := time.Now().Add(-time.Hour)
	value := uint16(17)
	s, _, _, _ := fixture(t, func(_ *config.Config, b *testBackend) {
		b.caps.CanReadInput = true
		b.read = func(context.Context) (model.InputObservation, error) {
			return model.InputObservation{Value: &value, ObservedAt: oldAt}, model.NewError(model.CodeUnreachable, "secret-path失联", nil)
		}
	})
	trial, err := s.PlanTrial(context.Background(), []model.SwitchRequest{{ID: "trial", TargetID: "host"}})
	if err != nil {
		t.Fatal(err)
	}
	progress, err := trial.Step(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	result := progress.Results[0].Displays[0]
	if result.State != model.StateUnknown || result.SentAt == nil || result.Observation == nil || !result.Observation.ObservedAt.Equal(oldAt) {
		t.Fatalf("发送后失联须保留未知、发送证据及旧观测时间: %+v", result)
	}
	for _, state := range []model.ExecutionState{model.StateSent, model.StateConfirmed, model.StateUnknown} {
		result.State = state
		if !strings.Contains(ExplainResult(result), "画面") {
			t.Fatal("状态说明必须交代画面确认边界")
		}
	}
}

func TestCapabilityErrorsAreExplainedWithoutHardwareDiagnosis(t *testing.T) {
	for _, code := range []model.ErrorCode{model.CodeUnsupportedOperation, model.CodePermissionDenied, model.CodeUnreachable, model.CodeBackendUnavailable, model.CodeInternal} {
		t.Run(string(code), func(t *testing.T) {
			s, _, _, _ := fixture(t, func(_ *config.Config, b *testBackend) { b.capsErr = model.NewError(code, "secret-error", nil) })
			diagnostic, err := s.Inspect(context.Background(), "screen", "")
			if err != nil || diagnostic.CapabilityIssue == nil || diagnostic.CapabilityIssue.Code != code || !diagnostic.CanCalibrate {
				t.Fatalf("应保留能力失败类别和人工路径: %+v %v", diagnostic, err)
			}
			if strings.Contains(diagnostic.CapabilityIssue.Message, "secret-error") || !strings.Contains(diagnostic.Note, "DDC") {
				t.Fatal("不得泄露底层错误或武断诊断硬件")
			}
		})
	}
}

func TestA02ReportUsesAllowlistAndMasksIdentityInEverySection(t *testing.T) {
	s, _, _, _ := fixture(t, func(_ *config.Config, b *testBackend) {
		b.caps.UnavailableReason = "secret-reason"
		b.caps.Protocols = []model.InputProtocol{model.ProtocolVCP, "secret-protocol"}
		b.capsErr = model.NewError(model.CodeUnreachable, "secret-capability", errors.New("secret-cause"))
	})
	diagnostic, err := s.Inspect(context.Background(), "screen", "hdmi")
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	err = EncodeReport(&output, Report{
		Diagnostics:  []Diagnostic{diagnostic},
		Trials:       []model.SwitchResult{{RequestID: "secret-request", Displays: []model.DisplayResult{{DisplayID: "screen", State: model.StateUnknown, DesiredValue: 17, Error: model.NewError(model.CodeTransport, "secret-transport", nil)}}}},
		Calibrations: []CalibrationEvidence{{DisplayID: "screen", TargetID: "secret-target", PortName: "secret-port", Scheme: model.InputScheme{ID: "secret-scheme", Protocol: model.ProtocolVCP, VCPCode: 0x60, Value: 17}, Basis: BasisManual}},
	})
	if err != nil {
		t.Fatal(err)
	}
	text := output.String()
	if strings.Contains(text, "secret") || strings.Contains(text, "screen") || !strings.Contains(text, "unreachable") || !strings.Contains(text, "unknown") || !strings.Contains(text, "display-1") {
		t.Fatalf("脱敏须覆盖名称、身份、路径、ID、厂商文本和错误，并保留分类/可信状态: %s", text)
	}
	if diagnostic.Display.Identity.Serial != "secret-serial" {
		t.Fatal("导出不能改写诊断快照")
	}
	if err := EncodeReport(nil, Report{}); err == nil {
		t.Fatal("空报告写入器须报错")
	}
	if err := EncodeReport(failingWriter{}, Report{}); model.CodeOf(err) != model.CodeIO {
		t.Fatalf("保留导出失败: %v", err)
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("writer failed") }

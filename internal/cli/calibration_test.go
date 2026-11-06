package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/orzlee/displayKvm/internal/diagnostics"
	"github.com/orzlee/displayKvm/internal/model"
)

func rawJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func snapshotResponse(t *testing.T) model.ManagementResponse {
	t.Helper()
	return model.ManagementResponse{Generation: testGeneration, Snapshot: &model.ManagementSnapshot{
		Generation: testGeneration, Configuration: rawJSON(t, fixtureConfig()), Rules: json.RawMessage(`{"version":1,"rules":[]}`),
	}}
}

func sessionResponse(t *testing.T, evidence, progress any) model.ManagementResponse {
	t.Helper()
	session := &model.CalibrationSession{ID: "session-test", Kind: "backend-owned", Generation: testGeneration, ExpiresAt: fixedTime.Add(time.Hour)}
	if evidence != nil {
		session.Evidence = rawJSON(t, evidence)
	}
	if progress != nil {
		session.Progress = rawJSON(t, progress)
	}
	return model.ManagementResponse{Generation: testGeneration, Session: session}
}

func TestDiagnoseUsesTypedManagementAndNativeDiagnosticData(t *testing.T) {
	for _, perDisplay := range []bool{false, true} {
		f := &fakeClient{managementFn: func(_ context.Context, request model.ManagementRequest) (model.ManagementResponse, error) {
			if request.Action == model.ManagementSnapshotAction {
				return snapshotResponse(t), nil
			}
			if request.Action != model.ManagementDiagnose || request.ExpectedGeneration != testGeneration {
				t.Fatalf("诊断绕过管理或未绑定运行代: %+v", request)
			}
			var payload model.ManagementDiagnoseData
			if err := model.DecodeJSON(request.Data, &payload); err != nil {
				t.Fatal(err)
			}
			if perDisplay {
				if payload.DisplayID != "left" || payload.SchemeID != "hdmi" {
					t.Fatalf("诊断参数被改写: %+v", payload)
				}
				return model.ManagementResponse{Generation: testGeneration, Data: rawJSON(t, diagnostics.Diagnostic{Display: fixtureConfig().Displays[0], Candidates: []model.Device{{BackendID: "current-only", Name: "原生候选"}}, InputConfidence: diagnostics.BasisUnknown, CanCalibrate: true})}, nil
			}
			if payload.DisplayID != "" || payload.SchemeID != "" {
				t.Fatal("后端诊断被附加默认设备或方案")
			}
			return model.ManagementResponse{Generation: testGeneration, Data: rawJSON(t, diagnostics.BackendDiagnostic{Available: true, Devices: []model.Device{{BackendID: "current-only", Name: "原生候选"}}})}, nil
		}}
		args := []string{"--json", "diagnose"}
		if perDisplay {
			args = append(args, "--display", "left", "--scheme", "hdmi")
		}
		got := invoke(context.Background(), f, args...)
		if got.code != ExitOK || len(f.managementRequests) != 2 || f.enumerations+len(f.requests)+len(f.events) != 0 {
			t.Fatalf("诊断未按真实服务执行: %+v", got)
		}
		field := `"Devices"`
		if perDisplay {
			field = `"Candidates"`
		}
		if !strings.Contains(got.out, field) || !strings.Contains(got.out, "current-only") {
			t.Fatalf("原生诊断字段或候选被丢失: %s", got.out)
		}
	}
}

func TestDiagnoseUnavailableOrIdentityIssueDoesNotReportSuccess(t *testing.T) {
	for _, perDisplay := range []bool{false, true} {
		f := &fakeClient{managementFn: func(_ context.Context, request model.ManagementRequest) (model.ManagementResponse, error) {
			if request.Action == model.ManagementSnapshotAction {
				return snapshotResponse(t), nil
			}
			issue := model.NewError(model.CodeIdentityAmbiguous, "需人工绑定", nil)
			if perDisplay {
				return model.ManagementResponse{Generation: testGeneration, Data: rawJSON(t, diagnostics.Diagnostic{Display: fixtureConfig().Displays[0], IdentityIssue: issue})}, nil
			}
			return model.ManagementResponse{Generation: testGeneration, Data: rawJSON(t, diagnostics.BackendDiagnostic{Available: false, Issue: issue})}, nil
		}}
		args := []string{"--json", "diagnose"}
		if perDisplay {
			args = append(args, "--display", "left")
		}
		got := invoke(context.Background(), f, args...)
		if got.code != ExitFailure || !strings.Contains(got.out, "identity_ambiguous") {
			t.Fatalf("诊断问题被当成成功: %+v", got)
		}
	}
	got := invoke(context.Background(), &fakeClient{}, "diagnose", "--json", "--scheme", "hdmi")
	if got.code != ExitUsage || got.dials != 0 {
		t.Fatal("缺显示器的方案诊断未预先拒绝")
	}
}

func calibrationBeginArguments() []string {
	return []string{"--json", "calibrate", "begin", "--display", "left", "--target", "work", "--port-name", "HDMI 1", "--scheme", "hdmi", "--protocol", "vcp", "--vcp-code", "0x60", "--value", "17"}
}

func TestCalibrationBeginOnlyReturnsMatchingEvidenceAndNoSave(t *testing.T) {
	f := &fakeClient{managementFn: func(_ context.Context, request model.ManagementRequest) (model.ManagementResponse, error) {
		if request.Action == model.ManagementSnapshotAction {
			return snapshotResponse(t), nil
		}
		if request.Action != model.ManagementCalibrationBegin || request.ExpectedGeneration != testGeneration || request.Confirmed || request.CommitID != "" {
			t.Fatalf("预览执行了保存或猜测动作: %+v", request)
		}
		var payload model.ManagementCalibrationBeginData
		if err := model.DecodeJSON(request.Data, &payload); err != nil {
			t.Fatal(err)
		}
		if payload.DisplayID != "left" || payload.TargetID != "work" || payload.PortName != "HDMI 1" || payload.Scheme.Value != 17 || payload.Scheme.VCPCode != 0x60 {
			t.Fatalf("人工参数被改写: %+v", payload)
		}
		return sessionResponse(t, diagnostics.CalibrationEvidence{DisplayID: payload.DisplayID, TargetID: payload.TargetID, PortName: payload.PortName, Scheme: payload.Scheme, Basis: diagnostics.BasisManual}, nil), nil
	}}
	got := invoke(context.Background(), f, calibrationBeginArguments()...)
	if got.code != ExitOK || len(f.managementRequests) != 2 || len(f.requests)+len(f.events) != 0 || !strings.Contains(got.out, "未保存") || !strings.Contains(got.out, "session-test") {
		t.Fatalf("校准预览丢失或误报保存: %+v", got)
	}
}

func TestCalibrationNeverDefaultsProtocolOrTruncatesInputParameters(t *testing.T) {
	for _, extra := range [][]string{{"--value", "65536"}, {"--vcp-code", "352"}, {"--protocol", ""}, {"--value", "0"}, {"--vcp-code", "0"}, {"--port-name", " "}} {
		args := append(calibrationBeginArguments(), extra...)
		got := invoke(context.Background(), &fakeClient{}, args...)
		if got.code != ExitUsage || got.dials != 0 {
			t.Fatalf("无效校准仍请求后台: %v %+v", extra, got)
		}
	}
}

func TestCalibrationBeginRejectsConflictingEvidence(t *testing.T) {
	f := &fakeClient{managementFn: func(_ context.Context, request model.ManagementRequest) (model.ManagementResponse, error) {
		if request.Action == model.ManagementSnapshotAction {
			return snapshotResponse(t), nil
		}
		return sessionResponse(t, diagnostics.CalibrationEvidence{DisplayID: "right", TargetID: "work", PortName: "HDMI 1", Scheme: fixtureConfig().InputSchemes[0]}, nil), nil
	}}
	got := invoke(context.Background(), f, calibrationBeginArguments()...)
	if got.code != ExitFailure || len(f.managementRequests) != 2 {
		t.Fatalf("冲突校准证据被接受: %+v", got)
	}
}

func TestCalibrationPrepareGeneratesIDBeforeSendAndNeverApplies(t *testing.T) {
	f := &fakeClient{managementFn: func(_ context.Context, request model.ManagementRequest) (model.ManagementResponse, error) {
		if request.Action != model.ManagementPrepareCalibration || request.ExpectedGeneration != testGeneration || request.CommitID != testCommitID || request.SessionID != "session-test" || !request.Confirmed || len(request.Data) != 0 {
			t.Fatalf("Prepare载荷或调用错误: %+v", request)
		}
		return commitResponse(model.CommitAccepted), nil
	}}
	got := invoke(context.Background(), f, "--json", "calibrate", "prepare", "--generation", string(testGeneration), "--session", "session-test", "--confirm")
	if got.code != ExitOK || len(f.managementRequests) != 1 || !strings.Contains(got.out, string(testCommitID)) || !strings.Contains(got.out, `"saved":false`) || !strings.Contains(got.out, "未生效") {
		t.Fatalf("Prepare被误报保存或自动Apply: %+v", got)
	}
}

func TestPrepareLosesResponseOnlyReconnectsAndQueriesOriginalID(t *testing.T) {
	for _, queryFails := range []bool{false, true} {
		f := &fakeClient{managementFn: func(_ context.Context, request model.ManagementRequest) (model.ManagementResponse, error) {
			if request.CommitID != testCommitID {
				t.Fatal("重新生成了提交ID")
			}
			if request.Action == model.ManagementPrepareCalibration {
				return model.ManagementResponse{}, model.NewError(model.CodeTransport, "Prepare应答丢失", nil)
			}
			if request.Action != model.ManagementCommitStatus {
				t.Fatalf("Prepare失联后出现副作用动作: %+v", request)
			}
			if queryFails {
				return model.ManagementResponse{}, model.NewError(model.CodeDaemonUnavailable, "后台不可达", nil)
			}
			return commitResponse(model.CommitAccepted), nil
		}}
		got := invoke(context.Background(), f, "--json", "calibrate", "prepare", "--generation", string(testGeneration), "--session", "session-test", "--confirm")
		want := ExitOK
		if queryFails {
			want = ExitFailure
		}
		if got.code != want || got.dials != 2 || len(f.managementRequests) != 2 || !strings.Contains(got.out, string(testCommitID)) {
			t.Fatalf("Prepare重发或未保留查询凭据: %+v", got)
		}
	}
}

func TestPrepareRequiresConfirmationBeforeClientOrCommitGeneration(t *testing.T) {
	got := invoke(context.Background(), &fakeClient{}, "--json", "calibrate", "prepare", "--generation", string(testGeneration), "--session", "session-test")
	if got.code != ExitUsage || got.dials != 0 {
		t.Fatalf("未确认仍Prepare: %+v", got)
	}
}

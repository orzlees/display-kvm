package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/orzlee/displayKvm/internal/diagnostics"
	"github.com/orzlee/displayKvm/internal/model"
)

func TestTrialBeginUsesExplicitPlansAndDoesNotExecuteStep(t *testing.T) {
	f := &fakeClient{managementFn: func(_ context.Context, request model.ManagementRequest) (model.ManagementResponse, error) {
		if request.Action == model.ManagementSnapshotAction {
			return snapshotResponse(t), nil
		}
		if request.Action != model.ManagementTrialBegin || request.ExpectedGeneration != testGeneration || request.Confirmed {
			t.Fatalf("规划自动执行了步骤: %+v", request)
		}
		var payload model.ManagementTrialBeginData
		if err := model.DecodeJSON(request.Data, &payload); err != nil {
			t.Fatal(err)
		}
		if len(payload.Requests) != 2 || payload.Requests[0].ID == payload.Requests[1].ID {
			t.Fatalf("步骤ID缺失或重复: %+v", payload)
		}
		for _, step := range payload.Requests {
			if step.TargetID != "work" || step.GroupID != "desk" || step.TimeoutMillis != 200 || len(step.DisplayIDs) != 0 {
				t.Fatalf("人工计划被改写: %+v", step)
			}
		}
		return sessionResponse(t, nil, diagnostics.TrialProgress{Next: 0, Total: 2, Warning: calibrationWarning}), nil
	}}
	got := invoke(context.Background(), f, "--json", "calibrate", "trial", "begin", "--target", "work", "--target", "work", "--group", "desk", "--timeout-ms", "200")
	if got.code != ExitOK || len(f.managementRequests) != 2 || len(f.requests)+len(f.events) != 0 || !strings.Contains(got.out, "不会自动执行下一步") {
		t.Fatalf("试切规划产生写入或未提示: %+v", got)
	}
}

func TestTrialRejectsNoTargetsTooManyStepsAndMissingMapping(t *testing.T) {
	for _, args := range [][]string{
		{"--json", "calibrate", "trial", "begin"},
		{"--json", "calibrate", "trial", "begin", "--target", "work", "--display", "left", "--group", "desk"},
		{"--json", "calibrate", "trial", "step", "--generation", string(testGeneration), "--session", "session-test"},
		{"--json", "calibrate", "trial", "stop", "--session", "session-test"},
	} {
		got := invoke(context.Background(), &fakeClient{}, args...)
		if got.code != ExitUsage || got.dials != 0 {
			t.Fatalf("无效试切仍调用客户端: %+v", got)
		}
	}
	tooMany := []string{"--json", "calibrate", "trial", "begin"}
	for i := 0; i < diagnostics.MaxTrialSteps+1; i++ {
		tooMany = append(tooMany, "--target", "work")
	}
	if got := invoke(context.Background(), &fakeClient{}, tooMany...); got.code != ExitUsage || got.dials != 0 {
		t.Fatalf("超出试切上限: %+v", got)
	}
	f := &fakeClient{managementFn: func(_ context.Context, request model.ManagementRequest) (model.ManagementResponse, error) {
		if request.Action != model.ManagementSnapshotAction {
			t.Fatalf("缺映射仍规划或发送试切: %+v", request)
		}
		return snapshotResponse(t), nil
	}}
	got := invoke(context.Background(), f, "--json", "calibrate", "trial", "begin", "--target", "work", "--display", "spare")
	if got.code != ExitUsage || len(f.managementRequests) != 1 || len(f.requests) != 0 {
		t.Fatalf("试切跳过缺映射: %+v", got)
	}
}

func confirmedResult() model.SwitchResult {
	request := model.SwitchRequest{ID: "trial-step", TargetID: "work"}
	result := successfulSwitch(request, "left")
	value := uint16(17)
	result.Displays[0].State = model.StateConfirmed
	result.Displays[0].Observation = &model.InputObservation{Value: &value, ObservedAt: fixedTime}
	return result
}

func TestTrialStepRequiresOneConfirmationAndPreservesNativeProgress(t *testing.T) {
	for _, confirmed := range []bool{true, false} {
		result := confirmedResult()
		if !confirmed {
			result.Displays[0].State = model.StateUnknown
			result.Displays[0].Error = model.NewError(model.CodeTransport, "发送后失联", nil)
		}
		f := &fakeClient{managementFn: func(_ context.Context, request model.ManagementRequest) (model.ManagementResponse, error) {
			if request.Action != model.ManagementTrialStep || request.ExpectedGeneration != testGeneration || request.SessionID != "session-test" || !request.Confirmed || len(request.Data) != 0 {
				t.Fatalf("步骤载荷不符合契约: %+v", request)
			}
			return sessionResponse(t, nil, diagnostics.TrialProgress{Next: 1, Total: 2, Stopped: !confirmed, Results: []model.SwitchResult{result}, Warning: calibrationWarning}), nil
		}}
		got := invoke(context.Background(), f, "--json", "calibrate", "trial", "step", "--generation", string(testGeneration), "--session", "session-test", "--confirm")
		want := ExitFailure
		if confirmed {
			want = ExitOK
		}
		if got.code != want || len(f.managementRequests) != 1 || !strings.Contains(got.out, `"Results"`) || !strings.Contains(got.out, "trial-step") || !strings.Contains(got.out, "不证明") {
			t.Fatalf("步骤结果丢失、重发或误报: %+v", got)
		}
	}
}

func TestTrialStopNeverClaimsRunningActionWasRecalled(t *testing.T) {
	f := &fakeClient{managementFn: func(_ context.Context, request model.ManagementRequest) (model.ManagementResponse, error) {
		if request.Action != model.ManagementTrialStop || request.Confirmed || request.SessionID != "session-test" || len(request.Data) != 0 {
			t.Fatalf("停止变成步骤或添加非法载荷: %+v", request)
		}
		return sessionResponse(t, nil, diagnostics.TrialProgress{Total: 2, Stopped: true, Running: true, Warning: calibrationWarning}), nil
	}}
	got := invoke(context.Background(), f, "calibrate", "trial", "stop", "--json", "--generation", string(testGeneration), "--session", "session-test")
	if got.code != ExitFailure || len(f.managementRequests) != 1 || !strings.Contains(got.out, `"Running":true`) || !strings.Contains(got.out, "不撤回") {
		t.Fatalf("停止掩盖在途动作: %+v", got)
	}
}

func TestApplyReconnectionOnlyQueriesOriginalCommitOnNewClient(t *testing.T) {
	oldClient := &fakeClient{managementFn: func(_ context.Context, request model.ManagementRequest) (model.ManagementResponse, error) {
		if request.Action == model.ManagementCommitStatus {
			return commitResponse(model.CommitAccepted), nil
		}
		if request.Action != model.ManagementApplyCommit {
			t.Fatalf("旧运行代出现额外副作用: %+v", request)
		}
		return model.ManagementResponse{}, model.NewError(model.CodeTransport, "旧运行代端点断开", nil)
	}}
	newClient := &fakeClient{managementFn: func(_ context.Context, request model.ManagementRequest) (model.ManagementResponse, error) {
		if request.Action != model.ManagementCommitStatus || request.CommitID != testCommitID {
			t.Fatalf("新运行代重发了旧提交: %+v", request)
		}
		return commitResponse(model.CommitApplied), nil
	}}
	var out, stderr bytes.Buffer
	dials := 0
	code := Run(context.Background(), []string{"--json", "calibrate", "apply", "--generation", string(testGeneration), "--commit", string(testCommitID), "--confirm"}, Options{Stdout: &out, Stderr: &stderr, Connect: func(string) (Client, error) {
		dials++
		if dials == 1 {
			return oldClient, nil
		}
		return newClient, nil
	}})
	if code != ExitOK || dials != 2 || len(oldClient.managementRequests) != 2 || len(newClient.managementRequests) != 1 {
		t.Fatalf("重连未保持只读: %d %d %s", code, dials, out.String())
	}
}

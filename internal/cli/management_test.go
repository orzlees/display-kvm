package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/orzlee/displayKvm/internal/model"
)

const testGeneration model.RuntimeGeneration = "run-test"
const testCommitID model.RequestID = "commit-run-test-1234"

func boolValue(value bool) *bool { return &value }

func commitResponse(phase model.CommitPhase) model.ManagementResponse {
	commit := &model.CommitStatus{ID: testCommitID, Kind: model.ManagementPrepareCalibration, Phase: phase, PreparedGeneration: testGeneration}
	if phase == model.CommitAccepted {
		commit.Saved, commit.Effective = boolValue(false), boolValue(false)
	}
	if phase == model.CommitApplied {
		commit.Saved, commit.Effective = boolValue(true), boolValue(true)
		commit.EffectiveGeneration = "run-next"
	}
	return model.ManagementResponse{Generation: testGeneration, Commit: commit}
}

func TestApplyRequiresConfirmationBeforeAnyClientCall(t *testing.T) {
	f := &fakeClient{}
	got := invoke(context.Background(), f, "--json", "calibrate", "apply", "--generation", string(testGeneration), "--commit", string(testCommitID))
	if got.code != ExitUsage || got.dials != 0 || len(f.managementRequests) != 0 {
		t.Fatalf("未确认仍发送管理请求: %+v", got)
	}
}

func TestApplyOnlyAfterMatchingAcceptedCalibration(t *testing.T) {
	f := &fakeClient{managementFn: func(_ context.Context, request model.ManagementRequest) (model.ManagementResponse, error) {
		if request.Action == model.ManagementCommitStatus {
			return commitResponse(model.CommitAccepted), nil
		}
		if request.Action != model.ManagementApplyCommit || !request.Confirmed || request.ExpectedGeneration != testGeneration || request.CommitID != testCommitID {
			t.Fatalf("Apply参数被改写: %+v", request)
		}
		return commitResponse(model.CommitApplied), nil
	}}
	got := invoke(context.Background(), f, "--json", "calibrate", "apply", "--generation", string(testGeneration), "--commit", string(testCommitID), "--confirm")
	if got.code != ExitOK || len(f.managementRequests) != 2 || f.managementRequests[0].Action != model.ManagementCommitStatus || f.managementRequests[1].Action != model.ManagementApplyCommit {
		t.Fatalf("Apply顺序错误: %+v %+v", got, f.managementRequests)
	}
}

func TestApplyNeverResendsSavedFailedUnknownOrInFlightCommit(t *testing.T) {
	for _, phase := range []model.CommitPhase{model.CommitSaving, model.CommitSaved, model.CommitApplying, model.CommitApplied, model.CommitFailed, model.CommitUnknown} {
		t.Run(string(phase), func(t *testing.T) {
			f := &fakeClient{managementFn: func(context.Context, model.ManagementRequest) (model.ManagementResponse, error) {
				return commitResponse(phase), nil
			}}
			got := invoke(context.Background(), f, "--json", "calibrate", "apply", "--generation", string(testGeneration), "--commit", string(testCommitID), "--confirm")
			want := ExitFailure
			if phase == model.CommitApplied {
				want = ExitOK
			}
			if got.code != want || len(f.managementRequests) != 1 || f.managementRequests[0].Action != model.ManagementCommitStatus {
				t.Fatalf("重复Apply或误报: %+v %+v", got, f.managementRequests)
			}
		})
	}
}

func TestApplyRejectsWrongCommitKindOrGeneration(t *testing.T) {
	for _, change := range []func(*model.CommitStatus){
		func(c *model.CommitStatus) { c.ID = "other" },
		func(c *model.CommitStatus) { c.Kind = model.ManagementPrepareRules },
		func(c *model.CommitStatus) { c.PreparedGeneration = "old-run" },
	} {
		response := commitResponse(model.CommitAccepted)
		change(response.Commit)
		f := &fakeClient{managementFn: func(context.Context, model.ManagementRequest) (model.ManagementResponse, error) { return response, nil }}
		got := invoke(context.Background(), f, "--json", "calibrate", "apply", "--generation", string(testGeneration), "--commit", string(testCommitID), "--confirm")
		if got.code == ExitOK || len(f.managementRequests) != 1 {
			t.Fatalf("不符提交仍Apply: %+v", got)
		}
	}
}

func TestApplyDisconnectOnlyQueriesSameID(t *testing.T) {
	for _, finalPhase := range []model.CommitPhase{model.CommitApplied, model.CommitSaved, model.CommitUnknown} {
		t.Run(string(finalPhase), func(t *testing.T) {
			queries := 0
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			f := &fakeClient{managementFn: func(callCtx context.Context, request model.ManagementRequest) (model.ManagementResponse, error) {
				if request.CommitID != testCommitID {
					t.Fatal("查询改用了其他提交ID")
				}
				if request.Action == model.ManagementApplyCommit {
					cancel()
					return model.ManagementResponse{}, context.Canceled
				}
				if request.Action != model.ManagementCommitStatus {
					t.Fatalf("出现额外动作: %+v", request)
				}
				queries++
				if queries == 1 {
					return commitResponse(model.CommitAccepted), nil
				}
				if callCtx.Err() != nil {
					t.Fatal("只读结果查询被原Apply取消所覆盖")
				}
				return commitResponse(finalPhase), nil
			}}
			got := invoke(ctx, f, "--json", "calibrate", "apply", "--generation", string(testGeneration), "--commit", string(testCommitID), "--confirm")
			want := ExitFailure
			if finalPhase == model.CommitApplied {
				want = ExitOK
			}
			if got.code != want || len(f.managementRequests) != 3 || queries != 2 || !strings.Contains(got.out, "未重发Apply") {
				t.Fatalf("失联后重发或误报: %+v %+v", got, f.managementRequests)
			}
		})
	}
}

func TestApplyAndQueryFailurePreserveCommitIDAndUnknownWarning(t *testing.T) {
	calls := 0
	f := &fakeClient{managementFn: func(context.Context, model.ManagementRequest) (model.ManagementResponse, error) {
		calls++
		if calls == 1 {
			return commitResponse(model.CommitAccepted), nil
		}
		return model.ManagementResponse{}, model.NewError(model.CodeTransport, "连接中断", nil)
	}}
	got := invoke(context.Background(), f, "--json", "calibrate", "apply", "--generation", string(testGeneration), "--commit", string(testCommitID), "--confirm")
	if got.code != ExitFailure || calls != 3 || !strings.Contains(got.out, string(testCommitID)) || !strings.Contains(got.out, "状态未知") {
		t.Fatalf("无法恢复查询ID: %+v", got)
	}
}

func TestCommitQueryDistinguishesPersistenceAndEffectiveEvidence(t *testing.T) {
	for _, tc := range []struct {
		saved, effective *bool
		phase            model.CommitPhase
		want             int
	}{
		{boolValue(true), boolValue(true), model.CommitApplied, ExitOK},
		{boolValue(true), boolValue(false), model.CommitSaved, ExitFailure},
		{nil, nil, model.CommitUnknown, ExitFailure},
		{boolValue(true), nil, model.CommitApplied, ExitFailure},
		{boolValue(false), boolValue(true), model.CommitApplied, ExitFailure},
	} {
		response := commitResponse(tc.phase)
		response.Commit.Saved, response.Commit.Effective = tc.saved, tc.effective
		f := &fakeClient{managementFn: func(context.Context, model.ManagementRequest) (model.ManagementResponse, error) { return response, nil }}
		got := invoke(context.Background(), f, "calibrate", "query", "--json", "--commit", string(testCommitID))
		if got.code != tc.want || len(f.managementRequests) != 1 || f.managementRequests[0].Action != model.ManagementCommitStatus {
			t.Fatalf("状态证据或只读调用错误: %+v", got)
		}
		var data struct {
			Response model.ManagementResponse `json:"response"`
		}
		if err := json.Unmarshal(decodeEnvelope(t, got.out)["data"], &data); err != nil {
			t.Fatal(err)
		}
		if data.Response.Commit == nil || (tc.saved == nil) != (data.Response.Commit.Saved == nil) || (tc.effective == nil) != (data.Response.Commit.Effective == nil) {
			t.Fatal("未知证据被改成false或丢失")
		}
	}
}

func TestOldDaemonManagementErrorRemainsExplicit(t *testing.T) {
	f := &fakeClient{}
	got := invoke(context.Background(), f, "--json", "calibrate", "query", "--commit", string(testCommitID))
	if got.code != ExitFailure || len(f.managementRequests) != 1 || !strings.Contains(got.out, "unsupported_operation") || len(f.requests)+len(f.events) != 0 {
		t.Fatalf("旧后台被绕过或静默成功: %+v", got)
	}
}

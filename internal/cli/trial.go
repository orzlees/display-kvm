package cli

import (
	"bytes"
	"context"
	"encoding/json"

	"github.com/orzlee/displayKvm/internal/config"
	"github.com/orzlee/displayKvm/internal/diagnostics"
	"github.com/orzlee/displayKvm/internal/model"
)

func (r *runner) trial(ctx context.Context, args []string) (string, outcome) {
	if len(args) == 0 {
		return "calibrate trial", failure(usageError("command", "试切必须指定begin、step或stop"))
	}
	command := "calibrate trial " + args[0]
	switch args[0] {
	case "begin":
		return command, r.beginTrial(ctx, args[1:])
	case "step", "stop":
		return command, r.controlTrial(ctx, args[0], args[1:])
	default:
		return command, failure(usageError("command", "未知试切步骤；只允许begin、step或stop"))
	}
}

func (r *runner) beginTrial(ctx context.Context, args []string) outcome {
	fs := r.flags("calibrate trial begin")
	var targets, displays repeated
	fs.Var(&targets, "target", "明确已有目标，每项为一个步骤，最多8步")
	fs.Var(&displays, "display", "每步选择相同的显示器ID，可重复不同屏")
	group := fs.String("group", "", "每步选择的分组，与逐屏选择互斥")
	timeout := fs.Int64("timeout-ms", 0, "每步超时，0使用统一默认值")
	if err := parse(fs, args); err != nil {
		return r.parseFailure(err)
	}
	if len(targets) == 0 || len(targets) > diagnostics.MaxTrialSteps {
		return failure(usageError("target", "试切只允许1至8个明确已有目标；不会自动枚举输入值"))
	}
	requests := make([]model.SwitchRequest, 0, len(targets))
	for _, target := range targets {
		request := model.SwitchRequest{ID: "cli-validation", TargetID: model.TargetID(target), GroupID: model.GroupID(*group), TimeoutMillis: *timeout}
		for _, display := range displays {
			request.DisplayIDs = append(request.DisplayIDs, model.DisplayID(display))
		}
		if err := request.Validate(); err != nil {
			return failure(err)
		}
		requests = append(requests, request)
	}
	seen := make(map[model.RequestID]bool, len(requests))
	for i := range requests {
		id, err := r.opts.NewID()
		if err != nil {
			return failure(err)
		}
		requests[i].ID = model.RequestID(id)
		if err := requests[i].Validate(); err != nil {
			return failure(err)
		}
		if seen[requests[i].ID] {
			return failure(model.NewError(model.CodeInternal, "无法生成不同的试切步骤关联ID", nil))
		}
		seen[requests[i].ID] = true
	}
	c, err := r.connect(ctx)
	if err != nil {
		return failure(err)
	}
	snapshot, err := managementSnapshot(ctx, c)
	if err != nil {
		return failure(err)
	}
	cfg, err := config.Load(bytes.NewReader(snapshot.Snapshot.Configuration))
	if err != nil {
		return failure(err)
	}
	for _, request := range requests {
		if _, err := projectSelection(cfg, request); err != nil {
			return failure(err)
		}
	}
	data, err := json.Marshal(model.ManagementTrialBeginData{Requests: requests})
	if err != nil {
		return failure(model.NewError(model.CodeInternal, "无法编码试切步骤", err))
	}
	response, err := managementCall(ctx, c, model.ManagementRequest{Action: model.ManagementTrialBegin, ExpectedGeneration: snapshot.Generation, Data: data})
	if err != nil {
		return failure(err)
	}
	if response.Generation != snapshot.Generation {
		return failure(model.NewError(model.CodeTransport, "试切计划响应运行代与请求不符", nil))
	}
	return r.trialOutcome(response, "", false)
}

func (r *runner) controlTrial(ctx context.Context, action string, args []string) outcome {
	fs := r.flags("calibrate trial " + action)
	generation := fs.String("generation", "", "后台试切会话所属运行代")
	session := fs.String("session", "", "后台试切会话ID")
	confirmed := fs.Bool("confirm", false, "已人工核对当前画面，明确执行下一步")
	if err := parse(fs, args); err != nil {
		return r.parseFailure(err)
	}
	operation := model.ManagementTrialStop
	if action == "step" {
		operation = model.ManagementTrialStep
		if !*confirmed {
			return failure(usageError("confirm", calibrationWarning+" 每一步必须单独提供--confirm"))
		}
	}
	request := model.ManagementRequest{Action: operation, ExpectedGeneration: model.RuntimeGeneration(*generation), SessionID: *session, Confirmed: action == "step" && *confirmed}
	if err := request.Validate(); err != nil {
		return failure(err)
	}
	c, err := r.connect(ctx)
	if err != nil {
		return failure(err)
	}
	response, err := managementCall(ctx, c, request)
	if err != nil {
		result := failure(err)
		result.data = managementData{Response: response, Note: calibrationWarning + " 本次未取得确定应答，未自动执行下一步或自动回切；不可将取消理解为动作未发出"}
		return result
	}
	if response.Generation != request.ExpectedGeneration {
		return failure(model.NewError(model.CodeTransport, "试切响应运行代与会话不符", nil))
	}
	return r.trialOutcome(response, *session, action == "step")
}

func (r *runner) trialOutcome(response model.ManagementResponse, expectedSession string, stepped bool) outcome {
	data := managementData{Response: response, Note: calibrationWarning + " 此命令不会自动执行下一步；会话运行代及ID须保留，用于显式step或stop"}
	if response.Session == nil || !response.Session.ExpiresAt.After(r.opts.Now()) || expectedSession != "" && response.Session.ID != expectedSession {
		return outcome{data: data, err: model.NewError(model.CodeTransport, "后台未返回有效的同一试切会话", nil), code: ExitFailure}
	}
	var progress diagnostics.TrialProgress
	if err := model.DecodeJSON(response.Session.Progress, &progress); err != nil {
		return outcome{data: data, err: model.NewError(model.CodeTransport, "试切进度载荷无效", err), code: ExitFailure}
	}
	if progress.Total < 1 || progress.Total > diagnostics.MaxTrialSteps || progress.Next < 0 || progress.Next > progress.Total || progress.Next != len(progress.Results) {
		return outcome{data: data, err: model.NewError(model.CodeTransport, "试切进度与已返回结果不一致", nil), code: ExitFailure}
	}
	if progress.Running || stepped && len(progress.Results) == 0 {
		return outcome{data: data, code: ExitFailure}
	}
	for _, result := range progress.Results {
		if result.Validate() != nil || result.Error != nil || len(result.Displays) == 0 {
			return outcome{data: data, code: ExitFailure}
		}
		for _, display := range result.Displays {
			// 人工试切复用09的更严格停止语义：非confirmed即停止计划，不能仅凭sent
			// 提示继续下一目标。confirmed也只证明输入读回，仍需人工核对实际画面。
			if display.State != model.StateConfirmed {
				return outcome{data: data, code: ExitFailure}
			}
		}
	}
	return outcome{data: data}
}

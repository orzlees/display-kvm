package cli

import (
	"context"

	"github.com/orzlee/displayKvm/internal/model"
)

const calibrationWarning = "必须人工核对端口、实际画面和当前连接；读回一致不证明目标主机画面。试切可能失去控制通道；停止或取消不撤回已发送动作，也不自动回切。"

type managementData struct {
	Response model.ManagementResponse `json:"response"`
	CommitID model.RequestID          `json:"commit_id,omitempty"`
	Note     string                   `json:"note"`
}

func managementCall(ctx context.Context, c Client, request model.ManagementRequest) (model.ManagementResponse, error) {
	if err := request.Validate(); err != nil {
		return model.ManagementResponse{}, err
	}
	response, err := c.Management(ctx, request)
	if err != nil {
		return response, err
	}
	if err := response.Validate(); err != nil {
		return response, model.NewError(model.CodeTransport, "后台管理响应无效；有副作用的请求不会自动重发", err)
	}
	return response, nil
}

func managementSnapshot(ctx context.Context, c Client) (model.ManagementResponse, error) {
	response, err := managementCall(ctx, c, model.ManagementRequest{Action: model.ManagementSnapshotAction})
	if err == nil && response.Snapshot == nil {
		err = model.NewError(model.CodeTransport, "后台未返回管理快照", nil)
	}
	return response, err
}

func (r *runner) calibrate(ctx context.Context, args []string) (string, outcome) {
	if len(args) == 0 {
		return "calibrate", failure(usageError("command", "校准必须指定begin、prepare、apply、query或trial；使用help查看用法"))
	}
	command := "calibrate " + args[0]
	switch args[0] {
	case "begin":
		return command, r.beginCalibration(ctx, args[1:])
	case "prepare":
		return command, r.prepareCalibration(ctx, args[1:])
	case "trial":
		return r.trial(ctx, args[1:])
	case "apply":
		return command, r.applyCalibration(ctx, args[1:])
	case "query":
		return command, r.queryCalibration(ctx, args[1:])
	default:
		return command, failure(usageError("command", "未知校准步骤；使用help查看用法"))
	}
}

func (r *runner) queryCalibration(ctx context.Context, args []string) outcome {
	fs := r.flags("calibrate query")
	id := fs.String("commit", "", "既有提交ID")
	if err := parse(fs, args); err != nil {
		return r.parseFailure(err)
	}
	request := model.ManagementRequest{Action: model.ManagementCommitStatus, CommitID: model.RequestID(*id)}
	if err := request.Validate(); err != nil {
		return failure(err)
	}
	c, err := r.connect(ctx)
	if err != nil {
		return failure(err)
	}
	response, err := managementCall(ctx, c, request)
	return commitOutcome(response, request.CommitID, err, "仅查询既有提交，不保存、不Apply，也不重发；saved与effective分别表示落盘和当前运行代生效证据")
}

func (r *runner) applyCalibration(ctx context.Context, args []string) outcome {
	fs := r.flags("calibrate apply")
	id := fs.String("commit", "", "已Prepare受理的提交ID")
	generation := fs.String("generation", "", "Prepare时的运行代")
	confirmed := fs.Bool("confirm", false, "已人工核对预览并明确要求保存及生效")
	if err := parse(fs, args); err != nil {
		return r.parseFailure(err)
	}
	request := model.ManagementRequest{Action: model.ManagementApplyCommit, CommitID: model.RequestID(*id), ExpectedGeneration: model.RuntimeGeneration(*generation), Confirmed: *confirmed}
	if err := request.Validate(); err != nil {
		return failure(err)
	}
	if !*confirmed {
		return failure(usageError("confirm", calibrationWarning+" 保存与重建必须显式提供--confirm"))
	}
	c, err := r.connect(ctx)
	if err != nil {
		return failure(err)
	}
	// CLI命令可在不同进程中执行，Apply前必须查询同一提交，确认后台确实已Prepare。
	// 已保存、在途、失败或未知状态都不能再次Apply；落盘与运行代生效是两份独立证据。
	prepared, err := managementCall(ctx, c, model.ManagementRequest{Action: model.ManagementCommitStatus, CommitID: request.CommitID})
	if err != nil {
		return commitOutcome(prepared, request.CommitID, err, "尚未发送Apply；请查询同一CommitID核对Prepare结果")
	}
	if prepared.Commit == nil || prepared.Commit.ID != request.CommitID || prepared.Commit.Kind != model.ManagementPrepareCalibration || prepared.Commit.PreparedGeneration != request.ExpectedGeneration {
		return commitOutcome(prepared, request.CommitID, model.NewError(model.CodeInvalidRequest, "提交不是该运行代已受理的校准提交，未发送Apply", nil), "请核对校准Prepare返回的提交ID及运行代")
	}
	if prepared.Commit.Phase != model.CommitAccepted {
		return commitOutcome(prepared, request.CommitID, nil, "该提交已不处于accepted阶段，未再次Apply；只查询保存和生效状态")
	}
	if prepared.Generation != request.ExpectedGeneration || prepared.Commit.Error != nil || prepared.Commit.Saved != nil && *prepared.Commit.Saved || prepared.Commit.Effective != nil && *prepared.Commit.Effective {
		return commitOutcome(prepared, request.CommitID, model.NewError(model.CodeTransport, "accepted提交的运行代或保存证据不一致，未发送Apply", nil), "请只查询同一CommitID，不能根据不一致证据重复保存")
	}
	response, err := managementCall(ctx, c, request)
	if err == nil {
		return commitOutcome(response, request.CommitID, nil, "Apply只发送一次；saved和effective须分别核对，取消不回滚已发生的保存或设备动作")
	}
	// Apply断连后只查询相同ID；不能把传输错误解释为保存失败，更不能重发Apply。
	// 原context可能已被取消，单独限时的只读查询用于尽可能保留已发生的副作用证据。
	queryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), model.ResponseTimeout)
	defer cancel()
	latest, queryErr := r.reconnectCommitQuery(queryCtx, request.CommitID)
	if queryErr == nil {
		return commitOutcome(latest, request.CommitID, nil, "Apply未取得完整应答，随后仅查询同一CommitID；未重发Apply。请保留此ID继续查询")
	}
	return commitOutcome(response, request.CommitID, err, "Apply应答和后续查询均未取得确定结果；保存/生效状态未知。仅用calibrate query --commit查询此ID，勿重发Apply")
}

func commitOutcome(response model.ManagementResponse, id model.RequestID, err error, note string) outcome {
	data := managementData{Response: response, CommitID: id, Note: note}
	if err != nil {
		result := failure(err)
		result.data = data
		return result
	}
	if response.Commit == nil || response.Commit.ID != id {
		return outcome{data: data, err: model.NewError(model.CodeTransport, "后台未返回相同CommitID的状态；不能推断保存或生效", nil), code: ExitFailure}
	}
	commit := response.Commit
	if commit.Error != nil {
		return outcome{data: data, err: commit.Error, code: ExitFailure}
	}
	// Prepare accepted只表明后台保留了提交意图。只有applied且两项证据均为true，
	// 才能把保存及当前运行代生效视为成功；nil、saved或applying都不能提前宣布完成。
	if commit.Phase == model.CommitApplied && commit.Saved != nil && *commit.Saved && commit.Effective != nil && *commit.Effective {
		return outcome{data: data}
	}
	return outcome{data: data, code: ExitFailure}
}

// 运行代重建会使旧端点/客户端失效。重新发现仅用于只读查询同一提交，绝不把
// 原Prepare或Apply带到新连接重发，否则可能重复持久化或覆盖已经变化的配置。
func (r *runner) reconnectCommitQuery(ctx context.Context, id model.RequestID) (model.ManagementResponse, error) {
	c, err := r.connect(ctx)
	if err != nil {
		return model.ManagementResponse{}, err
	}
	return managementCall(ctx, c, model.ManagementRequest{Action: model.ManagementCommitStatus, CommitID: id})
}

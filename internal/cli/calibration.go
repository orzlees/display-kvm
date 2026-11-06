package cli

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/orzlee/displayKvm/internal/diagnostics"
	"github.com/orzlee/displayKvm/internal/model"
)

func (r *runner) beginCalibration(ctx context.Context, args []string) outcome {
	fs := r.flags("calibrate begin")
	display := fs.String("display", "", "显示器逻辑ID")
	target := fs.String("target", "", "校准端口对应目标ID")
	portName := fs.String("port-name", "", "人工核对的物理端口名称")
	schemeID := fs.String("scheme", "", "输入方案ID，新参数须使用新ID")
	protocol := fs.String("protocol", "", "明确选择vcp或lg_input_alt，不推断厂商方案")
	value := fs.Uint("value", 0, "人工核对的输入值1..65535")
	vcp := fs.Uint("vcp-code", 0, "vcp必须显式为96（0x60），lg_input_alt不设置")
	if err := parse(fs, args); err != nil {
		return r.parseFailure(err)
	}
	if !model.ValidID(*display) || !model.ValidID(*target) || strings.TrimSpace(*portName) == "" || len(*portName) > 256 || *value > 65535 || *vcp > 255 {
		return failure(usageError("calibration", "校准必须明确显示器、目标、端口名称及有效协议参数"))
	}
	payload := model.ManagementCalibrationBeginData{DisplayID: model.DisplayID(*display), TargetID: model.TargetID(*target), PortName: *portName, Scheme: model.InputScheme{ID: model.SchemeID(*schemeID), Protocol: model.InputProtocol(*protocol), VCPCode: uint8(*vcp), Value: uint16(*value)}}
	if err := payload.Scheme.Validate(); err != nil {
		result := failure(err)
		result.code = ExitUsage
		return result
	}
	c, err := r.connect(ctx)
	if err != nil {
		return failure(err)
	}
	snapshot, err := managementSnapshot(ctx, c)
	if err != nil {
		return failure(err)
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return failure(model.NewError(model.CodeInternal, "无法编码校准参数", err))
	}
	response, err := managementCall(ctx, c, model.ManagementRequest{Action: model.ManagementCalibrationBegin, ExpectedGeneration: snapshot.Generation, Data: data})
	if err != nil {
		return failure(err)
	}
	if response.Generation != snapshot.Generation || response.Session == nil || !response.Session.ExpiresAt.After(r.opts.Now()) {
		return failure(model.NewError(model.CodeTransport, "后台未返回当前运行代的有效校准会话", nil))
	}
	var evidence diagnostics.CalibrationEvidence
	if err := model.DecodeJSON(response.Session.Evidence, &evidence); err != nil {
		return failure(model.NewError(model.CodeTransport, "校准证据载荷无效", err))
	}
	if evidence.DisplayID != payload.DisplayID || evidence.TargetID != payload.TargetID || evidence.PortName != payload.PortName || evidence.Scheme != payload.Scheme {
		return failure(model.NewError(model.CodeTransport, "校准证据与人工填写参数不符，未Prepare或保存", nil))
	}
	return outcome{data: managementData{Response: response, Note: calibrationWarning + " 当前只生成预览、未保存、未生效。核对证据后使用calibrate prepare --generation及--session --confirm"}}
}

func (r *runner) prepareCalibration(ctx context.Context, args []string) outcome {
	fs := r.flags("calibrate prepare")
	generation := fs.String("generation", "", "预览会话所属运行代")
	session := fs.String("session", "", "后台生成的校准预览会话ID")
	confirmed := fs.Bool("confirm", false, "已人工核对该会话中的端口和校准证据")
	if err := parse(fs, args); err != nil {
		return r.parseFailure(err)
	}
	if !model.ValidID(*generation) || !model.ValidID(*session) || !*confirmed {
		return failure(usageError("confirm", calibrationWarning+" 必须指定有效运行代、预览会话及--confirm"))
	}
	id, err := r.opts.NewCommitID(model.RuntimeGeneration(*generation))
	if err != nil {
		return failure(err)
	}
	request := model.ManagementRequest{Action: model.ManagementPrepareCalibration, ExpectedGeneration: model.RuntimeGeneration(*generation), SessionID: *session, Confirmed: true, CommitID: id}
	if err := request.Validate(); err != nil {
		return failure(err)
	}
	c, err := r.connect(ctx)
	if err != nil {
		return commitOutcome(model.ManagementResponse{}, id, err, "尚未发送Prepare；本次预生成CommitID供排查，不表示已受理")
	}
	response, err := managementCall(ctx, c, request)
	if err != nil {
		// Prepare也可能已受理但应答丢失，必须保留原ID并只读查询，不能换ID重放意图。
		queryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), model.ResponseTimeout)
		defer cancel()
		latest, queryErr := r.reconnectCommitQuery(queryCtx, id)
		if queryErr != nil {
			return commitOutcome(response, id, err, "Prepare应答和同ID查询均未取得确定结果；请用calibrate query保留并查询此CommitID，不重新Prepare，也不Apply")
		}
		response = latest
	}
	data := managementData{Response: response, CommitID: id, Note: "Prepare仅accepted、未保存、未生效。核对后显式calibrate apply --generation --commit --confirm；保留此CommitID，断连只查询，不重发Prepare或Apply"}
	if response.Commit != nil && response.Commit.Error != nil {
		return outcome{data: data, err: response.Commit.Error, code: ExitFailure}
	}
	if response.Commit == nil || response.Commit.ID != id || response.Commit.Kind != model.ManagementPrepareCalibration || response.Commit.PreparedGeneration != request.ExpectedGeneration || response.Commit.Phase != model.CommitAccepted || response.Commit.Saved != nil && *response.Commit.Saved || response.Commit.Effective != nil && *response.Commit.Effective {
		return outcome{data: data, err: model.NewError(model.CodeTransport, "未取得匹配的accepted校准提交，不应继续Apply", nil), code: ExitFailure}
	}
	return outcome{data: data}
}

package cli

import (
	"context"
	"encoding/json"

	"github.com/orzlee/displayKvm/internal/diagnostics"
	"github.com/orzlee/displayKvm/internal/model"
)

func (r *runner) diagnose(ctx context.Context, args []string) outcome {
	fs := r.flags("diagnose")
	display := fs.String("display", "", "显示器逻辑ID，省略时诊断后端")
	scheme := fs.String("scheme", "", "只读取该已配置方案，不猜测输入码")
	if err := parse(fs, args); err != nil {
		return r.parseFailure(err)
	}
	if *display != "" && !model.ValidID(*display) || *scheme != "" && (!model.ValidID(*scheme) || *display == "") {
		return failure(usageError("display", "诊断需要有效显示器ID；指定方案时不能省略显示器"))
	}
	c, err := r.connect(ctx)
	if err != nil {
		return failure(err)
	}
	snapshot, err := managementSnapshot(ctx, c)
	if err != nil {
		return failure(err)
	}
	payload, err := json.Marshal(model.ManagementDiagnoseData{DisplayID: model.DisplayID(*display), SchemeID: model.SchemeID(*scheme)})
	if err != nil {
		return failure(model.NewError(model.CodeInternal, "无法编码诊断参数", err))
	}
	response, err := managementCall(ctx, c, model.ManagementRequest{Action: model.ManagementDiagnose, ExpectedGeneration: snapshot.Generation, Data: payload})
	if err != nil {
		return failure(err)
	}
	if response.Generation != snapshot.Generation {
		return failure(model.NewError(model.CodeTransport, "诊断响应运行代与请求不符", nil))
	}
	result := outcome{data: managementData{Response: response, Note: "诊断只读取已配置方案；枚举、发送或读回一致都不证明实际画面。候选标识仅供本轮展示，不用于后续设备操作"}}
	if *display == "" {
		var diagnostic diagnostics.BackendDiagnostic
		if err := model.DecodeJSON(response.Data, &diagnostic); err != nil {
			return failure(model.NewError(model.CodeTransport, "后台诊断载荷无效", err))
		}
		if !diagnostic.Available || diagnostic.Issue != nil {
			result.code, result.err = ExitFailure, diagnostic.Issue
		}
	} else {
		var diagnostic diagnostics.Diagnostic
		if err := model.DecodeJSON(response.Data, &diagnostic); err != nil {
			return failure(model.NewError(model.CodeTransport, "显示器诊断载荷无效", err))
		}
		if diagnostic.Display.ID != model.DisplayID(*display) {
			return failure(model.NewError(model.CodeTransport, "诊断响应显示器与请求不符", nil))
		}
		for _, issue := range []*model.Error{diagnostic.BackendIssue, diagnostic.IdentityIssue, diagnostic.CapabilityIssue, diagnostic.ObservationIssue} {
			if issue != nil {
				result.code, result.err = ExitFailure, issue
				break
			}
		}
	}
	return result
}

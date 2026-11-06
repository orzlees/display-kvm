package diagnostics

import (
	"encoding/json"
	"io"
	"strconv"

	"github.com/orzlee/displayKvm/internal/model"
)

type Report struct {
	Backend      *BackendDiagnostic
	Diagnostics  []Diagnostic
	Trials       []model.SwitchResult
	Calibrations []CalibrationEvidence
}

// EncodeReport固定脱敏，不提供容易被默认开启的原始导出选项。采用字段白名单，
// 而非在日志字符串里替换已知序列号：设备名称、路径、摘要、逻辑ID、厂商返回
// 文本及错误Message都可能夹带标识或认证材料；遗漏新敏感字段会破坏遮蔽效果。
// 同一导出内逻辑ID以分类编号关联，不输出可跨报告追踪的稳定硬件摘要。
func EncodeReport(writer io.Writer, report Report) error {
	if writer == nil {
		return model.NewError(model.CodeInvalidRequest, "诊断报告写入器不能为空", nil)
	}
	aliases := make(map[string]string)
	counts := make(map[string]int)
	alias := func(kind, id string) string {
		key := kind + "\x00" + id
		if value, ok := aliases[key]; ok {
			return value
		}
		counts[kind]++
		value := kind + "-" + strconv.Itoa(counts[kind])
		aliases[key] = value
		return value
	}
	type backendView struct {
		Available   bool         `json:"available"`
		DeviceCount int          `json:"device_count"`
		Issue       *model.Error `json:"issue,omitempty"`
	}
	type diagnosticView struct {
		DisplayID        string                  `json:"display_id"`
		CandidateCount   int                     `json:"candidate_count"`
		CanCalibrate     bool                    `json:"can_calibrate"`
		BackendIssue     *model.Error            `json:"backend_issue,omitempty"`
		IdentityIssue    *model.Error            `json:"identity_issue,omitempty"`
		Protocols        []model.InputProtocol   `json:"protocols"`
		CanReadInput     bool                    `json:"can_read_input"`
		CapabilityIssue  *model.Error            `json:"capability_issue,omitempty"`
		Observation      *model.InputObservation `json:"observation,omitempty"`
		ObservationIssue *model.Error            `json:"observation_issue,omitempty"`
		InputConfidence  EvidenceBasis           `json:"input_confidence"`
	}
	type calibrationView struct {
		DisplayID        string                  `json:"display_id"`
		TargetID         string                  `json:"target_id"`
		Scheme           model.InputScheme       `json:"scheme"`
		Basis            EvidenceBasis           `json:"basis"`
		Observation      *model.InputObservation `json:"observation,omitempty"`
		ObservationIssue *model.Error            `json:"observation_issue,omitempty"`
		HumanConfirmed   bool                    `json:"human_confirmed"`
	}
	output := struct {
		Redacted     bool                 `json:"redacted"`
		Note         string               `json:"note"`
		Backend      *backendView         `json:"backend,omitempty"`
		Diagnostics  []diagnosticView     `json:"diagnostics"`
		Trials       []model.SwitchResult `json:"trials"`
		Calibrations []calibrationView    `json:"calibrations"`
	}{Redacted: true, Note: trialWarning}
	if report.Backend != nil {
		output.Backend = &backendView{
			Available: report.Backend.Available, DeviceCount: len(report.Backend.Devices), Issue: reportError(report.Backend.Issue),
		}
	}
	for _, diagnostic := range report.Diagnostics {
		view := diagnosticView{
			DisplayID: alias("display", string(diagnostic.Display.ID)), CandidateCount: len(diagnostic.Candidates),
			CanCalibrate: diagnostic.CanCalibrate, BackendIssue: reportError(diagnostic.BackendIssue),
			IdentityIssue: reportError(diagnostic.IdentityIssue), CanReadInput: diagnostic.Capabilities.CanReadInput,
			CapabilityIssue: reportError(diagnostic.CapabilityIssue), Observation: copyObservation(diagnostic.Observation),
			ObservationIssue: reportError(diagnostic.ObservationIssue), InputConfidence: reportBasis(diagnostic.InputConfidence),
		}
		for _, protocol := range diagnostic.Capabilities.Protocols {
			if protocol == model.ProtocolVCP || protocol == model.ProtocolLGInputAlt {
				view.Protocols = append(view.Protocols, protocol)
			}
		}
		output.Diagnostics = append(output.Diagnostics, view)
	}
	for _, result := range report.Trials {
		view := model.SwitchResult{RequestID: model.RequestID(alias("request", string(result.RequestID))), Error: reportError(result.Error)}
		for _, display := range result.Displays {
			copy := display
			copy.DisplayID = model.DisplayID(alias("display", string(display.DisplayID)))
			copy.Error = reportError(display.Error)
			switch display.State {
			case model.StateAccepted, model.StateSent, model.StateConfirmed, model.StateFailed, model.StateUnknown:
			default:
				copy.State = model.StateUnknown
			}
			view.Displays = append(view.Displays, copy)
		}
		output.Trials = append(output.Trials, view)
	}
	for _, evidence := range report.Calibrations {
		scheme := evidence.Scheme
		scheme.ID = model.SchemeID(alias("scheme", string(scheme.ID)))
		if scheme.Protocol != model.ProtocolVCP && scheme.Protocol != model.ProtocolLGInputAlt {
			scheme.Protocol = ""
		}
		output.Calibrations = append(output.Calibrations, calibrationView{
			DisplayID: alias("display", string(evidence.DisplayID)), TargetID: alias("target", string(evidence.TargetID)),
			Scheme: scheme, Basis: reportBasis(evidence.Basis), Observation: copyObservation(evidence.Observation),
			ObservationIssue: reportError(evidence.ObservationIssue), HumanConfirmed: evidence.ConfirmedAt != nil,
		})
	}
	data, err := json.MarshalIndent(output, "", "  ")
	if err != nil {
		return model.NewError(model.CodeIO, "诊断报告编码失败", err)
	}
	data = append(data, '\n')
	written, err := writer.Write(data)
	if err == nil && written != len(data) {
		err = io.ErrShortWrite
	}
	if err != nil {
		return model.NewError(model.CodeIO, "诊断报告写入失败", err)
	}
	return nil
}

func reportBasis(basis EvidenceBasis) EvidenceBasis {
	if basis == BasisReadback || basis == BasisManual {
		return basis
	}
	return BasisUnknown
}

func reportError(issue *model.Error) *model.Error {
	if issue == nil {
		return nil
	}
	code := issue.Code
	if err := issue.Validate(); err != nil {
		code = model.CodeInternal
	}
	// 只借用分类生成静态说明，不能保留调用方的Message、Field、cause或重试提示。
	return explainError(model.NewError(code, "诊断失败", nil))
}

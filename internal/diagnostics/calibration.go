package diagnostics

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/orzlee/displayKvm/internal/config"
	"github.com/orzlee/displayKvm/internal/model"
)

// CalibrationRequest必须明确给出人工选择的协议、输入值和名称；数值在合法范围
// 内仍不证明设备支持。新端口应使用新Scheme.ID，不能暗改其他目标共用的方案。
type CalibrationRequest struct {
	DisplayID model.DisplayID
	TargetID  model.TargetID
	PortName  string
	Scheme    model.InputScheme
}

// CalibrationEvidence与配置一起交给持久化回调，保留端口名称和校准依据。
// BasisReadback只代表本次有效读回，BasisManual代表读回不可用时的人工核对；
// 两种依据均须ConfirmedAt存在才表示明确人工确认，均不承诺视觉成功。
type CalibrationEvidence struct {
	DisplayID        model.DisplayID         `json:"display_id"`
	TargetID         model.TargetID          `json:"target_id"`
	PortName         string                  `json:"port_name"`
	Scheme           model.InputScheme       `json:"scheme"`
	Basis            EvidenceBasis           `json:"basis"`
	Observation      *model.InputObservation `json:"observation,omitempty"`
	ObservationIssue *model.Error            `json:"observation_issue,omitempty"`
	ConfirmedAt      *time.Time              `json:"confirmed_at,omitempty"`
}

// Calibration是进程内不可编辑的预览。调用者只能取得依据副本，不能在人工
// 确认前偷偷替换显示器、名称或输入值，也不能把别的Service的预览拿来保存。
type Calibration struct {
	owner    *Service
	cfg      config.Config
	evidence CalibrationEvidence
	mu       sync.Mutex
}

func (c *Calibration) Evidence() CalibrationEvidence {
	c.mu.Lock()
	defer c.mu.Unlock()
	return copyEvidence(c.evidence)
}

func (s *Service) BeginCalibration(ctx context.Context, request CalibrationRequest) (*Calibration, error) {
	if !model.ValidID(string(request.TargetID)) || strings.TrimSpace(request.PortName) == "" || len(request.PortName) > 256 {
		return nil, model.NewError(model.CodeInvalidRequest, "人工校准需要有效目标ID和不超过256字节的非空端口名称", nil)
	}
	if err := request.Scheme.Validate(); err != nil {
		return nil, err
	}
	for _, scheme := range s.cfg.InputSchemes {
		if scheme.ID == request.Scheme.ID && scheme != request.Scheme {
			return nil, model.NewError(model.CodeInvalidConfig, "已有方案可能被其他目标引用，新参数必须使用新的输入方案ID", nil)
		}
	}
	diagnostic, err := s.inspect(ctx, request.DisplayID, &request.Scheme)
	if err != nil {
		return nil, err
	}
	if diagnostic.BackendIssue != nil {
		return nil, diagnostic.BackendIssue
	}
	if diagnostic.IdentityIssue != nil {
		return nil, diagnostic.IdentityIssue
	}
	if !diagnostic.CanCalibrate {
		return nil, model.NewError(model.CodeIdentityAmbiguous, "设备身份尚未确认，不能校准", nil)
	}
	if err := ctx.Err(); err != nil {
		return nil, explainError(err)
	}
	if diagnostic.InputConfidence == BasisReadback && *diagnostic.Observation.Value != request.Scheme.Value {
		return nil, model.NewError(model.CodeInvalidRequest, "本次输入读回与人工填写值冲突，须核对当前端口后重新校准", nil)
	}
	updated, err := copyConfig(s.cfg)
	if err != nil {
		return nil, err
	}
	foundScheme := false
	for _, scheme := range updated.InputSchemes {
		if scheme.ID == request.Scheme.ID {
			foundScheme = true
			break
		}
	}
	if !foundScheme {
		updated.InputSchemes = append(updated.InputSchemes, request.Scheme)
	}
	targetIndex := -1
	for i, target := range updated.Targets {
		if target.ID == request.TargetID {
			targetIndex = i
			break
		}
	}
	if targetIndex < 0 {
		updated.Targets = append(updated.Targets, model.Target{ID: request.TargetID, Name: request.PortName})
		targetIndex = len(updated.Targets) - 1
	}
	// 现有目标可能覆盖多屏，不能为单屏端口改名而重命名整个目标；每屏人工名称
	// 由校准依据单独保存，仅替换这一屏的映射，其他屏及分组保持原样。
	target := &updated.Targets[targetIndex]
	foundInput := false
	for i, input := range target.Inputs {
		if input.DisplayID == request.DisplayID {
			target.Inputs[i].SchemeID = request.Scheme.ID
			foundInput = true
			break
		}
	}
	if !foundInput {
		target.Inputs = append(target.Inputs, model.TargetInput{DisplayID: request.DisplayID, SchemeID: request.Scheme.ID})
	}
	if err := updated.Validate(); err != nil {
		return nil, err
	}
	basis := BasisManual
	if diagnostic.InputConfidence == BasisReadback {
		basis = BasisReadback
	}
	return &Calibration{owner: s, cfg: updated, evidence: CalibrationEvidence{
		DisplayID: request.DisplayID, TargetID: request.TargetID, PortName: request.PortName,
		Scheme: request.Scheme, Basis: basis, Observation: copyObservation(diagnostic.Observation),
		ObservationIssue: diagnostic.ObservationIssue,
	}}, nil
}

// SaveCalibration的confirmed必须来自针对本预览的明确人工确认，不能用“已发送”
// 或读回一致替代。persist负责原子、私有权限保存配置及依据；本包不假定文件位置。
// 先停用并Close旧队列，再调用persist，保证调用方重建队列前没有第二个后端所有者。
// 保存失败可能已经产生文件副作用，旧服务仍停用，不自动重试；调用方须核对落盘
// 实际状态，再用确认后的配置恢复唯一队列。回调不得把旧Service当成仍可用的队列。
func (s *Service) SaveCalibration(ctx context.Context, calibration *Calibration, confirmed bool, persist func(context.Context, config.Config, CalibrationEvidence) error) (config.Config, error) {
	if calibration == nil || calibration.owner != s || !confirmed || persist == nil {
		return config.Config{}, model.NewError(model.CodeInvalidRequest, "必须明确人工确认本服务生成的校准预览，并提供配置及依据保存函数", nil)
	}
	if err := ctx.Err(); err != nil {
		return config.Config{}, explainError(err)
	}
	updated, err := copyConfig(calibration.cfg)
	if err != nil {
		return config.Config{}, err
	}
	forWriter, err := copyConfig(updated)
	if err != nil {
		return config.Config{}, err
	}
	s.mu.Lock()
	if s.retired {
		s.mu.Unlock()
		return config.Config{}, model.NewError(model.CodeBackendUnavailable, "旧配置已停用，不能重复保存或继续使用其他旧预览", nil)
	}
	s.retired = true
	s.mu.Unlock()
	if err := s.queue.Close(); err != nil {
		return config.Config{}, explainError(err)
	}
	if err := ctx.Err(); err != nil {
		return config.Config{}, explainError(err)
	}
	now := time.Now()
	calibration.mu.Lock()
	calibration.evidence.ConfirmedAt = &now
	evidence := copyEvidence(calibration.evidence)
	calibration.mu.Unlock()
	if err := persist(ctx, forWriter, evidence); err != nil {
		return config.Config{}, model.NewError(model.CodeIO, "校准保存失败，旧队列已关闭，须核对实际配置后恢复；不会自动重试", err)
	}
	return updated, nil
}

func copyEvidence(evidence CalibrationEvidence) CalibrationEvidence {
	evidence.Observation = copyObservation(evidence.Observation)
	if evidence.ObservationIssue != nil {
		issue := *evidence.ObservationIssue
		evidence.ObservationIssue = &issue
	}
	if evidence.ConfirmedAt != nil {
		at := *evidence.ConfirmedAt
		evidence.ConfirmedAt = &at
	}
	return evidence
}

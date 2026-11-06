package model

import (
	"encoding/json"
	"strings"
	"time"
)

type RuntimeGeneration string
type ManagementAction string

const (
	ManagementSnapshotAction     ManagementAction = "snapshot"
	ManagementDiagnose           ManagementAction = "diagnose"
	ManagementCalibrationBegin   ManagementAction = "calibration_begin"
	ManagementTrialBegin         ManagementAction = "trial_begin"
	ManagementTrialStep          ManagementAction = "trial_step"
	ManagementTrialStop          ManagementAction = "trial_stop"
	ManagementPrepareConfig      ManagementAction = "prepare_config"
	ManagementPrepareRules       ManagementAction = "prepare_rules"
	ManagementPrepareCalibration ManagementAction = "prepare_calibration"
	ManagementApplyCommit        ManagementAction = "apply_commit"
	ManagementCommitStatus       ManagementAction = "commit_status"
	ManagementPreviewRules       ManagementAction = "preview_rules"
	ManagementRuleRecords        ManagementAction = "rule_records"
	ManagementSchedulesState     ManagementAction = "schedules_state"
	ManagementPrepareSchedules   ManagementAction = "prepare_schedules"
	ManagementPreviewStart       ManagementAction = "preview_start"
	ManagementPreviewStatus      ManagementAction = "preview_status"
)

// Data的唯一线格式如下，所有字段由对应操作严格解码，不接受其他包裹层：
// diagnose: ManagementDiagnoseData；display_id与scheme_id都为空时调用InspectBackend，
// 响应Data为diagnostics.BackendDiagnostic（含本次完整Devices）；display_id非空时
// 响应Data为diagnostics.Diagnostic，scheme_id可空。响应Generation始终绑定该轮配置。
// calibration_begin: ManagementCalibrationBeginData（蛇形小写字段），后台转换为既有
// diagnostics.CalibrationRequest；Session.Evidence为diagnostics.CalibrationEvidence。
// trial_begin: ManagementTrialBeginData；Session.Progress为diagnostics.TrialProgress。
// trial_step/trial_stop: Data必须缺省，仅使用SessionID，step还须Confirmed=true。
// prepare_calibration: Data必须缺省，仅SessionID、CommitID、Confirmed=true。
// prepare_config: 完整config.Config对象；prepare_rules: 既有{version:1,rules:[...]}对象。
// preview_rules: ManagementPreviewRulesData（{event:Event}）；响应Data直接为
// []automation.Decision。rule_records无Data，响应Data直接为[]automation.Record。
// snapshot/apply_commit/commit_status无Data。所有Prepare的CommitID均须由客户端预生成；
// Prepare应答丢失仍只查询原ID，不能生成新ID重放保存。core client.NewCommitID提供生成入口。
// diagnostics既有结果使用其原生JSON字段（如Display、Candidates），不改写历史结果格式。
// Data保存既有配置、规则或诊断类型的JSON，避免底层模型反向依赖业务包。
// 各操作仍须在后台按唯一确定的结构严格解码，不能把RawMessage当作绕过验证的入口。
// schedules_state无Data且必须绑定已读运行代，响应Data为ManagementSchedulesData；
// prepare_schedules的Data是既有{version:1,schedules:[...]}文档，仍走原提交事务。
// 日程字段只通过新动作的Data传递，不扩展旧ManagementSnapshot：旧客户端严格拒绝
// 未知字段，直接给历史快照加字段会使原本可用的配置与规则页面无法解码。
// preview_start: ManagementPreviewStartData，仅允许已保存目标映射；Session.Kind为
// input_preview，Data为InputPreviewStatus。preview_status无Data，SessionID缺省查
// 当前运行代最近会话（含终态），没有会话则Session和Data均缺省。两个动作均校验
// ExpectedGeneration；启动请求的断开不会取消由后台轮次持有的预览和恢复责任。
type ManagementRequest struct {
	Action             ManagementAction  `json:"action"`
	ExpectedGeneration RuntimeGeneration `json:"expected_generation,omitempty"`
	CommitID           RequestID         `json:"commit_id,omitempty"`
	SessionID          string            `json:"session_id,omitempty"`
	Confirmed          bool              `json:"confirmed,omitempty"`
	Data               json.RawMessage   `json:"data,omitempty"`
}

type ManagementDiagnoseData struct {
	DisplayID DisplayID `json:"display_id,omitempty"`
	SchemeID  SchemeID  `json:"scheme_id,omitempty"`
}

type ManagementCalibrationBeginData struct {
	DisplayID DisplayID   `json:"display_id"`
	TargetID  TargetID    `json:"target_id"`
	PortName  string      `json:"port_name"`
	Scheme    InputScheme `json:"scheme"`
}

type ManagementTrialBeginData struct {
	Requests []SwitchRequest `json:"requests"`
}
type ManagementPreviewRulesData struct {
	Event Event `json:"event"`
}

type ManagementPreviewStartData struct {
	Request SwitchRequest `json:"request"`
}

type InputPreviewState string
type InputPreviewPhase string

type InputPreviewStatus struct {
	State           InputPreviewState           `json:"state"`
	Phase           InputPreviewPhase           `json:"phase"`
	TargetID        TargetID                    `json:"target_id"`
	ActiveDisplayID DisplayID                   `json:"active_display_id,omitempty"`
	StartedAt       time.Time                   `json:"started_at"`
	FinishedAt      *time.Time                  `json:"finished_at,omitempty"`
	HoldUntil       *time.Time                  `json:"hold_until,omitempty"`
	Displays        []InputPreviewDisplayResult `json:"displays"`
}

type InputPreviewDisplayResult struct {
	DisplayID     DisplayID `json:"display_id"`
	OriginalInput *uint16   `json:"original_input,omitempty"`
	TargetInput   uint16    `json:"target_input"`
	ObservedInput *uint16   `json:"observed_input,omitempty"`
	SwitchStatus  string    `json:"switch_status"`
	RestoreStatus string    `json:"restore_status"`
	Detail        string    `json:"detail,omitempty"`
}

// Clone隔离同步进度回调、后台会话和响应各自拥有的快照；仅复制切片仍会共享
// 观测值及时间指针，页面读状态或回调修改旧快照时就可能污染恢复依据并产生竞态。
func (s InputPreviewStatus) Clone() InputPreviewStatus {
	if s.FinishedAt != nil {
		at := *s.FinishedAt
		s.FinishedAt = &at
	}
	if s.HoldUntil != nil {
		at := *s.HoldUntil
		s.HoldUntil = &at
	}
	s.Displays = append([]InputPreviewDisplayResult{}, s.Displays...)
	for i := range s.Displays {
		if s.Displays[i].OriginalInput != nil {
			value := *s.Displays[i].OriginalInput
			s.Displays[i].OriginalInput = &value
		}
		if s.Displays[i].ObservedInput != nil {
			value := *s.Displays[i].ObservedInput
			s.Displays[i].ObservedInput = &value
		}
	}
	return s
}

// 业务文档及状态保留原生JSON，由消费端使用scheduler严格解码，避免model反向
// 依赖scheduler形成循环。Statuses只能来自当前真实实例，空列表不能伪造下次触发。
// Statuses的唯一结构为{"items":[...ScheduleStatus]}，items必须显式提供且非null；
// 无日程时为{"items":[]}。对象外壳用于复用DecodeJSON的严格对象入口，使嵌套列表
// 也保留重复键、未知字段与深度检查；不能直接发送顶层数组或改用宽松解码。
type ManagementSchedulesData struct {
	Document json.RawMessage   `json:"document"`
	Writable bool              `json:"writable"`
	Statuses json.RawMessage   `json:"statuses"`
	Timer    TimerRuntimeState `json:"timer"`
}

type ManagementResponse struct {
	Generation RuntimeGeneration   `json:"generation"`
	Snapshot   *ManagementSnapshot `json:"snapshot,omitempty"`
	Session    *CalibrationSession `json:"session,omitempty"`
	Commit     *CommitStatus       `json:"commit,omitempty"`
	Data       json.RawMessage     `json:"data,omitempty"`
}

type ManagementSnapshot struct {
	Generation      RuntimeGeneration `json:"generation"`
	Configuration   json.RawMessage   `json:"configuration"`
	Rules           json.RawMessage   `json:"rules"`
	RulesWritable   bool              `json:"rules_writable"`
	EngineAvailable bool              `json:"engine_available"`
	Capabilities    []EventCapability `json:"capabilities"`
	Timer           TimerRuntimeState `json:"timer"`
}

type TimerRuntimeState struct {
	Configured      bool   `json:"configured"`
	Running         bool   `json:"running"`
	ResumeAvailable bool   `json:"resume_available"`
	SkipGuaranteed  bool   `json:"skip_guaranteed"`
	Reason          string `json:"reason,omitempty"`
}

type CalibrationSession struct {
	ID         string            `json:"id"`
	Kind       string            `json:"kind"`
	Generation RuntimeGeneration `json:"generation"`
	ExpiresAt  time.Time         `json:"expires_at"`
	Evidence   json.RawMessage   `json:"evidence,omitempty"`
	Progress   json.RawMessage   `json:"progress,omitempty"`
}

type CommitPhase string

const (
	CommitAccepted CommitPhase = "accepted"
	CommitSaving   CommitPhase = "saving"
	CommitSaved    CommitPhase = "saved"
	CommitApplying CommitPhase = "applying"
	CommitApplied  CommitPhase = "applied"
	CommitFailed   CommitPhase = "failed"
	CommitUnknown  CommitPhase = "unknown"
)

// Saved/Effective的nil表示证据不足；false只用于已确定未保存或未生效的结果。
// Prepare仅登记accepted，客户端必须收到该应答后才显式Apply；传输中断只查询ID。
type CommitStatus struct {
	ID                  RequestID         `json:"id"`
	Kind                ManagementAction  `json:"kind"`
	Phase               CommitPhase       `json:"phase"`
	PreparedGeneration  RuntimeGeneration `json:"prepared_generation,omitempty"`
	EffectiveGeneration RuntimeGeneration `json:"effective_generation,omitempty"`
	Saved               *bool             `json:"saved"`
	Effective           *bool             `json:"effective"`
	Error               *Error            `json:"error,omitempty"`
}

func (r ManagementRequest) Validate() error {
	switch r.Action {
	case ManagementSnapshotAction, ManagementCommitStatus:
	case ManagementDiagnose, ManagementCalibrationBegin, ManagementTrialBegin,
		ManagementTrialStep, ManagementTrialStop, ManagementPrepareConfig,
		ManagementPrepareRules, ManagementPrepareCalibration, ManagementApplyCommit,
		ManagementPreviewRules, ManagementRuleRecords, ManagementSchedulesState, ManagementPrepareSchedules,
		ManagementPreviewStart, ManagementPreviewStatus:
		if !ValidID(string(r.ExpectedGeneration)) {
			return invalid("expected_generation", "操作必须绑定读取时的运行代")
		}
	default:
		return NewError(CodeUnsupportedOperation, "不支持的管理操作", nil)
	}
	if r.CommitID != "" && !ValidID(string(r.CommitID)) {
		return invalid("commit_id", "提交ID无效")
	}
	if r.SessionID != "" && !ValidID(r.SessionID) {
		return invalid("session_id", "会话ID无效")
	}
	switch r.Action {
	case ManagementPrepareConfig, ManagementPrepareRules, ManagementPrepareCalibration, ManagementPrepareSchedules:
		if !ValidID(string(r.CommitID)) || !strings.HasPrefix(string(r.CommitID), "commit-"+string(r.ExpectedGeneration)+"-") {
			return invalid("commit_id", "提交ID必须由客户端预生成并绑定当前运行代")
		}
	case ManagementApplyCommit, ManagementCommitStatus:
		if !ValidID(string(r.CommitID)) {
			return invalid("commit_id", "操作需要提交ID")
		}
	}
	switch r.Action {
	case ManagementTrialStep, ManagementTrialStop, ManagementPrepareCalibration:
		if !ValidID(r.SessionID) {
			return invalid("session_id", "操作需要后台会话ID")
		}
	}
	if len(r.Data) > MaxMessageBytes {
		return NewError(CodeMessageTooLarge, "管理负载超过1MiB", nil)
	}
	return nil
}

func (r ManagementResponse) Validate() error {
	if !ValidID(string(r.Generation)) {
		return invalid("generation", "管理响应缺少有效运行代")
	}
	if r.Snapshot != nil {
		if r.Snapshot.Generation != r.Generation || !json.Valid(r.Snapshot.Configuration) || !json.Valid(r.Snapshot.Rules) {
			return invalid("snapshot", "管理快照不完整或运行代不一致")
		}
		for _, c := range r.Snapshot.Capabilities {
			if err := c.Validate(); err != nil {
				return err
			}
		}
	}
	if r.Session != nil && (!ValidID(r.Session.ID) || r.Session.Generation != r.Generation || r.Session.ExpiresAt.IsZero()) {
		return invalid("session", "管理会话无效")
	}
	if r.Commit != nil {
		if !ValidID(string(r.Commit.ID)) {
			return invalid("commit_id", "提交响应ID无效")
		}
		switch r.Commit.Phase {
		case CommitAccepted, CommitSaving, CommitSaved, CommitApplying, CommitApplied, CommitFailed, CommitUnknown:
		default:
			return invalid("phase", "提交阶段无效")
		}
		if r.Commit.Error != nil {
			return r.Commit.Error.Validate()
		}
	}
	return nil
}

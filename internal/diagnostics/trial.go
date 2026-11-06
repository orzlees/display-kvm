package diagnostics

import (
	"context"
	"sync"

	"github.com/orzlee/displayKvm/internal/identity"
	"github.com/orzlee/displayKvm/internal/model"
)

// MaxTrialSteps限制一次人工计划的范围；该上限不是自动探测输入值的预算。
// 计划只含显式配置的目标，每一步仍需人工确认，不自动回切或保证恢复画面。
const MaxTrialSteps = 8

const trialWarning = "试切可能失去DDC或当前主机画面，每步均需人工确认；可随时停止，停止不撤销已发请求，不自动回切。读回一致也不证明目标主机画面成功。"

type Trial struct {
	service  *Service
	requests []model.SwitchRequest
	mu       sync.Mutex
	next     int
	running  bool
	stopped  bool
	cancel   context.CancelFunc
	results  []model.SwitchResult
}

type TrialProgress struct {
	Next    int
	Total   int
	Running bool
	Stopped bool
	Results []model.SwitchResult
	Warning string
}

// PlanTrial只验证已有目标、映射和当前完整身份快照，不发切换、不扩展输入码。
// 后续真正执行时仍由08重新解析身份，不能把计划时枚举的设备引用跨周期缓存。
func (s *Service) PlanTrial(ctx context.Context, requests []model.SwitchRequest) (*Trial, error) {
	if err := s.active(); err != nil {
		return nil, err
	}
	if len(requests) == 0 || len(requests) > MaxTrialSteps {
		return nil, model.NewError(model.CodeInvalidRequest, "人工试切计划必须包含1至8个明确的已知目标步骤", nil)
	}
	seen := make(map[model.RequestID]bool, len(requests))
	selected := make(map[model.DisplayID]bool)
	plan := make([]model.SwitchRequest, 0, len(requests))
	for _, request := range requests {
		if err := request.Validate(); err != nil {
			return nil, err
		}
		if seen[request.ID] {
			return nil, model.NewError(model.CodeInvalidRequest, "试切步骤必须使用不同请求ID，不能把重试伪装成新步骤", nil)
		}
		seen[request.ID] = true
		var target *model.Target
		for i := range s.cfg.Targets {
			if s.cfg.Targets[i].ID == request.TargetID {
				target = &s.cfg.Targets[i]
				break
			}
		}
		if target == nil {
			return nil, model.NewError(model.CodeNotFound, "试切仅允许当前配置中明确存在的目标", nil)
		}
		ids := append([]model.DisplayID(nil), request.DisplayIDs...)
		if request.GroupID != "" {
			found := false
			for _, group := range s.cfg.Groups {
				if group.ID == request.GroupID {
					ids = append([]model.DisplayID(nil), group.DisplayIDs...)
					found = true
					break
				}
			}
			if !found {
				return nil, model.NewError(model.CodeNotFound, "试切分组不在当前配置中", nil)
			}
		} else if len(ids) == 0 {
			for _, input := range target.Inputs {
				ids = append(ids, input.DisplayID)
			}
		}
		for _, id := range ids {
			mapped := false
			for _, input := range target.Inputs {
				if input.DisplayID == id {
					mapped = true
					break
				}
			}
			if !mapped {
				return nil, model.NewError(model.CodeInvalidRequest, "所选显示器缺少该目标映射，不能跳过该屏或猜测输入", nil)
			}
			selected[id] = true
		}
		// 将分组和默认选择固化成明确逻辑ID；只冻结选择，不冻结设备对象或能力。
		request.DisplayIDs, request.GroupID = ids, ""
		plan = append(plan, request)
	}
	devices, err := s.queue.Enumerate(ctx)
	if err != nil {
		return nil, explainError(err)
	}
	resolutions, err := identity.Resolve(s.cfg.Displays, devices)
	if err != nil {
		return nil, err
	}
	for _, resolution := range resolutions {
		if !selected[resolution.DisplayID] {
			continue
		}
		if resolution.Error != nil {
			return nil, explainError(resolution.Error)
		}
		if resolution.Device == nil {
			return nil, model.NewError(model.CodeNotFound, "试切设备未能唯一解析，必须先人工绑定", nil)
		}
	}
	return &Trial{service: s, requests: plan}, nil
}

func (t *Trial) Requests() []model.SwitchRequest {
	requests := append([]model.SwitchRequest(nil), t.requests...)
	for i := range requests {
		requests[i].DisplayIDs = append([]model.DisplayID(nil), requests[i].DisplayIDs...)
	}
	return requests
}

// Step只执行下一步且不重试写入。任何非confirmed的逐屏结果都会停止计划，
// 防止失联后继续切其他目标；需要后续操作时应先核对现状，再明确创建新计划。
// 外层error表示08未接受该步骤；已入队的部分失败/取消均保留原SwitchResult。
func (t *Trial) Step(ctx context.Context, confirmed bool) (TrialProgress, error) {
	t.mu.Lock()
	if !confirmed || t.running || t.stopped || t.next >= len(t.requests) {
		progress := t.progressLocked()
		t.mu.Unlock()
		return progress, model.NewError(model.CodeInvalidRequest, "步骤须明确人工确认，且计划不能已停止、完成或有在途步骤", nil)
	}
	if err := t.service.active(); err != nil {
		progress := t.progressLocked()
		t.mu.Unlock()
		return progress, err
	}
	if err := ctx.Err(); err != nil {
		progress := t.progressLocked()
		t.mu.Unlock()
		return progress, explainError(err)
	}
	operation, cancel := context.WithCancel(ctx)
	request := t.requests[t.next]
	t.running, t.cancel = true, cancel
	t.mu.Unlock()
	result, err := t.service.queue.Switch(operation, request)
	cancel()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.running, t.cancel = false, nil
	if err != nil {
		result = model.SwitchResult{RequestID: request.ID, Error: explainError(err)}
		t.stopped = true
	}
	t.results = append(t.results, result)
	t.next++
	if result.Error != nil {
		t.stopped = true
	}
	for _, display := range result.Displays {
		if display.State != model.StateConfirmed {
			t.stopped = true
		}
	}
	return t.progressLocked(), err
}

// Stop只取消并阻止后续步骤，不清空历史、不把失败当成未发。停止时仍在途的
// 最终结果由Step返回；Progress.Running为true时不能宣称硬件操作已经结束。
func (t *Trial) Stop() TrialProgress {
	t.mu.Lock()
	t.stopped = true
	cancel := t.cancel
	progress := t.progressLocked()
	t.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return progress
}

func (t *Trial) Progress() TrialProgress {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.progressLocked()
}

func (t *Trial) progressLocked() TrialProgress {
	progress := TrialProgress{Next: t.next, Total: len(t.requests), Running: t.running, Stopped: t.stopped, Warning: trialWarning}
	for _, result := range t.results {
		copy := result
		if result.Error != nil {
			issue := *result.Error
			copy.Error = &issue
		}
		copy.Displays = append([]model.DisplayResult(nil), result.Displays...)
		for i := range copy.Displays {
			display := &copy.Displays[i]
			if display.Error != nil {
				issue := *display.Error
				display.Error = &issue
			}
			if display.SentAt != nil {
				at := *display.SentAt
				display.SentAt = &at
			}
			display.Observation = copyObservation(display.Observation)
		}
		progress.Results = append(progress.Results, copy)
	}
	return progress
}

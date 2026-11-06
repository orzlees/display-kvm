package switcher

import (
	"context"

	"github.com/orzlee/displayKvm/internal/identity"
	"github.com/orzlee/displayKvm/internal/model"
)

type action struct {
	id     model.DisplayID
	scheme model.InputScheme
}

func (q *Queue) plan(request model.SwitchRequest) ([]action, error) {
	var target *model.Target
	for i := range q.cfg.Targets {
		if q.cfg.Targets[i].ID == request.TargetID {
			target = &q.cfg.Targets[i]
			break
		}
	}
	if target == nil {
		return nil, model.NewError(model.CodeNotFound, "切换目标不在当前配置中", nil)
	}
	selected := request.DisplayIDs
	if request.GroupID != "" {
		found := false
		for _, group := range q.cfg.Groups {
			if group.ID == request.GroupID {
				selected, found = group.DisplayIDs, true
				break
			}
		}
		if !found {
			return nil, model.NewError(model.CodeNotFound, "切换分组不在当前配置中", nil)
		}
	} else if len(selected) == 0 {
		for _, input := range target.Inputs {
			selected = append(selected, input.DisplayID)
		}
	}
	inputs := make(map[model.DisplayID]model.SchemeID, len(target.Inputs))
	for _, input := range target.Inputs {
		inputs[input.DisplayID] = input.SchemeID
	}
	schemes := make(map[model.SchemeID]model.InputScheme, len(q.cfg.InputSchemes))
	for _, scheme := range q.cfg.InputSchemes {
		schemes[scheme.ID] = scheme
	}
	plan := make([]action, 0, len(selected))
	for _, id := range selected {
		if err := q.validateDisplayID(id); err != nil {
			return nil, err
		}
		schemeID, ok := inputs[id]
		if !ok {
			err := model.NewError(model.CodeInvalidRequest, "所选显示器缺少该目标的输入映射，整请求未发送", nil)
			err.Field = "display_ids"
			return nil, err
		}
		plan = append(plan, action{id: id, scheme: schemes[schemeID]})
	}
	return plan, nil
}

func (q *Queue) execute(ctx context.Context, requestID model.RequestID, plan []action) model.SwitchResult {
	devices, err := retryRead(ctx, q.backend.Enumerate)
	if err != nil {
		return failedRequest(requestID, plan, err)
	}
	resolutions, err := identity.Resolve(q.cfg.Displays, devices)
	if err != nil {
		return failedRequest(requestID, plan, err)
	}
	resolved := make(map[model.DisplayID]identity.Resolution, len(resolutions))
	for _, resolution := range resolutions {
		resolved[resolution.DisplayID] = resolution
	}
	// 先检查全部所选屏，再对任意一屏写入。若边发边解析，第二屏身份歧义时
	// 第一屏可能已经切走，违背缺映射/身份歧义必须整请求拒绝的边界。
	for _, action := range plan {
		resolution := resolved[action.id]
		if resolution.Error != nil {
			return failedRequest(requestID, plan, resolution.Error)
		}
		if resolution.Device == nil {
			return failedRequest(requestID, plan, model.NewError(model.CodeNotFound, "所选显示器未能唯一解析，整请求未发送", nil))
		}
	}
	result := model.SwitchResult{RequestID: requestID, Displays: make([]model.DisplayResult, 0, len(plan))}
	for _, action := range plan {
		if ctx.Err() != nil {
			// 已发送的屏保持原结果；未发屏逐项说明取消，不把多屏操作当作事务回滚。
			result.Displays = append(result.Displays, model.DisplayResult{
				DisplayID: action.id, DesiredValue: action.scheme.Value,
				State: model.StateFailed, Error: operationError(ctx.Err()),
			})
			continue
		}
		result.Displays = append(result.Displays, q.executeDisplay(ctx, action, *resolved[action.id].Device))
	}
	if ctx.Err() != nil {
		result.Error = operationError(ctx.Err())
	}
	return result
}

func (q *Queue) executeDisplay(ctx context.Context, action action, device model.Device) model.DisplayResult {
	result := model.DisplayResult{DisplayID: action.id, DesiredValue: action.scheme.Value, State: model.StateFailed}
	caps, err := retryRead(ctx, func(ctx context.Context) (model.DeviceCapabilities, error) {
		return q.backend.Capabilities(ctx, device)
	})
	if err != nil {
		result.Error = operationError(err)
		return result
	}
	if caps.UnavailableReason != "" {
		result.Error = model.NewError(model.CodeBackendUnavailable, "显示器当前控制能力不可用："+caps.UnavailableReason, nil)
		return result
	}
	supported := false
	for _, protocol := range caps.Protocols {
		if protocol == action.scheme.Protocol {
			supported = true
			break
		}
	}
	if !supported {
		result.Error = model.NewError(model.CodeUnsupportedProtocol, "显示器后端不支持配置中的输入协议", nil)
		return result
	}
	if caps.CanReadInput {
		// 切走后可能失去DDC。发送前观测只保留为历史事实，不参与本次确认；
		// 一般读失败不禁止仍被能力声明支持的写入，取消/超时则必须阻止写入。
		observation, readErr := q.readObservation(ctx, device, action.scheme)
		retainObservation(&result, observation)
		if code := model.CodeOf(readErr); code == model.CodeCanceled || code == model.CodeTimeout {
			result.Error = operationError(readErr)
			return result
		}
	}
	if ctx.Err() != nil {
		result.Error = operationError(ctx.Err())
		return result
	}
	receipt, err := q.backend.SetInput(ctx, device, action.scheme)
	// 一旦调用SetInput，通路错误、取消甚至零回执都无法证明尚未发送。
	// 不使用失败回执填SentAt，不重发、不回切，也不靠错误类别推断失败阶段。
	if err != nil || receipt.SentAt.IsZero() {
		result.State = model.StateUnknown
		if err != nil {
			result.Error = operationError(err)
		} else {
			result.Error = model.NewError(model.CodeTransport, "后端未提供有效发送回执，提交结果未知", nil)
		}
		return result
	}
	sent := receipt.SentAt
	result.SentAt = &sent
	result.State = model.StateUnknown
	if ctx.Err() != nil {
		result.Error = operationError(ctx.Err())
		return result
	}
	if !caps.CanReadInput {
		result.Error = model.NewError(model.CodeUnsupportedOperation, "切换已发送，但后端不支持输入读回，无法确认", nil)
		return result
	}
	observation, err := q.readObservation(ctx, device, action.scheme)
	retainObservation(&result, observation)
	if err != nil {
		result.Error = operationError(err)
		return result
	}
	// 必须检查本次读回本身，而非retainObservation保存的历史观测。旧值即使
	// 等于目标也不能确认本次发送；读回一致仍不等于目标主机已经显示画面。
	if validObservation(observation) && !observation.ObservedAt.Before(sent) && *observation.Value == action.scheme.Value {
		confirmed := cloneObservation(observation)
		result.Observation = &confirmed
		result.State = model.StateConfirmed
		return result
	}
	result.Error = model.NewError(model.CodeTransport, "切换已发送，但没有发送后与目标一致的有效读回，结果未知", nil)
	return result
}

func failedRequest(id model.RequestID, plan []action, err error) model.SwitchResult {
	result := model.SwitchResult{RequestID: id, Error: operationError(err), Displays: make([]model.DisplayResult, 0, len(plan))}
	for _, action := range plan {
		result.Displays = append(result.Displays, model.DisplayResult{
			DisplayID: action.id, DesiredValue: action.scheme.Value,
			State: model.StateFailed, Error: operationError(err),
		})
	}
	return result
}

func validObservation(observation model.InputObservation) bool {
	return observation.Value != nil && !observation.ObservedAt.IsZero()
}

func retainObservation(result *model.DisplayResult, observation model.InputObservation) {
	if validObservation(observation) && (result.Observation == nil || !observation.ObservedAt.Before(result.Observation.ObservedAt)) {
		copy := cloneObservation(observation)
		result.Observation = &copy
	}
}

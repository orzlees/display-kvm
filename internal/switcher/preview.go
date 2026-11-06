package switcher

import (
	"context"
	"time"

	"github.com/orzlee/displayKvm/internal/model"
)

const previewHold = 5 * time.Second

// PlanPreview仅根据队列固定配置校验已保存映射并生成逐屏排队快照，绝不入队或
// 访问硬件。管理入口据此在短应答中提供完整选择，避免复制分组和目标解析规则。
func (q *Queue) PlanPreview(request model.SwitchRequest) (model.InputPreviewStatus, error) {
	if err := request.Validate(); err != nil {
		return model.InputPreviewStatus{}, err
	}
	plan, err := q.plan(request)
	if err != nil {
		return model.InputPreviewStatus{}, err
	}
	return previewInitial(request.TargetID, plan), nil
}

// Preview将整次往返放入唯一worker。请求超时只约束每屏控制阶段，固定保持期
// 不占其它屏的控制预算；取消后仍等同一作业完成恢复，不提前遗弃原生后端调用。
func (q *Queue) Preview(ctx context.Context, request model.SwitchRequest, progress func(model.InputPreviewStatus)) (model.InputPreviewStatus, error) {
	request.DisplayIDs = append([]model.DisplayID(nil), request.DisplayIDs...)
	if err := request.Validate(); err != nil {
		return model.InputPreviewStatus{}, err
	}
	plan, err := q.plan(request)
	if err != nil {
		return model.InputPreviewStatus{}, err
	}
	operation, cancel := context.WithCancel(ctx)
	defer cancel()
	j := &job{ctx: operation, cancel: cancel, done: make(chan struct{})}
	j.run = func(ctx context.Context) outcome {
		return outcome{preview: q.executePreview(ctx, request, plan, progress)}
	}
	j.abort = func(err error) outcome {
		status := previewInitial(request.TargetID, plan)
		status.State = "failed"
		if code := model.CodeOf(err); code == model.CodeCanceled || code == model.CodeTimeout {
			status.State = "cancelled"
		}
		previewSkipRemaining(&status, 0, "预览未执行："+err.Error())
		previewFinish(&status)
		return outcome{preview: status}
	}
	if _, _, err := q.enqueue(j); err != nil {
		return model.InputPreviewStatus{}, err
	}
	<-j.done
	return j.result.preview.Clone(), j.result.err
}

func previewInitial(target model.TargetID, plan []action) model.InputPreviewStatus {
	status := model.InputPreviewStatus{State: "queued", Phase: "queued", TargetID: target, StartedAt: time.Now(), Displays: make([]model.InputPreviewDisplayResult, len(plan))}
	for i, action := range plan {
		status.Displays[i] = model.InputPreviewDisplayResult{DisplayID: action.id, TargetInput: action.scheme.Value, SwitchStatus: "pending", RestoreStatus: "pending"}
	}
	return status
}

func previewFinish(status *model.InputPreviewStatus) {
	now := time.Now()
	status.Phase, status.ActiveDisplayID, status.HoldUntil, status.FinishedAt = "finished", "", nil, &now
}

func previewSkipRemaining(status *model.InputPreviewStatus, start int, detail string) {
	for i := start; i < len(status.Displays); i++ {
		status.Displays[i].SwitchStatus, status.Displays[i].RestoreStatus, status.Displays[i].Detail = "skipped", "not_needed", detail
	}
}

func previewPublish(status model.InputPreviewStatus, progress func(model.InputPreviewStatus)) {
	if progress != nil {
		progress(status.Clone())
	}
}

func previewDetail(result *model.InputPreviewDisplayResult, stage string, err error) {
	if result.Detail != "" {
		result.Detail += "；"
	}
	result.Detail += stage + "：" + err.Error()
}

func (q *Queue) executePreview(ctx context.Context, request model.SwitchRequest, plan []action, progress func(model.InputPreviewStatus)) model.InputPreviewStatus {
	status := previewInitial(request.TargetID, plan)
	status.State = "running"
	for i, action := range plan {
		if ctx.Err() != nil {
			status.State = "cancelled"
			previewSkipRemaining(&status, i, "预览已取消，未继续切换")
			break
		}
		status.ActiveDisplayID, status.HoldUntil = action.id, nil
		ok := q.previewDisplay(ctx, request.EffectiveTimeout(), action, &status, i, progress)
		if !ok || ctx.Err() != nil {
			status.State = "failed"
			if ctx.Err() != nil {
				status.State = "cancelled"
			}
			// 当前屏尚有未决失败时不再切走其它屏，保留本机可见画面的机会。
			previewSkipRemaining(&status, i+1, "前一显示器测试未完成，未继续切换")
			break
		}
	}
	if status.State == "running" {
		status.State = "completed"
	}
	previewFinish(&status)
	previewPublish(status, progress)
	return status
}

func (q *Queue) previewDisplay(ctx context.Context, budget time.Duration, action action, status *model.InputPreviewStatus, index int, progress func(model.InputPreviewStatus)) bool {
	result := &status.Displays[index]
	status.Phase = "pre_read"
	previewPublish(*status, progress)
	control, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	device, err := q.resolveDevice(control, action.id)
	if err == nil {
		var caps model.DeviceCapabilities
		caps, err = retryRead(control, func(ctx context.Context) (model.DeviceCapabilities, error) {
			return q.backend.Capabilities(ctx, device)
		})
		if err == nil {
			supported := false
			for _, protocol := range caps.Protocols {
				supported = supported || protocol == action.scheme.Protocol
			}
			if caps.UnavailableReason != "" || !caps.CanReadInput || !supported {
				err = model.NewError(model.CodeUnsupportedOperation, "预览需要可用且支持目标协议的真实输入读写能力："+caps.UnavailableReason, nil)
			}
		}
	}
	var original model.InputObservation
	if err == nil {
		readAt := time.Now()
		original, err = q.readObservation(control, device, action.scheme)
		if err == nil && original.ObservedAt.Before(readAt) {
			err = model.NewError(model.CodeTransport, "预读返回历史观测，不能作为恢复依据", nil)
		}
	}
	if err != nil {
		result.SwitchStatus, result.RestoreStatus = "failed", "not_needed"
		previewDetail(result, "预读失败，未写入", err)
		return false
	}
	value := *original.Value
	observed := value
	result.OriginalInput, result.ObservedInput = &value, &observed
	if value == action.scheme.Value {
		result.SwitchStatus, result.RestoreStatus = "skipped", "not_needed"
		previewPublish(*status, progress)
		return true
	}
	status.Phase = "switching"
	previewPublish(*status, progress)
	if err := control.Err(); err != nil {
		result.SwitchStatus, result.RestoreStatus = "failed", "not_needed"
		previewDetail(result, "切换前取消，未写入", err)
		return false
	}
	// 从首次SetInput调用前即承担恢复责任：错误或零回执仍可能已经切走。
	// 只发一次；保持期以同步写返回为起点，读回耗时必须包含在这五秒内。
	result.SwitchStatus = "unknown"
	receipt, writeErr := q.backend.SetInput(control, device, action.scheme)
	returned := time.Now()
	holdUntil := returned.Add(previewHold)
	status.HoldUntil = &holdUntil
	if writeErr != nil {
		previewDetail(result, "测试写结果未知", writeErr)
	} else if receipt.SentAt.IsZero() {
		previewDetail(result, "测试写结果未知", model.NewError(model.CodeTransport, "后端未提供有效发送回执", nil))
	} else if control.Err() != nil {
		previewDetail(result, "测试写后控制已取消", control.Err())
	} else {
		// 验证读也受保持截止点约束，不能让一个遵守context的慢读把固定五秒
		// 延长成完整控制预算。原生调用不可抢占时仍须等待其返回后在本worker恢复。
		verification, stopVerification := context.WithDeadline(control, holdUntil)
		observation, readErr := q.readObservation(verification, device, action.scheme)
		stopVerification()
		if validObservation(observation) {
			observed := *observation.Value
			result.ObservedInput = &observed
		}
		if readErr == nil && !observation.ObservedAt.Before(receipt.SentAt) && *observation.Value == action.scheme.Value {
			result.SwitchStatus = "verified"
		} else {
			if readErr == nil {
				readErr = model.NewError(model.CodeTransport, "没有发送后与目标一致的有效读回", nil)
			}
			previewDetail(result, "测试写读回未知", readErr)
		}
	}
	cancel()
	status.Phase = "holding"
	previewPublish(*status, progress)
	timer := time.NewTimer(time.Until(holdUntil))
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
	timer.Stop()
	status.Phase, status.HoldUntil = "restoring", nil
	previewPublish(*status, progress)
	// Queue.Close已取消life时仍在同一worker恢复。独立有限预算不能继承取消，
	// 也不能另起硬件协程；后端原生调用不支持抢占时必须等其真实返回后才交付。
	recovery, finish := context.WithTimeout(context.Background(), model.DefaultRequestTimeout)
	defer finish()
	q.previewRestore(recovery, action, value, result)
	previewPublish(*status, progress)
	return result.SwitchStatus == "verified" && (result.RestoreStatus == "restored" || result.RestoreStatus == "not_needed")
}

func (q *Queue) previewRestore(ctx context.Context, action action, original uint16, result *model.InputPreviewDisplayResult) {
	device, err := q.resolveDevice(ctx, action.id)
	readAt := time.Now()
	var observation model.InputObservation
	if err == nil {
		observation, err = q.readObservation(ctx, device, action.scheme)
		if err == nil && observation.ObservedAt.Before(readAt) {
			err = model.NewError(model.CodeTransport, "恢复前读回不是本次新鲜观测", nil)
		}
	}
	if err != nil {
		result.RestoreStatus = "failed"
		previewDetail(result, "恢复前解析或预读失败，未写入", err)
		return
	}
	value := *observation.Value
	result.ObservedInput = &value
	if value == original {
		result.RestoreStatus = "not_needed"
		return
	}
	if value != action.scheme.Value {
		result.RestoreStatus = "conflict"
		previewDetail(result, "恢复冲突，未写入", model.NewError(model.CodeInvalidRequest, "显示器输入已由其它来源改变", nil))
		return
	}
	restoreScheme := action.scheme
	restoreScheme.Value = original
	// 复用该屏已保存协议，只替换真实预读值；不构造临时配置、不重发恢复写。
	// 新鲜读返回后预算仍可能耗尽，发写前再次检查，避免向已结束的恢复阶段加写。
	if err := ctx.Err(); err != nil {
		result.RestoreStatus = "failed"
		previewDetail(result, "恢复写入前预算已结束，未写入", err)
		return
	}
	receipt, writeErr := q.backend.SetInput(ctx, device, restoreScheme)
	returned := time.Now()
	result.RestoreStatus = "unknown"
	if writeErr != nil {
		previewDetail(result, "恢复写结果未知", writeErr)
	} else if receipt.SentAt.IsZero() {
		previewDetail(result, "恢复写结果未知", model.NewError(model.CodeTransport, "后端未提供有效恢复回执", nil))
	}
	observation, readErr := q.readObservation(ctx, device, restoreScheme)
	if validObservation(observation) {
		value := *observation.Value
		result.ObservedInput = &value
	}
	if readErr == nil && !observation.ObservedAt.Before(returned) && *observation.Value == original {
		result.RestoreStatus = "restored"
		return
	}
	if readErr == nil {
		readErr = model.NewError(model.CodeTransport, "没有恢复写后与原输入一致的有效读回", nil)
	}
	previewDetail(result, "恢复读回未知", readErr)
}

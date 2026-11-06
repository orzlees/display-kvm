package cli

import (
	"context"

	"github.com/orzlee/displayKvm/internal/config"
	"github.com/orzlee/displayKvm/internal/model"
)

type plannedDisplay struct {
	DisplayID model.DisplayID   `json:"display_id"`
	Scheme    model.InputScheme `json:"scheme"`
}

func (r *runner) switchDisplays(ctx context.Context, args []string) outcome {
	fs := r.flags("switch")
	target := fs.String("target", "", "目标ID")
	group := fs.String("group", "", "分组ID，与--display互斥")
	timeout := fs.Int64("timeout-ms", 0, "操作超时，0使用统一默认值")
	dryRun := fs.Bool("dry-run", false, "只投影配置与映射，不发送切换请求")
	var displays repeated
	fs.Var(&displays, "display", "显示器ID，可重复选择不同屏")
	if err := parse(fs, args); err != nil {
		return r.parseFailure(err)
	}
	request := model.SwitchRequest{ID: "cli-validation", TargetID: model.TargetID(*target), GroupID: model.GroupID(*group), TimeoutMillis: *timeout}
	for _, display := range displays {
		request.DisplayIDs = append(request.DisplayIDs, model.DisplayID(display))
	}
	if err := request.Validate(); err != nil {
		return failure(err)
	}
	id, err := r.opts.NewID()
	if err != nil {
		return failure(err)
	}
	request.ID = model.RequestID(id)
	if err := request.Validate(); err != nil {
		return failure(err)
	}
	c, err := r.connect(ctx)
	if err != nil {
		return failure(err)
	}
	cfg, err := c.Configuration()
	if err != nil {
		return failure(err)
	}
	plan, err := projectSelection(cfg, request)
	if err != nil {
		return failure(err)
	}
	if *dryRun {
		return outcome{data: struct {
			DryRun        bool                `json:"dry_run"`
			Request       model.SwitchRequest `json:"request"`
			Displays      []plannedDisplay    `json:"displays"`
			HardwareState string              `json:"hardware_state"`
			Note          string              `json:"note"`
		}{true, request, plan, "unknown", "仅验证配置及映射；未检查设备可达性、身份或实际输入，未发送任何操作"}}
	}
	result, err := c.Switch(ctx, request)
	if err != nil {
		failed := failure(err)
		if result.RequestID != "" || len(result.Displays) != 0 || result.Error != nil {
			failed.data = result
		}
		return failed
	}
	// 已返回的逐屏证据优先于随后发生的context取消。不能用ctx.Err替换真实结果，
	// 也不能因结果缺失就重发；原请求可能已经切走硬件控制通道。
	if result.RequestID != request.ID || result.Validate() != nil {
		return outcome{data: result, err: model.NewError(model.CodeTransport, "切换响应关联ID或逐屏证据无效；不会自动重发", nil), code: ExitFailure}
	}
	if result.Error != nil {
		failed := failure(result.Error)
		failed.data = result
		return failed
	}
	expected := make(map[model.DisplayID]uint16, len(plan))
	for _, display := range plan {
		expected[display.DisplayID] = display.Scheme.Value
	}
	complete := len(result.Displays) == len(expected)
	for _, display := range result.Displays {
		value, found := expected[display.DisplayID]
		if !found || value != display.DesiredValue {
			return outcome{data: result, err: model.NewError(model.CodeTransport, "切换响应的显示器集合或目标输入与请求不符；不会自动重发", nil), code: ExitFailure}
		}
		delete(expected, display.DisplayID)
		if display.State != model.StateSent && display.State != model.StateConfirmed {
			complete = false
		}
	}
	if !complete || len(expected) != 0 {
		return outcome{data: result, code: ExitFailure}
	}
	return outcome{data: result}
}

// projectSelection仅在配置层展开选择，用于缺映射预检、dry-run和响应集合核对。
// 后台队列的执行计划未导出，因此这里不能读取后端、匹配设备或替代后台的最终校验；
// 配置可能在请求前发生变化，后台仍是执行事实的唯一来源，CLI绝不据此补发或补选屏。
func projectSelection(cfg config.Config, request model.SwitchRequest) ([]plannedDisplay, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	var target *model.Target
	for i := range cfg.Targets {
		if cfg.Targets[i].ID == request.TargetID {
			target = &cfg.Targets[i]
			break
		}
	}
	if target == nil {
		return nil, usageError("target", "目标不存在于后台配置中")
	}
	selected := append([]model.DisplayID(nil), request.DisplayIDs...)
	if request.GroupID != "" {
		found := false
		for _, group := range cfg.Groups {
			if group.ID == request.GroupID {
				selected = append(selected, group.DisplayIDs...)
				found = true
				break
			}
		}
		if !found {
			return nil, usageError("group", "分组不存在于后台配置中")
		}
	} else if len(selected) == 0 {
		for _, input := range target.Inputs {
			selected = append(selected, input.DisplayID)
		}
	}
	schemes := make(map[model.SchemeID]model.InputScheme, len(cfg.InputSchemes))
	for _, scheme := range cfg.InputSchemes {
		schemes[scheme.ID] = scheme
	}
	mappings := make(map[model.DisplayID]model.SchemeID, len(target.Inputs))
	for _, input := range target.Inputs {
		mappings[input.DisplayID] = input.SchemeID
	}
	plan := make([]plannedDisplay, 0, len(selected))
	for _, display := range selected {
		schemeID, found := mappings[display]
		if !found {
			return nil, usageError("display_ids", "选择的显示器不存在或缺少目标映射，未发送切换请求")
		}
		plan = append(plan, plannedDisplay{DisplayID: display, Scheme: schemes[schemeID]})
	}
	return plan, nil
}

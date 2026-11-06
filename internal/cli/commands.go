package cli

import (
	"context"
	"errors"
	"os"
	"strings"

	"github.com/orzlee/displayKvm/internal/config"
	"github.com/orzlee/displayKvm/internal/model"
)

func (r *runner) list(ctx context.Context, args []string) outcome {
	if err := parse(r.flags("list"), args); err != nil {
		return r.parseFailure(err)
	}
	c, err := r.connect(ctx)
	if err != nil {
		return failure(err)
	}
	devices, err := c.Enumerate(ctx)
	if err != nil {
		return failure(err)
	}
	if devices == nil {
		devices = []model.Device{}
	}
	return outcome{data: struct {
		Devices []model.Device `json:"devices"`
	}{devices}}
}

type statusData struct {
	Daemon            string                  `json:"daemon"`
	Configuration     *config.Config          `json:"configuration"`
	Devices           []model.Device          `json:"devices"`
	EventCapabilities []model.EventCapability `json:"event_capabilities"`
	CurrentInput      string                  `json:"current_input"`
	LastSwitch        string                  `json:"last_switch"`
	Note              string                  `json:"note"`
}

func (r *runner) status(ctx context.Context, args []string) outcome {
	if err := parse(r.flags("status"), args); err != nil {
		return r.parseFailure(err)
	}
	data := statusData{Daemon: "unknown", CurrentInput: "unknown", LastSwitch: "unknown", Note: "当前接口未提供输入或历史切换观测；目标映射不能证明当前画面"}
	c, err := r.connect(ctx)
	if err != nil {
		result := failure(err)
		result.data = data
		return result
	}
	cfg, err := c.Configuration()
	if err == nil {
		data.Configuration = &cfg
		data.Devices, err = c.Enumerate(ctx)
	}
	if err == nil {
		data.Daemon = "reachable"
		if data.Devices == nil {
			data.Devices = []model.Device{}
		}
		data.EventCapabilities, err = c.EventCapabilities(ctx)
		if err == nil && data.EventCapabilities == nil {
			data.EventCapabilities = []model.EventCapability{}
		}
	}
	result := failure(err)
	result.data = data
	return result
}

func (r *runner) checkConfig(args []string) outcome {
	if len(args) == 0 || args[0] != "check" {
		return failure(usageError("command", "配置检查用法为config check --file PATH"))
	}
	fs := r.flags("config check")
	file := fs.String("file", "", "待检查的配置文件")
	if err := parse(fs, args[1:]); err != nil {
		return r.parseFailure(err)
	}
	if *file == "" {
		return failure(usageError("file", "配置检查必须显式指定--file"))
	}
	store, err := r.settingsStore()
	if err != nil {
		return failure(err)
	}
	// 检查只读打开完整文档；即使版本或内容无效，也不能重置或覆盖原文件。
	snapshot, loadErr := store.ReadFile(*file)
	if loadErr != nil {
		if errors.Is(loadErr, os.ErrNotExist) {
			return failure(model.NewError(model.CodeIO, "无法打开待检查的配置文件", loadErr))
		}
		result := failure(loadErr)
		// 未知版本、超长或格式错误均是此命令的配置输入错误，不冒充后台操作失败。
		if model.CodeOf(loadErr) != model.CodeIO {
			result.code = ExitUsage
		}
		return result
	}
	return outcome{data: struct {
		Valid         bool          `json:"valid"`
		Saved         bool          `json:"saved"`
		Configuration config.Config `json:"configuration"`
	}{true, false, snapshot.Document.Configuration}}
}

type repeated []string

func (values *repeated) String() string { return strings.Join(*values, ",") }
func (values *repeated) Set(value string) error {
	*values = append(*values, value)
	return nil
}

func (r *runner) emit(ctx context.Context, args []string) outcome {
	if len(args) == 0 || args[0] != "emit" {
		return failure(usageError("command", "自定义事件用法为event emit --name NAME"))
	}
	fs := r.flags("event emit")
	name := fs.String("name", "", "自定义事件名称")
	var attributes repeated
	fs.Var(&attributes, "attribute", "事件属性KEY=VALUE，可重复")
	if err := parse(fs, args[1:]); err != nil {
		return r.parseFailure(err)
	}
	event := model.Event{ID: "cli-validation", Kind: model.EventCustom, Source: "cli", ObservedAt: r.opts.Now(), Name: *name}
	if len(attributes) != 0 {
		event.Attributes = make(map[string]string, len(attributes))
		for _, attribute := range attributes {
			key, value, found := strings.Cut(attribute, "=")
			if _, duplicate := event.Attributes[key]; !found || duplicate {
				return failure(usageError("attribute", "属性必须为不重复的KEY=VALUE"))
			}
			event.Attributes[key] = value
		}
	}
	if err := event.Validate(); err != nil {
		return failure(err)
	}
	id, err := r.opts.NewID()
	if err != nil {
		return failure(err)
	}
	event.ID = id
	if err := event.Validate(); err != nil {
		return failure(err)
	}
	c, err := r.connect(ctx)
	if err == nil {
		err = c.EmitEvent(ctx, event)
	}
	if err != nil {
		return failure(err)
	}
	return outcome{data: struct {
		EventID  string `json:"event_id"`
		Accepted bool   `json:"accepted"`
		Note     string `json:"note"`
	}{id, true, "仅确认事件已受理，不代表规则匹配或显示器已切换"}}
}

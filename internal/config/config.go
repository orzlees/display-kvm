// Package config提供无文件副作用的配置加载、校验和版本迁移；原子保存和私有权限
// 由后续后台实现负责，不能在配置读取失败时静默重置并覆盖原文件。
package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/orzlee/displayKvm/internal/model"
)

const (
	CurrentVersion = 3
	MaxConfigBytes = 1 << 20
)

type Config struct {
	Version      int                   `json:"version"`
	Displays     []model.Display       `json:"displays"`
	InputSchemes []model.InputScheme   `json:"input_schemes"`
	Targets      []model.Target        `json:"targets"`
	Groups       []model.Group         `json:"groups"`
	Calibrations []CalibrationMetadata `json:"calibrations,omitempty"`
}

// v3只为已确认校准增加可选依据；旧v1/v2加载时不推测或补造依据。
// 旧binary不能读v3，保存方应明确提示该限制，不能把元数据静默写进v2文档。
// 依据是历史确认记录；显示当前映射时还须匹配DisplayID/TargetID/Scheme完整值。
// 不能要求其永远匹配当前映射，否则既有校准服务在更新映射、尚未替换依据时无法校验。
type CalibrationMetadata struct {
	DisplayID        model.DisplayID         `json:"display_id"`
	TargetID         model.TargetID          `json:"target_id"`
	PortName         string                  `json:"port_name"`
	Scheme           model.InputScheme       `json:"scheme"`
	Basis            string                  `json:"basis"`
	Observation      *model.InputObservation `json:"observation,omitempty"`
	ObservationIssue *model.Error            `json:"observation_issue,omitempty"`
	ConfirmedAt      *time.Time              `json:"confirmed_at"`
}

// Validate检查逻辑身份和所有引用。硬件提示相同或缺失是正常情况；只有逻辑ID
// 必须唯一，不以EDID、序列号或当前枚举次序合并显示器。目标可以只覆盖部分
// 显示器；后续请求选择到未映射屏时必须报错，不能猜测端口或自动扩展映射。
func (c Config) Validate() error {
	if c.Version != CurrentVersion {
		return model.NewError(model.CodeUnsupportedVersion, "配置不是当前版本，须通过Load迁移", nil)
	}
	displays := make(map[model.DisplayID]bool, len(c.Displays))
	bindings := make(map[string]bool)
	for i, display := range c.Displays {
		field := fmt.Sprintf("displays[%d]", i)
		if err := display.Validate(); err != nil {
			return configError(field, "显示器配置无效", err)
		}
		if displays[display.ID] {
			return configError(field+".id", "显示器逻辑ID重复", nil)
		}
		displays[display.ID] = true
		if display.Binding != nil {
			if bindings[display.Binding.Path] {
				return configError(field+".binding.path", "同一路径不能人工绑定多个逻辑显示器", nil)
			}
			bindings[display.Binding.Path] = true
		}
	}
	schemes := make(map[model.SchemeID]bool, len(c.InputSchemes))
	for i, scheme := range c.InputSchemes {
		field := fmt.Sprintf("input_schemes[%d]", i)
		if err := scheme.Validate(); err != nil {
			return configError(field, "输入控制方案无效", err)
		}
		if schemes[scheme.ID] {
			return configError(field+".id", "输入方案ID重复", nil)
		}
		schemes[scheme.ID] = true
	}
	targets := make(map[model.TargetID]bool, len(c.Targets))
	for i, target := range c.Targets {
		field := fmt.Sprintf("targets[%d]", i)
		if !model.ValidID(string(target.ID)) || targets[target.ID] {
			return configError(field+".id", "目标ID无效或重复", nil)
		}
		targets[target.ID] = true
		if len(target.Inputs) == 0 {
			return configError(field+".inputs", "目标必须至少含一屏映射", nil)
		}
		seen := make(map[model.DisplayID]bool, len(target.Inputs))
		for j, input := range target.Inputs {
			if !displays[input.DisplayID] || !schemes[input.SchemeID] || seen[input.DisplayID] {
				return configError(fmt.Sprintf("%s.inputs[%d]", field, j), "映射引用不存在或同一目标重复映射显示器", nil)
			}
			seen[input.DisplayID] = true
		}
	}
	groups := make(map[model.GroupID]bool, len(c.Groups))
	for i, group := range c.Groups {
		field := fmt.Sprintf("groups[%d]", i)
		if !model.ValidID(string(group.ID)) || groups[group.ID] {
			return configError(field+".id", "分组ID无效或重复", nil)
		}
		groups[group.ID] = true
		if len(group.DisplayIDs) == 0 {
			return configError(field+".display_ids", "分组必须至少含一屏", nil)
		}
		seen := make(map[model.DisplayID]bool, len(group.DisplayIDs))
		for _, id := range group.DisplayIDs {
			if !displays[id] || seen[id] {
				return configError(field+".display_ids", "分组包含不存在或重复的显示器", nil)
			}
			seen[id] = true
		}
	}
	seenCalibration := make(map[string]bool, len(c.Calibrations))
	for i, item := range c.Calibrations {
		field := fmt.Sprintf("calibrations[%d]", i)
		key := string(item.DisplayID) + "\x00" + string(item.TargetID)
		if !model.ValidID(string(item.DisplayID)) || !model.ValidID(string(item.TargetID)) || seenCalibration[key] || strings.TrimSpace(item.PortName) == "" || len(item.PortName) > 256 || item.ConfirmedAt == nil || item.ConfirmedAt.IsZero() || (item.Basis != "readback" && item.Basis != "manual") {
			return configError(field, "校准依据缺少有效引用、人工确认或端口名称", nil)
		}
		seenCalibration[key] = true
		if err := item.Scheme.Validate(); err != nil {
			return configError(field, "校准方案依据无效", err)
		}
		if item.Observation != nil && (item.Observation.Value == nil || item.Observation.ObservedAt.IsZero()) {
			return configError(field, "校准读回缺少数值或时间", nil)
		}
		if item.Basis == "readback" && (item.Observation == nil || item.Observation.Value == nil || *item.Observation.Value != item.Scheme.Value) {
			return configError(field, "读回校准依据与输入值不一致", nil)
		}
		if item.ObservationIssue != nil {
			if err := item.ObservationIssue.Validate(); err != nil {
				return configError(field, "校准错误依据无效", err)
			}
		}
	}
	return nil
}

func Load(reader io.Reader) (Config, error) {
	if reader == nil {
		return Config{}, configError("", "配置读取器不能为空", nil)
	}
	// 多读一个字节区分恰好到达上限与已被截断；禁止把截断文档当成有效配置。
	data, err := io.ReadAll(io.LimitReader(reader, MaxConfigBytes+1))
	if err != nil {
		return Config{}, model.NewError(model.CodeIO, "读取配置失败", err)
	}
	if len(data) > MaxConfigBytes {
		return Config{}, model.NewError(model.CodeMessageTooLarge, "配置超过1MiB", nil)
	}
	var header struct {
		Version *int `json:"version"`
	}
	if err := json.Unmarshal(data, &header); err != nil || header.Version == nil {
		return Config{}, configError("version", "配置缺少版本或不是有效JSON对象", err)
	}
	var cfg Config
	switch *header.Version {
	case CurrentVersion:
		if err := model.DecodeJSON(data, &cfg); err != nil {
			return Config{}, configError("", "配置JSON字段或结构无效", err)
		}
	case 2:
		// 先按原v2完整字段集合严格解码，禁止把声称v2的新字段偷偷当成兼容格式。
		var old struct {
			Version      int                 `json:"version"`
			Displays     []model.Display     `json:"displays"`
			InputSchemes []model.InputScheme `json:"input_schemes"`
			Targets      []model.Target      `json:"targets"`
			Groups       []model.Group       `json:"groups"`
		}
		if err := model.DecodeJSON(data, &old); err != nil {
			return Config{}, configError("", "v2兼容配置字段或结构无效", err)
		}
		cfg = Config{Version: CurrentVersion, Displays: old.Displays, InputSchemes: old.InputSchemes, Targets: old.Targets, Groups: old.Groups}
	case 1:
		var legacy legacyConfig
		if err := model.DecodeJSON(data, &legacy); err != nil {
			return Config{}, configError("", "v1兼容配置字段或结构无效", err)
		}
		cfg = migrateV1(legacy)
	default:
		return Config{}, model.NewError(model.CodeUnsupportedVersion, "只接受配置版本1、2和3，不能猜测未来版本语义", nil)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Encode仅向调用者提供的流编码当前版本，不创建目录、不覆盖原文件。
// 先完成校验与编码再写，避免非法配置造成半份输出；流自身失败仍可能部分写入，
// 真正落盘须由调用者以私有临时文件和原子替换完成，失败不得覆盖原配置。
func Encode(writer io.Writer, cfg Config) error {
	if writer == nil {
		return configError("", "配置写入器不能为空", nil)
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return configError("", "配置编码失败", err)
	}
	data = append(data, '\n')
	if len(data) > MaxConfigBytes {
		return model.NewError(model.CodeMessageTooLarge, "配置超过1MiB", nil)
	}
	written, err := writer.Write(data)
	if err == nil && written != len(data) {
		err = io.ErrShortWrite
	}
	if err != nil {
		return model.NewError(model.CodeIO, "写入配置失败", err)
	}
	return nil
}

type legacyInput struct {
	DisplayID model.DisplayID     `json:"display_id"`
	Protocol  model.InputProtocol `json:"protocol"`
	VCPCode   uint8               `json:"vcp_code,omitempty"`
	Value     uint16              `json:"value"`
}

type legacyTarget struct {
	ID     model.TargetID `json:"id"`
	Name   string         `json:"name"`
	Inputs []legacyInput  `json:"inputs"`
}

// v1是本契约明确接收的兼容格式，不代表已有历史发布或实际数据。它把协议参数
// 内联在每屏映射中；迁移只提取方案，保留目标、显示器、分组、顺序及原始参数。
type legacyConfig struct {
	Version  int             `json:"version"`
	Displays []model.Display `json:"displays"`
	Targets  []legacyTarget  `json:"targets"`
	Groups   []model.Group   `json:"groups"`
}

func migrateV1(legacy legacyConfig) Config {
	cfg := Config{Version: CurrentVersion, Displays: legacy.Displays, Groups: legacy.Groups}
	for _, target := range legacy.Targets {
		converted := model.Target{ID: target.ID, Name: target.Name}
		for _, input := range target.Inputs {
			// ID不依赖数组次序或系统路径。使用完整摘要和不可出现在合法ID中的分隔符，
			// 避免(a:b,c)/(a,b:c)拼接碰撞；重复原映射仍由统一校验拒绝，不做去重。
			digest := sha256.Sum256([]byte(string(target.ID) + "\x00" + string(input.DisplayID)))
			id := model.SchemeID("migrated-" + hex.EncodeToString(digest[:]))
			cfg.InputSchemes = append(cfg.InputSchemes, model.InputScheme{
				ID: id, Protocol: input.Protocol, VCPCode: input.VCPCode, Value: input.Value,
			})
			converted.Inputs = append(converted.Inputs, model.TargetInput{DisplayID: input.DisplayID, SchemeID: id})
		}
		cfg.Targets = append(cfg.Targets, converted)
	}
	return cfg
}

func configError(field, message string, cause error) *model.Error {
	code := model.CodeInvalidConfig
	if model.CodeOf(cause) == model.CodeUnsupportedProtocol {
		code = model.CodeUnsupportedProtocol
	}
	err := model.NewError(code, message, cause)
	err.Field = field
	return err
}

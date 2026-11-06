// Package settings聚合完整配置文档；业务规则仍由原模块校验，避免配置基础包
// 反向依赖规则和日程。读取失败绝不转为空文档后覆盖磁盘。
package settings

import (
	"bytes"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"

	"github.com/orzlee/displayKvm/internal/automation"
	"github.com/orzlee/displayKvm/internal/config"
	"github.com/orzlee/displayKvm/internal/controltools"
	"github.com/orzlee/displayKvm/internal/model"
	"github.com/orzlee/displayKvm/internal/scheduler"
)

const (
	Kind     = "displaykvm-settings"
	Version  = 1
	MaxBytes = config.MaxConfigBytes
)

type RuntimeSettings = controltools.Paths

type DesktopSettings struct {
	Language string `json:"language"`
}

type Document struct {
	Kind          string               `json:"kind"`
	Version       int                  `json:"version"`
	Configuration config.Config        `json:"configuration"`
	Rules         []automation.Rule    `json:"rules"`
	Schedules     []scheduler.Schedule `json:"schedules"`
	Runtime       RuntimeSettings      `json:"runtime"`
	Desktop       DesktopSettings      `json:"desktop"`
}

func NewDocument() Document {
	return Document{
		Kind: Kind, Version: Version,
		Configuration: config.Config{
			Version:  config.CurrentVersion,
			Displays: []model.Display{}, InputSchemes: []model.InputScheme{},
			Targets: []model.Target{}, Groups: []model.Group{},
		},
		Rules: []automation.Rule{}, Schedules: []scheduler.Schedule{},
		Desktop: DesktopSettings{Language: "system"},
	}
}

func Load(reader io.Reader) (Document, error) {
	if reader == nil {
		return Document{}, documentError("", "完整配置读取器不能为空", nil)
	}
	data, err := io.ReadAll(io.LimitReader(reader, MaxBytes+1))
	if err != nil {
		return Document{}, model.NewError(model.CodeIO, "读取完整配置失败", err)
	}
	if len(data) > MaxBytes {
		return Document{}, model.NewError(model.CodeMessageTooLarge, "完整配置超过1MiB", nil)
	}
	var doc Document
	if err := model.DecodeJSON(data, &doc); err != nil {
		return Document{}, documentError("", "完整配置JSON字段或结构无效", err)
	}
	// Go结构体解码会接受大小写别名，并将缺失/null标量化为零值。完整文档的
	// 原有分区和tool_path必须显式出现，不能把拼写错误或丢失字段解释为空设置。
	root, err := documentFields(data, []string{"kind", "version", "configuration", "rules", "schedules", "runtime"}, []string{"desktop"})
	if err != nil {
		return Document{}, err
	}
	if _, err := documentFields(root["runtime"], []string{"tool_path"}, []string{"busctl_path", "udevadm_path"}); err != nil {
		return Document{}, documentError("runtime", "运行工具字段无效", err)
	}
	// 新增桌面偏好前的完整文档没有desktop分区，只在内存补默认，不自动改写原件。
	// 一旦显式提供分区，就必须给出有效语言；不能把{}或空字符串当作旧格式缺省，
	// 否则损坏或拼写错误的偏好会被静默覆盖。内存字段构造的零值在Encode边界另行规范。
	if desktop, present := root["desktop"]; present {
		if _, err := documentFields(desktop, []string{"language"}, nil); err != nil {
			return Document{}, documentError("desktop", "桌面设置字段无效", err)
		}
		if doc.Desktop.Language == "" {
			return Document{}, documentError("desktop.language", "明确的桌面语言不能为空", nil)
		}
	} else {
		doc.Desktop.Language = "system"
	}
	if err := doc.Validate(); err != nil {
		return Document{}, err
	}
	return doc, nil
}

// documentFields只补充文档根对象、运行工具和桌面设置对象的必填/非null与准确键名检查；重复键、
// 深度及各业务类型的未知字段仍统一交给model.DecodeJSON，避免另建解析规则。
func documentFields(data []byte, required, optional []string) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
		return nil, documentError("", "配置分区必须是非null对象", err)
	}
	allowed := make(map[string]bool, len(required)+len(optional))
	for _, name := range required {
		allowed[name] = true
		if raw, ok := fields[name]; !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return nil, documentError(name, "完整配置缺少必填字段或字段为null", nil)
		}
	}
	for _, name := range optional {
		allowed[name] = true
	}
	for name, raw := range fields {
		if !allowed[name] || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return nil, documentError(name, "完整配置包含未知字段或null字段", nil)
		}
	}
	return fields, nil
}

func (d Document) Validate() error {
	if d.Kind != Kind {
		return documentError("kind", "不是DisplayKVM完整配置文档", nil)
	}
	if d.Version != Version {
		return model.NewError(model.CodeUnsupportedVersion, "只接受完整配置版本1", nil)
	}
	if err := d.Configuration.Validate(); err != nil {
		return err
	}
	if err := automation.ValidateRules(d.Configuration, d.Rules); err != nil {
		return err
	}
	if err := scheduler.ValidateSchedules(d.Schedules); err != nil {
		return err
	}
	// Prepare及既有字段构造可能未填写新增桌面分区；内存零值按system处理，
	// 统一编码时补成明确值。文件输入的空语言已由Load拒绝，不能在这里放宽坏JSON。
	switch d.Desktop.Language {
	case "", "system", "zh-CN", "en":
	default:
		return documentError("desktop.language", "桌面语言只接受system、zh-CN或en", nil)
	}
	for field, path := range map[string]string{"tool_path": d.Runtime.ToolPath, "busctl_path": d.Runtime.BusctlPath, "udevadm_path": d.Runtime.UdevadmPath} {
		if path != "" && (!filepath.IsAbs(path) || strings.IndexByte(path, 0) >= 0) {
			return documentError("runtime."+field, "非空运行工具位置必须是绝对路径", nil)
		}
	}
	return nil
}

func (d Document) ValidateForService(resolver controltools.Resolver) (controltools.Paths, error) {
	if err := d.Validate(); err != nil {
		return controltools.Paths{}, err
	}
	// 草稿保存不依赖工具已安装；真正启动时才由唯一解析器定位并核对随包工具。
	// 返回的安装绝对路径只供本次运行使用，不回写文档，否则移动应用会留下过期路径。
	return resolver.Resolve(d.Runtime)
}

func Encode(writer io.Writer, doc Document) error {
	if writer == nil {
		return documentError("", "完整配置写入器不能为空", nil)
	}
	if err := doc.Validate(); err != nil {
		return err
	}
	// Prepare/Clone可把合法的零长度切片变成nil；内存校验沿用原业务语义。
	// 只在统一编码边界规范根列表，保证磁盘rules/schedules始终为[]而非null，
	// 不把数组补齐责任分散到各消费者，也不修改调用者持有的草稿或业务配置。
	if doc.Rules == nil {
		doc.Rules = []automation.Rule{}
	}
	if doc.Schedules == nil {
		doc.Schedules = []scheduler.Schedule{}
	}
	// 只规范局部副本，兼容未填写新增字段的内存草稿；不让每个分区保存方各补默认，
	// 也不修改仍由界面持有的草稿。Load对显式空语言的严格检查保持独立。
	if doc.Desktop.Language == "" {
		doc.Desktop.Language = "system"
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return documentError("", "完整配置编码失败", err)
	}
	data = append(data, '\n')
	if len(data) > MaxBytes {
		return model.NewError(model.CodeMessageTooLarge, "完整配置超过1MiB", nil)
	}
	written, err := writer.Write(data)
	if err == nil && written != len(data) {
		err = io.ErrShortWrite
	}
	if err != nil {
		return model.NewError(model.CodeIO, "写入完整配置失败", err)
	}
	return nil
}

func documentError(field, message string, cause error) *model.Error {
	err := model.NewError(model.CodeInvalidConfig, message, cause)
	err.Field = field
	return err
}

// Package linux适配用户显式提供的外部ddcutil，不安装、捆绑或链接libddcutil。
// 首个输出兼容基线为2.2.7，3.0.2仅增加源码格式适配；具体发行版、权限、端口和显示器链路仍需实机验证。
package linux

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/orzlee/displayKvm/internal/backend"
	"github.com/orzlee/displayKvm/internal/model"
)

// SupportedDDCUtilVersion是已核对的外部CLI输出基线，不是自动安装或分发要求。
// 2.2.7-release的COPYING为GPLv2，main.c标注GPL-2.0-or-later；
// 本包只经标准库启动外部进程，不导入上游代码或链接库，不据此外推分发许可结论。
const SupportedDDCUtilVersion = "2.2.7"

type Backend struct {
	toolPath string
	runner   CommandRunner
	goos     string
}

var _ backend.Backend = (*Backend)(nil)

// New不做I/O，空路径保持未配置；由上层注入用户提供的绝对工具路径。
// runner为nil时使用标准库执行器，注入实现也必须在Run返回前结束进程与资源生命周期。
func New(toolPath string, runner CommandRunner) *Backend {
	if runner == nil {
		runner = ExecRunner{}
	}
	return &Backend{toolPath: toolPath, runner: runner, goos: runtime.GOOS}
}

func (b *Backend) Name() string { return "linux" }

func (b *Backend) availability(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return model.NewError(model.CodeCanceled, "ddcutil操作在启动命令前已取消或超时", err)
	}
	if b.goos != "linux" {
		return model.NewError(model.CodeBackendUnavailable, "ddcutil后端仅适用于Linux", nil)
	}
	if !filepath.IsAbs(b.toolPath) || strings.IndexByte(b.toolPath, 0) >= 0 {
		return model.NewError(model.CodeBackendUnavailable, "ddcutil未配置有效绝对路径，请显式指定用户自备的可执行文件", nil)
	}
	if b.runner == nil {
		return model.NewError(model.CodeBackendUnavailable, "ddcutil命令执行器未配置", nil)
	}
	return nil
}

func (b *Backend) Enumerate(ctx context.Context) ([]model.Device, error) {
	version, err := b.run(ctx, "确认工具版本", "--terse", "--version")
	if err != nil {
		return nil, err
	}
	// 仅精确接受既有基线和已核对同构字段的3.0.2，不能按版本大小推断未来输出兼容。
	// --version的真实字节及写入回执仍需实机确认，带前缀、开发后缀或诊断的输出保持失败。
	switch strings.TrimSpace(version.Stdout) {
	case SupportedDDCUtilVersion, "3.0.2":
	default:
		return nil, model.NewError(model.CodeUnsupportedVersion, "ddcutil仅适配2.2.7基线与3.0.2源码格式，请核对外部工具版本", fmt.Errorf("版本输出=%q", version.Stdout))
	}
	output, err := b.run(ctx, "枚举显示器", "detect", "--brief")
	if err != nil {
		return nil, err
	}
	return parseDisplays(output)
}

// Capabilities只描述本适配器在当前枚举对象上提供的标准VCP读写操作，
// CanReadInput不是显示器响应保证；枚举成功、品牌与OS均不能证明VCP 0x60已实测。
// LG alternate未实现，不把该协议隐式转成标准VCP或尝试其他厂商命令。
func (b *Backend) Capabilities(ctx context.Context, device model.Device) (model.DeviceCapabilities, error) {
	if err := b.validateDevice(ctx, device); err != nil {
		return model.DeviceCapabilities{UnavailableReason: err.Error()}, err
	}
	return model.DeviceCapabilities{Protocols: []model.InputProtocol{model.ProtocolVCP}, CanReadInput: true}, nil
}

func (b *Backend) ReadInput(ctx context.Context, device model.Device, scheme model.InputScheme) (model.InputObservation, error) {
	if err := scheme.Validate(); err != nil {
		return model.InputObservation{}, err
	}
	if scheme.Protocol != model.ProtocolVCP {
		return model.InputObservation{}, model.NewError(model.CodeUnsupportedProtocol, "ddcutil后端未实现该输入协议", nil)
	}
	if err := b.validateDevice(ctx, device); err != nil {
		return model.InputObservation{}, err
	}
	bus := strings.TrimPrefix(device.BackendID, "i2c-")
	output, err := b.run(ctx, "读取输入", "--bus", bus, "getvcp", "--terse", "60")
	if err != nil {
		return model.InputObservation{}, err
	}
	value, err := parseInput(output)
	if err != nil {
		return model.InputObservation{}, err
	}
	return model.InputObservation{Value: &value, ObservedAt: time.Now()}, nil
}

func (b *Backend) SetInput(ctx context.Context, device model.Device, scheme model.InputScheme) (model.WriteReceipt, error) {
	if err := scheme.Validate(); err != nil {
		return model.WriteReceipt{}, err
	}
	if scheme.Protocol != model.ProtocolVCP {
		return model.WriteReceipt{}, model.NewError(model.CodeUnsupportedProtocol, "ddcutil后端未实现该输入协议", nil)
	}
	if err := b.validateDevice(ctx, device); err != nil {
		return model.WriteReceipt{}, err
	}
	bus := strings.TrimPrefix(device.BackendID, "i2c-")
	value := strconv.FormatUint(uint64(scheme.Value), 10)
	output, err := b.run(ctx, "发送输入切换", "--bus", bus, "setvcp", "60", value, "--noverify")
	if err != nil {
		// 终止或非零退出不能撤销已经送达的DDC，也不能证明未发送；不填成功回执，
		// 由上层保留unknown语义，禁止本后端盲重试、换协议或回退到其他显示器。
		return model.WriteReceipt{}, err
	}
	if strings.TrimSpace(output.Stdout) != "" {
		return model.WriteReceipt{}, invalidOutput("发送输入切换，无法确认提交", output, nil)
	}
	// 两版上游手册均未保证固定成功回执；只保留零退出且双流无诊断的保守提交条件，
	// 非空输出不得猜测成功。真实回执仍需实机确认，不能从模拟执行器推出上游保证。
	// --noverify禁止内置读回，后续确认须取得不早于此时间的独立观测，不能承诺画面成功。
	return model.WriteReceipt{SentAt: time.Now()}, nil
}

func (b *Backend) validateDevice(ctx context.Context, device model.Device) error {
	devices, err := b.Enumerate(ctx)
	if err != nil {
		return err
	}
	for _, current := range devices {
		if current == device {
			// --bus只选择当前总线，不与--mfg/--model/--sn组成原子身份断言。
			// 此次重新枚举减少陈旧绑定误写，但检查与执行之间仍可能发生瞬间设备替换；
			// 相同或缺失EDID字段也不能区分物理设备，必须保留路径绑定与实机验证边界。
			// 本包不把EDIDSHA256传给--edid，后者要求原始EDID而非SHA256摘要。
			return nil
		}
	}
	return model.NewError(model.CodeNotFound, "ddcutil当前枚举中不存在原设备，请重新枚举和绑定；未发送输入命令", nil)
}

var (
	permissionDiagnostic  = regexp.MustCompile(`(?m)^Device /dev/i2c-[0-9]+ is not readable and writable\.  Error = .*\b(EACCES|EPERM|Permission denied|Operation not permitted)\b.*$`)
	unreachableDiagnostic = regexp.MustCompile(`(?m)^(No monitor detected on bus /dev/i2c-[0-9]+|Bus /dev/i2c-[0-9]+ (not found|does not exist\.)|Device /dev/i2c-[0-9]+ does not exist\. Error = .+|Display not found|No displays implementing DDC/CI found)$`)
)

func (b *Backend) run(ctx context.Context, stage string, args ...string) (CommandOutput, error) {
	if err := b.availability(ctx); err != nil {
		return CommandOutput{}, err
	}
	// 不读取用户ddcutil配置或特征定义，以免隐式选择器、重试和自定义字段改变语义。
	// 2.2.7默认不扫描USB；不传仅在ENABLE_USB构建中存在的--disable-usb选项，
	// 避免把未编译USB支持的合法工具误判为不可用。这里只适配明确的I2C总线。
	argv := append([]string{"--noconfig", "--disable-udf"}, args...)
	output, err := b.runner.Run(ctx, b.toolPath, argv...)
	if ctx.Err() != nil {
		err = errors.Join(ctx.Err(), err)
		return output, model.NewError(model.CodeCanceled, "ddcutil"+stage+"已取消或超时；提交结果可能不确定", err)
	}
	// 真实执行器在捕获时限量；注入执行器仍需在解析前遵守相同的共享边界。
	if len(output.Stdout) > MaxCommandOutputBytes || len(output.Stderr) > MaxCommandOutputBytes-len(output.Stdout) {
		return CommandOutput{}, model.NewError(model.CodeMessageTooLarge, "ddcutil"+stage+"输出超过合计1MiB上限", err)
	}
	code := model.CodeOf(err)
	if code == model.CodeInternal {
		code = model.CodeTransport
	}
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, exec.ErrNotFound) {
		code = model.CodeBackendUnavailable
	} else if errors.Is(err, os.ErrPermission) {
		code = model.CodePermissionDenied
	} else if err == nil || code == model.CodeTransport {
		text := output.Stdout + "\n" + output.Stderr
		switch {
		case permissionDiagnostic.MatchString(text):
			code = model.CodePermissionDenied
		case strings.Contains(text, "Driver i2c_dev must be loaded or builtin"),
			strings.Contains(text, "No /sys/bus/i2c buses that might be used for DDC/CI communication found."),
			strings.Contains(text, "No /dev/i2c devices exist."),
			strings.Contains(text, "ddcutil requires module i2c-dev."):
			code = model.CodeBackendUnavailable
		case unreachableDiagnostic.MatchString(text):
			code = model.CodeUnreachable
		default:
			if err == nil && strings.TrimSpace(output.Stderr) != "" {
				code = model.CodeTransport
			}
		}
		// main.c的detect路径会忽略verify_i2c_access返回值并零退出，
		// 因此已识别诊断或未知stderr也必须失败，不能吞成空设备列表或操作成功。
		if err == nil && code != "" {
			err = errors.New("ddcutil报告诊断，零退出不构成操作成功证据")
		}
	}
	if err == nil {
		return output, nil
	}
	// ERR及非结构化输出不能区分不支持与通信故障；保留根因而不推测硬件能力。
	// 原始输出可能包含路径或序列号，只放在不序列化的cause中，且不做自动重试。
	cause := fmt.Errorf("ddcutil%s；stdout=%q；stderr=%q: %w", stage, output.Stdout, output.Stderr, err)
	return output, model.NewError(code, "ddcutil"+stage+"失败；命令错误不能证明输入切换未发送", cause)
}

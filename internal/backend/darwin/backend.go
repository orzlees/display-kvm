// Package darwin提供用户显式配置的外部m1ddc适配，不安装、搜索或注册工具。
// 工具具备命令不等于显示器链路已验证；具体SoC、端口、扩展坞和显示器仍需实测。
package darwin

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

// Backend仅适配Apple Silicon上的m1ddc命令。每次操作重新枚举并用UUID选择，
// 不保存原生句柄，也不负责上层唯一写入者的排队、重试或持久身份匹配。
type Backend struct {
	toolPath string
	runner   CommandRunner
	goos     string
	goarch   string
}

var _ backend.Backend = (*Backend)(nil)

// New要求用户提供可执行文件的绝对路径；空路径保持未配置，不查询PATH。
// runner为nil时使用标准库执行器；注入实现也必须在返回前完成取消和资源清理。
// 当前进程在Rosetta下报告amd64时保守拒绝，不据此宣称Intel或转译执行受支持。
func New(toolPath string, runner CommandRunner) *Backend {
	if runner == nil {
		runner = ExecRunner{}
	}
	return &Backend{toolPath: toolPath, runner: runner, goos: runtime.GOOS, goarch: runtime.GOARCH}
}

func (b *Backend) Name() string { return "darwin" }

func (b *Backend) availability(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return model.NewError(model.CodeCanceled, "m1ddc操作在启动命令前已取消或超时", err)
	}
	if b.goos != "darwin" || b.goarch != "arm64" {
		return model.NewError(model.CodeBackendUnavailable, "m1ddc后端仅适用于macOS Apple Silicon；Intel及其他运行架构未支持", nil)
	}
	if !filepath.IsAbs(b.toolPath) || strings.IndexByte(b.toolPath, 0) >= 0 {
		return model.NewError(model.CodeBackendUnavailable, "m1ddc未配置有效绝对路径，请显式指定用户自备的可执行文件", nil)
	}
	if b.runner == nil {
		return model.NewError(model.CodeBackendUnavailable, "m1ddc命令执行器未配置", nil)
	}
	return nil
}

func (b *Backend) Enumerate(ctx context.Context) ([]model.Device, error) {
	output, err := b.run(ctx, "枚举显示器", "display", "list")
	if err != nil {
		// 上游无外接屏时以非零退出返回这条明确文本；仅识别这一完整输出，
		// 工具缺失、权限拒绝、取消或其他枚举失败不能被当成空列表吞掉。
		if model.CodeOf(err) == model.CodeTransport && strings.TrimSpace(output.Stdout) == "No external display found, aborting" && strings.TrimSpace(output.Stderr) == "" {
			return []model.Device{}, nil
		}
		return nil, err
	}
	return parseDisplays(output)
}

// Capabilities报告当前工具和枚举对象可用时本适配器能执行的协议，不探测或
// 推断具体链路是否接受DDC。CanReadInput表示提供读取操作，实际读回仍可失败；
// LG alternate只在方案显式选择时使用，品牌、OS和枚举成功均不作为硬件支持证据。
func (b *Backend) Capabilities(ctx context.Context, device model.Device) (model.DeviceCapabilities, error) {
	if err := b.validateDevice(ctx, device); err != nil {
		return model.DeviceCapabilities{UnavailableReason: err.Error()}, err
	}
	return model.DeviceCapabilities{Protocols: []model.InputProtocol{model.ProtocolVCP, model.ProtocolLGInputAlt}, CanReadInput: true}, nil
}

func (b *Backend) ReadInput(ctx context.Context, device model.Device, scheme model.InputScheme) (model.InputObservation, error) {
	if err := scheme.Validate(); err != nil {
		return model.InputObservation{}, err
	}
	if err := b.validateDevice(ctx, device); err != nil {
		return model.InputObservation{}, err
	}
	field, err := inputField(scheme.Protocol)
	if err != nil {
		return model.InputObservation{}, err
	}
	output, err := b.run(ctx, "读取输入", "display", device.BackendID, "get", field)
	if err != nil {
		return model.InputObservation{}, err
	}
	valueText := strings.TrimSpace(output.Stdout)
	value, parseErr := strconv.ParseUint(valueText, 10, 16)
	if parseErr != nil || value == 0 || !decimalDigits(valueText) {
		return model.InputObservation{}, invalidOutput("读取输入", output, parseErr)
	}
	input := uint16(value)
	return model.InputObservation{Value: &input, ObservedAt: time.Now()}, nil
}

func (b *Backend) SetInput(ctx context.Context, device model.Device, scheme model.InputScheme) (model.WriteReceipt, error) {
	if err := scheme.Validate(); err != nil {
		return model.WriteReceipt{}, err
	}
	if err := b.validateDevice(ctx, device); err != nil {
		return model.WriteReceipt{}, err
	}
	field, err := inputField(scheme.Protocol)
	if err != nil {
		return model.WriteReceipt{}, err
	}
	value := strconv.FormatUint(uint64(scheme.Value), 10)
	output, err := b.run(ctx, "发送输入切换", "display", device.BackendID, "set", field, value)
	if err != nil {
		// 进程失败或被终止不能撤销已经送达显示器的DDC；也不能证明尚未发送。
		// 失败时不填发送时间，上层须保留原错误并按unknown处理不确定提交，禁止盲重试。
		return model.WriteReceipt{}, err
	}
	if strings.TrimSpace(output.Stdout) != "Writing "+value {
		return model.WriteReceipt{}, invalidOutput("发送输入切换，无法确认提交", output, nil)
	}
	// 上游Writing和零退出只提供传输提交证据，不能推出读回相同或画面已切换。
	// 时间记录于命令成功完成后；后续确认必须再取得不早于此时间的独立读回。
	return model.WriteReceipt{SentAt: time.Now()}, nil
}

func inputField(protocol model.InputProtocol) (string, error) {
	switch protocol {
	case model.ProtocolVCP:
		return "input", nil
	case model.ProtocolLGInputAlt:
		return "input-alt", nil
	default:
		return "", model.NewError(model.CodeUnsupportedProtocol, "m1ddc后端不支持该输入协议", nil)
	}
}

func (b *Backend) validateDevice(ctx context.Context, device model.Device) error {
	devices, err := b.Enumerate(ctx)
	if err != nil {
		return err
	}
	for _, current := range devices {
		if current == device {
			return nil
		}
	}
	return model.NewError(model.CodeNotFound, "m1ddc当前枚举中不存在原设备，请重新枚举和绑定；未发送输入命令", nil)
}

// 只依赖上游普通list头行，不将未保证稳定的详细字段解释成持久身份。
// 序号随插拔重排，因此选择时用本次系统UUID；系统UUID和Path仍只是当前候选，
// 不承诺跨端口、重启或系统更新保持不变。重新枚举与执行间仍可能热拔插，
// 上游找不到UUID时须报错，绝不能退回序号或主显示器。
var displayLine = regexp.MustCompile(`^\[([1-9][0-9]*)\] (.+) \(([0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12})\)$`)

func parseDisplays(output CommandOutput) ([]model.Device, error) {
	if strings.TrimSpace(output.Stdout) == "" {
		return nil, invalidOutput("枚举显示器", output, nil)
	}
	var devices []model.Device
	seenUUIDs, seenOrdinals := make(map[string]bool), make(map[uint64]bool)
	for _, line := range strings.Split(strings.TrimSpace(output.Stdout), "\n") {
		parts := displayLine.FindStringSubmatch(strings.TrimSpace(line))
		if parts == nil {
			return nil, invalidOutput("枚举显示器", output, nil)
		}
		ordinal, err := strconv.ParseUint(parts[1], 10, 64)
		if err != nil || seenOrdinals[ordinal] {
			return nil, invalidOutput("枚举显示器", output, err)
		}
		seenOrdinals[ordinal] = true
		uuid := strings.ToUpper(parts[3])
		if seenUUIDs[uuid] {
			return nil, model.NewError(model.CodeIdentityAmbiguous, "m1ddc枚举返回重复系统UUID，不能唯一选择显示器", nil)
		}
		seenUUIDs[uuid] = true
		devices = append(devices, model.Device{BackendID: "uuid=" + uuid, Name: parts[2], Identity: model.IdentityHints{Path: "m1ddc:uuid:" + uuid}})
	}
	return devices, nil
}

func decimalDigits(text string) bool {
	if text == "" {
		return false
	}
	for _, c := range text {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func invalidOutput(stage string, output CommandOutput, cause error) error {
	// 原始输出可能含本机路径或设备信息，只放进不序列化的根因，公共错误保留阶段。
	if cause == nil {
		cause = errors.New("命令退出成功但输出不符合已核对的m1ddc格式")
	}
	return model.NewError(model.CodeTransport, "m1ddc"+stage+"输出无效，不能据此确认操作结果", fmt.Errorf("stdout=%q stderr=%q: %w", output.Stdout, output.Stderr, cause))
}

func (b *Backend) run(ctx context.Context, stage string, args ...string) (CommandOutput, error) {
	if err := b.availability(ctx); err != nil {
		return CommandOutput{}, err
	}
	output, err := b.runner.Run(ctx, b.toolPath, args...)
	// 注入传输也需守住输出边界；真实执行器会在捕获过程中限量并终止过量进程。
	if len(output.Stdout) > MaxCommandOutputBytes || len(output.Stderr) > MaxCommandOutputBytes-len(output.Stdout) {
		return CommandOutput{}, model.NewError(model.CodeMessageTooLarge, "m1ddc"+stage+"输出超过上限", err)
	}
	if ctx.Err() != nil {
		err = errors.Join(ctx.Err(), err)
	}
	if err == nil {
		return output, nil
	}
	code := model.CodeOf(err)
	if code == model.CodeInternal {
		code = model.CodeTransport
	}
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, exec.ErrNotFound) {
		code = model.CodeBackendUnavailable
	} else if errors.Is(err, os.ErrPermission) {
		code = model.CodePermissionDenied
	} else if code == model.CodeTransport {
		text := strings.TrimSpace(output.Stdout)
		switch {
		case text == "Could not find a suitable external display.":
			code = model.CodeUnreachable
		case strings.HasPrefix(text, "The specified display does not exist."):
			code = model.CodeNotFound
		}
	}
	// m1ddc没有结构化DDC错误码；保留非零退出、原始输出和阶段，不据字符串
	// 猜测所有失败均为unsupported、权限不足或未发送，也不在此处重试或协议回退。
	cause := fmt.Errorf("m1ddc%s；stdout=%q；stderr=%q: %w", stage, output.Stdout, output.Stderr, err)
	return output, model.NewError(code, "m1ddc"+stage+"失败；命令执行错误不能证明输入切换未发送", cause)
}

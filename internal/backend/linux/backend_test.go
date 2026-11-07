package linux

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/orzlee/displayKvm/internal/model"
)

type commandCall struct {
	executable string
	args       []string
	ctx        context.Context
}

type fakeRunner struct {
	calls   []commandCall
	run     func(context.Context, string, ...string) (CommandOutput, error)
	version string
}

func (f *fakeRunner) Run(ctx context.Context, executable string, args ...string) (CommandOutput, error) {
	f.calls = append(f.calls, commandCall{executable: executable, args: append([]string(nil), args...), ctx: ctx})
	if f.run != nil {
		return f.run(ctx, executable, args...)
	}
	switch {
	case containsArg(args, "--version"):
		if f.version != "" {
			return CommandOutput{Stdout: f.version + "\n"}, nil
		}
		return CommandOutput{Stdout: "2.2.7\n"}, nil
	case containsArg(args, "detect"):
		if f.version == "3.0.2" {
			return CommandOutput{Stdout: displayFixture302}, nil
		}
		return CommandOutput{Stdout: displayFixture}, nil
	case containsArg(args, "getvcp"):
		return CommandOutput{Stdout: "VCP 60 SNC x11\n"}, nil
	case containsArg(args, "setvcp"):
		return CommandOutput{}, nil
	default:
		return CommandOutput{}, errors.New("未预期的命令")
	}
}

func containsArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}

func newTestBackend(t *testing.T, runner CommandRunner) *Backend {
	t.Helper()
	toolPath, err := filepath.Abs(filepath.Join("user supplied", "ddcutil;literal"))
	if err != nil {
		t.Fatal(err)
	}
	b := New(toolPath, runner)
	// 只替换后端的平台判断，让不同宿主覆盖真实Linux业务分支；执行器仍完全可注入。
	// 路径校验使用宿主规则，合法夹具须按宿主生成，并保留空格和分号验证参数原样传递。
	b.goos = "linux"
	return b
}

func fixtureDevice() model.Device {
	return model.Device{BackendID: "i2c-7", Name: "DELL P2411H", Identity: model.IdentityHints{Manufacturer: "DEL", Serial: "SERIAL7", Path: "/dev/i2c-7"}}
}

func fixtureScheme() model.InputScheme {
	return model.InputScheme{ID: "hdmi", Protocol: model.ProtocolVCP, VCPCode: 0x60, Value: 17}
}

func TestBackendPassesSeparateArgumentsAndSameRequestContext(t *testing.T) {
	for _, version := range []string{"2.2.7", "3.0.2"} {
		t.Run(version, func(t *testing.T) {
			runner := &fakeRunner{version: version}
			b := newTestBackend(t, runner)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if b.Name() != "linux" {
				t.Fatalf("后端名称错误：%q", b.Name())
			}
			observation, err := b.ReadInput(ctx, fixtureDevice(), fixtureScheme())
			if err != nil || observation.Value == nil || *observation.Value != 17 || observation.ObservedAt.IsZero() {
				t.Fatalf("真实读取路径须产生带时间的观测：%+v %v", observation, err)
			}
			receipt, err := b.SetInput(ctx, fixtureDevice(), fixtureScheme())
			if err != nil || receipt.SentAt.IsZero() || receipt.SentAt.Before(observation.ObservedAt) {
				t.Fatalf("零退出且无错误的写命令仅产生提交回执：%+v %v", receipt, err)
			}
			want := [][]string{
				{"--noconfig", "--disable-udf", "--terse", "--version"},
				{"--noconfig", "--disable-udf", "detect", "--brief"},
				{"--noconfig", "--disable-udf", "--bus", "7", "getvcp", "--terse", "60"},
				{"--noconfig", "--disable-udf", "--terse", "--version"},
				{"--noconfig", "--disable-udf", "detect", "--brief"},
				{"--noconfig", "--disable-udf", "--bus", "7", "setvcp", "60", "17", "--noverify"},
			}
			if len(runner.calls) != len(want) {
				t.Fatalf("禁止重试或隐式写后读回，calls=%+v", runner.calls)
			}
			for i, call := range runner.calls {
				if call.executable != b.toolPath || !reflect.DeepEqual(call.args, want[i]) || call.ctx != ctx {
					t.Fatalf("参数须分离、工具路径原样传递并共用原请求预算：call=%+v want=%v", call, want[i])
				}
			}
		})
	}
}

func TestBackendValidatesSchemeBeforeAnyCommand(t *testing.T) {
	tests := []struct {
		scheme model.InputScheme
		code   model.ErrorCode
	}{
		{model.InputScheme{}, model.CodeInvalidRequest},
		{model.InputScheme{ID: "bad", Protocol: model.ProtocolVCP, VCPCode: 0x10, Value: 17}, model.CodeInvalidRequest},
		{model.InputScheme{ID: "bad", Protocol: model.ProtocolVCP, VCPCode: 0x60}, model.CodeInvalidRequest},
		{model.InputScheme{ID: "alt", Protocol: model.ProtocolLGInputAlt, Value: 17}, model.CodeUnsupportedProtocol},
		{model.InputScheme{ID: "bad", Protocol: "unknown", Value: 17}, model.CodeUnsupportedProtocol},
	}
	for _, tt := range tests {
		runner := &fakeRunner{}
		b := newTestBackend(t, runner)
		_, readErr := b.ReadInput(context.Background(), fixtureDevice(), tt.scheme)
		_, writeErr := b.SetInput(context.Background(), fixtureDevice(), tt.scheme)
		if model.CodeOf(readErr) != tt.code || model.CodeOf(writeErr) != tt.code || len(runner.calls) != 0 {
			t.Fatalf("无效/未实现协议必须在I/O前拒绝：scheme=%+v read=%v write=%v calls=%+v", tt.scheme, readErr, writeErr, runner.calls)
		}
	}
}

func TestBackendRejectsStaleOrInjectedDeviceWithoutWrite(t *testing.T) {
	for _, changed := range []model.Device{
		{BackendID: "i2c-7;rm -rf /", Name: fixtureDevice().Name, Identity: fixtureDevice().Identity},
		{BackendID: "i2c-7", Name: fixtureDevice().Name, Identity: model.IdentityHints{Manufacturer: "DEL", Serial: "OTHER", Path: "/dev/i2c-7"}},
		{BackendID: "i2c-8", Name: fixtureDevice().Name, Identity: fixtureDevice().Identity},
	} {
		runner := &fakeRunner{}
		receipt, err := newTestBackend(t, runner).SetInput(context.Background(), changed, fixtureScheme())
		if model.CodeOf(err) != model.CodeNotFound || !receipt.SentAt.IsZero() {
			t.Fatalf("重新枚举不匹配时必须停止：%+v %v", receipt, err)
		}
		for _, call := range runner.calls {
			if containsArg(call.args, "setvcp") {
				t.Fatal("陈旧或伪造对象不得触发写入")
			}
		}
	}
}

func TestBackendMalformedEnumerationCannotLeadToWrite(t *testing.T) {
	runner := &fakeRunner{run: func(_ context.Context, _ string, args ...string) (CommandOutput, error) {
		if containsArg(args, "--version") {
			return CommandOutput{Stdout: "2.2.7\n"}, nil
		}
		return CommandOutput{Stdout: displayFixture + "unexpected\n"}, nil
	}}
	_, err := newTestBackend(t, runner).SetInput(context.Background(), fixtureDevice(), fixtureScheme())
	if model.CodeOf(err) != model.CodeTransport || len(runner.calls) != 2 {
		t.Fatalf("异常枚举必须在写入前失败：%v %+v", err, runner.calls)
	}
}

func TestBackendVersionAndAvailabilityFailBeforeDetect(t *testing.T) {
	for _, version := range []string{"3.0.1\n", "3.0.3\n", "2.2.7-dev\n", "3.0.2-dev\n", "ddcutil 2.2.7\n", "ddcutil 3.0.2\n", ""} {
		runner := &fakeRunner{run: func(context.Context, string, ...string) (CommandOutput, error) {
			return CommandOutput{Stdout: version}, nil
		}}
		_, err := newTestBackend(t, runner).Enumerate(context.Background())
		if model.CodeOf(err) != model.CodeUnsupportedVersion || len(runner.calls) != 1 {
			t.Fatalf("未核对版本不得继续硬件命令：%q %v %+v", version, err, runner.calls)
		}
	}
	for _, path := range []string{"", "ddcutil", "./ddcutil", "/usr/bin/ddcutil\x00"} {
		runner := &fakeRunner{}
		b := newTestBackend(t, runner)
		b.toolPath = path
		_, err := b.Enumerate(context.Background())
		if model.CodeOf(err) != model.CodeBackendUnavailable || len(runner.calls) != 0 {
			t.Fatalf("无效工具路径不能启动进程：%q %v", path, err)
		}
	}
	runner := &fakeRunner{}
	b := newTestBackend(t, runner)
	b.goos = "darwin"
	if _, err := b.Enumerate(context.Background()); model.CodeOf(err) != model.CodeBackendUnavailable || len(runner.calls) != 0 {
		t.Fatalf("真实平台检查不能虚报Linux可用：%v", err)
	}
}

func TestBackendErrorsDistinguishPermissionsMissingToolAndTransport(t *testing.T) {
	exitFailure := &exec.ExitError{}
	tests := []struct {
		name   string
		output CommandOutput
		err    error
		code   model.ErrorCode
	}{
		{"missing", CommandOutput{}, &os.PathError{Op: "fork/exec", Path: "/missing", Err: os.ErrNotExist}, model.CodeBackendUnavailable},
		{"executable_permission", CommandOutput{}, os.ErrPermission, model.CodePermissionDenied},
		{"device_permission_zero_exit", CommandOutput{Stdout: displayFixture, Stderr: "Device /dev/i2c-7 is not readable and writable.  Error = EACCES(13): Permission denied\n"}, nil, model.CodePermissionDenied},
		{"missing_driver", CommandOutput{Stderr: "Driver i2c_dev must be loaded or builtin\n"}, nil, model.CodeBackendUnavailable},
		{"no_i2c_adapters", CommandOutput{Stderr: "No /sys/bus/i2c buses that might be used for DDC/CI communication found.\n"}, nil, model.CodeBackendUnavailable},
		{"unreachable", CommandOutput{Stderr: "No monitor detected on bus /dev/i2c-7\n"}, exitFailure, model.CodeUnreachable},
		{"removed_bus", CommandOutput{Stderr: "Bus /dev/i2c-7 does not exist.\n"}, exitFailure, model.CodeUnreachable},
		{"unknown_zero_exit_stderr", CommandOutput{Stdout: displayFixture, Stderr: "unexpected diagnostic\n"}, nil, model.CodeTransport},
		{"unknown_nonzero", CommandOutput{Stderr: "unexpected diagnostic\n"}, exitFailure, model.CodeTransport},
		{"feature_err_not_proof_of_unsupported", CommandOutput{Stdout: "VCP 60 ERR\n"}, exitFailure, model.CodeTransport},
		{"typed_unsupported", CommandOutput{}, model.NewError(model.CodeUnsupportedOperation, "明确不支持读取", nil), model.CodeUnsupportedOperation},
		{"canceled", CommandOutput{}, context.Canceled, model.CodeCanceled},
		{"deadline", CommandOutput{}, context.DeadlineExceeded, model.CodeTimeout},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runner := &fakeRunner{run: func(context.Context, string, ...string) (CommandOutput, error) { return tt.output, tt.err }}
			_, err := newTestBackend(t, runner).Enumerate(context.Background())
			if model.CodeOf(err) != tt.code || len(runner.calls) != 1 {
				t.Fatalf("错误分类不匹配或进行了隐式重试：%v want=%s calls=%d", err, tt.code, len(runner.calls))
			}
			if tt.err != nil && !errors.Is(err, tt.err) {
				t.Fatalf("必须保留原始错误链：%v", err)
			}
		})
	}
}

func TestBackendFailedWriteHasNoReceiptAndNeverRetries(t *testing.T) {
	for _, version := range []string{"2.2.7", "3.0.2"} {
		t.Run(version, func(t *testing.T) {
			for _, failure := range []struct {
				output CommandOutput
				err    error
			}{
				{CommandOutput{}, errors.New("发送阶段失败")},
				{CommandOutput{Stderr: "unknown write diagnostic"}, nil},
				{CommandOutput{Stdout: "unexpected success text"}, nil},
				{CommandOutput{Stdout: "VCP 60 ERR\n"}, nil},
				{CommandOutput{}, context.Canceled},
				{CommandOutput{}, context.DeadlineExceeded},
			} {
				runner := &fakeRunner{}
				runner.run = func(ctx context.Context, path string, args ...string) (CommandOutput, error) {
					if containsArg(args, "setvcp") {
						return failure.output, failure.err
					}
					return (&fakeRunner{version: version}).Run(ctx, path, args...)
				}
				receipt, err := newTestBackend(t, runner).SetInput(context.Background(), fixtureDevice(), fixtureScheme())
				if err == nil || !receipt.SentAt.IsZero() || len(runner.calls) != 3 {
					t.Fatalf("失败/不确定提交不能伪造回执或重试：%+v %v %+v", receipt, err, runner.calls)
				}
			}
		})
	}
}

func TestBackendBothVersionsRejectUncertainReadAndCanceledWrite(t *testing.T) {
	// 模拟执行器仅证明命令与失败语义，不证明真实ddcutil版本响应或显示器链路。
	for _, version := range []string{"2.2.7", "3.0.2"} {
		t.Run(version, func(t *testing.T) {
			for _, output := range []CommandOutput{{Stdout: "VCP 60 ERR\n"}, {Stdout: "VCP 60 SNC x11\n", Stderr: "diagnostic\n"}} {
				runner := &fakeRunner{run: func(ctx context.Context, path string, args ...string) (CommandOutput, error) {
					if containsArg(args, "getvcp") {
						return output, nil
					}
					return (&fakeRunner{version: version}).Run(ctx, path, args...)
				}}
				observation, err := newTestBackend(t, runner).ReadInput(context.Background(), fixtureDevice(), fixtureScheme())
				if err == nil || observation.Value != nil || len(runner.calls) != 3 {
					t.Fatalf("ERR或诊断不得产生观测或重试：%+v %v", observation, err)
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			runner := &fakeRunner{run: func(ctx context.Context, path string, args ...string) (CommandOutput, error) {
				if containsArg(args, "setvcp") {
					cancel()
					return CommandOutput{}, nil
				}
				return (&fakeRunner{version: version}).Run(ctx, path, args...)
			}}
			receipt, err := newTestBackend(t, runner).SetInput(ctx, fixtureDevice(), fixtureScheme())
			if model.CodeOf(err) != model.CodeCanceled || !receipt.SentAt.IsZero() || len(runner.calls) != 3 {
				t.Fatalf("写入期间取消不能认定成功或回滚：%+v %v", receipt, err)
			}
		})
	}
}

func TestBackendCancellationAndSharedOutputBudget(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	runner := &fakeRunner{}
	if _, err := newTestBackend(t, runner).Enumerate(ctx); !errors.Is(err, context.Canceled) || len(runner.calls) != 0 {
		t.Fatalf("已取消请求不得启动命令：%v", err)
	}
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	runner = &fakeRunner{run: func(context.Context, string, ...string) (CommandOutput, error) {
		cancel()
		return CommandOutput{Stdout: strings.Repeat("x", MaxCommandOutputBytes+1)}, nil
	}}
	if _, err := newTestBackend(t, runner).Enumerate(ctx); model.CodeOf(err) != model.CodeCanceled {
		t.Fatalf("已取消语义优先于输出超限：%v", err)
	}
	runner = &fakeRunner{run: func(context.Context, string, ...string) (CommandOutput, error) {
		return CommandOutput{Stdout: strings.Repeat("x", MaxCommandOutputBytes/2), Stderr: strings.Repeat("y", MaxCommandOutputBytes/2+1)}, nil
	}}
	if _, err := newTestBackend(t, runner).Enumerate(context.Background()); model.CodeOf(err) != model.CodeMessageTooLarge {
		t.Fatalf("注入执行器也必须遵守stdout/stderr共享预算：%v", err)
	}
}

func TestBackendCapabilitiesDescribeAdapterWithoutInventingHardwareSupport(t *testing.T) {
	runner := &fakeRunner{}
	capabilities, err := newTestBackend(t, runner).Capabilities(context.Background(), fixtureDevice())
	if err != nil || !reflect.DeepEqual(capabilities.Protocols, []model.InputProtocol{model.ProtocolVCP}) || !capabilities.CanReadInput || capabilities.UnavailableReason != "" || len(runner.calls) != 2 {
		t.Fatalf("能力仅描述当前适配器提供的VCP操作：%+v %v %+v", capabilities, err, runner.calls)
	}
	device := fixtureDevice()
	device.Identity.Serial = "changed"
	capabilities, err = newTestBackend(t, &fakeRunner{}).Capabilities(context.Background(), device)
	if model.CodeOf(err) != model.CodeNotFound || capabilities.CanReadInput || len(capabilities.Protocols) != 0 || capabilities.UnavailableReason == "" {
		t.Fatalf("失效设备不得报告可用能力：%+v %v", capabilities, err)
	}
}

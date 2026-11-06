package darwin

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/orzlee/displayKvm/internal/backend"
	"github.com/orzlee/displayKvm/internal/model"
)

const (
	firstUUID  = "12345678-1234-1234-1234-123456789ABC"
	secondUUID = "ABCDEF12-5678-5678-5678-ABCDEF123456"
	firstList  = "[1] Screen (Desk) (" + firstUUID + ")\n"
)

type runnerFunc func(context.Context, string, ...string) (CommandOutput, error)

func (f runnerFunc) Run(ctx context.Context, path string, args ...string) (CommandOutput, error) {
	return f(ctx, path, args...)
}

func testBackend(runner CommandRunner) *Backend {
	b := New("/user supplied/m1ddc", runner)
	b.goos, b.goarch = "darwin", "arm64"
	return b
}

func testDevice() model.Device {
	return model.Device{BackendID: "uuid=" + firstUUID, Name: "Screen (Desk)", Identity: model.IdentityHints{Path: "m1ddc:uuid:" + firstUUID}}
}

func testScheme(protocol model.InputProtocol, value uint16) model.InputScheme {
	s := model.InputScheme{ID: "calibrated", Protocol: protocol, Value: value}
	if protocol == model.ProtocolVCP {
		s.VCPCode = 0x60
	}
	return s
}

func TestArchitectureAndExplicitPathBoundaries(t *testing.T) {
	var _ backend.Backend = (*Backend)(nil)
	for _, tc := range []struct{ name, goos, arch, path string }{
		{"Intel", "darwin", "amd64", "/user/m1ddc"},
		{"other architecture", "darwin", "386", "/user/m1ddc"},
		{"other OS", "linux", "arm64", "/user/m1ddc"},
		{"not configured", "darwin", "arm64", ""},
		{"PATH name", "darwin", "arm64", "m1ddc"},
		{"relative path", "darwin", "arm64", "./m1ddc"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			b := testBackend(runnerFunc(func(context.Context, string, ...string) (CommandOutput, error) {
				calls++
				return CommandOutput{}, nil
			}))
			b.goos, b.goarch, b.toolPath = tc.goos, tc.arch, tc.path
			_, enumErr := b.Enumerate(context.Background())
			caps, capErr := b.Capabilities(context.Background(), testDevice())
			_, readErr := b.ReadInput(context.Background(), testDevice(), testScheme(model.ProtocolVCP, 17))
			receipt, writeErr := b.SetInput(context.Background(), testDevice(), testScheme(model.ProtocolVCP, 17))
			for _, err := range []error{enumErr, capErr, readErr, writeErr} {
				if model.CodeOf(err) != model.CodeBackendUnavailable {
					t.Fatalf("架构/路径应明确不可用，得到%v", err)
				}
			}
			if calls != 0 || caps.CanReadInput || len(caps.Protocols) != 0 || caps.UnavailableReason == "" || !receipt.SentAt.IsZero() {
				t.Fatalf("拒绝路径不得执行命令或宣称能力：calls=%d caps=%+v receipt=%+v", calls, caps, receipt)
			}
		})
	}
	if New("", nil).Name() != "darwin" {
		t.Fatal("平台名称应为darwin")
	}
}

func TestEnumerateAndCapabilitiesUseCurrentUUIDs(t *testing.T) {
	calls := 0
	b := testBackend(runnerFunc(func(_ context.Context, path string, args ...string) (CommandOutput, error) {
		calls++
		if path != "/user supplied/m1ddc" || !reflect.DeepEqual(args, []string{"display", "list"}) {
			t.Fatalf("只允许显式路径和枚举参数，得到%q %q", path, args)
		}
		return CommandOutput{Stdout: firstList + "[2] Other (" + secondUUID + ")\n"}, nil
	}))
	devices, err := b.Enumerate(context.Background())
	if err != nil || len(devices) != 2 || devices[0] != testDevice() || devices[1].BackendID != "uuid="+secondUUID {
		t.Fatalf("枚举信息不符：%+v，%v", devices, err)
	}
	if devices[0].Identity.Serial != "" || devices[0].Identity.EDIDSHA256 != "" {
		t.Fatal("不能从系统UUID伪造序列号或EDID摘要")
	}
	caps, err := b.Capabilities(context.Background(), devices[0])
	if err != nil || !caps.CanReadInput || !reflect.DeepEqual(caps.Protocols, []model.InputProtocol{model.ProtocolVCP, model.ProtocolLGInputAlt}) || calls != 2 {
		t.Fatalf("能力必须核对当前设备和工具，得到%+v，%v，calls=%d", caps, err, calls)
	}
}

func TestEnumerationRejectsUnrecognizedAndAmbiguousOutput(t *testing.T) {
	for _, tc := range []struct {
		name, output string
		code         model.ErrorCode
	}{
		{"empty successful output", "", model.CodeTransport},
		{"unknown format", "display: Screen", model.CodeTransport},
		{"invalid UUID", "[1] Screen (not-a-uuid)\n", model.CodeTransport},
		{"extra unrecognized line", firstList + "unexpected\n", model.CodeTransport},
		{"duplicate UUID", firstList + "[2] Other (" + firstUUID + ")\n", model.CodeIdentityAmbiguous},
		{"duplicate ordinal", firstList + "[1] Other (" + secondUUID + ")\n", model.CodeTransport},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := testBackend(runnerFunc(func(context.Context, string, ...string) (CommandOutput, error) {
				return CommandOutput{Stdout: tc.output}, nil
			}))
			devices, err := b.Enumerate(context.Background())
			if model.CodeOf(err) != tc.code || len(devices) != 0 {
				t.Fatalf("不应返回部分或猜测设备：%+v，%v", devices, err)
			}
		})
	}
	underlying := errors.New("fixture exit 1")
	b := testBackend(runnerFunc(func(context.Context, string, ...string) (CommandOutput, error) {
		return CommandOutput{Stdout: "No external display found, aborting"}, underlying
	}))
	devices, err := b.Enumerate(context.Background())
	if err != nil || len(devices) != 0 {
		t.Fatalf("仅上游明确的无设备输出可转为空列表：%+v，%v", devices, err)
	}
}

func TestSchemesAreValidatedBeforeAnyDeviceIO(t *testing.T) {
	for _, tc := range []struct {
		scheme model.InputScheme
		code   model.ErrorCode
	}{
		{model.InputScheme{ID: "invalid", Protocol: "unknown", Value: 17}, model.CodeUnsupportedProtocol},
		{model.InputScheme{ID: "invalid", Protocol: model.ProtocolVCP, VCPCode: 0x10, Value: 17}, model.CodeInvalidRequest},
		{model.InputScheme{ID: "invalid", Protocol: model.ProtocolLGInputAlt, VCPCode: 0x60, Value: 144}, model.CodeInvalidRequest},
		{testScheme(model.ProtocolVCP, 0), model.CodeInvalidRequest},
		{model.InputScheme{Protocol: model.ProtocolVCP, VCPCode: 0x60, Value: 17}, model.CodeInvalidRequest},
	} {
		calls := 0
		b := testBackend(runnerFunc(func(context.Context, string, ...string) (CommandOutput, error) {
			calls++
			return CommandOutput{}, nil
		}))
		observation, readErr := b.ReadInput(context.Background(), testDevice(), tc.scheme)
		receipt, writeErr := b.SetInput(context.Background(), testDevice(), tc.scheme)
		if model.CodeOf(readErr) != tc.code || model.CodeOf(writeErr) != tc.code || calls != 0 || observation.Value != nil || !receipt.SentAt.IsZero() {
			t.Fatalf("方案校验应先于枚举或DDC命令：read=%v write=%v calls=%d", readErr, writeErr, calls)
		}
	}
}

func TestExplicitInputStrategiesAndSendEvidence(t *testing.T) {
	for _, tc := range []struct {
		protocol model.InputProtocol
		field    string
		value    uint16
	}{
		{model.ProtocolVCP, "input", 1},
		{model.ProtocolVCP, "input", 17},
		{model.ProtocolLGInputAlt, "input-alt", 144},
		{model.ProtocolLGInputAlt, "input-alt", 65535},
	} {
		t.Run(fmt.Sprintf("%s/%d", tc.protocol, tc.value), func(t *testing.T) {
			var commands [][]string
			b := testBackend(runnerFunc(func(_ context.Context, _ string, args ...string) (CommandOutput, error) {
				commands = append(commands, append([]string(nil), args...))
				if reflect.DeepEqual(args, []string{"display", "list"}) {
					// 枚举序号可变；命令仍须选择原UUID，不能按旧序号误切另一屏。
					return CommandOutput{Stdout: "[1] Other (" + secondUUID + ")\n[2] Screen (Desk) (" + firstUUID + ")\n"}, nil
				}
				if len(args) == 4 && args[2] == "get" {
					return CommandOutput{Stdout: fmt.Sprintf("%d\n", tc.value)}, nil
				}
				return CommandOutput{Stdout: fmt.Sprintf("Writing %d\n", tc.value)}, nil
			}))
			started := time.Now()
			observation, err := b.ReadInput(context.Background(), testDevice(), testScheme(tc.protocol, tc.value))
			if err != nil || observation.Value == nil || *observation.Value != tc.value || observation.ObservedAt.Before(started) {
				t.Fatalf("读取必须来自本次命令：%+v，%v", observation, err)
			}
			receipt, err := b.SetInput(context.Background(), testDevice(), testScheme(tc.protocol, tc.value))
			if err != nil || receipt.SentAt.IsZero() || receipt.SentAt.Before(started) {
				t.Fatalf("成功发送须有非零回执：%+v，%v", receipt, err)
			}
			want := [][]string{{"display", "list"}, {"display", "uuid=" + firstUUID, "get", tc.field}, {"display", "list"}, {"display", "uuid=" + firstUUID, "set", tc.field, fmt.Sprint(tc.value)}}
			if !reflect.DeepEqual(commands, want) {
				t.Fatalf("协议参数应保持原值且无回退/重试：got=%q want=%q", commands, want)
			}
		})
	}
}

func TestStaleOrAlteredDevicesNeverWrite(t *testing.T) {
	for _, tc := range []struct {
		name, list string
		device     model.Device
	}{
		{"removed UUID", "[1] Other (" + secondUUID + ")\n", testDevice()},
		{"changed device description", "[1] Replaced (" + firstUUID + ")\n", testDevice()},
		{"forged backend selector", firstList, model.Device{BackendID: "1"}},
		{"unbound current UUID", firstList, model.Device{BackendID: "uuid=" + firstUUID}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			b := testBackend(runnerFunc(func(_ context.Context, _ string, args ...string) (CommandOutput, error) {
				calls++
				if !reflect.DeepEqual(args, []string{"display", "list"}) {
					t.Fatalf("失效设备不能发送DDC：%q", args)
				}
				return CommandOutput{Stdout: tc.list}, nil
			}))
			receipt, err := b.SetInput(context.Background(), tc.device, testScheme(model.ProtocolVCP, 17))
			if model.CodeOf(err) != model.CodeNotFound || !receipt.SentAt.IsZero() || calls > 1 {
				t.Fatalf("失效身份应明确拒绝：%+v，%v，calls=%d", receipt, err, calls)
			}
		})
	}
}

func TestCommandFailuresKeepStageCauseAndNeverBecomeSuccess(t *testing.T) {
	underlying := errors.New("fixture command failed")
	for _, tc := range []struct {
		name, stdout string
		err          error
		code         model.ErrorCode
	}{
		{"missing tool", "", os.ErrNotExist, model.CodeBackendUnavailable},
		{"execution permission", "", os.ErrPermission, model.CodePermissionDenied},
		{"DDC failed", "DDC communication failure: fixture I/O error\n", underlying, model.CodeTransport},
		{"no transport", "Could not find a suitable external display.\n", underlying, model.CodeUnreachable},
		{"removed during command", "The specified display does not exist. Use 'display list' to list displays\n", underlying, model.CodeNotFound},
		{"canceled", "", context.Canceled, model.CodeCanceled},
		{"timeout", "", context.DeadlineExceeded, model.CodeTimeout},
	} {
		for _, operation := range []string{"enumerate", "read", "set"} {
			t.Run(tc.name+"/"+operation, func(t *testing.T) {
				calls := 0
				b := testBackend(runnerFunc(func(_ context.Context, _ string, _ ...string) (CommandOutput, error) {
					calls++
					if operation != "enumerate" && calls == 1 {
						return CommandOutput{Stdout: firstList}, nil
					}
					return CommandOutput{Stdout: tc.stdout, Stderr: "fixture stderr"}, tc.err
				}))
				var err error
				switch operation {
				case "enumerate":
					_, err = b.Enumerate(context.Background())
				case "read":
					var observation model.InputObservation
					observation, err = b.ReadInput(context.Background(), testDevice(), testScheme(model.ProtocolVCP, 17))
					if observation.Value != nil || !observation.ObservedAt.IsZero() {
						t.Fatalf("读取失败不能制造观测：%+v", observation)
					}
				case "set":
					var receipt model.WriteReceipt
					receipt, err = b.SetInput(context.Background(), testDevice(), testScheme(model.ProtocolLGInputAlt, 144))
					if !receipt.SentAt.IsZero() {
						t.Fatal("失败回执不能填发送时间")
					}
				}
				if model.CodeOf(err) != tc.code || !errors.Is(err, tc.err) || !strings.Contains(err.Error(), "m1ddc") {
					t.Fatalf("应保留阶段、类别和根因：%v", err)
				}
				if calls > 2 {
					t.Fatalf("失败不能自动回退或重试：calls=%d", calls)
				}
				if tc.stdout != "" && !strings.Contains(errors.Unwrap(err).Error(), strings.TrimSpace(tc.stdout)) {
					t.Fatalf("进程内根因应保留输出：%v", errors.Unwrap(err))
				}
			})
		}
	}
}

func TestMalformedSuccessfulReadOrWriteIsNotEvidence(t *testing.T) {
	for _, output := range []string{"", "0\n", "65536\n", "-1\n", "+17\n", "0x11\n", "17\n18\n", "DDC communication failure: fixture\n"} {
		b := testBackend(runnerFunc(func(_ context.Context, _ string, args ...string) (CommandOutput, error) {
			if len(args) == 2 {
				return CommandOutput{Stdout: firstList}, nil
			}
			return CommandOutput{Stdout: output}, nil
		}))
		observation, err := b.ReadInput(context.Background(), testDevice(), testScheme(model.ProtocolVCP, 17))
		if model.CodeOf(err) != model.CodeTransport || observation.Value != nil || !observation.ObservedAt.IsZero() {
			t.Fatalf("非法读回%q不能作为观测：%+v，%v", output, observation, err)
		}
	}
	for _, output := range []string{"", "Writing 18\n", "unrecognized success\n", "DDC communication failure: fixture\n"} {
		calls := 0
		b := testBackend(runnerFunc(func(_ context.Context, _ string, args ...string) (CommandOutput, error) {
			calls++
			if len(args) == 2 {
				return CommandOutput{Stdout: firstList}, nil
			}
			return CommandOutput{Stdout: output}, nil
		}))
		receipt, err := b.SetInput(context.Background(), testDevice(), testScheme(model.ProtocolVCP, 17))
		if model.CodeOf(err) != model.CodeTransport || !receipt.SentAt.IsZero() || calls != 2 {
			t.Fatalf("仅exit0不足以作为发送证据：output=%q receipt=%+v err=%v calls=%d", output, receipt, err, calls)
		}
	}
}

func TestCanceledContextDoesNotStartIO(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	b := testBackend(runnerFunc(func(context.Context, string, ...string) (CommandOutput, error) {
		t.Fatal("已取消请求不得启动命令")
		return CommandOutput{}, nil
	}))
	_, enumErr := b.Enumerate(ctx)
	_, capErr := b.Capabilities(ctx, testDevice())
	_, readErr := b.ReadInput(ctx, testDevice(), testScheme(model.ProtocolVCP, 17))
	_, writeErr := b.SetInput(ctx, testDevice(), testScheme(model.ProtocolVCP, 17))
	for _, err := range []error{enumErr, capErr, readErr, writeErr} {
		if !errors.Is(err, context.Canceled) || model.CodeOf(err) != model.CodeCanceled {
			t.Fatalf("必须保留取消类别：%v", err)
		}
	}
}

func TestWriteCancellationWaitsForRunnerCleanup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started, cleaned := make(chan struct{}), make(chan struct{})
	b := testBackend(runnerFunc(func(ctx context.Context, _ string, args ...string) (CommandOutput, error) {
		if len(args) == 2 {
			return CommandOutput{Stdout: firstList}, nil
		}
		close(started)
		<-ctx.Done()
		defer close(cleaned)
		return CommandOutput{Stdout: "Writing 17\n"}, ctx.Err()
	}))
	type result struct {
		receipt model.WriteReceipt
		err     error
	}
	done := make(chan result, 1)
	go func() {
		receipt, err := b.SetInput(ctx, testDevice(), testScheme(model.ProtocolVCP, 17))
		done <- result{receipt, err}
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("注入写命令未启动")
	}
	cancel()
	select {
	case got := <-done:
		if !errors.Is(got.err, context.Canceled) || !got.receipt.SentAt.IsZero() {
			t.Fatalf("取消可能发生在发送之后，不能返回发送证据：%+v", got)
		}
		select {
		case <-cleaned:
		default:
			t.Fatal("不得在Runner清理前返回")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("取消后未完成资源清理")
	}
}

func TestInjectedOutputCannotBypassLimit(t *testing.T) {
	b := testBackend(runnerFunc(func(context.Context, string, ...string) (CommandOutput, error) {
		return CommandOutput{Stdout: strings.Repeat("x", MaxCommandOutputBytes), Stderr: "x"}, nil
	}))
	if devices, err := b.Enumerate(context.Background()); model.CodeOf(err) != model.CodeMessageTooLarge || len(devices) != 0 {
		t.Fatalf("注入传输也须遵守输出上限：%+v，%v", devices, err)
	}
}

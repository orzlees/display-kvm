package switcher

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/orzlee/displayKvm/internal/model"
)

// 只替换物理读写，配置解析、身份解析、队列及预览恢复流程均使用生产实现。
// 值存放在独立的模拟设备状态中，读回不能直接由请求中的目标值构造。
func previewBackend() *fakeBackend {
	f := &fakeBackend{}
	values := map[string]uint16{"/left": 18, "/right": 23}
	f.readHook = func(_ context.Context, device model.Device, _ model.InputScheme, _ int) (model.InputObservation, error) {
		f.mu.Lock()
		value := values[device.Identity.Path]
		f.mu.Unlock()
		return model.InputObservation{Value: &value, ObservedAt: time.Now()}, nil
	}
	f.writeHook = func(_ context.Context, device model.Device, scheme model.InputScheme) (model.WriteReceipt, error) {
		f.mu.Lock()
		values[device.Identity.Path] = scheme.Value
		f.mu.Unlock()
		return model.WriteReceipt{SentAt: time.Now()}, nil
	}
	return f
}

func TestPreviewStrictPreReadFailureNeverWrites(t *testing.T) {
	for _, mode := range []string{"read_error", "value_with_error", "missing_value", "missing_time", "stale_time", "no_read_capability", "identity_lost"} {
		t.Run(mode, func(t *testing.T) {
			f := previewBackend()
			switch mode {
			case "read_error":
				f.readHook = func(context.Context, model.Device, model.InputScheme, int) (model.InputObservation, error) {
					return model.InputObservation{}, model.NewError(model.CodeUnreachable, "预读通路失联", nil)
				}
			case "missing_value":
				f.readHook = func(context.Context, model.Device, model.InputScheme, int) (model.InputObservation, error) {
					return model.InputObservation{ObservedAt: time.Now()}, nil
				}
			case "value_with_error":
				f.readHook = func(context.Context, model.Device, model.InputScheme, int) (model.InputObservation, error) {
					value := uint16(18)
					return model.InputObservation{Value: &value, ObservedAt: time.Now()}, model.NewError(model.CodeTransport, "预读清理失败，不能采信", nil)
				}
			case "missing_time":
				f.readHook = func(context.Context, model.Device, model.InputScheme, int) (model.InputObservation, error) {
					value := uint16(18)
					return model.InputObservation{Value: &value}, nil
				}
			case "no_read_capability":
				f.capsHook = func(model.Device) (model.DeviceCapabilities, error) {
					return model.DeviceCapabilities{Protocols: []model.InputProtocol{model.ProtocolVCP}}, nil
				}
			case "stale_time":
				f.readHook = func(context.Context, model.Device, model.InputScheme, int) (model.InputObservation, error) {
					value := uint16(18)
					return model.InputObservation{Value: &value, ObservedAt: time.Now().Add(-time.Hour)}, nil
				}
			case "identity_lost":
				f.enumHook = func(context.Context, int) error { return model.NewError(model.CodeNotFound, "设备已断开", nil) }
			}
			q := newTestQueue(t, f, switchConfig())
			status, err := q.Preview(context.Background(), onlyLeft("strict-pre-read"), nil)
			if err != nil || status.State != "failed" || len(status.Displays) != 1 || status.Displays[0].SwitchStatus != "failed" || status.Displays[0].Detail == "" || writeCount(f) != 0 {
				t.Fatalf("没有可靠原输入时不得写入: %+v err=%v writes=%d", status, err, writeCount(f))
			}
		})
	}
}

func TestPreviewPlanReportsSavedRowsWithoutHardwareAndRejectsMissingMappings(t *testing.T) {
	f := previewBackend()
	cfg := switchConfig()
	cfg.Targets[0].Inputs = cfg.Targets[0].Inputs[:1]
	q := newTestQueue(t, f, cfg)
	status, err := q.PlanPreview(onlyLeft("queued-plan"))
	if err != nil || status.State != "queued" || status.Phase != "queued" || len(status.Displays) != 1 || status.Displays[0].DisplayID != "left" || status.Displays[0].TargetInput != 17 || status.Displays[0].SwitchStatus != "pending" || status.Displays[0].RestoreStatus != "pending" || status.Displays[0].OriginalInput != nil {
		t.Fatalf("排队快照丢失保存映射或伪造原输入: %+v %v", status, err)
	}
	if _, err := q.PlanPreview(model.SwitchRequest{ID: "missing-mapping-preview", TargetID: "work", GroupID: "desk"}); model.CodeOf(err) != model.CodeInvalidRequest {
		t.Fatalf("缺少映射的分组未在受理前拒绝: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.generation != 0 || len(f.reads) != 0 || len(f.writes) != 0 {
		t.Fatal("仅准备排队快照就访问了硬件")
	}
}

func TestPreviewCancellationAtWorkerStartNeverWritesOrLeavesPendingRows(t *testing.T) {
	f := previewBackend()
	q := newTestQueue(t, f, switchConfig())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	status, err := q.Preview(ctx, model.SwitchRequest{ID: "cancel-at-worker-start", TargetID: "work"}, func(status model.InputPreviewStatus) {
		if status.Phase == "pre_read" {
			cancel()
		}
	})
	if err != nil || status.State != "cancelled" || status.Phase != "finished" || writeCount(f) != 0 || status.Displays[0].RestoreStatus != "not_needed" || status.Displays[1].SwitchStatus != "skipped" || status.Displays[1].RestoreStatus != "not_needed" || status.Displays[1].Detail == "" {
		t.Fatalf("开始执行时取消仍写入或遗留pending行: %+v %v", status, err)
	}
}

func TestPreviewUnknownForwardStaysFailedAfterVerifiedRecovery(t *testing.T) {
	f := previewBackend()
	write := f.writeHook
	calls := 0
	f.writeHook = func(ctx context.Context, device model.Device, scheme model.InputScheme) (model.WriteReceipt, error) {
		calls++
		receipt, err := write(ctx, device, scheme)
		if calls == 1 {
			return model.WriteReceipt{}, model.NewError(model.CodeTransport, "切走已生效但发送回执丢失", nil)
		}
		return receipt, err
	}
	q := newTestQueue(t, f, switchConfig())
	status, err := q.Preview(context.Background(), model.SwitchRequest{ID: "unknown-without-cancel", TargetID: "work"}, nil)
	if err != nil || status.State != "failed" || status.Displays[0].SwitchStatus != "unknown" || status.Displays[0].RestoreStatus != "restored" || status.Displays[1].SwitchStatus != "skipped" || status.Displays[1].RestoreStatus != "not_needed" || writeCount(f) != 2 {
		t.Fatalf("恢复成功掩盖未知切出或继续切换后续屏: %+v %v", status, err)
	}
}

func TestPreviewVerifiedForwardRecoveryFailureStopsRemainingDisplays(t *testing.T) {
	for _, mode := range []string{"third_value", "unreadable", "stale_read", "restore_write_error"} {
		mode := mode
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			f := previewBackend()
			read, write := f.readHook, f.writeHook
			f.readHook = func(ctx context.Context, device model.Device, scheme model.InputScheme, call int) (model.InputObservation, error) {
				if call >= 3 {
					switch mode {
					case "third_value":
						value := uint16(25)
						return model.InputObservation{Value: &value, ObservedAt: time.Now()}, nil
					case "unreadable":
						return model.InputObservation{}, model.NewError(model.CodeUnreachable, "恢复前通路失联", nil)
					case "stale_read":
						value := uint16(17)
						return model.InputObservation{Value: &value, ObservedAt: time.Now().Add(-time.Hour)}, nil
					}
				}
				return read(ctx, device, scheme, call)
			}
			f.writeHook = func(ctx context.Context, device model.Device, scheme model.InputScheme) (model.WriteReceipt, error) {
				if mode == "restore_write_error" && scheme.Value == 18 {
					return model.WriteReceipt{}, model.NewError(model.CodeTransport, "恢复发送结果未知", nil)
				}
				return write(ctx, device, scheme)
			}
			q := newTestQueue(t, f, switchConfig())
			status, err := q.Preview(context.Background(), model.SwitchRequest{ID: "restore-failed", TargetID: "work"}, nil)
			wantRestore, wantWrites := "failed", 1
			if mode == "third_value" {
				wantRestore = "conflict"
			} else if mode == "restore_write_error" {
				wantRestore, wantWrites = "unknown", 2
			}
			if err != nil || status.State != "failed" || status.Displays[0].SwitchStatus != "verified" || status.Displays[0].RestoreStatus != wantRestore || status.Displays[0].Detail == "" || status.Displays[1].SwitchStatus != "skipped" || status.Displays[1].RestoreStatus != "not_needed" || status.Displays[1].Detail == "" || writeCount(f) != wantWrites {
				t.Fatalf("恢复失败被当作成功、猜测写入或继续下一屏: %+v err=%v writes=%d", status, err, writeCount(f))
			}
		})
	}
}

func TestPreviewSameInputSkips(t *testing.T) {
	f := previewBackend()
	cfg := switchConfig()
	target := cfg.InputSchemes[0].Value
	f.readHook = func(context.Context, model.Device, model.InputScheme, int) (model.InputObservation, error) {
		value := target
		return model.InputObservation{Value: &value, ObservedAt: time.Now()}, nil
	}
	q := newTestQueue(t, f, cfg)
	status, err := q.Preview(context.Background(), onlyLeft("already-target"), nil)
	if err != nil || status.State != "completed" || writeCount(f) != 0 || status.Displays[0].SwitchStatus != "skipped" || status.Displays[0].RestoreStatus != "not_needed" || status.Displays[0].OriginalInput == nil || *status.Displays[0].OriginalInput != target {
		t.Fatalf("原输入未保留，或相同输入仍被写入: %+v %v", status, err)
	}
}

func TestPreviewStatusCloneAndJSONKeepZeroObservation(t *testing.T) {
	// 观测值0与缺失观测不同，但保存目标仍须合法；独立状态测试避免将0伪造为目标配置。
	zero := uint16(0)
	status := model.InputPreviewStatus{
		State: "running", Phase: "pre_read", TargetID: "work", StartedAt: time.Now(),
		Displays: []model.InputPreviewDisplayResult{{DisplayID: "left", OriginalInput: &zero, TargetInput: 17, ObservedInput: &zero, SwitchStatus: "pending", RestoreStatus: "pending"}},
	}
	cloned := status.Clone()
	zero = 18
	if cloned.Displays[0].OriginalInput == nil || *cloned.Displays[0].OriginalInput != 0 || cloned.Displays[0].ObservedInput == nil || *cloned.Displays[0].ObservedInput != 0 {
		t.Fatalf("状态复制丢失零观测或共享原观测指针: %+v", cloned)
	}
	data, err := json.Marshal(cloned)
	if err != nil {
		t.Fatal(err)
	}
	var decoded model.InputPreviewStatus
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Displays) != 1 || decoded.Displays[0].TargetInput != 17 || decoded.Displays[0].OriginalInput == nil || *decoded.Displays[0].OriginalInput != 0 || decoded.Displays[0].ObservedInput == nil || *decoded.Displays[0].ObservedInput != 0 {
		t.Fatalf("序列化丢失有效零观测或合法目标值: %s", data)
	}
}

func TestPreviewFullRoundTripsHoldFiveSecondsFromWriteReturnAndExcludeOtherJobs(t *testing.T) {
	f := previewBackend()
	read := f.readHook
	f.readHook = func(ctx context.Context, device model.Device, scheme model.InputScheme, call int) (model.InputObservation, error) {
		if device.Identity.Path == "/left" && call == 2 {
			// 验证读取消耗两秒；保持截止点仍须来自写返回，不能在读回后再等五秒。
			timer := time.NewTimer(2 * time.Second)
			defer timer.Stop()
			select {
			case <-timer.C:
			case <-ctx.Done():
				return model.InputObservation{}, ctx.Err()
			}
		}
		return read(ctx, device, scheme, call)
	}
	var mu sync.Mutex
	times := make(map[string][]time.Time)
	write := f.writeHook
	f.writeHook = func(ctx context.Context, device model.Device, scheme model.InputScheme) (model.WriteReceipt, error) {
		receipt, err := write(ctx, device, scheme)
		mu.Lock()
		times[device.Identity.Path] = append(times[device.Identity.Path], time.Now())
		mu.Unlock()
		return receipt, err
	}
	q := newTestQueue(t, f, switchConfig())
	holding := make(chan model.InputPreviewStatus, 1)
	finished := make(chan model.InputPreviewStatus, 1)
	go func() {
		status, _ := q.Preview(context.Background(), model.SwitchRequest{ID: "two-screen-preview", TargetID: "work", TimeoutMillis: 3000}, func(status model.InputPreviewStatus) {
			if status.Phase == "holding" && status.ActiveDisplayID == "left" {
				holding <- status
			}
		})
		finished <- status
	}()
	var first model.InputPreviewStatus
	select {
	case first = <-holding:
	case <-time.After(4 * time.Second):
		t.Fatal("未进入实际读回后的保持阶段")
	}
	mu.Lock()
	returned := times["/left"][0]
	mu.Unlock()
	if first.HoldUntil == nil || first.HoldUntil.Sub(returned) < 4900*time.Millisecond || first.HoldUntil.Sub(returned) > 5100*time.Millisecond {
		t.Fatalf("保持截止时间不是目标写返回后五秒: %+v returned=%v", first, returned)
	}
	other := make(chan managementlessSwitchReply, 1)
	go func() {
		result, err := q.Switch(context.Background(), onlyLeft("after-preview"))
		other <- managementlessSwitchReply{result, err}
	}()
	awaitQueued(t, q, 1)
	var status model.InputPreviewStatus
	select {
	case status = <-finished:
	case <-time.After(13 * time.Second):
		t.Fatal("多屏完整往返没有结束")
	}
	if status.State != "completed" || len(status.Displays) != 2 {
		t.Fatalf("完整往返缺少终态证据: %+v", status)
	}
	for _, display := range status.Displays {
		if display.SwitchStatus != "verified" || display.RestoreStatus != "restored" {
			t.Fatalf("没有验证完整往返: %+v", display)
		}
	}
	if reply := await(t, other); reply.err != nil || reply.result.Displays[0].State != model.StateConfirmed {
		t.Fatalf("排在预览后的普通切换失败: %+v", reply)
	}
	f.mu.Lock()
	var sequence []string
	for _, entry := range f.writes {
		sequence = append(sequence, fmt.Sprintf("%s:%d", entry.device.Identity.Path, entry.scheme.Value))
	}
	concurrent, stale := f.concurrent, f.stale
	f.mu.Unlock()
	if !reflect.DeepEqual(sequence, []string{"/left:17", "/left:18", "/right:22", "/right:23", "/left:17"}) || concurrent || stale {
		t.Fatalf("多屏未逐屏往返或队列发生穿插/旧引用: %v concurrent=%v stale=%v", sequence, concurrent, stale)
	}
	mu.Lock()
	leftDuration := times["/left"][1].Sub(times["/left"][0])
	rightDuration := times["/right"][1].Sub(times["/right"][0])
	mu.Unlock()
	if leftDuration < 4900*time.Millisecond || leftDuration > 6*time.Second || rightDuration < 4900*time.Millisecond {
		t.Fatalf("保持期错误或在读回后重复增加五秒: left=%v right=%v", leftDuration, rightDuration)
	}
}

func TestPreviewSlowVerificationUsesHoldDeadlineBeforeFreshRecovery(t *testing.T) {
	f := previewBackend()
	read, write := f.readHook, f.writeHook
	var verificationDeadline, returned, restored time.Time
	f.readHook = func(ctx context.Context, device model.Device, scheme model.InputScheme, call int) (model.InputObservation, error) {
		if call == 2 {
			verificationDeadline, _ = ctx.Deadline()
			<-ctx.Done()
			return model.InputObservation{}, ctx.Err()
		}
		return read(ctx, device, scheme, call)
	}
	f.writeHook = func(ctx context.Context, device model.Device, scheme model.InputScheme) (model.WriteReceipt, error) {
		receipt, err := write(ctx, device, scheme)
		if scheme.Value == 17 {
			returned = time.Now()
		} else {
			restored = time.Now()
		}
		return receipt, err
	}
	q := newTestQueue(t, f, switchConfig())
	status, err := q.Preview(context.Background(), onlyLeft("slow-verification"), nil)
	if err != nil || status.State != "failed" || status.Displays[0].SwitchStatus != "unknown" || status.Displays[0].RestoreStatus != "restored" || verificationDeadline.Sub(returned) < 4900*time.Millisecond || verificationDeadline.Sub(returned) > 5100*time.Millisecond || restored.Sub(returned) > 6*time.Second || writeCount(f) != 2 {
		t.Fatalf("慢验证读挤占恢复或伪造切出成功: %+v err=%v deadline=%v duration=%v", status, err, verificationDeadline.Sub(returned), restored.Sub(returned))
	}
}

type managementlessSwitchReply struct {
	result model.SwitchResult
	err    error
}

func TestPreviewCancellationRestoresCurrentScreenAndCopiesProgress(t *testing.T) {
	f := previewBackend()
	q := newTestQueue(t, f, switchConfig())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	status, err := q.Preview(ctx, model.SwitchRequest{ID: "cancel-one-screen", TargetID: "work"}, func(snapshot model.InputPreviewStatus) {
		if snapshot.Phase == "holding" {
			*snapshot.Displays[0].OriginalInput = 999
			*snapshot.Displays[0].ObservedInput = 888
			*snapshot.HoldUntil = time.Time{}
			snapshot.Displays[0].TargetInput = 777
			cancel()
		}
	})
	if err != nil || status.State != "cancelled" || len(status.Displays) != 2 || status.Displays[0].RestoreStatus != "restored" || *status.Displays[0].OriginalInput != 18 || status.Displays[0].TargetInput != 17 || status.Displays[1].SwitchStatus != "skipped" || status.Displays[1].RestoreStatus != "not_needed" || status.Displays[1].Detail == "" || writeCount(f) != 2 {
		t.Fatalf("取消丢失恢复责任、启动后续屏或回调污染内部状态: %+v %v", status, err)
	}
}

func TestPreviewUnknownWriteUsesFreshObservationWithoutRetry(t *testing.T) {
	for _, mode := range []string{"target", "original", "third_value", "unreadable", "restore_error", "restore_mismatch"} {
		t.Run(mode, func(t *testing.T) {
			f := previewBackend()
			read, write := f.readHook, f.writeHook
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			f.writeHook = func(ctx context.Context, device model.Device, scheme model.InputScheme) (model.WriteReceipt, error) {
				calls++
				if calls == 1 {
					if mode != "original" {
						_, _ = write(ctx, device, scheme)
					}
					cancel()
					return model.WriteReceipt{}, model.NewError(model.CodeTransport, "发送回执丢失，可能已生效", nil)
				}
				if mode == "restore_error" {
					return model.WriteReceipt{}, model.NewError(model.CodeUnreachable, "恢复写通路失联", nil)
				}
				if mode == "restore_mismatch" {
					return model.WriteReceipt{SentAt: time.Now()}, nil
				}
				return write(ctx, device, scheme)
			}
			f.readHook = func(ctx context.Context, device model.Device, scheme model.InputScheme, call int) (model.InputObservation, error) {
				if call >= 2 && mode == "unreadable" {
					return model.InputObservation{}, model.NewError(model.CodeUnreachable, "恢复前读失败", nil)
				}
				if call >= 2 && mode == "third_value" {
					value := uint16(25)
					return model.InputObservation{Value: &value, ObservedAt: time.Now()}, nil
				}
				return read(ctx, device, scheme, call)
			}
			q := newTestQueue(t, f, switchConfig())
			status, err := q.Preview(ctx, model.SwitchRequest{ID: "unknown-write", TargetID: "work"}, nil)
			wantRestore, wantWrites := "restored", 2
			switch mode {
			case "original":
				wantRestore, wantWrites = "not_needed", 1
			case "third_value":
				wantRestore, wantWrites = "conflict", 1
			case "unreadable":
				wantRestore, wantWrites = "failed", 1
			case "restore_error", "restore_mismatch":
				wantRestore = "unknown"
			}
			if err != nil || status.State == "completed" || status.Displays[0].SwitchStatus != "unknown" || status.Displays[0].RestoreStatus != wantRestore || status.Displays[0].Detail == "" || status.Displays[1].SwitchStatus != "skipped" || status.Displays[1].RestoreStatus != "not_needed" || writeCount(f) != wantWrites {
				t.Fatalf("未知写被重试、猜测恢复或伪报成功: %+v err=%v writes=%d", status, err, writeCount(f))
			}
		})
	}
}

func TestPreviewCloseWaitsForIndependentRecovery(t *testing.T) {
	f := previewBackend()
	write := f.writeHook
	restoreEntered, release := make(chan context.Context, 1), make(chan struct{})
	f.writeHook = func(ctx context.Context, device model.Device, scheme model.InputScheme) (model.WriteReceipt, error) {
		if scheme.Value == 18 {
			restoreEntered <- ctx
			<-release
		}
		return write(ctx, device, scheme)
	}
	q := newTestQueue(t, f, switchConfig())
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	holding, result := make(chan struct{}, 1), make(chan model.InputPreviewStatus, 1)
	go func() {
		status, _ := q.Preview(context.Background(), onlyLeft("close-during-preview"), func(status model.InputPreviewStatus) {
			if status.Phase == "holding" {
				holding <- struct{}{}
			}
		})
		result <- status
	}()
	await(t, holding)
	closed := make(chan error, 1)
	go func() { closed <- q.Close() }()
	recovery := await(t, restoreEntered)
	if _, bounded := recovery.Deadline(); !bounded || recovery.Err() != nil {
		t.Fatal("恢复继承了已取消context，或没有有限预算")
	}
	select {
	case <-closed:
		t.Fatal("原生恢复尚未返回，Close却已经返回")
	case <-result:
		t.Fatal("实际恢复完成前Preview已经发布终态")
	default:
	}
	close(release)
	if err := await(t, closed); err != nil {
		t.Fatal(err)
	}
	if status := await(t, result); status.State != "cancelled" || status.Displays[0].RestoreStatus != "restored" || writeCount(f) != 2 {
		t.Fatalf("关闭没有收口恢复: %+v", status)
	}
}

func TestPreviewCancelledBeforeAndAfterEnqueueNeverWrites(t *testing.T) {
	f := previewBackend()
	q := newTestQueue(t, f, switchConfig())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := q.Preview(ctx, onlyLeft("cancel-before-enqueue"), nil); model.CodeOf(err) != model.CodeCanceled || writeCount(f) != 0 {
		t.Fatalf("入队前取消仍受理: %v", err)
	}
	entered, release := make(chan struct{}, 1), make(chan struct{})
	f.enumHook = func(context.Context, int) error { entered <- struct{}{}; <-release; return nil }
	readDone := make(chan error, 1)
	go func() { _, err := q.Enumerate(context.Background()); readDone <- err }()
	await(t, entered)
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	queued, stop := context.WithCancel(context.Background())
	defer stop()
	result := make(chan model.InputPreviewStatus, 1)
	go func() { status, _ := q.Preview(queued, onlyLeft("cancel-queued"), nil); result <- status }()
	awaitQueued(t, q, 1)
	stop()
	if status := await(t, result); status.State != "cancelled" || status.Phase != "finished" || writeCount(f) != 0 {
		t.Fatalf("未执行abort没有发布明确取消终态: %+v", status)
	}
	close(release)
	if err := await(t, readDone); err != nil {
		t.Fatal(err)
	}
}

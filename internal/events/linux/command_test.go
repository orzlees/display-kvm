package linux

import (
	"context"
	"errors"
	"testing"

	"github.com/orzlee/displayKvm/internal/model"
)

func TestUdevReadyMarkerRequiresPostRegistrationLine(t *testing.T) {
	for _, line := range []string{"monitor will print the received events for:", "KERNEL - the kernel uevent", "READY=1", "UDEV [1] add /devices/x (usb)"} {
		if udevReadyLine(line) {
			t.Fatalf("前置标题或事件被当作就绪: %q", line)
		}
	}
	if !udevReadyLine("UDEV - the event which udev sends out after rule processing") {
		t.Fatal("固定版本的监听后标记未识别")
	}
}

func TestCommandRejectsRelativePathAndClassifiesPermission(t *testing.T) {
	r := execRunner{}
	if _, err := r.run(context.Background(), "busctl", "--version"); model.CodeOf(err) != model.CodeInvalidConfig {
		t.Fatalf("允许隐式PATH查找: %v", err)
	}
	cause := errors.New("exit status 1")
	err := commandError(context.Background(), cause, "Call failed: org.freedesktop.DBus.Error.AccessDenied: denied")
	if model.CodeOf(err) != model.CodePermissionDenied || !errors.Is(err, cause) {
		t.Fatalf("权限/底层原因丢失: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := commandError(ctx, cause, "permission denied"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestBoundedOutputFailsRatherThanTruncates(t *testing.T) {
	b := boundedOutput{limit: 3}
	if _, err := b.Write([]byte("four")); model.CodeOf(err) != model.CodeMessageTooLarge {
		t.Fatalf("超限静默成功: %v", err)
	}
}

func TestNotifyPayloadAndCredentialsMustBothMatch(t *testing.T) {
	if validNotification([]byte("READY=1\nSTATUS=ok"), 42, 42) != true {
		t.Fatal("有效就绪通知未接受")
	}
	for _, c := range []struct {
		body          string
		sender, child int
	}{{"READY=1", 43, 42}, {"READY=0", 42, 42}, {"STATUS=READY=1", 42, 42}, {"READY=1", 0, 0}} {
		if validNotification([]byte(c.body), c.sender, c.child) {
			t.Fatalf("伪造通知被接受: %+v", c)
		}
	}
}

func TestBusStdoutCannotForgeReadyAndQueueOverflowCancels(t *testing.T) {
	canceled := false
	p := &execSubscription{lines: make(chan string, 1), ready: make(chan error, 1), cancel: func() { canceled = true }}
	w := &lineOutput{p: p, mode: readyNotify}
	if _, err := w.Write([]byte("READY=1\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.ready:
		t.Fatal("busctl标准输出伪造了就绪")
	default:
	}
	if _, err := w.Write([]byte("another\n")); model.CodeOf(err) != model.CodeMessageTooLarge || !canceled {
		t.Fatalf("缓存超限没有取消订阅: %v", err)
	}
}

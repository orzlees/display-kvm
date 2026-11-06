package linux

import (
	"testing"

	"github.com/orzlee/displayKvm/internal/model"
)

func TestLogindSignalsUseSystemOwnerAndRealSemantics(t *testing.T) {
	owner := ":1.9"
	for _, c := range []struct {
		line string
		kind model.EventKind
	}{
		{`{"type":"signal","sender":":1.9","path":"/org/freedesktop/login1","interface":"org.freedesktop.login1.Manager","member":"PrepareForSleep","payload":{"type":"b","data":[false]}}`, model.EventResume},
		{`{"type":"signal","sender":":1.9","path":"/org/freedesktop/login1","interface":"org.freedesktop.login1.Manager","member":"PrepareForSleep","payload":{"type":"b","data":[true]}}`, ""},
		{`{"type":"signal","sender":":1.8","path":"/org/freedesktop/login1","interface":"org.freedesktop.login1.Manager","member":"PrepareForSleep","payload":{"type":"b","data":[false]}}`, ""},
		{`{"type":"signal","sender":":1.9","path":"/org/freedesktop/login1","interface":"org.freedesktop.login1.Session","member":"Lock","payload":{"type":"","data":[]}}`, ""},
	} {
		signal, err := parseLogindSignal(c.line, owner)
		if err != nil {
			t.Fatal(err)
		}
		if signal.kind != c.kind {
			t.Fatalf("睡眠前、其他发送者或锁屏被误报: %+v", signal)
		}
	}
}

func TestListSessionsChecksTargetUIDAndAbsence(t *testing.T) {
	data := []byte(`{"type":"a(susso)","data":[[["c1",1000,"alice","seat0","/org/freedesktop/login1/session/c1"],["c2",2000,"bob","seat0","/org/freedesktop/login1/session/c2"]]]}`)
	present, err := sessionInList(data, "c1", 1000)
	if err != nil || !present {
		t.Fatalf("当前用户会话丢失: %v %v", present, err)
	}
	if _, err := sessionInList(data, "c2", 1000); model.CodeOf(err) != model.CodePermissionDenied {
		t.Fatalf("接受其他UID会话: %v", err)
	}
	present, err = sessionInList(data, "missing", 1000)
	if err != nil || present {
		t.Fatalf("明确缺席被当作未知/存在: %v %v", present, err)
	}
}

func TestMalformedLogindPayloadFails(t *testing.T) {
	_, err := parseLogindSignal(`{"type":"signal","sender":":1.9","path":"/org/freedesktop/login1","interface":"org.freedesktop.login1.Manager","member":"PrepareForSleep","payload":{"type":"s","data":["false"]}}`, ":1.9")
	if model.CodeOf(err) != model.CodeUnsupportedProtocol {
		t.Fatalf("错误类型被当作真实唤醒: %v", err)
	}
	_, err = parseLogindSignal(`{"type":"signal","sender":":1.9","path":"/org/freedesktop/login1","interface":"org.freedesktop.login1.Manager","member":"PrepareForSleep","payload":{"type":"b","data":[null]}}`, ":1.9")
	if model.CodeOf(err) != model.CodeUnsupportedProtocol {
		t.Fatalf("null被当成恢复事实: %v", err)
	}
	if _, err := sessionInList([]byte(`{"type":"a(susso)","data":[null]}`), "c1", 1000); model.CodeOf(err) != model.CodeUnsupportedProtocol {
		t.Fatalf("未知列表被当作明确缺席: %v", err)
	}
}

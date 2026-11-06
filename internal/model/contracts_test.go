package model_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/orzlee/displayKvm/internal/model"
)

func TestManagementProtocolAndGenerationContracts(t *testing.T) {
	probe := model.Request{Version: model.ProtocolVersion, ID: "probe", Token: strings.Repeat("A", 43), Operation: model.OperationManagementCapabilities}
	if err := probe.Validate(); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(probe)
	if err != nil || strings.Contains(string(encoded), `"management"`) {
		t.Fatalf("旧后台探测不得带新字段: %s %v", encoded, err)
	}
	prepare := model.ManagementRequest{Action: model.ManagementPrepareConfig, ExpectedGeneration: "run-one", CommitID: "commit-run-one-a", Data: json.RawMessage(`{}`)}
	if err := prepare.Validate(); err != nil {
		t.Fatal(err)
	}
	prepare.ExpectedGeneration = "run-two"
	if err := prepare.Validate(); err == nil {
		t.Fatal("跨代提交ID必须拒绝")
	}
	query := model.ManagementRequest{Action: model.ManagementCommitStatus, ExpectedGeneration: "run-old", CommitID: "commit-run-one-a"}
	if err := query.Validate(); err != nil {
		t.Fatalf("旧运行代仍可查原ID: %v", err)
	}
	request := probe
	request.Operation, request.Management = model.OperationManagement, &query
	if err := request.Validate(); err != nil {
		t.Fatal(err)
	}
	request.Event = &model.Event{}
	if err := request.Validate(); err == nil {
		t.Fatal("管理与事件负载不能混合")
	}
	request.Event = nil
	request.Version++
	if err := request.Validate(); model.CodeOf(err) != model.CodeUnsupportedVersion {
		t.Fatalf("协议版本错误: %v", err)
	}
	unknown := model.ManagementResponse{Generation: "run-new", Commit: &model.CommitStatus{ID: query.CommitID, Phase: model.CommitUnknown}}
	if err := unknown.Validate(); err != nil || unknown.Commit.Saved != nil || unknown.Commit.Effective != nil {
		t.Fatalf("未知不能伪称已保存或失败: %+v %v", unknown, err)
	}
}

func TestSwitchRequestRejectsAmbiguousSelection(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  model.SwitchRequest
	}{
		{"missing correlation", model.SwitchRequest{TargetID: "work"}},
		{"missing target", model.SwitchRequest{ID: "req-1"}},
		{"two selectors", model.SwitchRequest{ID: "req-1", TargetID: "work", GroupID: "desk", DisplayIDs: []model.DisplayID{"left"}}},
		{"duplicate display", model.SwitchRequest{ID: "req-1", TargetID: "work", DisplayIDs: []model.DisplayID{"left", "left"}}},
		{"negative timeout", model.SwitchRequest{ID: "req-1", TargetID: "work", TimeoutMillis: -1}},
		{"unbounded timeout", model.SwitchRequest{ID: "req-1", TargetID: "work", TimeoutMillis: 30001}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.req.Validate(); model.CodeOf(err) != model.CodeInvalidRequest {
				t.Fatalf("应拒绝非法请求，得到 %v", err)
			}
		})
	}
	req := model.SwitchRequest{ID: "req-1", TargetID: "work"}
	if err := req.Validate(); err != nil || req.EffectiveTimeout() != 10*time.Second {
		t.Fatalf("省略选择器应选择目标的全部显示器，并使用有界默认超时：%v", err)
	}
	req.TimeoutMillis = 30000
	if err := req.Validate(); err != nil || req.EffectiveTimeout() != 30*time.Second {
		t.Fatalf("超时上界应被接受：%v", err)
	}
}

func TestInputSchemeRejectsUnknownAndMixedProtocols(t *testing.T) {
	for _, scheme := range []model.InputScheme{
		{ID: "future", Protocol: "future", VCPCode: 0x60, Value: 17},
		{ID: "wrong-code", Protocol: model.ProtocolVCP, VCPCode: 0x10, Value: 17},
		{ID: "mixed", Protocol: model.ProtocolLGInputAlt, VCPCode: 0x60, Value: 144},
		{ID: "zero", Protocol: model.ProtocolVCP, VCPCode: 0x60},
	} {
		if err := scheme.Validate(); err == nil {
			t.Fatalf("非法方案未被拒绝：%+v", scheme)
		}
	}
	for _, scheme := range []model.InputScheme{
		{ID: "standard", Protocol: model.ProtocolVCP, VCPCode: 0x60, Value: 17},
		{ID: "calibrated-lg", Protocol: model.ProtocolLGInputAlt, Value: 144},
	} {
		if err := scheme.Validate(); err != nil {
			t.Fatalf("有效方案被拒绝：%v", err)
		}
	}
}

func TestPerDisplayResultPreservesEvidenceAndCorrelation(t *testing.T) {
	when := time.Date(2026, 11, 5, 3, 0, 0, 0, time.UTC)
	value := uint16(17)
	result := model.SwitchResult{
		RequestID: "req-7",
		Displays: []model.DisplayResult{
			{DisplayID: "left", State: model.StateSent, DesiredValue: 17, SentAt: &when},
			{DisplayID: "right", State: model.StateFailed, DesiredValue: 17,
				Error: model.NewError(model.CodeUnreachable, "切走后控制通路不可达", nil)},
		},
	}
	if err := result.Validate(); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var decoded model.SwitchResult
	if err := model.DecodeJSON(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.RequestID != "req-7" || decoded.Displays[0].State != model.StateSent || decoded.Displays[1].Error.Code != model.CodeUnreachable {
		t.Fatalf("关联、逐屏状态或失败原因丢失：%s", raw)
	}
	decoded.Displays[0].State = model.StateConfirmed
	if err := decoded.Validate(); err == nil {
		t.Fatal("没有读回证据时不能把已发送升级为已确认")
	}
	decoded.Displays[0].Observation = &model.InputObservation{Value: &value, ObservedAt: when}
	if err := decoded.Validate(); err != nil {
		t.Fatal(err)
	}
	decoded.Displays[0].Observation.ObservedAt = when.Add(-time.Second)
	if err := decoded.Validate(); err == nil {
		t.Fatal("旧缓存读回不能确认本次发送")
	}
	decoded.Displays[0].Observation.ObservedAt = when
	value = 15
	if err := decoded.Validate(); err == nil {
		t.Fatal("读回值不同不能确认")
	}
	value = 17
	decoded.Displays[1].DisplayID = "left"
	if err := decoded.Validate(); err == nil {
		t.Fatal("逐屏结果不能含重复逻辑ID")
	}
}

func TestErrorRetainsCauseAndCancellationClassification(t *testing.T) {
	err := model.NewError(model.CodeTransport, "操作已取消", context.Canceled)
	if !errors.Is(err, context.Canceled) || model.CodeOf(err) != model.CodeCanceled {
		t.Fatalf("取消原因应可追踪且统一分类：%v", err)
	}
	if got := model.CodeOf(model.NewError(model.CodeTransport, "超时", context.DeadlineExceeded)); got != model.CodeTimeout {
		t.Fatalf("超时误分类：%s", got)
	}
	raw, marshalErr := json.Marshal(err)
	if marshalErr != nil || strings.Contains(string(raw), "cause") {
		t.Fatalf("内部原因不应泄露到消息：%s, %v", raw, marshalErr)
	}
}

func validEndpoint() model.Endpoint {
	return model.Endpoint{
		Version: model.EndpointVersion, ProtocolVersion: model.ProtocolVersion,
		Network: "tcp4", Address: "127.0.0.1:49152",
		Token: base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("x", 32))),
		PID:   42, StartedAt: time.Date(2026, 11, 5, 3, 0, 0, 0, time.UTC),
	}
}

func TestEndpointRejectsPublicOrUnusableAddresses(t *testing.T) {
	for _, address := range []string{"0.0.0.0:1", "192.168.1.2:42", "localhost:42", "[::1]:42", "127.0.0.1:0", "127.0.0.1:65536"} {
		t.Run(address, func(t *testing.T) {
			endpoint := validEndpoint()
			endpoint.Address = address
			if err := endpoint.Validate(); err == nil {
				t.Fatal("应拒绝非约定的回环端点或无效端口")
			}
		})
	}
	endpoint := validEndpoint()
	if err := endpoint.Validate(); err != nil {
		t.Fatal(err)
	}
	endpoint.Token = "short"
	if err := endpoint.Validate(); model.CodeOf(err) != model.CodeAuthenticationFailed {
		t.Fatalf("短令牌应拒绝：%v", err)
	}
}

func TestWireRequestVersionAuthenticationAndCorrelation(t *testing.T) {
	req := model.Request{
		Version: model.ProtocolVersion, ID: "request-1", Token: validEndpoint().Token,
		Operation: model.OperationSwitch,
		Switch:    &model.SwitchRequest{ID: "request-1", TargetID: "work"},
	}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := model.DecodeRequest(raw)
	if err != nil || decoded.Switch.TargetID != "work" {
		t.Fatalf("请求往返失败：%v", err)
	}
	req.Switch.ID = "another-request"
	if err := req.Validate(); model.CodeOf(err) != model.CodeInvalidRequest {
		t.Fatalf("内外关联ID不一致应拒绝：%v", err)
	}
	req.Version++
	if err := req.Validate(); model.CodeOf(err) != model.CodeUnsupportedVersion {
		t.Fatalf("不支持版本应明确分类：%v", err)
	}
	if _, err := model.DecodeRequest([]byte(strings.Repeat("x", model.MaxMessageBytes+1))); model.CodeOf(err) != model.CodeMessageTooLarge {
		t.Fatalf("超限请求应在解码前拒绝：%v", err)
	}
	req.Version = model.ProtocolVersion
	req.Switch.ID = req.ID
	req.Token = "short"
	if err := req.Validate(); model.CodeOf(err) != model.CodeAuthenticationFailed {
		t.Fatalf("请求短令牌必须拒绝：%v", err)
	}
	req.Token = validEndpoint().Token
	req.Operation = model.OperationEnumerate
	if err := req.Validate(); err == nil {
		t.Fatal("枚举操作不能夹带切换负载")
	}
}

func TestStrictJSONRejectsAmbiguousDocuments(t *testing.T) {
	for _, raw := range []string{
		`{"id":"first","id":"second"}`,
		`{"id":"first"} {"id":"second"}`,
		`{"id":"first","unknown":true}`,
		`{"id":"first","nested":{"a":1,"a":2}}`,
		`null`,
	} {
		var dst struct {
			ID string `json:"id"`
		}
		if err := model.DecodeJSON([]byte(raw), &dst); err == nil {
			t.Fatalf("歧义或非法JSON被接受：%s", raw)
		}
	}
	var nested map[string]any
	if err := model.DecodeJSON([]byte(`{"value":`+strings.Repeat("[", 66)+`0`+strings.Repeat("]", 66)+`}`), &nested); err == nil {
		t.Fatal("过深嵌套必须拒绝")
	}
	if err := model.DecodeJSON([]byte{'{', '"', 'i', 'd', '"', ':', '"', 0xff, '"', '}'}, &nested); err == nil {
		t.Fatal("无效UTF-8不能静默替换为其他标识")
	}
}

func TestResponseRoundTripRejectsCrossRequestAndMixedPayload(t *testing.T) {
	response := model.Response{
		Version: model.ProtocolVersion, ID: "req-9", Operation: model.OperationSwitch,
		SwitchResult: &model.SwitchResult{RequestID: "req-9", Displays: []model.DisplayResult{
			{DisplayID: "left", State: model.StateUnknown, DesiredValue: 17,
				Error: model.NewError(model.CodeUnsupportedOperation, "设备不支持读回", nil)},
		}},
	}
	raw, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := model.DecodeResponse(raw)
	if err != nil || decoded.SwitchResult.Displays[0].State != model.StateUnknown {
		t.Fatalf("未知结果及原因必须保留：%s, %v", raw, err)
	}
	response.SwitchResult.RequestID = "someone-else"
	if err := response.Validate(); err == nil {
		t.Fatal("响应不能装入另一请求的逐屏结果")
	}
	response.SwitchResult.RequestID = response.ID
	response.Error = model.NewError(model.CodeInternal, "失败", nil)
	if err := response.Validate(); err == nil {
		t.Fatal("请求级错误不能夹带成功负载")
	}
	response.SwitchResult = nil
	response.Error.Code = "future-error"
	if err := response.Validate(); err == nil {
		t.Fatal("未知错误类别不能静默接受")
	}
	if _, err := model.DecodeResponse([]byte(strings.Repeat("x", model.MaxMessageBytes+1))); model.CodeOf(err) != model.CodeMessageTooLarge {
		t.Fatalf("响应同样必须限制体积：%v", err)
	}
	response.Operation = "future-op"
	response.Error = model.NewError(model.CodeUnsupportedOperation, "未知操作", nil)
	if err := response.Validate(); err != nil {
		t.Fatalf("未知操作应能关联明确拒绝：%v", err)
	}
}

func TestEventCarriesPlatformFactsWithoutAssumingUniqueUSBSerial(t *testing.T) {
	event := model.Event{
		ID: "event-1", Kind: model.EventUSBAdded, Source: "test-usb",
		ObservedAt: time.Date(2026, 11, 5, 3, 0, 0, 0, time.UTC),
		USB:        &model.USBDevice{VendorID: 0x1234, ProductID: 0x5678},
	}
	if err := event.Validate(); err != nil {
		t.Fatalf("USB空序列号是合法现实：%v", err)
	}
	message := model.EventMessage{Version: model.ProtocolVersion, Event: event}
	raw, err := json.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	var decoded model.EventMessage
	if err := model.DecodeJSON(raw, &decoded); err != nil || decoded.Event.USB.ProductID != 0x5678 {
		t.Fatalf("事件往返失败：%s, %v", raw, err)
	}
	event.Kind = model.EventCustom
	event.USB = nil
	if err := event.Validate(); err == nil {
		t.Fatal("自定义事件必须有用于筛选的名称")
	}
	capability := model.EventCapability{Kind: model.EventUSBAdded, Available: false}
	if err := capability.Validate(); err == nil {
		t.Fatal("不可用能力必须提供明确原因")
	}
}

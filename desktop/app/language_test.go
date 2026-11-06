package app

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/orzlee/displayKvm/desktop/i18n"
	"github.com/orzlee/displayKvm/internal/config"
	"github.com/orzlee/displayKvm/internal/model"
)

func TestServiceLanguageDefaultAndInstanceIsolation(t *testing.T) {
	first, second := New(nil), New(nil)
	defer first.Close()
	defer second.Close()
	if first.Language() == second.Language() || first.Language().Snapshot().Preference != "system" || first.Language().Snapshot().Locale != "en" {
		t.Fatal("默认服务必须各自持有跟随系统的语言管理器")
	}
	if err := first.Language().SetPreference("zh-CN"); err != nil {
		t.Fatal(err)
	}
	if second.Language().Snapshot().Locale != "en" {
		t.Fatal("语言偏好泄漏到另一服务实例")
	}
}

func TestServiceLanguageDisconnectedStateRerendersWithoutReconnect(t *testing.T) {
	language, err := i18n.New("zh-CN", nil)
	if err != nil {
		t.Fatal(err)
	}
	diagnostic := model.NewError(model.CodeDaemonUnavailable, "原始后台诊断：连接拒绝", nil)
	connections := 0
	wrapped := fmt.Errorf("connect desktop: %w", diagnostic)
	service := NewWithLanguage(func() (Client, error) {
		connections++
		return nil, wrapped
	}, language)
	defer service.Close()
	if service.Language() != language {
		t.Fatal("注入管理器必须与宿主、页面共用同一实例")
	}
	if err := service.Reconnect(context.Background()); !errors.Is(err, diagnostic) {
		t.Fatal("连接返回值丢失原始错误")
	}
	chinese := service.Snapshot()
	if chinese.Connected || chinese.ConnectionMessage != language.Snapshot().Text("error.daemon_unavailable", nil) || chinese.ConnectionDiagnostic == nil || chinese.ConnectionDiagnostic.Code != diagnostic.Code || chinese.ConnectionDiagnostic.Message != diagnostic.Message || chinese.ConnectionDiagnostic.Detail != wrapped.Error() {
		t.Fatalf("断连消息未按代码呈现或诊断丢失: %+v", chinese)
	}
	if err := language.SetPreference("en"); err != nil {
		t.Fatal(err)
	}
	english := service.Snapshot()
	if english.ConnectionMessage == chinese.ConnectionMessage || english.Message == chinese.Message || english.ConnectionDiagnostic == nil || english.ConnectionDiagnostic.Detail != chinese.ConnectionDiagnostic.Detail || connections != 1 || english.Generation != chinese.Generation {
		t.Fatal("已保存断连状态切语言时没有重新呈现，或偷偷重连/丢失诊断")
	}
	if english.ConnectionMessage != language.Snapshot().Text("error.daemon_unavailable", nil) || english.BlockedReason != language.Snapshot().Text("app.wait.connect", nil) {
		t.Fatal("动态状态未使用同一词典")
	}
}

type languageClient struct {
	cfg      config.Config
	switches atomic.Int32
	problem  *model.Error
}

func (c *languageClient) Configuration() (config.Config, error) { return c.cfg, nil }

func (c *languageClient) Enumerate(context.Context) ([]model.Device, error) {
	return []model.Device{{BackendID: "backend-left", Identity: model.IdentityHints{Path: "left-path"}}}, nil
}

func (c *languageClient) Switch(_ context.Context, request model.SwitchRequest) (model.SwitchResult, error) {
	c.switches.Add(1)
	sent := time.Now()
	value := uint16(17)
	return model.SwitchResult{RequestID: request.ID, Displays: []model.DisplayResult{{DisplayID: "left", State: model.StateUnknown,
		SentAt: &sent, Observation: &model.InputObservation{Value: &value, ObservedAt: sent.Add(-time.Second)}, Error: c.problem}}}, nil
}

func TestServiceLanguageSwitchResultRerendersWithoutReplay(t *testing.T) {
	language, err := i18n.New("zh-CN", nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &languageClient{problem: model.NewError(model.CodeUnreachable, "原始显示器诊断", nil), cfg: config.Config{Version: config.CurrentVersion,
		Displays:     []model.Display{{ID: "left", Binding: &model.Binding{Path: "left-path"}}},
		InputSchemes: []model.InputScheme{{ID: "hdmi", Protocol: model.ProtocolVCP, VCPCode: 0x60, Value: 17}},
		Targets:      []model.Target{{ID: "work", Inputs: []model.TargetInput{{DisplayID: "left", SchemeID: "hdmi"}}}}}}
	service := NewWithLanguage(func() (Client, error) { return client, nil }, language)
	defer service.Close()
	if err := service.Reconnect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := service.BeginSwitch(Selection{Kind: "display", DisplayID: "left", TargetID: "work", Generation: service.Snapshot().Generation}); err != nil {
		t.Fatal(err)
	}
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	var chinese View
	for {
		chinese = service.Snapshot()
		if !chinese.Busy {
			break
		}
		select {
		case <-service.Changes():
		case <-deadline.C:
			t.Fatal("切换夹具未返回")
		}
	}
	if len(chinese.Results) != 1 || chinese.Results[0].Diagnostic == nil || chinese.Results[0].Observation == nil || chinese.Results[0].Observation.Value == nil {
		t.Fatal("未知结果或诊断丢失")
	}
	// 外部快照拥有自己的观测和诊断；修改它不能污染已保存的逐屏证据。
	*chinese.Results[0].Observation.Value = 99
	chinese.Results[0].Diagnostic.Message = "外部篡改"
	if err := language.SetPreference("en"); err != nil {
		t.Fatal(err)
	}
	english := service.Snapshot()
	row := english.Results[0]
	if row.State != "unknown" || row.ManualConfirmed || row.Detail != language.Snapshot().Text("error.unreachable", nil) || row.Detail == chinese.Results[0].Detail || row.Diagnostic.Message != client.problem.Message || *row.Observation.Value != 17 || client.switches.Load() != 1 || row.Name == chinese.Results[0].Name {
		t.Fatal("语言切换改变了证据、重复派发，或没有重新呈现结果和默认名称")
	}
}

func TestServiceLanguagePreservesUnclassifiedDiagnostic(t *testing.T) {
	language, err := i18n.New("en", nil)
	if err != nil {
		t.Fatal(err)
	}
	diagnostic := errors.New("connector diagnostic without a model code")
	service := NewWithLanguage(func() (Client, error) { return nil, diagnostic }, language)
	defer service.Close()
	if err := service.Reconnect(context.Background()); !errors.Is(err, diagnostic) {
		t.Fatal("不能替换连接原错误")
	}
	view := service.Snapshot()
	if view.ConnectionDiagnostic == nil || view.ConnectionDiagnostic.Detail != diagnostic.Error() || view.ConnectionMessage != language.Snapshot().Text("app.connection.failed", nil) {
		t.Fatal("未知错误需保留原诊断，并呈现可理解的连接失败说明")
	}
}

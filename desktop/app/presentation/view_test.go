package presentation

import (
	"testing"
	"time"

	"github.com/orzlee/displayKvm/desktop/i18n"
	"github.com/orzlee/displayKvm/internal/model"
)

func TestI18nResultProjectionPreservesEvidenceAndDiagnostic(t *testing.T) {
	manager, err := i18n.New("zh-CN", nil)
	if err != nil {
		t.Fatal(err)
	}
	sent := time.Now()
	value := uint16(17)
	diagnostic := model.NewError(model.CodeUnreachable, "设备原始诊断", nil)
	diagnostic.Field, diagnostic.Retryable = "display_id", true
	result := model.DisplayResult{DisplayID: "left", State: model.StateUnknown, SentAt: &sent,
		Observation: &model.InputObservation{Value: &value, ObservedAt: sent.Add(-time.Second)}, Error: diagnostic}
	row := ResultView{DisplayID: result.DisplayID, ManualConfirmed: true}
	ApplyResultWithCatalog(&row, result, manager.Snapshot())
	chinese := row.Detail
	if row.State != "unknown" || row.SentAt != result.SentAt || row.Observation != result.Observation || !row.ManualConfirmed || row.Diagnostic == nil || row.Diagnostic.Code != diagnostic.Code || row.Diagnostic.Message != diagnostic.Message || row.Diagnostic.Field != diagnostic.Field || !row.Diagnostic.Retryable {
		t.Fatal("翻译不能改变发送、历史观测、人工确认或诊断证据")
	}
	if err := manager.SetPreference("en"); err != nil {
		t.Fatal(err)
	}
	ApplyResultWithCatalog(&row, result, manager.Snapshot())
	if row.Detail == chinese || row.Detail != manager.Snapshot().Text("error.unreachable", nil) || row.Diagnostic.Detail != diagnostic.Error() {
		t.Fatal("结果重投影必须更新语言并保留原诊断")
	}
}

func TestI18nResultProjectionDoesNotPromoteInvalidObservation(t *testing.T) {
	manager, err := i18n.New("en", nil)
	if err != nil {
		t.Fatal(err)
	}
	zero := time.Time{}
	row := ResultView{}
	ApplyResultWithCatalog(&row, model.DisplayResult{State: model.StateUnknown, SentAt: &zero, Observation: &model.InputObservation{}}, manager.Snapshot())
	if row.SentAt != nil || row.Observation != nil || row.State != "unknown" {
		t.Fatal("语言投影不能把无效时间或空观测提升为有效证据")
	}
}

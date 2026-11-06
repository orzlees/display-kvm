package overview_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/orzlee/displayKvm/desktop/app"
	"github.com/orzlee/displayKvm/desktop/i18n"
	"github.com/orzlee/displayKvm/desktop/tray"
	"github.com/orzlee/displayKvm/desktop/views/overview"
	"github.com/orzlee/displayKvm/internal/model"
)

func TestOverviewAndTrayLanguageChangeDoesNotRepeatOperations(t *testing.T) {
	f := fixture()
	s, h := connected(t, f)
	choice := selection(s, true)
	request(t, h, "selection", choice, http.StatusOK)
	request(t, h, "switch", choice, http.StatusAccepted)
	before := completed(t, s)
	f.mu.Lock()
	requests, configurations, enumerations := len(f.requests), f.configurations, f.enumerations
	f.mu.Unlock()
	controller := tray.NewWithLanguage(s, nil, nil, s.Language())
	chinese := controller.Entries()[0].Label
	if err := s.Language().SetPreference("en"); err != nil {
		t.Fatal(err)
	}
	after := request(t, h, "status", nil, http.StatusOK)
	if after.Generation != before.Generation || !reflect.DeepEqual(after.Selection, before.Selection) || after.RequestID != before.RequestID || after.Results[0].State != before.Results[0].State || after.Results[0].Name != before.Results[0].Name {
		t.Fatal("切换语言不能改变请求、选择、状态证据或用户名称")
	}
	if after.Results[0].Label != s.Language().Snapshot().Text("app.result.confirmed", nil) || controller.Entries()[0].Label == chinese {
		t.Fatal("概览动态结果与托盘必须消费同一语言快照")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) != requests || f.configurations != configurations || f.enumerations != enumerations {
		t.Fatal("语言更新只能重投影已有状态，不能重连、枚举或重发切换")
	}
}

func TestOverviewLanguageCatalogAndDiagnosticAreStructured(t *testing.T) {
	language, err := i18n.New("en", nil)
	if err != nil {
		t.Fatal(err)
	}
	f := fixture()
	f.enumerationError = model.NewError(model.CodeUnreachable, "原始设备诊断", errors.New("native failure"))
	s := app.NewWithLanguage(func() (app.Client, error) { return f, nil }, language)
	defer s.Close()
	h := overview.NewHandler(s)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/client/language", nil))
	var catalog i18n.Catalog
	if err := json.Unmarshal(w.Body.Bytes(), &catalog); err != nil {
		t.Fatal(err)
	}
	if catalog.Preference != "en" || catalog.Locale != "en" || catalog.Revision != language.Snapshot().Revision || len(catalog.Messages) == 0 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("浏览器必须取得与服务/托盘一致且不可缓存的完整Catalog")
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/client/connect", bytes.NewBufferString("{}")))
	var result struct {
		Error      string                                 `json:"error"`
		Diagnostic struct{ Code, Message, Detail string } `json:"diagnostic"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusServiceUnavailable || result.Diagnostic.Code != string(model.CodeUnreachable) || result.Diagnostic.Message != "原始设备诊断" || result.Error == "原始设备诊断" || result.Diagnostic.Detail == "" {
		t.Fatal("摘要应翻译，错误代码和原始诊断不能删除或改写")
	}
}

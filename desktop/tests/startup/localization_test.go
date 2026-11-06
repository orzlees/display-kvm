package startup_test

import (
	"bytes"
	"html/template"
	"strings"
	"testing"

	"github.com/orzlee/displayKvm/desktop/i18n"
	"github.com/orzlee/displayKvm/desktop/views/localize"
)

func TestLocalizedTemplateCarriesStableKeysAndEscapesUserValues(t *testing.T) {
	manager, err := i18n.New("en", nil)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := template.Must(template.New("page").Funcs(localize.Functions(i18n.Catalog{})).Parse(`{{t "nav.settings"}}{{m "common.named" "name" .}}{{catalog}}`))
	var output bytes.Buffer
	if err := localize.Execute(tmpl, &output, `<script>alert(1)</script>`, manager.Snapshot()); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{`data-i18n="nav.settings"`, `data-i18n-values=`, `id="desktop-catalog"`, `/desktop-i18n.js`} {
		if !strings.Contains(output.String(), text) {
			t.Errorf("缺少同词典原位切换契约：%s", text)
		}
	}
	if strings.Contains(output.String(), `<script>alert(1)</script>`) {
		t.Fatal("用户值不能转为可执行HTML")
	}
}

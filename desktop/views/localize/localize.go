// Package localize将同一个语言快照交给模板和浏览器；不持有全局语言状态。
package localize

import (
	"encoding/json"
	"html"
	"html/template"
	"io"
	"strconv"

	"github.com/orzlee/displayKvm/desktop/i18n"
	"github.com/orzlee/displayKvm/internal/model"
)

func Functions(catalog i18n.Catalog) template.FuncMap {
	text := func(key string, pairs ...string) template.HTML {
		values := map[string]string{}
		for i := 0; i+1 < len(pairs); i += 2 {
			values[pairs[i]] = pairs[i+1]
		}
		encoded, _ := json.Marshal(values)
		// key及插值均作为文本/属性转义。页面原位换语言只改该span的文字，
		// 不能重建包含输入控件的父节点，否则会丢失未提交草稿和键盘焦点。
		return template.HTML(`<span data-i18n="` + html.EscapeString(key) + `" data-i18n-values="` + html.EscapeString(string(encoded)) + `">` + html.EscapeString(catalog.Text(key, values)) + `</span>`)
	}
	return template.FuncMap{
		"t": text, "m": text,
		"tr":     func(key string) string { return catalog.Text(key, nil) },
		"number": func(value int) string { return strconv.Itoa(value) },
		"catalog": func() template.HTML {
			// encoding/json默认转义<、>及&，避免词典或插值结束script元素。
			encoded, _ := json.Marshal(catalog)
			return template.HTML(`<script type="application/json" id="desktop-catalog">` + string(encoded) + `</script><script src="/desktop-i18n.js" defer></script>`)
		},
		"errorKey": func(code model.ErrorCode) string { return "error." + string(code) },
	}
}

// 每次响应克隆模板并绑定本次快照；并行页面不能通过共享Funcs互相覆盖语言。
func Execute(source *template.Template, output io.Writer, view any, catalog i18n.Catalog) error {
	current, err := source.Clone()
	if err != nil {
		return err
	}
	return current.Funcs(Functions(catalog)).Execute(output, view)
}

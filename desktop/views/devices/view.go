package devices

import (
	"bytes"
	"embed"
	"html/template"
	"net/http"
	"regexp"

	"github.com/orzlee/displayKvm/desktop/app/presentation"
	"github.com/orzlee/displayKvm/desktop/i18n"
	"github.com/orzlee/displayKvm/desktop/views/localize"
)

// 页面资源由桌面入口显式注册；本包不启动窗口、后台或第二条硬件队列，
// 因而同一套真实页面处理器也能在不加载原生宿主的情况下接受契约验证。
//
//go:embed assets/*
var resources embed.FS

var pageTemplate = template.Must(template.New("devices").Funcs(localize.Functions(i18n.Catalog{})).Funcs(template.FuncMap{"msg": messageHTML}).Parse(pageSource()))

// 动态投影和静态模板复用同一个m函数，消息参数只作为转义文本写入标记。
// 消息键与参数保留到浏览器，语言通知才能原位更新证据而不重建表单草稿。
func messageHTML(catalog i18n.Catalog, message i18n.Message) template.HTML {
	if message.Key == "" {
		return ""
	}
	pairs := make([]string, 0, len(message.Values)*2)
	for key, value := range message.Values {
		pairs = append(pairs, key, value)
	}
	return localize.Functions(catalog)["m"].(func(string, ...string) template.HTML)(message.Key, pairs...)
}

func pageSource() string {
	content, err := resources.ReadFile("assets/index.html")
	if err != nil {
		panic(err)
	}
	// 所有POST表单共享同一份凭据与运行代，集中插入可避免新增取消/查询按钮时
	// 漏掉保护字段。这里只处理本包嵌入的静态模板，人工输入仍由html/template转义。
	forms := regexp.MustCompile(`<form\b[^>]*>`)
	source := forms.ReplaceAllString(string(content), `${0}{{template "guard" $$}}`)
	return source + `{{define "guard"}}<input type="hidden" name="_token" value="{{.Token}}" /><input type="hidden" name="generation" value="{{.Generation}}" />{{end}}` +
		`{{define "diagnostic"}}{{if .}}<details class="original-diagnostic"><summary>{{t "common.original_detail"}}</summary><dl><dt>{{t "devices.diagnostic_code"}}</dt><dd>{{.Code}}</dd><dt>{{t "devices.diagnostic_message"}}</dt><dd>{{.Message}}</dd><dt>{{t "devices.diagnostic_field"}}</dt><dd>{{.Field}}</dd><dt>{{t "devices.diagnostic_retryable"}}</dt><dd>{{.Retryable}}</dd><dt>{{t "devices.diagnostic_detail"}}</dt><dd>{{.Detail}}</dd></dl></details>{{end}}{{end}}`
}

func renderPage(w http.ResponseWriter, status int, view pageView) error {
	// 先完整渲染再写响应，避免模板出错后已发送成功状态或半个保存确认页面。
	var rendered bytes.Buffer
	if err := localize.Execute(pageTemplate, &rendered, view, view.Catalog); err != nil {
		return err
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, err := w.Write(rendered.Bytes())
	return err
}

type pageView struct {
	Catalog              i18n.Catalog
	Token                string
	Generation           string
	Theme                string
	Connected            bool
	Connection           i18n.Message
	ConnectionDiagnostic *presentation.Diagnostic
	Error                i18n.Message
	ErrorCause           i18n.Message
	ErrorDiagnostic      *presentation.Diagnostic
	Notice               i18n.Message
	Busy                 bool
	CanScan              bool
	HasSelection         bool
	CanBind              bool
	CanCalibrate         bool
	CanTrial             bool
	IdentityIssue        i18n.Message
	IdentityDiagnostic   *presentation.Diagnostic
	CalibrationHint      i18n.Message
	SelectedName         string
	Displays             []displayView
	Candidates           []candidateView
	Targets              []targetView
	CalibrationTargets   []targetView
	Draft                draftView
	Diagnostic           *diagnosticView
	Calibration          *calibrationView
	Recorded             []recordedCalibrationView
	Trial                *trialView
	Save                 *saveView
}

type displayView struct {
	ID         string
	URL        string
	Name       string
	Detail     string
	State      i18n.Message
	Diagnostic *presentation.Diagnostic
	LogicalID  string
	Selected   bool
}

type candidateView struct {
	ID       string
	Name     string
	Detail   string
	Selected bool
}

type targetView struct {
	ID       string
	Name     string
	Selected bool
}

// 草稿仅保留人工输入，不代表后台配置已保存或已生效。提交结果未知时仍保留
// 原草稿及提交编号，避免通过重新生成一次保存请求来猜测上次是否成功。
type draftView struct {
	DisplayID string
	BackendID string
	Name      string
	TargetID  string
	PortName  string
	SchemeID  string
	Protocol  string
	Value     string
}

type diagnosticView struct {
	Backend           i18n.Message
	Identity          i18n.Message
	Capabilities      i18n.Message
	Protocols         string
	Readback          i18n.Message
	ReadbackBasis     i18n.Message
	Issues            []*presentation.Diagnostic
	UnavailableReason string
}

type calibrationView struct {
	Name       string
	Target     string
	Protocol   string
	Value      string
	Basis      i18n.Message
	Diagnostic *presentation.Diagnostic
	CanSave    bool
}

type recordedCalibrationView struct {
	Name      string
	Target    string
	Input     string
	Basis     i18n.Message
	Confirmed string
}

type trialView struct {
	Summary i18n.Message
	Running bool
	CanStep bool
	CanStop bool
	Results []trialResultView
}

type trialResultView struct {
	Index       int
	RequestID   string
	Name        string
	Status      i18n.Message
	Sent        i18n.Message
	Readback    i18n.Message
	Observation i18n.Message
	Issue       i18n.Message
	Diagnostic  *presentation.Diagnostic
	CanConfirm  bool
}

type saveView struct {
	CommitID   string
	Status     i18n.Message
	Saved      i18n.Message
	Effective  i18n.Message
	Diagnostic *presentation.Diagnostic
	CanApply   bool
	CanQuery   bool
}

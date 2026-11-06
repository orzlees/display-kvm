// Package startup提供独立于后台连接的配置与自动启动设置页面。
package startup

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/orzlee/displayKvm/desktop/app"
	"github.com/orzlee/displayKvm/desktop/app/presentation"
	"github.com/orzlee/displayKvm/desktop/i18n"
	"github.com/orzlee/displayKvm/desktop/views/localize"
	"github.com/orzlee/displayKvm/internal/settings"
)

const route = "/startup/"
const resourcePath = "/startup-assets/"

//go:embed startup.html assets/*
var files embed.FS

var pageTemplate = template.Must(template.New("startup.html").Funcs(localize.Functions(i18n.Catalog{})).ParseFS(files, "startup.html"))

// Known与Enabled分别表达能否确认状态和确认后的值；系统读取失败不能伪装成关闭。
type LoginState struct {
	Supported bool
	Known     bool
	Enabled   bool
	Ready     *bool
	Error     string
}

type State struct {
	ConfigPath    string
	ConfigKnown   bool
	ProfileExists bool
	ConfigError   string
	Service       LoginState
	Desktop       LoginState
	Version       string
}

// nil字段表示不改该项。禁用一个启动项不应重新保存历史参数或修改另一启动项，
// 否则旧路径已失效时会连关闭自动启动都被阻断。
type SaveRequest struct {
	ConfigPath     *string
	ServiceEnabled *bool
	DesktopEnabled *bool
	Expected       State
}

type Outcome struct {
	Attempted  bool
	Succeeded  bool
	Unknown    bool
	Message    string
	Diagnostic *presentation.Diagnostic
}

type Problem struct {
	Field      string
	Message    string
	Diagnostic *presentation.Diagnostic
}

// 页面自身的已知故障保存消息键，延迟到当前语言快照呈现；它与系统返回的
// 原始诊断不同，不能把消息键当作后台原文展示，也不能预先固定为某一种语言。
type messageError string

func (err messageError) Error() string { return string(err) }

type SaveResult struct {
	Configuration Outcome
	Service       Outcome
	Desktop       Outcome
	Problems      []Problem
}

// Backend只操作以后登录使用的本地设置，不通过后台快照推断路径，
// 不包含立即启停服务的方法，页面离线时也能使用同一能力。
type Backend interface {
	Read(context.Context) (State, error)
	Save(context.Context, SaveRequest) (SaveResult, error)
	ReadDocument(context.Context, string) (settings.Snapshot, error)
	SaveDocument(context.Context, string, settings.Document, settings.Revision) (settings.SaveResult, error)
}

// Open和Save均直接沿用宿主的canceled语义；取消不能清空草稿或更新文件位置。
type FileDialogs struct {
	Open func(context.Context, string) (string, bool, error)
	Save func(context.Context, string) (string, bool, error)
}

func Page(backend Backend, dialogs FileDialogs) app.Page {
	resources, err := fs.Sub(files, "assets")
	if err != nil {
		// 嵌入路径是编译期契约；缺少资源时不能启动一个表面可用的设置页。
		panic(err)
	}
	return app.Page{
		Route: route, ResourcePath: resourcePath, Resources: resources,
		Handler: func(service *app.Service) http.Handler {
			// 文件和登录项由独立Backend提供，无Service时仍须能够创建设置页。
			// 空语言交给NewHandler按实例创建默认Manager，避免共享全局语言状态。
			var language *i18n.Manager
			if service != nil {
				language = service.Language()
			}
			return NewHandler(backend, dialogs, language)
		},
	}
}

type draft struct {
	Service bool
	Desktop bool
}

type handler struct {
	mu                sync.Mutex
	backend           Backend
	dialogs           FileDialogs
	language          *i18n.Manager
	state             State
	base              State
	draft             draft
	loaded            bool
	dirty             bool
	uncertain         bool
	revision          uint64
	problems          []Problem
	notice            string
	result            SaveResult
	fileResult        Outcome
	readError         string
	readDiagnostic    *presentation.Diagnostic
	document          settings.Document
	documentBase      settings.Document
	path              string
	documentRevision  settings.Revision
	documentDirty     bool
	unsavedDocument   bool
	documentAvailable bool
	documentError     string
	fileUnknown       string
	pendingPath       string
	pendingRevision   settings.Revision
	pendingDocument   *settings.Document
	pendingSave       bool
	fileCanceled      bool
}

func NewHandler(backend Backend, dialogs FileDialogs, language *i18n.Manager) http.Handler {
	if language == nil {
		language, _ = i18n.New("system", nil)
	}
	document := settings.NewDocument()
	return &handler{backend: backend, dialogs: dialogs, language: language, document: document, documentBase: document, documentAvailable: true, unsavedDocument: true}
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if r.URL.Path != route {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, h.language.Snapshot().Text("common.method", nil), http.StatusMethodNotAllowed)
		return
	}
	// 原生选择器与保存都可能等待系统响应；同一页面的锁和表单版本共同阻止
	// 旧窗口、双击或刷新把同一操作重复派发。版本仅保护本页面，不声称跨CLI原子提交。
	h.mu.Lock()
	defer h.mu.Unlock()
	status := http.StatusOK
	if r.Method == http.MethodGet {
		if err := h.read(r.Context(), !h.dirty); err != nil && !h.canEditAny() {
			status = http.StatusServiceUnavailable
		}
	} else {
		h.problems, h.notice = nil, ""
		if !sameOrigin(r) {
			h.problems = []Problem{{Message: "settings.error.origin"}}
			status = http.StatusForbidden
		} else {
			r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
			if err := r.ParseForm(); err != nil || !validForm(r.PostForm) {
				h.problems = []Problem{{Message: "settings.error.form"}}
				status = http.StatusBadRequest
			} else if revision, err := strconv.ParseUint(r.PostForm.Get("revision"), 10, 64); err != nil || revision != h.revision {
				h.problems = []Problem{{Message: "settings.error.stale"}}
				status = http.StatusConflict
			} else {
				h.revision++
				status = h.act(r.Context(), r.PostForm)
				// 部分保存也可能返回409，但已受理后的草稿和新版本必须呈现。
				// 只有来源、字段和旧版本检查全部通过才附请求版本，客户端不能
				// 把动作之前拒绝的409旧页面误当作本次草稿导入。
				w.Header().Set("X-Settings-Request-Revision", r.PostForm.Get("revision"))
			}
		}
	}
	var body bytes.Buffer
	if err := localize.Execute(pageTemplate, &body, h.view(), h.language.Snapshot()); err != nil {
		log.Printf("呈现设置页面失败：%v", err)
		http.Error(w, h.language.Snapshot().Text("common.render_error", nil), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(status)
	if _, err := w.Write(body.Bytes()); err != nil {
		log.Printf("输出设置页面失败：%v", err)
	}
}

func (h *handler) read(ctx context.Context, replace bool) error {
	if h.backend == nil {
		h.readError = "settings.error.backend"
		h.readDiagnostic = nil
		h.state.ConfigKnown = false
		return messageError(h.readError)
	}
	state, err := h.backend.Read(ctx)
	h.state, h.readError, h.readDiagnostic = state, "", nil
	h.loaded = true
	if err != nil {
		problem := diagnosticProblem(err)
		h.readError, h.readDiagnostic = problem.Message, problem.Diagnostic
	} else if !state.ConfigKnown {
		h.readError = state.ConfigError
		if h.readError == "" {
			h.readError = "settings.error.read_unknown"
		}
		err = errors.New(h.readError)
	}
	if replace {
		// 汇总错误不代表所有字段都不可信：仅用已确认的分项替换草稿，
		// 无法读取的参数继续保留且禁写，正常桌面项仍可独立开启或关闭。
		h.base = state
		if state.Service.Known {
			h.draft.Service = state.Service.Enabled
		}
		if state.Desktop.Known {
			h.draft.Desktop = state.Desktop.Enabled
		}
		if state.ConfigKnown && state.ConfigPath != "" && h.fileUnknown == "" {
			if snapshot, readErr := h.backend.ReadDocument(ctx, state.ConfigPath); readErr == nil {
				h.acceptDocument(snapshot)
			} else {
				h.documentAvailable = false
				h.documentError = "settings.error.load_selected"
				if readErr != nil {
					h.problems = append(h.problems, diagnosticProblem(readErr))
				}
			}
		} else if state.ConfigKnown && state.ConfigPath == "" && h.fileUnknown == "" {
			h.document = settings.NewDocument()
			h.documentBase = h.document
			h.path, h.documentRevision = "", ""
			h.documentAvailable, h.unsavedDocument, h.documentDirty = true, true, false
			h.documentError = ""
		}
		h.updateDirty()
		h.revision++
	}
	return err
}

func (h *handler) canEditAny() bool {
	return h.loaded && (h.state.ConfigKnown || h.state.Service.Supported && h.state.Service.Known || h.state.Desktop.Supported && h.state.Desktop.Known)
}

func (h *handler) act(ctx context.Context, form url.Values) int {
	action := form.Get("action")
	if action == "reload" {
		err := h.read(ctx, true)
		// 明确重新读取后，未知分项由各自Known标记禁写；不能让坏服务项
		// 永久锁住可信桌面项，也不会把未确认的开关转换成关闭请求。
		h.uncertain = false
		h.result = SaveResult{}
		h.fileResult = Outcome{}
		h.pendingPath, h.pendingDocument, h.pendingSave = "", nil, false
		h.revision++
		if err != nil {
			h.notice = "settings.notice.partial_read"
			if !h.canEditAny() {
				return http.StatusServiceUnavailable
			}
			return http.StatusOK
		}
		h.notice = "settings.notice.read"
		return http.StatusOK
	}
	if action == "cancel" {
		_ = h.read(ctx, true)
		h.pendingPath, h.pendingDocument, h.pendingSave = "", nil, false
		h.updateDirty()
		h.result = SaveResult{}
		h.fileResult = Outcome{}
		h.notice = "settings.notice.cancel"
		return http.StatusOK
	}
	if !h.canEditAny() {
		h.problems = []Problem{{Message: "settings.error.not_loaded"}}
		return http.StatusConflict
	}
	// 语言是完整文档内容。修改下拉只更新草稿；不能在文件和配置选择确认前
	// 改Manager，也不能覆盖CLI设置的高级工具路径或其它业务分区。
	if values, ok := form["language"]; ok && h.documentAvailable {
		h.document.Desktop.Language = values[0]
		h.documentDirty = h.document.Desktop != h.documentBase.Desktop
	}
	// 开关可提交性必须与view使用同一份已呈现状态。草稿刷新不替换base，
	// 而已禁用的开关不会提交；按旧base.Known解析会把省略误当成关闭。
	// base仅承担版本与原始草稿比较，保存时查询恢复也不能补发未知项变更。
	if h.state.Service.Supported && h.state.Service.Known {
		h.draft.Service = form.Get("service_enabled") == "1"
	}
	if h.state.Desktop.Supported && h.state.Desktop.Known {
		h.draft.Desktop = form.Get("desktop_enabled") == "1"
	}
	h.updateDirty()
	if action != "save" {
		return h.documentAction(ctx, action, form)
	}
	h.result, h.fileResult = SaveResult{}, Outcome{}
	return h.saveSettings(ctx, true)
}

// 整页保存先写完整配置，再提交配置选择和系统开关。配置与系统项不是全局
// 事务：文件失败时不切配置、不启服务，但可信桌面项和服务关闭仍独立处理。
// 已确认写成的文件不重写；未知结果只读确认，不能因后续失败自动重发。
func (h *handler) saveSettings(ctx context.Context, writeFile bool) int {
	if h.uncertain {
		h.problems = []Problem{{Message: "settings.error.save_unknown"}}
		return http.StatusConflict
	}
	if h.pendingPath != "" {
		h.pendingSave = true
		h.notice = "settings.notice.confirm_first"
		return http.StatusOK
	}
	if h.fileUnknown != "" {
		h.problems = append(h.problems, Problem{Message: "settings.error.file_unknown"})
	}
	if writeFile && h.documentDirty && h.fileUnknown == "" {
		if h.unsavedDocument || h.path == "" {
			h.fileCanceled = false
			h.documentAction(ctx, "save_as", nil)
			if h.pendingPath != "" {
				h.pendingSave = true
				return http.StatusOK
			}
			if h.fileCanceled || !h.fileResult.Attempted {
				return http.StatusOK
			}
		} else {
			h.writeDocument(ctx, h.path, h.documentRevision, h.document)
		}
	}
	request := SaveRequest{Expected: h.base}
	if h.state.ConfigKnown && h.path != "" && !h.unsavedDocument && h.path != h.base.ConfigPath {
		path := h.path
		request.ConfigPath = &path
	}
	if h.state.Service.Supported && h.state.Service.Known && h.draft.Service != h.base.Service.Enabled {
		enabled := h.draft.Service
		request.ServiceEnabled = &enabled
	}
	if h.state.Desktop.Supported && h.state.Desktop.Known && h.draft.Desktop != h.base.Desktop.Enabled {
		enabled := h.draft.Desktop
		request.DesktopEnabled = &enabled
	}
	// 只在修改启动选择或开启服务时要求文档已保存。单纯禁用任一项不重新
	// 验证旧文件，例如历史工具已删除时仍必须允许取消下次登录启动。
	if request.ConfigPath != nil || request.ServiceEnabled != nil && *request.ServiceEnabled {
		if !h.state.ConfigKnown || h.fileUnknown != "" || h.documentDirty || h.unsavedDocument && !h.base.ProfileExists {
			h.problems = append(h.problems, Problem{Message: "settings.error.document_unconfirmed"})
			request.ConfigPath = nil
			if request.ServiceEnabled != nil && *request.ServiceEnabled {
				request.ServiceEnabled = nil
			}
		}
	}
	if request.ConfigPath == nil && request.ServiceEnabled == nil && request.DesktopEnabled == nil {
		h.applyLanguage()
		if len(h.problems) != 0 {
			return http.StatusConflict
		}
		h.updateDirty()
		if h.fileResult.Succeeded {
			h.notice = "settings.notice.saved"
		} else {
			h.notice = "settings.notice.no_changes"
		}
		return http.StatusOK
	}
	result, err := h.backend.Save(ctx, request)
	h.result, h.problems = result, append(h.problems, result.Problems...)
	h.uncertain = err != nil || result.Configuration.Unknown || result.Service.Unknown || result.Desktop.Unknown
	if err != nil {
		h.problems = append(h.problems, diagnosticProblem(err))
	}
	// 每次写入后只读回实际状态，不自动回滚或重发；多步保存可能部分成功。
	readErr := h.read(ctx, false)
	// 语言依赖文件和选择已确认，与登录开关独立。桌面或服务开关失败不能
	// 回滚已保存语言；选择失败/未知或读回不一致时继续使用原语言。
	selectionConfirmed := request.ConfigPath == nil || result.Configuration.Attempted && result.Configuration.Succeeded && !result.Configuration.Unknown && h.state.ConfigKnown && h.state.ConfigPath == *request.ConfigPath
	if selectionConfirmed {
		h.applyLanguage()
	}
	failed := err != nil || len(h.problems) != 0
	if readErr != nil && !h.canEditAny() {
		h.uncertain = true
	}
	if request.ConfigPath != nil && !result.Configuration.Attempted || request.ServiceEnabled != nil && !result.Service.Attempted || request.DesktopEnabled != nil && !result.Desktop.Attempted {
		failed = true
		h.uncertain = true
		h.problems = append(h.problems, Problem{Message: "settings.error.result_missing"})
	}
	for _, outcome := range []Outcome{result.Configuration, result.Service, result.Desktop} {
		if outcome.Attempted && (!outcome.Succeeded || outcome.Unknown) {
			failed = true
			if outcome.Message != "" {
				h.problems = append(h.problems, Problem{Message: outcome.Message, Diagnostic: outcome.Diagnostic})
			}
		}
	}
	if !matchesRequest(request, h.state) {
		failed = true
		h.problems = append(h.problems, Problem{Message: "settings.error.readback"})
	}
	if h.loaded {
		h.base = h.state
	}
	if failed {
		h.dirty = true
		h.notice = "settings.notice.partial_save"
		return http.StatusConflict
	}
	if h.state.Service.Known {
		h.draft.Service = h.state.Service.Enabled
	}
	if h.state.Desktop.Known {
		h.draft.Desktop = h.state.Desktop.Enabled
	}
	h.updateDirty()
	h.notice = "settings.notice.saved_login"
	if h.readError != "" {
		h.notice = "settings.notice.saved_partial_read"
	}
	return http.StatusOK
}

func (h *handler) applyLanguage() {
	if h.fileUnknown != "" || h.documentDirty || h.unsavedDocument || !h.documentAvailable || !h.state.ConfigKnown || h.state.ConfigPath != h.path {
		return
	}
	if err := h.language.SetPreference(h.document.Desktop.Language); err != nil {
		h.problems = append(h.problems, diagnosticProblem(err))
	}
}

func (h *handler) updateDirty() {
	h.dirty = h.documentDirty || h.path != "" && !h.unsavedDocument && h.path != h.base.ConfigPath || h.draft.Service != h.base.Service.Enabled || h.draft.Desktop != h.base.Desktop.Enabled
}

func (h *handler) acceptDocument(snapshot settings.Snapshot) {
	h.document, h.documentBase = snapshot.Document, snapshot.Document
	h.path, h.documentRevision = snapshot.Path, snapshot.Revision
	h.documentAvailable, h.unsavedDocument, h.documentDirty = true, false, false
	h.documentError, h.pendingPath = "", ""
	h.pendingDocument, h.pendingSave = nil, false
}

func suggestedPath() string {
	directory, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(directory, "DisplayKVM", "settings.json")
}

// 新建、加载和另存只处理文件；整页保存需要覆盖确认时，确认后继续同一次
// 保存。已受理的文件错误返回新版本表单，保留原位草稿；来源、格式和版本
// 错误仍在动作之前返回403/400/409，不能用旧响应替换浏览器中的新草稿。
func (h *handler) documentAction(ctx context.Context, action string, form url.Values) int {
	if action == "reload_file" {
		path := h.fileUnknown
		if path == "" {
			path = h.path
		}
		if path == "" {
			h.problems = []Problem{{Message: "settings.error.load_first"}}
			return http.StatusOK
		}
		snapshot, err := h.backend.ReadDocument(ctx, path)
		if errors.Is(err, os.ErrNotExist) && h.fileUnknown != "" {
			h.fileUnknown = ""
			h.notice = "settings.notice.absent"
		} else if err != nil {
			h.problems = []Problem{{Message: "settings.error.read_document"}}
			if err != nil {
				h.problems = append(h.problems, diagnosticProblem(err))
			}
		} else {
			h.acceptDocument(snapshot)
			h.fileUnknown = ""
			h.notice = "settings.notice.read_document"
		}
		h.updateDirty()
		return http.StatusOK
	}
	if h.fileUnknown != "" {
		h.problems = []Problem{{Message: "settings.error.file_unknown"}}
		return http.StatusConflict
	}
	if action == "cancel_overwrite" {
		h.pendingPath = ""
		h.pendingDocument = nil
		h.pendingSave = false
		h.notice = "settings.notice.cancel_overwrite"
		return http.StatusOK
	}
	if strings.HasPrefix(action, "choose_") {
		kind := strings.TrimPrefix(action, "choose_")
		if kind != "config" {
			h.problems = []Problem{{Message: "settings.error.kind"}}
			return http.StatusBadRequest
		}
		path := form.Get("config_path")
		if h.dialogs.Open != nil {
			chosen, canceled, err := h.dialogs.Open(ctx, kind)
			if err != nil {
				h.problems = []Problem{{Message: "settings.error.choose"}, diagnosticProblem(err)}
				return http.StatusOK
			}
			if canceled {
				h.notice = "settings.notice.cancel_choose"
				return http.StatusOK
			}
			path = chosen
		} else if kind != "config" || path == "" {
			h.problems = []Problem{{Message: "settings.error.picker"}}
			return http.StatusOK
		}
		if err := absolutePath(path); err != nil {
			h.problems = []Problem{{Message: err.Error()}}
			return http.StatusOK
		}
		snapshot, err := h.backend.ReadDocument(ctx, path)
		if err != nil {
			h.problems = []Problem{{Message: "settings.error.load"}, diagnosticProblem(err)}
			return http.StatusOK
		}
		h.acceptDocument(snapshot)
		h.notice = "settings.notice.loaded"
		h.updateDirty()
		return http.StatusOK
	}
	candidate := h.document
	replacingDraft := false
	switch action {
	case "new_config":
		candidate = settings.NewDocument()
		replacingDraft = true
	case "confirm_overwrite":
		if h.pendingPath == "" {
			h.problems = []Problem{{Message: "settings.error.no_overwrite"}}
			return http.StatusOK
		}
		if h.pendingDocument != nil {
			candidate = *h.pendingDocument
		}
		continueSave := h.pendingSave
		h.pendingSave = false
		h.writeDocument(ctx, h.pendingPath, h.pendingRevision, candidate)
		if continueSave {
			// 覆盖确认属于同一次整页保存。失败后也只处理独立系统项，不能
			// 再次进入文件写入阶段，否则一次确认会在失败时重复覆盖。
			return h.saveSettings(ctx, false)
		}
		return http.StatusOK
	case "save_as":
	default:
		h.problems = []Problem{{Message: "settings.error.action"}}
		return http.StatusBadRequest
	}
	h.updateDirty()
	if !h.documentAvailable && !replacingDraft {
		h.problems = []Problem{{Message: "settings.error.valid_document"}}
		return http.StatusOK
	}
	if err := candidate.Validate(); err != nil {
		h.problems = []Problem{{Message: "settings.error.validation"}, diagnosticProblem(err)}
		return http.StatusOK
	}
	if h.dialogs.Save == nil {
		h.problems = []Problem{{Message: "settings.error.save_dialog"}}
		return http.StatusOK
	}
	suggestion := h.path
	if suggestion == "" || h.unsavedDocument || replacingDraft {
		suggestion = suggestedPath()
	}
	path, canceled, err := h.dialogs.Save(ctx, suggestion)
	if err != nil {
		h.problems = []Problem{{Message: "settings.error.save_location"}, diagnosticProblem(err)}
		return http.StatusOK
	}
	if canceled {
		h.fileCanceled = true
		h.notice = "settings.notice.cancel_save"
		return http.StatusOK
	}
	if err := absolutePath(path); err != nil {
		h.problems = []Problem{{Message: err.Error()}}
		return http.StatusOK
	}
	snapshot, err := h.backend.ReadDocument(ctx, path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		h.writeDocument(ctx, path, "", candidate)
	case err != nil:
		h.problems = []Problem{{Message: "settings.error.target"}, diagnosticProblem(err)}
	default:
		// 只对刚读到的有效完整目标提供明确覆盖确认。目标版本保存至确认时，
		// 存储层在持锁后再次比较；外部改动、后台持锁或坏目标都不能绕过。
		h.pendingPath, h.pendingRevision = path, snapshot.Revision
		h.pendingDocument = nil
		if replacingDraft {
			h.pendingDocument = &candidate
		}
		h.notice = "settings.notice.overwrite"
	}
	h.updateDirty()
	return http.StatusOK
}

func (h *handler) writeDocument(ctx context.Context, path string, revision settings.Revision, document settings.Document) {
	if err := document.Validate(); err != nil {
		h.problems = []Problem{{Message: "settings.error.validation"}, diagnosticProblem(err)}
		return
	}
	result, err := h.backend.SaveDocument(ctx, path, document, revision)
	h.pendingPath = ""
	h.fileResult = Outcome{Attempted: true}
	if result.Saved == nil || *result.Saved && result.Snapshot == nil {
		// 结果未知时不移动原位置，不假造成功快照，也不自动重发；明确读取
		// 本次目标后才能判断它是否已写成，避免一次点击变成两次覆盖。
		h.fileUnknown = path
		h.fileResult.Unknown = true
		h.problems = []Problem{{Message: "settings.error.file_unknown"}}
	} else if !*result.Saved {
		h.problems = []Problem{{Message: "settings.error.not_saved"}}
	} else {
		h.fileResult.Succeeded = true
		h.acceptDocument(*result.Snapshot)
		h.notice = "settings.notice.file_saved"
	}
	if err != nil {
		h.fileResult.Message = err.Error()
		h.fileResult.Diagnostic = presentation.DiagnosticFromError(err)
		h.problems = append(h.problems, diagnosticProblem(err))
	}
	h.updateDirty()
}

func boolValue(value bool) *bool { return &value }

func absolutePath(path string) error {
	if path == "" || !filepath.IsAbs(path) || strings.ContainsAny(path, "\x00\r\n") {
		return errors.New("settings.error.absolute_path")
	}
	return nil
}

func matchesRequest(request SaveRequest, state State) bool {
	return (request.ConfigPath == nil || state.ConfigKnown && state.ConfigPath == *request.ConfigPath) &&
		(request.ServiceEnabled == nil || state.Service.Known && state.Service.Enabled == *request.ServiceEnabled) &&
		(request.DesktopEnabled == nil || state.Desktop.Known && state.Desktop.Enabled == *request.DesktopEnabled)
}

func validForm(form url.Values) bool {
	for key, values := range form {
		if len(values) != 1 {
			return false
		}
		switch key {
		case "action", "revision", "config_path":
		case "language":
			if values[0] != "system" && values[0] != "zh-CN" && values[0] != "en" {
				return false
			}
		case "service_enabled", "desktop_enabled":
			if values[0] != "1" {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func sameOrigin(r *http.Request) bool {
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		return false
	}
	scheme := r.URL.Scheme
	// Wails 3.1.0在原生资源请求进入Handler前清空URL.Scheme和URL.Host，
	// 但保留Request.Host及原始来源头；macOS资源固定来自wails://localhost。
	// 仅对该规范化形态且无端口的真实localhost主机识别原生来源，不能把
	// 任意协议、主机或前缀当作本地页面，否则外部来源也能派发本地设置操作。
	native := scheme == "" && r.URL.Host == "" && r.TLS == nil && strings.EqualFold(r.Host, "localhost")
	if scheme == "" {
		scheme = "http"
		if r.TLS != nil {
			scheme = "https"
		}
	}
	if !strings.EqualFold(scheme, "http") && !strings.EqualFold(scheme, "https") {
		return false
	}
	chosen := false
	for _, header := range []string{"Origin", "Referer"} {
		if raw := r.Header.Get(header); raw != "" {
			origin, err := url.Parse(raw)
			if err != nil || origin.Host == "" || origin.User != nil || !strings.EqualFold(origin.Host, r.Host) {
				return false
			}
			if !chosen && native && strings.EqualFold(origin.Scheme, "wails") && strings.EqualFold(origin.Host, "localhost") {
				scheme = "wails"
			}
			chosen = true
			// 两个来源头都存在时必须指向同一来源，不能混用HTTP与原生来源。
			if !strings.EqualFold(origin.Scheme, scheme) {
				return false
			}
		}
	}
	return true
}

type toggleView struct {
	Name, Label, Description, Actual, Detail string
	Checked, Editable                        bool
}

type pageView struct {
	Revision                                                                               uint64
	Dirty, CanSave, CanEdit, CanPick, CanSaveFile, Uncertain, FileUnknown, UnsavedDocument bool
	ReadError, Notice, Parameters, ParametersDetail                                        string
	Path, Language, DocumentError, PendingPath                                             string
	Toggles                                                                                []toggleView
	Problems                                                                               []Problem
	Results                                                                                []resultView
	ReadDiagnostic                                                                         *presentation.Diagnostic
}

type resultView struct {
	Label, Status, Detail string
	Diagnostic            *presentation.Diagnostic
}

func (h *handler) view() pageView {
	view := pageView{
		Revision: h.revision, Dirty: h.dirty, CanEdit: h.canEditAny() && h.documentAvailable && h.fileUnknown == "",
		CanPick: h.dialogs.Open != nil, Uncertain: h.uncertain, ReadError: h.readError,
		Notice: h.notice, Problems: h.problems, ReadDiagnostic: h.readDiagnostic,
		Path: h.path, Language: h.document.Desktop.Language, DocumentError: h.documentError, PendingPath: h.pendingPath,
		FileUnknown: h.fileUnknown != "", UnsavedDocument: h.unsavedDocument,
	}
	view.CanSave = h.canEditAny() && !h.uncertain
	view.CanSaveFile = h.canEditAny() && h.dialogs.Save != nil && h.fileUnknown == ""
	view.Parameters = "settings.no_selection"
	if !h.state.ConfigKnown {
		view.Parameters = "settings.selection_unread"
		view.ParametersDetail = h.state.ConfigError
	} else if h.state.ProfileExists {
		view.Parameters = "settings.next_service"
		view.ParametersDetail = h.state.ConfigPath
	}
	for _, item := range []struct {
		name, label, description, prefix string
		state                            LoginState
		checked                          bool
	}{
		{"service_enabled", "settings.service", "settings.service_hint", "settings.service", h.state.Service, h.draft.Service},
		{"desktop_enabled", "settings.desktop", "settings.desktop_hint", "settings.desktop", h.state.Desktop, h.draft.Desktop},
	} {
		toggle := toggleView{Name: item.name, Label: item.label, Description: item.description, Checked: item.checked, Detail: item.state.Error}
		toggle.Editable = h.loaded && item.state.Supported && item.state.Known
		switch {
		case !item.state.Supported:
			toggle.Actual = "settings.unsupported"
		case !item.state.Known:
			toggle.Actual = "settings.state_unknown"
		case item.state.Enabled:
			toggle.Actual = "settings.enabled"
		default:
			toggle.Actual = "settings.disabled"
		}
		if item.state.Ready != nil && !*item.state.Ready && toggle.Detail == "" {
			toggle.Detail = "settings.not_ready"
		}
		view.Toggles = append(view.Toggles, toggle)
	}
	for _, item := range []struct {
		label  string
		result Outcome
	}{
		{"settings.configuration", h.fileResult}, {"settings.service_configuration", h.result.Configuration}, {"settings.service", h.result.Service}, {"settings.desktop", h.result.Desktop},
	} {
		if !item.result.Attempted {
			continue
		}
		status := "settings.incomplete"
		if item.result.Unknown {
			status = "settings.result_unknown"
		} else if item.result.Succeeded {
			status = "settings.saved"
		}
		view.Results = append(view.Results, resultView{Label: item.label, Status: status, Detail: item.result.Message, Diagnostic: item.result.Diagnostic})
	}
	return view
}

func diagnosticProblem(err error) Problem {
	if message, ok := err.(messageError); ok {
		return Problem{Message: string(message)}
	}
	return Problem{Message: err.Error(), Diagnostic: presentation.DiagnosticFromError(err)}
}

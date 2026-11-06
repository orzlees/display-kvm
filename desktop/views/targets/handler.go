package targets

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/orzlee/displayKvm/desktop/app"
	"github.com/orzlee/displayKvm/desktop/i18n"
	"github.com/orzlee/displayKvm/desktop/views/localize"
	coreclient "github.com/orzlee/displayKvm/internal/client"
	"github.com/orzlee/displayKvm/internal/config"
	"github.com/orzlee/displayKvm/internal/model"
)

const route = "/targets/"
const resourcePath = "/targets-assets"

//go:embed targets.html assets/*
var files embed.FS

var pageTemplate = template.Must(template.New("targets.html").Funcs(localize.Functions(i18n.Catalog{})).ParseFS(files, "targets.html"))

func Page() app.Page {
	resources, err := fs.Sub(files, "assets")
	if err != nil {
		// 资源路径属于编译时嵌入契约；不以空资源继续启动，否则页面会失去样式与交互。
		panic(err)
	}
	return app.Page{Route: route, Handler: NewHandler, ResourcePath: resourcePath + "/", Resources: resources}
}

type handler struct {
	mu                sync.Mutex
	service           *app.Service
	draft             *draft
	base              config.Config
	generation        model.RuntimeGeneration
	deviceGeneration  uint64
	revision          uint64
	commit            *model.CommitStatus
	applySent         bool
	lastError         *model.Error
	notice            string
	previewID         string
	previewGeneration model.RuntimeGeneration
	preview           *inputPreviewStatus
	previewPending    bool
	previewUnknown    bool
	previewChecked    bool
	previewError      *model.Error
}

func NewHandler(service *app.Service) http.Handler {
	return &handler{service: service}
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
		http.Error(w, h.catalog().Text("common.method", nil), http.StatusMethodNotAllowed)
		return
	}
	// 一个桌面页面共享一份草稿。锁覆盖后台调用，避免两个窗口同时通过旧表单
	// 校验后重复Prepare/Apply；本地revision还拒绝刷新重发的旧POST。
	h.mu.Lock()
	defer h.mu.Unlock()
	status := http.StatusOK
	if h.draft == nil && r.Method == http.MethodGet {
		if err := h.load(r.Context()); err != nil {
			h.lastError = publicError(err)
			status = http.StatusServiceUnavailable
		}
	}
	if r.Method == http.MethodGet {
		if target := r.URL.Query().Get("target"); target != "" && h.draft != nil {
			h.draft.selectItem(selection{Kind: "target", ID: target})
		}
		if r.URL.Query().Has("preview_session") || r.URL.Query().Get("preview_status") == "1" {
			id := r.URL.Query().Get("preview_session")
			if id != "" && !model.ValidID(id) {
				h.previewUnknown = true
				h.previewError = model.NewError(model.CodeInvalidRequest, "targets.preview.error.session", nil)
			} else {
				if id != "" && id != h.previewID {
					h.previewID, h.preview = id, nil
					h.previewGeneration = ""
				}
				h.queryPreview(r.Context())
			}
		}
	}
	if r.Method == http.MethodPost {
		h.lastError, h.notice = nil, ""
		r.Body = http.MaxBytesReader(w, r.Body, config.MaxConfigBytes)
		if err := r.ParseForm(); err != nil {
			h.lastError = model.NewError(model.CodeInvalidRequest, "targets.error.form", err)
			status = http.StatusBadRequest
		} else {
			revision, err := strconv.ParseUint(r.PostForm.Get("revision"), 10, 64)
			if err != nil || revision != h.revision {
				h.lastError = model.NewError(model.CodeInvalidRequest, "targets.error.form_stale", nil)
				status = http.StatusConflict
			} else {
				h.revision++
				if err := h.act(r.Context(), r); err != nil {
					h.lastError = publicError(err)
					status = http.StatusConflict
				}
			}
		}
	}
	catalog := h.catalog()
	view := h.view(catalog)
	var body bytes.Buffer
	if err := localize.Execute(pageTemplate, &body, view, catalog); err != nil {
		log.Printf("呈现目标主机编辑页面失败：%v", err)
		http.Error(w, catalog.Text("common.render_error", nil), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(status)
	if _, err := w.Write(body.Bytes()); err != nil {
		log.Printf("输出目标主机编辑页面失败：%v", err)
	}
}

func (h *handler) load(ctx context.Context) error {
	if h.service == nil {
		return model.NewError(model.CodeDaemonUnavailable, "targets.error.service", nil)
	}
	// 重新读取是显式丢弃草稿的动作，必须重连后枚举当前设备，不能把旧连接的
	// 固定配置快照与新管理运行代拼接。读取失败时原草稿、基线和提交ID均保留。
	if err := h.service.Reconnect(ctx); err != nil {
		return err
	}
	snapshot, err := h.service.ManagementSnapshot(ctx)
	if err != nil {
		return err
	}
	cfg, err := config.Load(bytes.NewReader(snapshot.Configuration))
	if err != nil {
		return err
	}
	next, err := newDraft(cfg)
	if err != nil {
		return err
	}
	base, err := cloneConfig(cfg)
	if err != nil {
		return err
	}
	h.draft, h.base, h.generation = next, base, snapshot.Generation
	h.deviceGeneration = h.service.Snapshot().Generation
	h.commit, h.applySent, h.lastError = nil, false, nil
	h.revision++
	return nil
}

func (h *handler) act(ctx context.Context, r *http.Request) error {
	action := r.PostForm.Get("action")
	// 测试引用后台已保存的目标，不属于草稿提交。必须在保存锁之前分流，
	// 也不能调用draft.update，否则测试按钮会吸收尚未保存的名称或输入映射。
	if action == "preview_start" {
		return h.startPreview(ctx, r.PostForm.Get("target_id"))
	}
	if action == "preview_status" {
		h.queryPreview(ctx)
		return nil
	}
	if action == "query" {
		if h.commit == nil || r.PostForm.Get("commit_id") != string(h.commit.ID) {
			return model.NewError(model.CodeInvalidRequest, "targets.error.original_id", nil)
		}
		return h.query(ctx)
	}
	if action == "reload" {
		if h.unresolved() {
			return model.NewError(model.CodeInvalidRequest, "targets.error.query_first", nil)
		}
		if err := h.load(ctx); err != nil {
			return err
		}
		h.notice = "targets.notice.reloaded"
		return nil
	}
	if h.draft == nil {
		return model.NewError(model.CodeDaemonUnavailable, "targets.error.load_first", nil)
	}
	if h.commit != nil {
		return model.NewError(model.CodeInvalidRequest, "targets.error.commit_registered", nil)
	}
	switch action {
	case "select":
		if !h.draft.selectItem(selection{Kind: r.PostForm.Get("kind"), ID: r.PostForm.Get("id")}) {
			return model.NewError(model.CodeNotFound, "targets.error.selection_missing", nil)
		}
	case "add":
		return h.draft.add(r.PostForm.Get("kind"), h.catalog())
	case "update":
		if err := h.draft.update(r.PostForm); err != nil {
			return err
		}
		h.notice = "targets.notice.draft"
	case "remove":
		if (selection{Kind: r.PostForm.Get("kind"), ID: r.PostForm.Get("id")}) != h.draft.Selected {
			return model.NewError(model.CodeInvalidRequest, "targets.error.remove_changed", nil)
		}
		return h.draft.removeSelected()
	case "cancel":
		selected := h.draft.Selected
		next, err := newDraft(h.base)
		if err != nil {
			return err
		}
		next.selectItem(selected)
		h.draft = next
		h.notice = "targets.notice.cancelled"
	case "save", "check":
		if h.draft.Selected.Kind != "" {
			if err := h.draft.update(r.PostForm); err != nil {
				return err
			}
		}
		if err := h.draft.Config.Validate(); err != nil {
			return err
		}
		if action == "check" {
			h.notice = "targets.notice.valid"
			return nil
		}
		return h.submit(ctx)
	default:
		return model.NewError(model.CodeInvalidRequest, "targets.error.action", nil)
	}
	return nil
}

func (h *handler) query(ctx context.Context) error {
	// Apply应答或传输错误都可能出现在实际落盘之后。重连只恢复管理通道，
	// 此分支只能查询原ID，不能以新运行代重建请求或再次Prepare/Apply。
	if !h.service.Snapshot().Connected {
		if err := h.service.Reconnect(ctx); err != nil {
			return err
		}
	}
	response, err := h.service.Management(ctx, model.ManagementRequest{Action: model.ManagementCommitStatus, CommitID: h.commit.ID})
	if err != nil {
		return err
	}
	return h.acceptStatus(response)
}

func (h *handler) submit(ctx context.Context) error {
	current, err := h.service.ManagementSnapshot(ctx)
	if err != nil {
		return err
	}
	if current.Generation != h.generation {
		err := model.NewError(model.CodeInvalidRequest, "targets.error.draft_stale", nil)
		err.Field = "expected_generation"
		return err
	}
	var encoded bytes.Buffer
	if err := config.Encode(&encoded, h.draft.Config); err != nil {
		return err
	}
	id, err := coreclient.NewCommitID(h.generation)
	if err != nil {
		return err
	}
	// 在Prepare发出之前持有唯一ID。即使Prepare丢失应答，后台仍可能已登记，
	// 所以保留未知状态，只提供查询入口，不能生成新ID或自动再次发送Prepare。
	h.commit = &model.CommitStatus{ID: id, Kind: model.ManagementPrepareConfig, Phase: model.CommitUnknown, PreparedGeneration: h.generation}
	response, err := h.service.Management(ctx, model.ManagementRequest{
		Action: model.ManagementPrepareConfig, ExpectedGeneration: h.generation, CommitID: id, Data: encoded.Bytes(),
	})
	if err != nil {
		return err
	}
	if err := h.acceptStatus(response); err != nil {
		return err
	}
	if response.Generation != h.generation || h.commit.PreparedGeneration != h.generation || h.commit.Phase != model.CommitAccepted {
		return model.NewError(model.CodeInvalidRequest, "targets.error.prepare_receipt", nil)
	}
	// 保存按钮是显式提交意图；只有正常收到本次Prepare的accepted应答后，
	// 才允许发一次Apply。Apply超时、断连或应答丢失均不进入重试分支。
	h.applySent = true
	h.commit = &model.CommitStatus{ID: id, Kind: model.ManagementPrepareConfig, Phase: model.CommitUnknown, PreparedGeneration: h.generation}
	response, err = h.service.Management(ctx, model.ManagementRequest{Action: model.ManagementApplyCommit, ExpectedGeneration: h.generation, CommitID: id})
	if err != nil {
		return err
	}
	return h.acceptStatus(response)
}

func (h *handler) acceptStatus(response model.ManagementResponse) error {
	if response.Commit == nil || h.commit == nil || response.Commit.ID != h.commit.ID || response.Commit.Kind != model.ManagementPrepareConfig {
		return model.NewError(model.CodeInvalidRequest, "targets.error.commit_receipt", nil)
	}
	value := *response.Commit
	h.commit = &value
	if value.Error != nil {
		return value.Error
	}
	return nil
}

func (h *handler) unresolved() bool {
	if h.commit == nil {
		return false
	}
	if h.commit.Phase == model.CommitAccepted && !h.applySent {
		return false
	}
	return h.commit.Saved == nil || h.commit.Effective == nil || h.commit.Phase != model.CommitApplied && h.commit.Phase != model.CommitFailed
}

func (h *handler) view(catalog i18n.Catalog) pageView {
	view := pageView{Theme: "system", Locale: catalog.Locale}
	if h.service != nil {
		state := h.service.Snapshot()
		view.Theme = state.Appearance
		if h.draft != nil {
			presence := make(map[model.DisplayID]presenceView, len(state.Displays))
			for _, display := range state.Displays {
				// Reason随语言变化，是摘要而非原始故障证据；Diagnostic来自公共投影，
				// 必须一并传递，否则缺席与身份歧义会只剩通用提示，无法核对原原因。
				row := presenceView{Label: "targets.presence.unready", Detail: display.Reason, Tone: "notice", Diagnostic: display.Diagnostic}
				if !state.Connected || state.Generation != h.deviceGeneration {
					row.Label, row.DetailKey, row.Detail = "targets.presence.stale", "targets.presence.stale_detail", ""
				} else if display.Available {
					row.Label, row.Tone = "targets.presence.matched", "success"
				}
				presence[display.ID] = row
			}
			view = h.draft.present(presence, catalog)
			view.Theme = state.Appearance
		}
	}
	view.Endpoint, view.ResourcePath, view.Revision = route, resourcePath, h.revision
	view.Locale = catalog.Locale
	view.Error, view.Notice = h.lastError, h.notice
	if h.commit != nil {
		view.Save = saveView{Saved: evidence(h.commit.Saved, "targets.saved.yes", "targets.saved.no"), Applied: evidence(h.commit.Effective, "targets.effective.yes", "targets.effective.no"), CommitID: string(h.commit.ID), CanQuery: true, Locked: true}
		if h.commit.Saved != nil && *h.commit.Saved && h.commit.Effective != nil && *h.commit.Effective {
			view.Dirty = false
		}
		switch h.commit.Phase {
		case model.CommitAccepted:
			view.Save.Detail = "targets.commit.accepted"
		case model.CommitSaving, model.CommitSaved, model.CommitApplying:
			view.Save.Detail = "targets.commit.processing"
		case model.CommitApplied:
			view.Save.Detail = "targets.commit.applied"
		case model.CommitFailed:
			view.Save.Detail = "targets.commit.failed"
		default:
			view.Save.Detail = "targets.commit.unknown"
		}
		if h.commit.Error != nil && view.Error == nil {
			view.Error = h.commit.Error
		}
	}
	// 本页生成的错误使用稳定消息键；后台错误仍按Code翻译摘要并保留原始诊断，
	// 不能对后台中文字符串做替换，否则新诊断或用户数据会被误当作固定文案。
	if view.Error != nil && strings.HasPrefix(view.Error.Message, "targets.error.") {
		view.ErrorMessageKey = view.Error.Message
	}
	view.CanReload = !h.unresolved()
	view.Preview = h.previewView()
	return view
}

func (h *handler) startPreview(ctx context.Context, targetID string) error {
	if h.service == nil {
		return model.NewError(model.CodeDaemonUnavailable, "targets.error.service", nil)
	}
	// 应答丢失时后台可能已经开始切换。保留查询门禁，任何重复点击只能查询，
	// 不得因超时、空编号或页面重载生成第二次启动请求。
	if h.previewPending || h.previewUnknown || h.preview != nil && h.preview.active() {
		h.queryPreview(ctx)
		return nil
	}
	if !h.previewChecked {
		h.queryPreview(ctx)
		if h.previewUnknown || h.preview != nil && h.preview.active() {
			return nil
		}
	}
	if h.draft == nil || h.draft.Selected != (selection{Kind: "target", ID: targetID}) || h.view(h.catalog()).Dirty {
		return model.NewError(model.CodeInvalidRequest, "targets.preview.error.save_first", nil)
	}
	snapshot, err := h.service.ManagementSnapshot(ctx)
	if err != nil {
		return err
	}
	cfg, err := config.Load(bytes.NewReader(snapshot.Configuration))
	if err != nil {
		return err
	}
	var displays []model.DisplayID
	for _, target := range cfg.Targets {
		if string(target.ID) == targetID {
			for _, input := range target.Inputs {
				displays = append(displays, input.DisplayID)
			}
		}
	}
	if len(displays) == 0 {
		return model.NewError(model.CodeInvalidRequest, "targets.preview.error.save_first", nil)
	}
	id, err := coreclient.NewCommitID(snapshot.Generation)
	if err != nil {
		return err
	}
	data, err := json.Marshal(struct {
		Request model.SwitchRequest `json:"request"`
	}{Request: model.SwitchRequest{ID: model.RequestID(id), TargetID: model.TargetID(targetID), DisplayIDs: displays}})
	if err != nil {
		return err
	}
	h.previewID, h.preview, h.previewError = "", nil, nil
	h.previewGeneration, h.previewPending, h.previewUnknown = snapshot.Generation, true, true
	response, err := h.service.Management(ctx, model.ManagementRequest{Action: model.ManagementPreviewStart, ExpectedGeneration: snapshot.Generation, Data: data})
	if err != nil {
		h.previewError = publicError(err)
		// 启动响应没有会话编号时，空SessionID只查询后台current/latest；
		// 后台持有恢复计时，HTTP连接结束不能变成前端回切或自动重试启动。
		h.queryPreview(ctx)
		return nil
	}
	if err := h.acceptPreview(response); err != nil {
		h.previewError = publicError(err)
		h.queryPreview(ctx)
	}
	return nil
}

func (h *handler) queryPreview(ctx context.Context) {
	h.previewUnknown = true
	if h.service == nil {
		h.previewError = model.NewError(model.CodeDaemonUnavailable, "targets.error.service", nil)
		return
	}
	if !h.service.Snapshot().Connected {
		if err := h.service.Reconnect(ctx); err != nil {
			h.previewError = publicError(err)
			return
		}
	}
	if h.previewGeneration == "" {
		snapshot, err := h.service.ManagementSnapshot(ctx)
		if err != nil {
			h.previewError = publicError(err)
			return
		}
		h.previewGeneration = snapshot.Generation
	}
	response, err := h.service.Management(ctx, model.ManagementRequest{Action: model.ManagementPreviewStatus, ExpectedGeneration: h.previewGeneration, SessionID: h.previewID})
	if err != nil {
		h.previewError = publicError(err)
		return
	}
	if response.Session == nil && h.previewID == "" && !h.previewPending && response.Generation == h.previewGeneration {
		h.preview, h.previewError = nil, nil
		h.previewChecked, h.previewUnknown = true, false
		return
	}
	if err := h.acceptPreview(response); err != nil {
		h.previewError = publicError(err)
	}
}

func (h *handler) acceptPreview(response model.ManagementResponse) error {
	if response.Session == nil || response.Session.Kind != "input_preview" || response.Session.Generation != h.previewGeneration || response.Generation != h.previewGeneration || !model.ValidID(response.Session.ID) || h.previewID != "" && response.Session.ID != h.previewID {
		return model.NewError(model.CodeInvalidRequest, "targets.preview.error.receipt", nil)
	}
	// 即使负载损坏也保留有效会话编号，后续只查询同一会话；不能把解析失败
	// 当成后台未启动，更不能用空值覆盖已经取得的逐屏恢复证据。
	h.previewID = response.Session.ID
	var status inputPreviewStatus
	if err := json.Unmarshal(response.Data, &status); err != nil || !status.valid() {
		return model.NewError(model.CodeInvalidRequest, "targets.preview.error.receipt", err)
	}
	h.preview, h.previewError = &status, nil
	h.previewPending, h.previewUnknown, h.previewChecked = false, false, true
	return nil
}

func evidence(value *bool, yes, no string) string {
	if value == nil {
		return "common.unknown"
	}
	if *value {
		return yes
	}
	return no
}

func (h *handler) catalog() i18n.Catalog {
	if h.service != nil {
		return h.service.Language().Snapshot()
	}
	// 无服务仅用于呈现连接错误；沿用系统未知时的英文回退，不持有全局语言状态。
	language, _ := i18n.New("system", nil)
	return language.Snapshot()
}

func publicError(err error) *model.Error {
	var classified *model.Error
	if errors.As(err, &classified) {
		return classified
	}
	log.Printf("目标主机编辑操作失败：%v", err)
	return model.NewError(model.CodeDaemonUnavailable, "targets.error.operation", nil)
}

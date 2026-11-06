// Package schedules通过公共桌面服务编辑日程，不计算触发时间或直接控制显示器。
package schedules

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/orzlee/displayKvm/desktop/app"
	"github.com/orzlee/displayKvm/desktop/i18n"
	coreclient "github.com/orzlee/displayKvm/internal/client"
	"github.com/orzlee/displayKvm/internal/model"
	"github.com/orzlee/displayKvm/internal/scheduler"
)

func Page() app.Page {
	assets, err := fs.Sub(resources, "assets")
	if err != nil {
		panic(err)
	}
	return app.Page{Route: "/schedules/", Handler: NewHandler, ResourcePath: "/schedules-assets/", Resources: assets}
}

type handler struct {
	mu         sync.Mutex
	service    *app.Service
	token      string
	revision   uint64
	loaded     bool
	writable   bool
	generation model.RuntimeGeneration
	draft      draft
	statuses   []scheduler.ScheduleStatus
	timer      model.TimerRuntimeState
	commit     *model.CommitStatus
	applySent  bool
	error      error
	notice     string
}

func NewHandler(service *app.Service) http.Handler {
	if service == nil {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			language, _ := i18n.New("system", nil)
			http.Error(w, language.Snapshot().Text("schedules.error.service", nil), http.StatusServiceUnavailable)
		})
	}
	var nonce [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, service.Language().Snapshot().Text("schedules.error.security", nil), http.StatusInternalServerError)
		})
	}
	return &handler{service: service, token: hex.EncodeToString(nonce[:]), draft: newDraft(nil)}
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.Path != "/schedules/" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, h.catalog().Text("common.method", nil), http.StatusMethodNotAllowed)
		return
	}
	// 同一页面串行持有草稿及提交ID，网络调用受公共Service超时约束；表单版本使
	// 排队的旧点击不能在前一请求完成后再次Prepare或Apply。
	h.mu.Lock()
	defer h.mu.Unlock()
	if r.Method == http.MethodGet {
		if !h.loaded && h.commit == nil {
			if err := h.load(r.Context()); err != nil {
				h.error = err
			}
		}
		h.render(w, http.StatusOK)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") || r.ParseForm() != nil {
		http.Error(w, h.catalog().Text("schedules.error.form", nil), http.StatusBadRequest)
		return
	}
	for _, values := range r.PostForm {
		if len(values) != 1 {
			http.Error(w, h.catalog().Text("schedules.error.duplicate_fields", nil), http.StatusBadRequest)
			return
		}
	}
	if subtle.ConstantTimeCompare([]byte(r.PostForm.Get("_token")), []byte(h.token)) != 1 {
		http.Error(w, h.catalog().Text("schedules.error.expired_form", nil), http.StatusForbidden)
		return
	}
	revision, err := strconv.ParseUint(r.PostForm.Get("revision"), 10, 64)
	if err != nil || revision != h.revision {
		h.error = messageError("schedules.error.stale_form")
		h.render(w, http.StatusConflict)
		return
	}
	h.revision++
	h.error = nil
	h.notice = ""
	status, err := h.act(r.Context(), r)
	if err != nil {
		h.error = err
	}
	h.render(w, status)
}

func (h *handler) load(ctx context.Context) error {
	if err := h.service.Reconnect(ctx); err != nil {
		return err
	}
	snapshot, err := h.service.ManagementSnapshot(ctx)
	if err != nil {
		return err
	}
	response, err := h.service.Management(ctx, model.ManagementRequest{Action: model.ManagementSchedulesState, ExpectedGeneration: snapshot.Generation})
	if err != nil {
		return err
	}
	if response.Generation != snapshot.Generation {
		return messageError("schedules.error.generation")
	}
	var data model.ManagementSchedulesData
	if err := model.DecodeJSON(response.Data, &data); err != nil {
		return contextualError{messageError: "schedules.error.incomplete", cause: err}
	}
	items, err := scheduler.LoadSchedules(bytes.NewReader(data.Document))
	if err != nil {
		return err
	}
	var statusDocument struct {
		Items []scheduler.ScheduleStatus `json:"items"`
	}
	if err := model.DecodeJSON(data.Statuses, &statusDocument); err != nil {
		return contextualError{messageError: "schedules.error.missing_statuses", cause: err}
	}
	if statusDocument.Items == nil {
		return messageError("schedules.error.missing_statuses")
	}
	statuses := statusDocument.Items
	// 状态必须关联同一文档；可以没有正在运行的实例，但不能将另一代或另一条
	// 日程的下次触发显示在当前草稿旁。策略原值保留，未知策略不改写成skip。
	seen := make(map[string]bool, len(statuses))
	for _, status := range statuses {
		found := false
		for _, item := range items {
			if status.Schedule == item {
				found = true
				break
			}
		}
		if !found || seen[status.Schedule.ID] || status.MissedPolicy == "" || status.NextTrigger != nil && (!status.Schedule.Enabled || status.NextTrigger.IsZero()) {
			return messageError("schedules.error.mismatched_statuses")
		}
		seen[status.Schedule.ID] = true
	}
	h.generation = snapshot.Generation
	h.draft = newDraft(items)
	h.statuses = statuses
	h.timer = data.Timer
	h.loaded = true
	h.writable = data.Writable
	h.commit = nil
	h.applySent = false
	return nil
}

func (h *handler) act(ctx context.Context, r *http.Request) (int, error) {
	action := r.PostForm.Get("action")
	if action == "" && r.PostForm.Has("selection") {
		action = "select"
	}
	if action == "query" {
		return h.query(ctx)
	}
	if action == "reload" {
		if !h.canReload() {
			return http.StatusConflict, messageError("schedules.error.query_before_reload")
		}
		if err := h.load(ctx); err != nil {
			return http.StatusBadGateway, err
		}
		h.notice = "schedules.notice.reloaded"
		return http.StatusOK, nil
	}
	if action == "cancel" {
		if !h.canCancel() {
			return http.StatusConflict, messageError("schedules.error.query_before_cancel")
		}
		h.draft = newDraft(h.draft.base)
		h.commit = nil
		h.applySent = false
		h.notice = "schedules.notice.cancelled"
		return http.StatusOK, nil
	}
	if action == "apply" {
		return h.apply(ctx, r.PostForm.Get("confirmed") == "1")
	}
	if !h.loaded || !h.writable {
		return http.StatusConflict, messageError("schedules.error.readonly")
	}
	if h.commit != nil {
		return http.StatusConflict, messageError("schedules.error.pending_commit")
	}
	if err := h.draft.capture(r.PostForm); err != nil {
		return http.StatusBadRequest, err
	}
	switch action {
	case "select":
		index, err := strconv.Atoi(r.PostForm.Get("selection"))
		if err != nil || index < 0 || index >= len(h.draft.items) {
			return http.StatusBadRequest, messageError("schedules.error.selection")
		}
		h.draft.selected = index
	case "add":
		h.draft.items = append(h.draft.items, scheduler.Schedule{Time: "09:00", Timezone: "UTC"})
		h.draft.selected = len(h.draft.items) - 1
		h.notice = "schedules.notice.added"
	case "disable":
		if h.draft.selected < 0 {
			return http.StatusBadRequest, messageError("schedules.error.choose")
		}
		h.draft.items[h.draft.selected].Enabled = false
		h.notice = "schedules.notice.disabled"
	case "save":
		data, err := h.draft.document()
		if err != nil {
			return http.StatusBadRequest, err
		}
		id, err := coreclient.NewCommitID(h.generation)
		if err != nil {
			return http.StatusInternalServerError, err
		}
		// 发送前登记本地原ID。即使Prepare应答丢失，也只能查询这一ID，不能重新
		// 生成ID保存；后续确认只应用后台已登记内容，不再携带可替换的草稿。
		h.commit = &model.CommitStatus{ID: id, Kind: model.ManagementPrepareSchedules, Phase: model.CommitUnknown, PreparedGeneration: h.generation}
		response, err := h.service.Management(ctx, model.ManagementRequest{Action: model.ManagementPrepareSchedules, ExpectedGeneration: h.generation, CommitID: id, Data: json.RawMessage(data)})
		if err != nil {
			return http.StatusBadGateway, err
		}
		if response.Generation != h.generation {
			return http.StatusBadGateway, messageError("schedules.error.prepare_generation")
		}
		if err := h.acceptCommit(response); err != nil {
			return http.StatusBadGateway, err
		}
		h.notice = "schedules.notice.prepared"
	default:
		return http.StatusBadRequest, messageError("schedules.error.action")
	}
	return http.StatusOK, nil
}

func (h *handler) acceptCommit(response model.ManagementResponse) error {
	value := response.Commit
	if value == nil || h.commit == nil || value.ID != h.commit.ID || value.Kind != "" && value.Kind != model.ManagementPrepareSchedules || value.PreparedGeneration != "" && value.PreparedGeneration != h.generation {
		return messageError("schedules.error.receipt")
	}
	if value.Phase != model.CommitUnknown && (value.Kind != model.ManagementPrepareSchedules || value.PreparedGeneration != h.generation) {
		return messageError("schedules.error.receipt_evidence")
	}
	copy := *value
	h.commit = &copy
	return nil
}

func (h *handler) apply(ctx context.Context, confirmed bool) (int, error) {
	if h.commit == nil || h.commit.Phase != model.CommitAccepted || h.applySent {
		return http.StatusConflict, messageError("schedules.error.no_prepared")
	}
	if !confirmed {
		return http.StatusBadRequest, messageError("schedules.error.confirm")
	}
	response, err := h.service.Management(ctx, model.ManagementRequest{Action: model.ManagementCommitStatus, CommitID: h.commit.ID})
	if err != nil {
		return http.StatusBadGateway, err
	}
	if err := h.acceptCommit(response); err != nil {
		return http.StatusBadGateway, err
	}
	if h.commit.Phase != model.CommitAccepted || response.Generation != h.generation {
		return http.StatusConflict, messageError("schedules.error.expired_prepare")
	}
	h.applySent = true
	response, err = h.service.Management(ctx, model.ManagementRequest{Action: model.ManagementApplyCommit, ExpectedGeneration: h.generation, CommitID: h.commit.ID})
	if err != nil {
		h.commit.Phase = model.CommitUnknown
		h.commit.Saved = nil
		h.commit.Effective = nil
		return http.StatusBadGateway, err
	}
	if err := h.acceptCommit(response); err != nil {
		h.commit.Phase = model.CommitUnknown
		h.commit.Saved = nil
		h.commit.Effective = nil
		return http.StatusBadGateway, err
	}
	h.notice = "schedules.notice.sent"
	return http.StatusOK, nil
}

func (h *handler) query(ctx context.Context) (int, error) {
	if h.commit == nil {
		return http.StatusConflict, messageError("schedules.error.no_commit")
	}
	if err := h.service.Reconnect(ctx); err != nil {
		return http.StatusBadGateway, err
	}
	response, err := h.service.Management(ctx, model.ManagementRequest{Action: model.ManagementCommitStatus, CommitID: h.commit.ID})
	if err != nil {
		return http.StatusBadGateway, err
	}
	if err := h.acceptCommit(response); err != nil {
		return http.StatusBadGateway, err
	}
	h.notice = "schedules.notice.queried"
	return http.StatusOK, nil
}

func (h *handler) catalog() i18n.Catalog { return h.service.Language().Snapshot() }

func (h *handler) canCancel() bool {
	return h.commit == nil || !h.applySent && h.commit.Phase == model.CommitAccepted
}
func (h *handler) canReload() bool {
	return h.commit == nil || (h.commit.Phase == model.CommitApplied || h.commit.Phase == model.CommitFailed) && h.commit.Saved != nil && h.commit.Effective != nil
}

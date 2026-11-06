// Package rules提供事件规则编辑页；运行规则及其持久化始终归后台管理服务所有。
package rules

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"net/http"
	"reflect"

	"github.com/orzlee/displayKvm/desktop/app"
	"github.com/orzlee/displayKvm/desktop/i18n"
	"github.com/orzlee/displayKvm/desktop/views/localize"
	"github.com/orzlee/displayKvm/internal/automation"
	coreclient "github.com/orzlee/displayKvm/internal/client"
	"github.com/orzlee/displayKvm/internal/config"
	"github.com/orzlee/displayKvm/internal/model"
)

//go:embed assets/*
var resources embed.FS

var pageTemplate = template.Must(template.New("page.html").Funcs(localize.Functions(i18n.Catalog{})).ParseFS(resources, "assets/page.html"))

// Backend只暴露管理能力，不向页面提供硬件切换、事件提交或补扫入口。
type Backend interface {
	ManagementSnapshot(context.Context) (model.ManagementSnapshot, error)
	Management(context.Context, model.ManagementRequest) (model.ManagementResponse, error)
}

func Page() app.Page {
	return app.Page{Route: "/rules/", Handler: func(service *app.Service) http.Handler { return NewHandler(service) }, ResourcePath: "/rules-assets/", Resources: resources}
}

type option struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type pageState struct {
	Connected       bool                    `json:"connected"`
	Generation      model.RuntimeGeneration `json:"generation"`
	Writable        bool                    `json:"writable"`
	EngineAvailable bool                    `json:"engine_available"`
	Rules           []automation.Rule       `json:"rules"`
	Capabilities    []model.EventCapability `json:"capabilities"`
	Targets         []option                `json:"targets"`
	Groups          []option                `json:"groups"`
	Displays        []option                `json:"displays"`
	Records         []automation.Record     `json:"records"`
	RecordsError    string                  `json:"records_error,omitempty"`
	RecordsErrorKey string                  `json:"records_error_key,omitempty"`
}

type draftRequest struct {
	Generation model.RuntimeGeneration `json:"generation"`
	CommitID   model.RequestID         `json:"commit_id,omitempty"`
	Document   json.RawMessage         `json:"document"`
}

type commitRequest struct {
	Generation model.RuntimeGeneration `json:"generation,omitempty"`
	CommitID   model.RequestID         `json:"commit_id"`
	Confirmed  bool                    `json:"confirmed,omitempty"`
}

type previewRequest struct {
	Generation model.RuntimeGeneration `json:"generation"`
	Event      model.Event             `json:"event"`
}

type handler struct{ backend Backend }

func NewHandler(backend Backend) http.Handler {
	h := &handler{backend: backend}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /rules/{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		catalog := h.catalog()
		view := struct{ Theme, Locale string }{Theme: "system", Locale: catalog.Locale}
		// 页面继承同一Service的本次外观选择；仅输出受控主题值，不另建偏好或系统监听。
		if source, ok := backend.(interface{ Snapshot() app.View }); ok {
			mode := source.Snapshot().Appearance
			if mode == "light" || mode == "dark" || mode == "system" {
				view.Theme = mode
			}
		}
		var body bytes.Buffer
		if err := localize.Execute(pageTemplate, &body, view, catalog); err != nil {
			http.Error(w, catalog.Text("common.render_error", nil), http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(body.Bytes())
	})
	mux.HandleFunc("GET /rules/state", h.state)
	mux.HandleFunc("POST /rules/validate", h.validate)
	mux.HandleFunc("POST /rules/commit-id", func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Generation model.RuntimeGeneration `json:"generation"`
		}
		if !decode(w, r, &request) {
			return
		}
		id, err := coreclient.NewCommitID(request.Generation)
		if err != nil {
			fail(w, http.StatusBadRequest, err)
			return
		}
		respond(w, http.StatusOK, map[string]model.RequestID{"commit_id": id})
	})
	mux.HandleFunc("POST /rules/prepare", h.prepare)
	mux.HandleFunc("POST /rules/apply", h.apply)
	mux.HandleFunc("POST /rules/commit", h.commit)
	mux.HandleFunc("POST /rules/preview", h.preview)
	// 草稿不属于运行引擎，也不在后台建立会话。取消只结束页面编辑，不撤销已接收的提交。
	mux.HandleFunc("POST /rules/cancel", func(w http.ResponseWriter, r *http.Request) {
		var request struct{}
		if !decode(w, r, &request) {
			return
		}
		respond(w, http.StatusOK, map[string]bool{"cancelled": true})
	})
	return mux
}

func decode(w http.ResponseWriter, r *http.Request, destination any) bool {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, model.MaxMessageBytes))
	if err == nil {
		err = model.DecodeJSON(data, destination)
	}
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return false
	}
	return true
}

func respond(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func fail(w http.ResponseWriter, status int, err error) {
	view := struct {
		Message       string            `json:"message,omitempty"`
		SummaryKey    string            `json:"summary_key"`
		SummaryValues map[string]string `json:"summary_values,omitempty"`
		Code          model.ErrorCode   `json:"code,omitempty"`
	}{Message: err.Error(), SummaryKey: "rules.error.operation"}
	var local *messageError
	var backend *model.Error
	switch {
	case errors.As(err, &local):
		view.SummaryKey, view.SummaryValues, view.Message = local.Key, local.Values, local.Detail
	case errors.As(err, &backend):
		view.Code, view.SummaryKey = backend.Code, "error."+string(backend.Code)
	}
	respond(w, status, view)
}

// 页面本地错误保留语义键和原始能力诊断，JSON消费者用同一Catalog呈现摘要。
// 不缓存某次语言下的文本，避免保存请求与语言切换先后发生时显示过期语言。
type messageError struct {
	i18n.Message
	Detail string
}

func (e *messageError) Error() string { return e.Key }

func (h *handler) catalog() i18n.Catalog {
	if source, ok := h.backend.(interface{ Language() *i18n.Manager }); ok {
		return source.Language().Snapshot()
	}
	language, _ := i18n.New("system", nil)
	return language.Snapshot()
}

func (h *handler) snapshot(ctx context.Context) (model.ManagementSnapshot, config.Config, []automation.Rule, error) {
	if h.backend == nil {
		return model.ManagementSnapshot{}, config.Config{}, nil, &messageError{Message: i18n.Message{Key: "rules.error.connect"}}
	}
	snapshot, err := h.backend.ManagementSnapshot(ctx)
	if err != nil {
		return snapshot, config.Config{}, nil, err
	}
	if err := (model.ManagementResponse{Generation: snapshot.Generation, Snapshot: &snapshot}).Validate(); err != nil {
		return snapshot, config.Config{}, nil, err
	}
	cfg, err := config.Load(bytes.NewReader(snapshot.Configuration))
	if err != nil {
		return snapshot, cfg, nil, err
	}
	rules, err := automation.LoadRules(bytes.NewReader(snapshot.Rules))
	if err == nil {
		err = automation.ValidateRules(cfg, rules)
	}
	return snapshot, cfg, rules, err
}

func (h *handler) checkedDraft(ctx context.Context, request draftRequest) ([]automation.Rule, error) {
	snapshot, cfg, existing, err := h.snapshot(ctx)
	if err != nil {
		return nil, err
	}
	if request.Generation != snapshot.Generation {
		return nil, &messageError{Message: i18n.Message{Key: "rules.error.draft_generation"}}
	}
	if !snapshot.RulesWritable {
		return nil, &messageError{Message: i18n.Message{Key: "rules.error.readonly"}}
	}
	rules, err := automation.LoadRules(bytes.NewReader(request.Document))
	if err != nil {
		return nil, err
	}
	if err := automation.ValidateRules(cfg, rules); err != nil {
		return nil, err
	}
	if err := validateCapabilities(snapshot.Capabilities, existing, rules); err != nil {
		return nil, err
	}
	return rules, nil
}

// 能力校验只约束编辑入口，不复制事件匹配、冲突或时间裁决。保留未修改的历史条件，
// 允许在平台能力临时缺失时停用旧规则；事件规则重新启用或更改条件须取得真实能力。
// 既有USB状态规则综合插入、移除与补扫，不能用一个事件能力替代其状态判定，
// 因而只保留其原条件并允许启停或修改动作，禁止在此事件编辑器新增状态条件。
func validateCapabilities(capabilities []model.EventCapability, existing, rules []automation.Rule) error {
	for _, rule := range rules {
		unchanged := false
		for _, old := range existing {
			if old.ID == rule.ID && reflect.DeepEqual(old.When, rule.When) && (!rule.Enabled || old.Enabled || rule.When.USBPresent != nil) {
				unchanged = true
				break
			}
		}
		if unchanged {
			continue
		}
		if rule.When.USBPresent != nil {
			return &messageError{Message: i18n.Message{Key: "rules.error.usb_state"}}
		}
		var capability *model.EventCapability
		for i := range capabilities {
			if capabilities[i].Kind == rule.When.Kind {
				capability = &capabilities[i]
				break
			}
		}
		if capability == nil {
			return &messageError{Message: i18n.Message{Key: "rules.error.missing_capability", Values: map[string]string{"id": rule.ID}}}
		}
		if !capability.Available {
			return &messageError{Message: i18n.Message{Key: "rules.error.unavailable_capability", Values: map[string]string{"id": rule.ID}}, Detail: capability.Reason}
		}
		fields := make(map[string]bool, len(capability.FilterFields))
		for _, field := range capability.FilterFields {
			fields[field] = true
		}
		used := map[string]bool{"source": rule.When.Source != "", "name": rule.When.Name != ""}
		if usb := rule.When.USB; usb != nil {
			used["usb.vendor_id"], used["usb.product_id"] = usb.VendorID != nil, usb.ProductID != nil
			used["usb.serial"], used["usb.path"] = usb.Serial != "", usb.Path != ""
		}
		for key := range rule.When.Attributes {
			used["attributes."+key] = true
		}
		for field, present := range used {
			if present && !fields[field] {
				return &messageError{Message: i18n.Message{Key: "rules.error.unsupported_filter", Values: map[string]string{"id": rule.ID, "field": field}}}
			}
		}
	}
	return nil
}

func (h *handler) validate(w http.ResponseWriter, r *http.Request) {
	var request draftRequest
	if !decode(w, r, &request) {
		return
	}
	if _, err := h.checkedDraft(r.Context(), request); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	respond(w, http.StatusOK, map[string]bool{"valid": true})
}

func (h *handler) state(w http.ResponseWriter, r *http.Request) {
	snapshot, cfg, rules, err := h.snapshot(r.Context())
	if err != nil {
		fail(w, http.StatusServiceUnavailable, err)
		return
	}
	view := pageState{Connected: true, Generation: snapshot.Generation, Writable: snapshot.RulesWritable, EngineAvailable: snapshot.EngineAvailable,
		Rules: append([]automation.Rule{}, rules...), Capabilities: append([]model.EventCapability{}, snapshot.Capabilities...),
		Targets: []option{}, Groups: []option{}, Displays: []option{}, Records: []automation.Record{}}
	for _, target := range cfg.Targets {
		view.Targets = append(view.Targets, option{ID: string(target.ID), Name: target.Name})
	}
	for _, group := range cfg.Groups {
		view.Groups = append(view.Groups, option{ID: string(group.ID), Name: group.Name})
	}
	for _, display := range cfg.Displays {
		view.Displays = append(view.Displays, option{ID: string(display.ID), Name: display.Name})
	}
	if snapshot.EngineAvailable {
		if err := h.readData(r.Context(), model.ManagementRequest{Action: model.ManagementRuleRecords, ExpectedGeneration: snapshot.Generation}, &view.Records); err != nil {
			view.RecordsErrorKey, view.RecordsError = "rules.records.unreadable", err.Error()
		}
	} else {
		view.RecordsErrorKey = "rules.records.stopped"
	}
	respond(w, http.StatusOK, view)
}

func (h *handler) readData(ctx context.Context, request model.ManagementRequest, destination any) error {
	response, err := h.backend.Management(ctx, request)
	if err != nil {
		return err
	}
	if err := response.Validate(); err != nil {
		return err
	}
	if response.Generation != request.ExpectedGeneration {
		return &messageError{Message: i18n.Message{Key: "rules.error.read_generation"}}
	}
	if len(response.Data) == 0 {
		return &messageError{Message: i18n.Message{Key: "rules.error.missing_data"}}
	}
	return json.Unmarshal(response.Data, destination)
}

func (h *handler) preview(w http.ResponseWriter, r *http.Request) {
	var request previewRequest
	if !decode(w, r, &request) {
		return
	}
	if err := request.Event.Validate(); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	snapshot, _, _, err := h.snapshot(r.Context())
	if err != nil {
		fail(w, http.StatusServiceUnavailable, err)
		return
	}
	if request.Generation != snapshot.Generation {
		fail(w, http.StatusConflict, &messageError{Message: i18n.Message{Key: "rules.error.preview_generation"}})
		return
	}
	if !snapshot.EngineAvailable {
		fail(w, http.StatusConflict, &messageError{Message: i18n.Message{Key: "rules.error.engine"}})
		return
	}
	available := false
	for _, capability := range snapshot.Capabilities {
		if capability.Kind == request.Event.Kind {
			available = capability.Available
			break
		}
	}
	if !available {
		fail(w, http.StatusBadRequest, &messageError{Message: i18n.Message{Key: "rules.error.preview_capability"}})
		return
	}
	data, err := json.Marshal(model.ManagementPreviewRulesData{Event: request.Event})
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	decisions := []automation.Decision{}
	if err := h.readData(r.Context(), model.ManagementRequest{Action: model.ManagementPreviewRules, ExpectedGeneration: request.Generation, Data: data}, &decisions); err != nil {
		fail(w, http.StatusServiceUnavailable, err)
		return
	}
	respond(w, http.StatusOK, struct {
		Generation model.RuntimeGeneration `json:"generation"`
		Decisions  []automation.Decision   `json:"decisions"`
	}{Generation: snapshot.Generation, Decisions: decisions})
}

func (h *handler) prepare(w http.ResponseWriter, r *http.Request) {
	var request draftRequest
	if !decode(w, r, &request) {
		return
	}
	if _, err := h.checkedDraft(r.Context(), request); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	management := model.ManagementRequest{Action: model.ManagementPrepareRules, ExpectedGeneration: request.Generation, CommitID: request.CommitID, Data: request.Document}
	if err := management.Validate(); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	h.manage(w, r, management)
}

func (h *handler) apply(w http.ResponseWriter, r *http.Request) {
	var request commitRequest
	if !decode(w, r, &request) {
		return
	}
	if !request.Confirmed {
		fail(w, http.StatusBadRequest, &messageError{Message: i18n.Message{Key: "rules.error.confirm"}})
		return
	}
	h.manage(w, r, model.ManagementRequest{Action: model.ManagementApplyCommit, ExpectedGeneration: request.Generation, CommitID: request.CommitID, Confirmed: true})
}

func (h *handler) commit(w http.ResponseWriter, r *http.Request) {
	var request commitRequest
	if !decode(w, r, &request) {
		return
	}
	h.manage(w, r, model.ManagementRequest{Action: model.ManagementCommitStatus, CommitID: request.CommitID})
}

func (h *handler) manage(w http.ResponseWriter, r *http.Request, request model.ManagementRequest) {
	if err := request.Validate(); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	if h.backend == nil {
		fail(w, http.StatusServiceUnavailable, &messageError{Message: i18n.Message{Key: "rules.error.connect"}})
		return
	}
	if request.Action == model.ManagementApplyCommit {
		// Apply共用于配置、校准和规则；先只读核实原申请，避免规则页应用其他页面的提交。
		// 查询不替代后台最终运行代校验；发生竞争或断连时拒绝发送，不自动重放任何写操作。
		prior, err := h.backend.Management(r.Context(), model.ManagementRequest{Action: model.ManagementCommitStatus, CommitID: request.CommitID})
		if err == nil {
			err = ruleCommit(prior, request.CommitID)
		}
		if err != nil {
			fail(w, http.StatusServiceUnavailable, err)
			return
		}
		if prior.Generation != request.ExpectedGeneration || prior.Commit.PreparedGeneration != request.ExpectedGeneration || prior.Commit.Phase != model.CommitAccepted {
			fail(w, http.StatusConflict, &messageError{Message: i18n.Message{Key: "rules.error.changed_commit"}})
			return
		}
	}
	response, err := h.backend.Management(r.Context(), request)
	if err != nil {
		fail(w, http.StatusServiceUnavailable, err)
		return
	}
	if err := ruleCommit(response, request.CommitID); err != nil {
		fail(w, http.StatusBadGateway, err)
		return
	}
	respond(w, http.StatusOK, response)
}

func ruleCommit(response model.ManagementResponse, id model.RequestID) error {
	if err := response.Validate(); err != nil {
		return err
	}
	if response.Commit == nil || response.Commit.ID != id || response.Commit.Kind != model.ManagementPrepareRules {
		return &messageError{Message: i18n.Message{Key: "rules.error.receipt"}}
	}
	return nil
}

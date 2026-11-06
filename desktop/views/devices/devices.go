// Package devices通过桌面公共服务呈现设备配置与校准，不持有硬件后端。
package devices

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"io/fs"
	"net/http"
	"strings"
	"sync"

	"github.com/orzlee/displayKvm/desktop/app"
	"github.com/orzlee/displayKvm/desktop/i18n"
	"github.com/orzlee/displayKvm/internal/config"
	"github.com/orzlee/displayKvm/internal/diagnostics"
	"github.com/orzlee/displayKvm/internal/model"
)

func Page() app.Page {
	assets, err := fs.Sub(resources, "assets")
	if err != nil {
		panic(err)
	}
	return app.Page{Route: "/devices/", Handler: NewHandler, ResourcePath: "/devices-assets/", Resources: assets}
}

type handler struct {
	service *app.Service
	token   string
	mu      sync.Mutex
	opMu    sync.Mutex
	state   pageState
}

type pageState struct {
	generation     model.RuntimeGeneration
	cfg            config.Config
	devices        []model.Device
	selected       model.DisplayID
	draft          draftView
	diagnostic     *diagnostics.Diagnostic
	calibration    *model.CalibrationSession
	trial          *model.CalibrationSession
	progress       diagnostics.TrialProgress
	trialInFlight  bool
	trialUncertain bool
	observed       map[string]bool
	commit         *model.CommitStatus
	commitKnown    bool
	applyAttempted bool
	error          error
	notice         i18n.Message
}

// 调用方持有handler.mu。未知结果继续占用本次流程，不能因为网络等待结束就
// 允许另一份配置或试切覆盖尚未核清的硬件依据。
func (s *pageState) hasOpenTrial() bool {
	return s.trialInFlight || s.trialUncertain || s.trial != nil && !s.progress.Stopped && s.progress.Next < s.progress.Total
}

func (s *pageState) hasPendingCommit() bool {
	return s.commit != nil && s.commit.Phase != model.CommitApplied && s.commit.Phase != model.CommitFailed
}

func NewHandler(service *app.Service) http.Handler {
	var nonce [32]byte
	if service == nil {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			manager, err := i18n.New("system", nil)
			if err != nil {
				http.Error(w, err.Error(), http.StatusServiceUnavailable)
				return
			}
			http.Error(w, manager.Snapshot().Text("devices.error.service_required", nil), http.StatusServiceUnavailable)
		})
	}
	if _, err := rand.Read(nonce[:]); err != nil {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, service.Language().Snapshot().Text("devices.error.form_init", nil), http.StatusInternalServerError)
		})
	}
	return &handler{service: service, token: hex.EncodeToString(nonce[:]), state: pageState{observed: make(map[string]bool)}}
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.Path == "/devices/" && r.Method == http.MethodGet {
		if !h.opMu.TryLock() {
			h.render(w, http.StatusOK)
			return
		}
		defer h.opMu.Unlock()
		if err := h.load(r.Context()); err != nil {
			h.setError(err)
		} else if err := h.choose(r.URL.Query()); err != nil {
			h.setError(err)
		}
		h.render(w, http.StatusOK)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, h.service.Language().Snapshot().Text("common.method", nil), http.StatusMethodNotAllowed)
		return
	}
	if !strings.HasPrefix(r.URL.Path, "/devices/") {
		http.NotFound(w, r)
		return
	}
	// 设备页采用原生表单而非前端业务副本。每次提交必须携带当前页面随机凭据，
	// 防止其他页面用普通跨站表单触发本地保存或硬件操作；大小上限先于解析执行。
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") || r.ParseForm() != nil {
		http.Error(w, h.service.Language().Snapshot().Text("devices.error.form_invalid", nil), http.StatusBadRequest)
		return
	}
	if subtle.ConstantTimeCompare([]byte(r.PostForm.Get("_token")), []byte(h.token)) != 1 {
		http.Error(w, h.service.Language().Snapshot().Text("devices.error.form_expired", nil), http.StatusForbidden)
		return
	}
	for _, values := range r.PostForm {
		if len(values) != 1 {
			http.Error(w, h.service.Language().Snapshot().Text("devices.error.form_duplicate", nil), http.StatusBadRequest)
			return
		}
	}
	stop := r.URL.Path == "/devices/trial/stop"
	if !stop {
		if !h.opMu.TryLock() {
			http.Error(w, h.service.Language().Snapshot().Text("devices.error.operation_pending", nil), http.StatusConflict)
			return
		}
		defer h.opMu.Unlock()
	}
	if err := h.action(r); err != nil {
		h.setError(err)
		h.render(w, http.StatusBadRequest)
		return
	}
	// 成功操作统一跳回只读页，浏览器刷新不会再次提交Apply或试切。
	http.Redirect(w, r, "/devices/", http.StatusSeeOther)
}

func (h *handler) render(w http.ResponseWriter, status int) {
	view := h.view()
	if err := renderPage(w, status, view); err != nil {
		// renderPage在模板完整生成前不会写响应；这里可保留真实失败状态。
		http.Error(w, h.service.Language().Snapshot().Text("common.render_error", nil), http.StatusInternalServerError)
	}
}

func (h *handler) setError(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.state.error = err
	h.state.notice = i18n.Message{}
}

func freshID(prefix string) (string, error) {
	var value [8]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	id := prefix + hex.EncodeToString(value[:])
	if !model.ValidID(id) {
		return "", pageError("devices.error.operation_id", nil)
	}
	return id, nil
}

// 本页门禁保存语义键；包装后台失败时保留原错误链，不能把未知提交或试切
// 的结构化错误改成只有一段翻译文字，否则会丢失错误代码和诊断字段。
type localizedError struct {
	key   string
	cause error
}

func (e *localizedError) Error() string {
	if e.cause != nil {
		return e.key + ": " + e.cause.Error()
	}
	return e.key
}

func (e *localizedError) Unwrap() error { return e.cause }

func pageError(key string, cause error) error { return &localizedError{key: key, cause: cause} }

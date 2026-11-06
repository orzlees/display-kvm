// Package overview提供实际窗口使用的同源处理器；选择和切换规则统一交给app。
package overview

import (
	"encoding/json"
	"io"
	"log"
	"net/http"

	"github.com/orzlee/displayKvm/desktop/app"
	"github.com/orzlee/displayKvm/desktop/app/presentation"
	"github.com/orzlee/displayKvm/internal/model"
)

func NewHandler(service *app.Service) http.Handler {
	mux := http.NewServeMux()
	respond := func(w http.ResponseWriter, code int, err error) {
		response := struct {
			View       app.View                 `json:"view"`
			Error      string                   `json:"error,omitempty"`
			Diagnostic *presentation.Diagnostic `json:"diagnostic,omitempty"`
		}{View: service.Snapshot()}
		if err != nil {
			message := presentation.MessageForError(err)
			response.Error = service.Language().Snapshot().Text(message.Key, message.Values)
			response.Diagnostic = presentation.DiagnosticFromError(err)
		}
		w.WriteHeader(code)
		if err := json.NewEncoder(w).Encode(response); err != nil {
			// 响应可能部分写出，不能再追加另一份错误正文；保留关闭窗口或中断的根因。
			log.Printf("输出桌面状态失败：%v", err)
		}
	}
	mux.HandleFunc("GET /client/language", func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewEncoder(w).Encode(service.Language().Snapshot()); err != nil {
			log.Printf("输出界面语言失败：%v", err)
		}
	})
	decode := func(w http.ResponseWriter, r *http.Request, value any) bool {
		r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(value); err != nil {
			respond(w, http.StatusBadRequest, model.NewError(model.CodeInvalidRequest, "无法读取此操作，请刷新后重试", err))
			return false
		}
		if err := decoder.Decode(new(any)); err != io.EOF {
			respond(w, http.StatusBadRequest, model.NewError(model.CodeInvalidRequest, "操作内容不完整，请刷新后重试", err))
			return false
		}
		return true
	}
	mux.HandleFunc("GET /client/status", func(w http.ResponseWriter, r *http.Request) { respond(w, http.StatusOK, nil) })
	for _, action := range []string{"connect", "refresh"} {
		mux.HandleFunc("POST /client/"+action, func(w http.ResponseWriter, r *http.Request) {
			var err error
			if action == "connect" {
				err = service.Reconnect(r.Context())
			} else {
				err = service.Refresh(r.Context())
			}
			if err != nil {
				log.Printf("更新桌面后台连接失败：%v", err)
				// HTTP状态仍表示当前不可用；摘要按原错误代码翻译，原诊断和原因链
				// 交给独立Diagnostic保留，不能用一条通用文字替换真实失败证据。
				respond(w, http.StatusServiceUnavailable, err)
				return
			}
			respond(w, http.StatusOK, nil)
		})
	}
	for _, action := range []string{"selection", "switch"} {
		mux.HandleFunc("POST /client/"+action, func(w http.ResponseWriter, r *http.Request) {
			var choice app.Selection
			if !decode(w, r, &choice) {
				return
			}
			var err error
			code := http.StatusOK
			if action == "switch" {
				err, code = service.BeginSwitch(choice), http.StatusAccepted
			} else {
				err = service.SetSelection(choice)
			}
			if err != nil {
				respond(w, http.StatusConflict, err)
				return
			}
			respond(w, code, nil)
		})
	}
	mux.HandleFunc("POST /client/appearance", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Mode string `json:"mode"`
		}
		if !decode(w, r, &body) {
			return
		}
		if err := service.SetAppearance(body.Mode); err != nil {
			respond(w, http.StatusBadRequest, err)
			return
		}
		respond(w, http.StatusOK, nil)
	})
	mux.HandleFunc("POST /client/confirm", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			RequestID model.RequestID `json:"requestID"`
			DisplayID model.DisplayID `json:"displayID"`
		}
		if !decode(w, r, &body) {
			return
		}
		if err := service.Confirm(body.RequestID, body.DisplayID); err != nil {
			respond(w, http.StatusConflict, err)
			return
		}
		respond(w, http.StatusOK, nil)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		mux.ServeHTTP(w, r)
	})
}

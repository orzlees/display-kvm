package app

import (
	"context"
	"io/fs"
	"net/http"

	appclient "github.com/orzlee/displayKvm/desktop/app/client"
	"github.com/orzlee/displayKvm/desktop/i18n"
	"github.com/orzlee/displayKvm/internal/model"
)

// Page由纯Go页面构造；原生宿主只在最终入口做同形类型转换，页面测试不加载原生库。
type Page struct {
	Route        string
	Handler      func(*Service) http.Handler
	ResourcePath string
	Resources    fs.FS
}

func (s *Service) ManagementSnapshot(ctx context.Context) (model.ManagementSnapshot, error) {
	response, err := s.Management(ctx, model.ManagementRequest{Action: model.ManagementSnapshotAction})
	if err != nil {
		return model.ManagementSnapshot{}, err
	}
	if response.Snapshot == nil {
		return model.ManagementSnapshot{}, s.languageError("app.management.snapshot_missing", nil)
	}
	return *response.Snapshot, nil
}

// 所有页面共用原有busy门禁。停止只取消同一后台试切，不声称撤销已发操作，
// 因此允许在试切等待期间进入；其余管理调用不能与桌面切换并发派发。
func (s *Service) Management(ctx context.Context, request model.ManagementRequest) (model.ManagementResponse, error) {
	if err := request.Validate(); err != nil {
		return model.ManagementResponse{}, err
	}
	s.mu.Lock()
	stopTrial := request.Action == model.ManagementTrialStop
	allowedStop := stopTrial && s.managementAction == model.ManagementTrialStep && s.managementSession == request.SessionID
	if s.closed || s.connecting || s.busy && !allowedStop {
		s.mu.Unlock()
		return model.ManagementResponse{}, s.languageError("app.wait.operation", nil)
	}
	client, ok := s.client.(appclient.ManagementClient)
	if !s.connected || !ok {
		s.mu.Unlock()
		return model.ManagementResponse{}, model.NewError(model.CodeUnsupportedOperation, s.language.Snapshot().Text("app.management.unsupported", nil), nil)
	}
	if !stopTrial {
		s.busy = true
		s.managementAction, s.managementSession = request.Action, request.SessionID
	}
	s.changedLocked()
	s.mu.Unlock()
	bounded, cancel := context.WithTimeout(ctx, model.ResponseTimeout)
	stop := context.AfterFunc(s.ctx, cancel)
	defer cancel()
	defer stop()
	response, err := client.Management(bounded, request)
	if err == nil {
		err = response.Validate()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !stopTrial {
		s.busy = false
		s.managementAction, s.managementSession = "", ""
	}
	if err != nil {
		s.message, s.messageError = messageForError(err), err
		if request.Action == model.ManagementApplyCommit || request.Action == model.ManagementPrepareConfig || request.Action == model.ManagementPrepareRules || request.Action == model.ManagementPrepareCalibration || request.Action == model.ManagementPrepareSchedules {
			s.managementCommit = &model.CommitStatus{ID: request.CommitID, Kind: request.Action, Phase: model.CommitUnknown, PreparedGeneration: request.ExpectedGeneration}
			if request.Action == model.ManagementApplyCommit {
				s.managementCommit.Kind = ""
			}
		}
		// Apply已可能被后台接受，不能把断连改写成保存失败，更不能自动重发。
		if request.Action == model.ManagementApplyCommit {
			s.connected = false
			s.connectionMessage, s.connectionError = i18n.Message{Key: "app.management.pending_query"}, err
		}
	} else {
		if s.managementGeneration != "" && s.managementGeneration != response.Generation {
			s.generation++
			s.selection = Selection{Kind: "display", Generation: s.generation}
			s.managementSnapshot = nil
		}
		s.managementGeneration = response.Generation
		if response.Snapshot != nil {
			s.managementSnapshot = response.Snapshot
		}
		if response.Commit != nil {
			value := *response.Commit
			s.managementCommit = &value
		}
		if request.Action == model.ManagementApplyCommit {
			s.connected = false
			s.connectionMessage, s.connectionError = i18n.Message{Key: "app.management.applying"}, nil
		}
	}
	s.changedLocked()
	return response, err
}

package linux

import (
	"bytes"
	"context"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/orzlee/displayKvm/internal/model"
)

const (
	loginService = "org.freedesktop.login1"
	loginPath    = "/org/freedesktop/login1"
	loginManager = "org.freedesktop.login1.Manager"
	loginSession = "org.freedesktop.login1.Session"
)

type busValue struct {
	Type string            `json:"type"`
	Data []json.RawMessage `json:"data"`
}
type logindIdentity struct{ owner, session string }
type logindSignal struct {
	kind          model.EventKind
	session, path string
	removed       bool
}

func decodeBusValue(data []byte, signature string, count int) (busValue, error) {
	var v busValue
	if err := json.Unmarshal(data, &v); err != nil {
		return v, model.NewError(model.CodeUnsupportedProtocol, "busctl JSON无法解析", err)
	}
	if v.Type != signature || len(v.Data) != count {
		return v, model.NewError(model.CodeUnsupportedProtocol, "busctl返回签名或字段数量不符", nil)
	}
	return v, nil
}

func (s *Source) bus(ctx context.Context, service, object, iface, method, signature string, arguments ...string) ([]byte, error) {
	args := []string{"--system", "--json=short", "--no-pager", "--auto-start=no", "--timeout=5s", "call", service, object, iface, method}
	if signature != "" {
		args = append(args, signature)
		args = append(args, arguments...)
	}
	return s.runner.run(ctx, s.config.BusctlPath, args...)
}

func (s *Source) property(ctx context.Context, object, property string) ([]byte, error) {
	return s.runner.run(ctx, s.config.BusctlPath, "--system", "--json=short", "--no-pager", "--auto-start=no", "--timeout=5s", "get-property", loginService, object, loginSession, property)
}

func (s *Source) probeLogind(ctx context.Context) (*logindIdentity, error) {
	if err := validCommandPath(s.config.BusctlPath); err != nil {
		return nil, err
	}
	version, err := s.runner.run(ctx, s.config.BusctlPath, "--version")
	if err != nil {
		return nil, err
	}
	parts := strings.Fields(string(version))
	if len(parts) < 2 || parts[0] != "systemd" || (parts[1] != "257" && parts[1] != "258") {
		return nil, model.NewError(model.CodeUnsupportedVersion, "busctl监控及就绪协议仅核查systemd 257/258", nil)
	}
	data, err := s.bus(ctx, "org.freedesktop.DBus", "/org/freedesktop/DBus", "org.freedesktop.DBus", "GetNameOwner", "s", loginService)
	if err != nil {
		return nil, err
	}
	v, err := decodeBusValue(data, "s", 1)
	if err != nil {
		return nil, err
	}
	id := &logindIdentity{session: s.config.SessionID}
	if err := json.Unmarshal(v.Data[0], &id.owner); err != nil || !strings.HasPrefix(id.owner, ":") {
		return nil, model.NewError(model.CodeUnsupportedProtocol, "logind未返回真实总线唯一名称", err)
	}
	if id.session == "" {
		data, err = s.bus(ctx, loginService, loginPath, loginManager, "GetSessionByPID", "u", strconv.Itoa(s.pid))
		if err != nil {
			return nil, err
		}
		v, err = decodeBusValue(data, "o", 1)
		if err != nil {
			return nil, err
		}
		var object string
		if err := json.Unmarshal(v.Data[0], &object); err != nil {
			return nil, model.NewError(model.CodeUnsupportedProtocol, "当前进程会话路径无效", err)
		}
		data, err = s.property(ctx, object, "Id")
		if err != nil {
			return nil, err
		}
		v, err = decodeBusValue(data, "s", 1)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(v.Data[0], &id.session); err != nil {
			return nil, model.NewError(model.CodeUnsupportedProtocol, "当前会话ID无效", err)
		}
		if err := s.verifySession(ctx, object, id.session); err != nil {
			return nil, err
		}
	}
	if id.session == "" || len(id.session) > 128 || strings.ContainsAny(id.session, "\n\r\x00/") {
		return nil, model.NewError(model.CodeInvalidConfig, "需要可核查的当前或显式目标会话ID", nil)
	}
	// 显式目标可能暂时缺席，此时列表扫描可明确返回false；这不等于已经确认
	// 未来该ID属于当前用户。SessionNew到达时仍须查询对象User和Id后才能发事件。
	if _, err := s.sessionSnapshot(ctx, id); err != nil {
		return nil, err
	}
	return id, nil
}

func (s *Source) verifySession(ctx context.Context, object, target string) error {
	if !strings.HasPrefix(object, loginPath+"/session/") || strings.ContainsAny(object, "\n\r\x00") {
		return model.NewError(model.CodeUnsupportedProtocol, "logind会话对象路径无效", nil)
	}
	data, err := s.property(ctx, object, "User")
	if err != nil {
		return err
	}
	v, err := decodeBusValue(data, "(uo)", 1)
	if err != nil {
		return err
	}
	var user []json.RawMessage
	if err := json.Unmarshal(v.Data[0], &user); err != nil || len(user) != 2 {
		return model.NewError(model.CodeUnsupportedProtocol, "会话用户结构无效", err)
	}
	var uid uint32
	if err := json.Unmarshal(user[0], &uid); err != nil || bytes.Equal(bytes.TrimSpace(user[0]), []byte("null")) {
		return model.NewError(model.CodeUnsupportedProtocol, "会话UID无效", err)
	}
	if uid != uint32(s.uid) {
		return model.NewError(model.CodePermissionDenied, "目标会话属于其他UID", nil)
	}
	data, err = s.property(ctx, object, "Id")
	if err != nil {
		return err
	}
	v, err = decodeBusValue(data, "s", 1)
	if err != nil {
		return err
	}
	var id string
	if err := json.Unmarshal(v.Data[0], &id); err != nil || id != target {
		return model.NewError(model.CodeIdentityAmbiguous, "会话路径与目标ID不一致", err)
	}
	return nil
}

func (s *Source) sessionSnapshot(ctx context.Context, id *logindIdentity) (bool, error) {
	data, err := s.bus(ctx, loginService, loginPath, loginManager, "ListSessions", "")
	if err != nil {
		return false, err
	}
	return sessionInList(data, id.session, uint32(s.uid))
}

func sessionInList(data []byte, target string, uid uint32) (bool, error) {
	v, err := decodeBusValue(data, "a(susso)", 1)
	if err != nil {
		return false, err
	}
	var entries [][]json.RawMessage
	if raw := bytes.TrimSpace(v.Data[0]); len(raw) == 0 || raw[0] != '[' {
		return false, model.NewError(model.CodeUnsupportedProtocol, "logind会话列表不是数组", nil)
	}
	if err := json.Unmarshal(v.Data[0], &entries); err != nil {
		return false, model.NewError(model.CodeUnsupportedProtocol, "logind会话列表无法解析", err)
	}
	for _, entry := range entries {
		if len(entry) != 5 {
			return false, model.NewError(model.CodeUnsupportedProtocol, "logind会话列表字段不完整", nil)
		}
		var id string
		var owner uint32
		if err := json.Unmarshal(entry[0], &id); err != nil || id == "" {
			return false, model.NewError(model.CodeUnsupportedProtocol, "会话列表ID无效", err)
		}
		if id != target {
			continue
		}
		if err := json.Unmarshal(entry[1], &owner); err != nil || bytes.Equal(bytes.TrimSpace(entry[1]), []byte("null")) {
			return false, model.NewError(model.CodeUnsupportedProtocol, "会话列表UID无效", err)
		}
		if owner != uid {
			return false, model.NewError(model.CodePermissionDenied, "目标会话属于其他UID", nil)
		}
		return true, nil
	}
	return false, nil
}

func logindArguments(owner string) []string {
	// 只订阅系统总线的管理器事实及服务所有者变化；不订阅桌面用户总线，
	// Lock/Unlock是请求而非完成事实，不能映射成会话启动或唤醒。
	return []string{"--system", "--json=short", "--no-pager",
		"--match=type='signal',sender='" + owner + "',path='" + loginPath + "',interface='" + loginManager + "'",
		"--match=type='signal',sender='org.freedesktop.DBus',interface='org.freedesktop.DBus',member='NameOwnerChanged',arg0='" + loginService + "'", "monitor"}
}

func parseLogindSignal(line, owner string) (logindSignal, error) {
	var message struct {
		Type, Sender, Path, Interface, Member string
		Payload                               busValue
	}
	var signal logindSignal
	if err := json.Unmarshal([]byte(line), &message); err != nil {
		return signal, model.NewError(model.CodeUnsupportedProtocol, "busctl监控JSON无法解析", err)
	}
	if message.Type != "signal" {
		return signal, nil
	}
	if message.Sender == "org.freedesktop.DBus" && message.Interface == "org.freedesktop.DBus" && message.Member == "NameOwnerChanged" {
		if message.Payload.Type != "sss" || len(message.Payload.Data) != 3 {
			return signal, model.NewError(model.CodeUnsupportedProtocol, "服务所有者通知格式无效", nil)
		}
		var service, next string
		if err := json.Unmarshal(message.Payload.Data[0], &service); err != nil {
			return signal, model.NewError(model.CodeUnsupportedProtocol, "服务所有者名称无效", err)
		}
		if err := json.Unmarshal(message.Payload.Data[2], &next); err != nil {
			return signal, model.NewError(model.CodeUnsupportedProtocol, "服务所有者身份无效", err)
		}
		if service == loginService && next != owner {
			return signal, model.NewError(model.CodeTransport, "logind服务所有者已变化，需重新核查身份", nil)
		}
		return signal, nil
	}
	if message.Sender != owner || message.Path != loginPath || message.Interface != loginManager {
		return signal, nil
	}
	switch message.Member {
	case "PrepareForSleep":
		if message.Payload.Type != "b" || len(message.Payload.Data) != 1 {
			return signal, model.NewError(model.CodeUnsupportedProtocol, "睡眠通知签名无效", nil)
		}
		var sleeping *bool
		if err := json.Unmarshal(message.Payload.Data[0], &sleeping); err != nil || sleeping == nil {
			return signal, model.NewError(model.CodeUnsupportedProtocol, "睡眠通知不是布尔值", err)
		}
		if !*sleeping {
			signal.kind = model.EventResume
		}
	case "SessionNew", "SessionRemoved":
		if message.Payload.Type != "so" || len(message.Payload.Data) != 2 {
			return signal, model.NewError(model.CodeUnsupportedProtocol, "会话通知签名无效", nil)
		}
		if err := json.Unmarshal(message.Payload.Data[0], &signal.session); err != nil {
			return signal, model.NewError(model.CodeUnsupportedProtocol, "会话通知ID无效", err)
		}
		if err := json.Unmarshal(message.Payload.Data[1], &signal.path); err != nil {
			return signal, model.NewError(model.CodeUnsupportedProtocol, "会话通知路径无效", err)
		}
		if signal.session == "" || len(signal.session) > 128 || !strings.HasPrefix(signal.path, loginPath+"/session/") {
			return signal, model.NewError(model.CodeUnsupportedProtocol, "会话通知缺少具体ID或对象路径", nil)
		}
		signal.removed = message.Member == "SessionRemoved"
		if !signal.removed {
			signal.kind = model.EventSessionStarted
		}
	}
	return signal, nil
}

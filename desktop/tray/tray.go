// Package tray描述托盘动作；原生菜单只消费该模型，不再实现一套切换规则。
package tray

import (
	"context"
	"errors"

	"github.com/orzlee/displayKvm/desktop/app"
	"github.com/orzlee/displayKvm/desktop/i18n"
	"github.com/orzlee/displayKvm/internal/model"
)

type Entry struct {
	Kind      string
	Label     string
	Enabled   bool
	Selection app.Selection
}

type Controller struct {
	service  *app.Service
	show     func()
	quit     func()
	startup  func()
	language *i18n.Manager
}

func New(service *app.Service, show, quit func()) *Controller {
	return NewWithLanguage(service, show, quit, nil)
}

func NewWithLanguage(service *app.Service, show, quit func(), language *i18n.Manager) *Controller {
	if language == nil && service != nil {
		language = service.Language()
	}
	if language == nil {
		// 离线设置动作不依赖Service；独立Manager保留该入口，且不共享全局语言。
		// 内嵌词典损坏属于构建错误，不能静默降级为缺少语言状态的控制器。
		var err error
		language, err = i18n.New("system", nil)
		if err != nil {
			panic(err)
		}
	}
	return &Controller{service: service, show: show, quit: quit, language: language}
}

func (c *Controller) SetStartupAction(open func()) {
	c.startup = open
}

func (c *Controller) Entries() []Entry {
	v := c.service.Snapshot()
	catalog := c.language.Snapshot()
	entries := []Entry{{Kind: "show", Label: catalog.Text("tray.show", nil), Enabled: true}}
	status := v.ConnectionMessage
	if v.Busy {
		status = catalog.Text("tray.waiting", nil)
	} else if v.Summary != "" {
		status = catalog.Text("tray.previous", map[string]string{"status": status, "summary": v.Summary})
	}
	entries = append(entries, Entry{Kind: "status", Label: status})
	for _, display := range v.Displays {
		for _, target := range v.Targets {
			choice := app.Selection{Kind: "display", DisplayID: display.ID, TargetID: model.TargetID(target.ID), Generation: v.Generation}
			reason := c.service.Preview(choice)
			label := display.Name + " → " + target.Name
			if reason != "" {
				label = catalog.Text("tray.unavailable", map[string]string{"name": label})
			}
			entries = append(entries, Entry{Kind: "switch", Label: label, Enabled: reason == "", Selection: choice})
		}
	}
	for _, group := range v.Groups {
		for _, target := range v.Targets {
			choice := app.Selection{Kind: "group", GroupID: model.GroupID(group.ID), TargetID: model.TargetID(target.ID), Generation: v.Generation}
			reason := c.service.Preview(choice)
			label := group.Name + " → " + target.Name
			if reason != "" {
				label = catalog.Text("tray.unavailable", map[string]string{"name": label})
			}
			entries = append(entries, Entry{Kind: "switch", Label: label, Enabled: reason == "", Selection: choice})
		}
	}
	entries = append(entries,
		Entry{Kind: "connect", Label: catalog.Text("tray.connect", nil), Enabled: !v.Busy && !v.Connecting},
		Entry{Kind: "startup", Label: catalog.Text("tray.settings", nil), Enabled: c.startup != nil},
		Entry{Kind: "quit", Label: catalog.Text("tray.quit", nil), Enabled: true})
	return entries
}

func (c *Controller) Activate(ctx context.Context, entry Entry) error {
	switch entry.Kind {
	case "startup":
		if c.startup == nil {
			return errors.New(c.language.Snapshot().Text("tray.settings_unavailable", nil))
		}
		c.startup()
		return nil
	case "show":
		if c.show != nil {
			c.show()
		}
		return nil
	case "quit":
		if c.quit != nil {
			c.quit()
		}
		return nil
	case "connect":
		return c.service.Reconnect(ctx)
	case "switch":
		// 不信任菜单绘制时的Enabled。后台重连、窗口操作或旧菜单派发可能使它过期，
		// 必须重新走同一service的代际检查、映射检查和单次请求门禁。
		return c.service.BeginSwitch(entry.Selection)
	default:
		return errors.New(c.language.Snapshot().Text("tray.unknown_action", nil))
	}
}

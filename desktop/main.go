package main

import (
	"context"
	"embed"
	"errors"
	"log"
	"os"

	"github.com/orzlee/displayKvm/desktop/app/host"
	"github.com/orzlee/displayKvm/desktop/i18n"
	"github.com/orzlee/displayKvm/desktop/views/devices"
	"github.com/orzlee/displayKvm/desktop/views/rules"
	"github.com/orzlee/displayKvm/desktop/views/schedules"
	"github.com/orzlee/displayKvm/desktop/views/startup"
	"github.com/orzlee/displayKvm/desktop/views/targets"
	"github.com/orzlee/displayKvm/internal/loginstart"
	"github.com/orzlee/displayKvm/internal/settings"
)

// 静态页面与框架自带的运行时一起嵌入客户端，运行时不依赖源码目录、
// 前端开发服务器或 Node；production 构建也不会生成另一份页面或绑定。
//
//go:embed all:frontend
var assets embed.FS

func main() {
	manager, err := loginstart.New(loginstart.Options{})
	if err != nil {
		log.Fatalf("初始化登录启动管理失败：%v", err)
	}
	files, err := settings.NewStore(settings.Options{})
	if err != nil {
		log.Fatalf("初始化完整配置存储失败：%v", err)
	}
	preferred, languageErr := host.SystemLanguages()
	if languageErr != nil {
		log.Printf("读取系统语言失败，跟随系统将使用英文：%v", languageErr)
	}
	// 语言只来自已选完整文档，不依赖后台是否在线。首次没有档案时使用system；
	// 损坏或不可读的档案/文档保留诊断，不保存默认值，也不替换用户原文件。
	preference := "system"
	profile, profileErr := manager.Profile(context.Background())
	if profileErr != nil {
		if !errors.Is(profileErr, os.ErrNotExist) {
			log.Printf("读取语言设置的选中文档位置失败：%v", profileErr)
		}
	} else if snapshot, readErr := files.ReadFile(profile.ConfigPath); readErr != nil {
		log.Printf("读取选中文档的语言设置失败：%v", readErr)
	} else {
		preference = snapshot.Document.Desktop.Language
	}
	language, err := i18n.New(preference, preferred)
	if err != nil {
		log.Fatalf("初始化桌面语言失败：%v", err)
	}
	appPath, err := host.ApplicationPath()
	if err != nil {
		log.Fatalf("定位 DisplayKVM 应用失败：%v", err)
	}
	bridge := host.NewStartupBridge()
	bridge.SetLanguage(language)
	startupPage := startup.Page(startup.NewBackend(manager, files, appPath), startup.FileDialogs{Open: bridge.PickFile, Save: bridge.SaveFile})
	if err := host.Run(assets, bridge, language, host.Page(devices.Page()), host.Page(targets.Page()), host.Page(rules.Page()), host.Page(schedules.Page()), host.Page(startupPage)); err != nil {
		log.Fatalf("启动 DisplayKVM 客户端失败：%v", err)
	}
}

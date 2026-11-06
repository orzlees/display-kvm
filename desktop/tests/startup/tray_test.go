package startup_test

import (
	"context"
	"testing"

	"github.com/orzlee/displayKvm/desktop/i18n"
	"github.com/orzlee/displayKvm/desktop/tray"
)

func TestTrayStartupActionDoesNotNeedDaemon(t *testing.T) {
	controller := tray.New(nil, nil, nil)
	opened := 0
	controller.SetStartupAction(func() { opened++ })
	if err := controller.Activate(context.Background(), tray.Entry{Kind: "startup"}); err != nil || opened != 1 {
		t.Fatalf("离线启动设置入口不可用：opened=%d err=%v", opened, err)
	}
}

func TestUnwiredTrayStartupActionFailsExplicitly(t *testing.T) {
	language, err := i18n.New("zh-CN", nil)
	if err != nil {
		t.Fatal(err)
	}
	controller := tray.NewWithLanguage(nil, nil, nil, language)
	if err := controller.Activate(context.Background(), tray.Entry{Kind: "startup"}); err == nil || err.Error() != "设置暂不可用，请重新打开桌面程序。" {
		t.Fatal("未接线时必须使用设置入口文案明确提示，不能冒充已打开")
	}
}

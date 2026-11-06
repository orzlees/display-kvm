package i18n

import (
	"fmt"
	"sync"
	"testing"
)

func TestI18nPreferenceAndSystemPriority(t *testing.T) {
	for _, tc := range []struct {
		name, preference string
		preferred        []string
		locale           string
	}{
		{"默认回退", "system", nil, "en"},
		{"中文覆盖", "zh-CN", []string{"en-US"}, "zh-CN"},
		{"英文覆盖", "en", []string{"zh-TW"}, "en"},
		{"跳过不支持语言", "system", []string{"fr-FR", "zh-Hant-TW", "en"}, "zh-CN"},
		{"优先英文", "system", []string{"en-GB", "zh-CN"}, "en"},
		{"系统下划线及大小写", "system", []string{"ZH_CN.UTF-8"}, "zh-CN"},
		{"系统不支持", "system", []string{"ja-JP", "de-DE"}, "en"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager, err := New(tc.preference, tc.preferred)
			if err != nil {
				t.Fatal(err)
			}
			catalog := manager.Snapshot()
			if catalog.Preference != tc.preference || catalog.Locale != tc.locale || catalog.Revision == 0 || len(catalog.Messages) == 0 {
				t.Fatalf("语言解析或词典缺失: %+v", catalog)
			}
		})
	}
	for _, invalid := range []string{"", "zh", "EN", "fr", " system "} {
		if _, err := New(invalid, nil); err == nil {
			t.Errorf("非法偏好应拒绝: %q", invalid)
		}
	}
}

func TestI18nSnapshotsAndInstancesAreIndependent(t *testing.T) {
	preferred := []string{"zh-CN"}
	first, err := New("system", preferred)
	if err != nil {
		t.Fatal(err)
	}
	second, err := New("en", nil)
	if err != nil {
		t.Fatal(err)
	}
	preferred[0] = "en"
	before := first.Snapshot()
	for key := range before.Messages {
		before.Messages[key] = "被外部覆盖"
		break
	}
	before.Messages["outside"] = "outside"
	if err := second.SetPreference("zh-CN"); err != nil {
		t.Fatal(err)
	}
	after := first.Snapshot()
	if after.Locale != "zh-CN" || after.Preference != "system" || after.Revision != before.Revision || after.Messages["outside"] != "" {
		t.Fatal("实例、系统语言输入或快照消息表被外部改写")
	}
	for _, text := range after.Messages {
		if text == "被外部覆盖" {
			t.Fatal("快照消息表不是独立副本")
		}
	}
}

func TestI18nRevisionAndCoalescedChanges(t *testing.T) {
	manager, err := New("system", nil)
	if err != nil {
		t.Fatal(err)
	}
	before := manager.Snapshot()
	if err := manager.SetPreference("en"); err != nil {
		t.Fatal(err)
	}
	// system与en虽解析为相同语言，偏好本身仍是可见状态，必须同步推进版本。
	after := manager.Snapshot()
	if after.Preference != "en" || after.Revision != before.Revision+1 {
		t.Fatal("更新返回前未同步提交偏好与版本")
	}
	select {
	case <-manager.Changes():
	default:
		t.Fatal("缺少语言变更通知")
	}
	for _, preference := range []string{"en", "invalid"} {
		err := manager.SetPreference(preference)
		if (preference == "invalid") != (err != nil) {
			t.Fatal("偏好校验不正确")
		}
	}
	if manager.Snapshot().Revision != after.Revision {
		t.Fatal("无变化或非法偏好推进了版本")
	}
	select {
	case <-manager.Changes():
		t.Fatal("无变化或非法偏好不应通知")
	default:
	}
	for _, preference := range []string{"zh-CN", "en", "system"} {
		if err := manager.SetPreference(preference); err != nil {
			t.Fatal(err)
		}
	}
	if got := manager.Snapshot(); got.Preference != "system" || got.Revision != after.Revision+3 {
		t.Fatal("未消费通知阻塞了同步状态更新")
	}
}

func TestI18nConcurrentPreferenceAndSnapshot(t *testing.T) {
	manager, err := New("system", []string{"zh-CN"})
	if err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	failures := make(chan string, 8)
	for worker := 0; worker < 8; worker++ {
		group.Add(1)
		go func(worker int) {
			defer group.Done()
			for index := 0; index < 80; index++ {
				if err := manager.SetPreference([]string{"system", "zh-CN", "en"}[(worker+index)%3]); err != nil {
					failures <- err.Error()
					return
				}
				catalog := manager.Snapshot()
				if catalog.Locale != "en" && catalog.Locale != "zh-CN" || catalog.Preference == "en" && catalog.Locale != "en" || catalog.Preference != "en" && catalog.Locale != "zh-CN" {
					failures <- fmt.Sprintf("非原子语言快照: %+v", catalog)
					return
				}
				catalog.Messages["outside"] = "outside"
			}
		}(worker)
	}
	group.Wait()
	close(failures)
	for failure := range failures {
		t.Error(failure)
	}
	if manager.Snapshot().Messages["outside"] != "" {
		t.Fatal("并发快照改写共享消息表")
	}
}

func TestI18nTextUsesLiteralSinglePassTokens(t *testing.T) {
	catalog := Catalog{Messages: map[string]string{"message": "Hello {name}; {missing}; {name}; {script}"}}
	values := map[string]string{"name": "{script}", "script": "<script>$1</script>"}
	if got := catalog.Text("message", values); got != "Hello {script}; {missing}; {script}; <script>$1</script>" {
		t.Fatalf("插值不能递归解释参数或运行模板: %q", got)
	}
	if values["name"] != "{script}" || len(values) != 2 || catalog.Messages["message"] != "Hello {name}; {missing}; {name}; {script}" {
		t.Fatal("插值修改调用方值或词典")
	}
	if catalog.Text("missing.key", values) != "missing.key" || catalog.Text("", nil) != "" {
		t.Fatal("缺失消息键必须保留可诊断标识")
	}
}

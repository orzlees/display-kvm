package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/orzlee/displayKvm/internal/config"
)

const automationConfigDocument = `{"version":3,"displays":[{"id":"left","name":"左屏","identity":{"manufacturer":"ABC","serial":"same"}}],"input_schemes":[{"id":"hdmi","protocol":"vcp","vcp_code":96,"value":17}],"targets":[{"id":"work","name":"工作机","inputs":[{"display_id":"left","scheme_id":"hdmi"}]}],"groups":[{"id":"desk","name":"桌面","display_ids":["left"]}]}`

const automationRulesDocument = `{"version":1,"rules":[{"id":"dock-work","enabled":true,"priority":100,"debounce_ms":250,"cooldown_ms":3000,"when":{"usb_present":true,"usb":{"vendor_id":1234,"product_id":5678}},"action":{"target_id":"work","group_id":"desk"}}]}`

func automationConfig(t *testing.T) config.Config {
	t.Helper()
	cfg, err := config.Load(strings.NewReader(automationConfigDocument))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestManagementAutomationUsesSchedulerComposition(t *testing.T) {
	m, cfg, rules, root, physical := managementFixture(t)
	before, err := encodeManagedConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	_, snapshot := managementStart(t, m, cfg, rules, root)
	m.mu.Lock()
	engine := m.current.engine
	m.mu.Unlock()
	if !snapshot.EngineAvailable || engine == nil {
		t.Fatal("完整文档规则未由真实后台构造调度引擎")
	}
	physical.mu.Lock()
	writes := physical.sets
	physical.mu.Unlock()
	after, err := encodeManagedConfig(cfg)
	if err != nil || !bytes.Equal(before, after) || writes != 0 {
		t.Fatalf("自动化初始化改变同源配置或写入硬件: writes=%d err=%v", writes, err)
	}
}

package automation

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/orzlee/displayKvm/internal/config"
	"github.com/orzlee/displayKvm/internal/model"
)

func testConfig() config.Config {
	return config.Config{
		Version:      config.CurrentVersion,
		Displays:     []model.Display{{ID: "left"}, {ID: "right"}},
		InputSchemes: []model.InputScheme{{ID: "hdmi", Protocol: model.ProtocolVCP, VCPCode: 0x60, Value: 17}},
		Targets: []model.Target{
			{ID: "self", Inputs: []model.TargetInput{{DisplayID: "left", SchemeID: "hdmi"}, {DisplayID: "right", SchemeID: "hdmi"}}},
			{ID: "other", Inputs: []model.TargetInput{{DisplayID: "left", SchemeID: "hdmi"}}},
		},
		Groups: []model.Group{{ID: "desk", DisplayIDs: []model.DisplayID{"left", "right"}}},
	}
}

func customRule(id string) Rule {
	return Rule{ID: id, Enabled: true, When: Condition{Kind: model.EventCustom, Name: "dock"}, Action: model.SwitchRequest{TargetID: "self"}}
}

func TestLoadRulesUsesStrictBoundedDecoder(t *testing.T) {
	valid := `{"version":1,"rules":[{"id":"dock","enabled":true,"when":{"kind":"custom","name":"dock","attributes":{"role":"owner"}},"action":{"target_id":"self","display_ids":["left"]}}]}`
	rules, err := LoadRules(strings.NewReader(valid))
	if err != nil || len(rules) != 1 || rules[0].When.Attributes["role"] != "owner" {
		t.Fatalf("有效规则无法加载: %v %#v", err, rules)
	}
	if err := ValidateRules(testConfig(), rules); err != nil {
		t.Fatal(err)
	}
	for name, text := range map[string]string{
		"unknown":       strings.Replace(valid, `"enabled":true`, `"enabled":true,"enabeld":true`, 1),
		"duplicate":     strings.Replace(valid, `"version":1`, `"version":1,"version":1`, 1),
		"trailing":      valid + `{}`,
		"null":          `null`,
		"version":       `{"version":2,"rules":[]}`,
		"missing_rules": `{"version":1}`,
		"null_rules":    `{"version":1,"rules":null}`,
		"oversize":      strings.Repeat(" ", config.MaxConfigBytes+1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadRules(strings.NewReader(text)); err == nil {
				t.Fatal("非法规则文件未拒绝")
			}
		})
	}
	if _, err := LoadRules(nil); err == nil {
		t.Fatal("空读取器未拒绝")
	}
	if rules, err := LoadRules(strings.NewReader(`{"version":1,"rules":[]}`)); err != nil || len(rules) != 0 {
		t.Fatalf("显式空规则应可表示关闭: %v", err)
	}
	failure := errors.New("read failed")
	if _, err := LoadRules(errorReader{failure}); !errors.Is(err, failure) || model.CodeOf(err) != model.CodeIO {
		t.Fatalf("读取根因丢失: %v", err)
	}
}

type errorReader struct{ err error }

func (r errorReader) Read([]byte) (int, error) { return 0, r.err }

var _ io.Reader = errorReader{}

func TestValidateRulesRejectsUnsafeConditionsAndReferences(t *testing.T) {
	for name, change := range map[string]func(*Rule){
		"id":                func(r *Rule) { r.ID = "bad id" },
		"duration":          func(r *Rule) { r.DebounceMillis = -1 },
		"overflow":          func(r *Rule) { r.CooldownMillis = 1<<63 - 1 },
		"action_id":         func(r *Rule) { r.Action.ID = "reused" },
		"missing_target":    func(r *Rule) { r.Action.TargetID = "missing" },
		"missing_display":   func(r *Rule) { r.Action.DisplayIDs = []model.DisplayID{"missing"} },
		"unmapped_display":  func(r *Rule) { r.Action.TargetID = "other"; r.Action.DisplayIDs = []model.DisplayID{"right"} },
		"unmapped_group":    func(r *Rule) { r.Action.TargetID = "other"; r.Action.GroupID = "desk" },
		"mixed_selection":   func(r *Rule) { r.Action.GroupID = "desk"; r.Action.DisplayIDs = []model.DisplayID{"left"} },
		"unknown_kind":      func(r *Rule) { r.When.Kind = "login" },
		"unnamed_custom":    func(r *Rule) { r.When.Name = "" },
		"mixed_state_event": func(r *Rule) { v := true; r.When.USBPresent = &v },
		"state_source": func(r *Rule) {
			v := true
			r.When = Condition{USBPresent: &v, Source: "linux", USB: &USBMatch{Path: "dock"}}
		},
		"usb_without_filter": func(r *Rule) { r.When = Condition{Kind: model.EventUSBAdded} },
		"usb_on_custom":      func(r *Rule) { r.When.USB = &USBMatch{Path: "dock"} },
		"attribute_key":      func(r *Rule) { r.When.Attributes = map[string]string{"bad key": "x"} },
	} {
		t.Run(name, func(t *testing.T) {
			rule := customRule("rule")
			change(&rule)
			if err := ValidateRules(testConfig(), []Rule{rule}); err == nil {
				t.Fatal("非法规则未拒绝")
			}
		})
	}
	if err := ValidateRules(testConfig(), []Rule{customRule("same"), customRule("same")}); err == nil {
		t.Fatal("重复规则ID未拒绝")
	}
	zero := uint16(0)
	rule := customRule("zero")
	rule.When = Condition{Kind: model.EventUSBAdded, USB: &USBMatch{VendorID: &zero}}
	if err := ValidateRules(testConfig(), []Rule{rule}); err != nil {
		t.Fatalf("显式零VID不应解释为未配置: %v", err)
	}
}

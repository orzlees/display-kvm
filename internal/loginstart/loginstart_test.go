package loginstart

import (
	"errors"
	"runtime"
	"testing"

	"github.com/orzlee/displayKvm/internal/model"
)

func TestLoginStartProfileStrictStructure(t *testing.T) {
	valid := `{"version":2,"config_path":"/settings.json"}`
	if _, err := decodeProfile([]byte(valid)); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{
		`{"version":1,"config_path":"/config.json","rules_path":"","schedules_path":"","tool_path":"/tool"}`,
		`{"version":1,"config_path":"/settings.json"}`,
		`{"version":2,"config_path":"relative"}`,
		`{"version":2,"config_path":""}`,
		`{"version":2,"config_path":"/settings.json","extra":true}`,
		`{"version":2,"config_path":"/settings.json"} {}`,
		`{"version":2,"config_path":"/a/../settings.json"}`,
		`null`,
		`{"version":2,"config_path":"/settings.json","rules_path":null}`,
		`{"version":2,"config_path":"/settings.json","rules_path":""}`,
		`{"version":2,"config_path":"/settings.json","schedules_path":""}`,
		`{"version":2,"config_path":"/settings.json","tool_path":""}`,
		`{"version":2,"version":2,"config_path":"/settings.json"}`,
		`{"version":3,"displays":[],"input_schemes":[],"targets":[],"groups":[]}`,
	} {
		if _, err := decodeProfile([]byte(text)); err == nil {
			t.Errorf("必须拒绝不明确的启动档案: %s", text)
		}
	}
}

func TestLoginStartUnsupportedHasClassifiableError(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("仅验证其它平台的不支持边界")
	}
	_, err := New(Options{})
	if model.CodeOf(err) != model.CodeUnsupportedOperation || !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("平台错误不可判断: %v", err)
	}
}

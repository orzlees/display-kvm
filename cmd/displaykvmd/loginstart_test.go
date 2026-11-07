package main

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/orzlee/displayKvm/internal/loginstart"
	"github.com/orzlee/displayKvm/internal/model"
)

func TestLoginStartDaemonProfileMapsExistingOptions(t *testing.T) {
	// Windows要求绝对路径包含卷信息；入口和文档路径都使用宿主格式，
	// 否则测试会在加载档案前被拒绝，无法覆盖档案到选项的映射。
	root := t.TempDir()
	settingsPath := filepath.Join(root, "settings.json")
	profilePath := filepath.Join(root, "profile.json")
	profile := loginstart.ServiceProfile{Version: 2, ConfigPath: settingsPath}
	called := false
	opts, err := parseOptions([]string{"-startup-profile", profilePath}, func(path string) (loginstart.ServiceProfile, error) {
		called = true
		if path != profilePath {
			t.Fatal(path)
		}
		return profile, nil
	})
	if err != nil || !called || opts.settingsPath != settingsPath || opts.toolPath != "" {
		t.Fatalf("档案未映射完整文档入口: %+v %v", opts, err)
	}
}

func TestLoginStartDaemonRejectsEveryExplicitBusinessFlag(t *testing.T) {
	for _, name := range []string{"config", "rules", "schedules", "tool", "busctl", "udevadm"} {
		_, err := parseOptions([]string{"-startup-profile", "/profile.json", "-" + name, ""}, func(string) (loginstart.ServiceProfile, error) {
			t.Fatal("混用参数仍加载档案")
			return loginstart.ServiceProfile{}, nil
		})
		if model.CodeOf(err) != model.CodeInvalidRequest {
			t.Fatalf("未拒绝显式-%s: %v", name, err)
		}
	}
}

func TestLoginStartDaemonRejectsRemovedFlagsAndProfiles(t *testing.T) {
	for _, name := range []string{"config", "rules", "schedules", "tool", "busctl", "udevadm"} {
		for _, value := range []string{"", "/explicit"} {
			if _, err := parseOptions([]string{"-" + name, value}, nil); model.CodeOf(err) != model.CodeInvalidRequest {
				t.Fatalf("接受已删除参数-%s=%q: %v", name, value, err)
			}
		}
	}
	if _, err := parseOptions(nil, nil); model.CodeOf(err) != model.CodeInvalidRequest {
		t.Fatalf("无完整文档入口仍被接受: %v", err)
	}
	profilePath := filepath.Join(t.TempDir(), "profile.json")
	if _, err := parseOptions([]string{"-startup-profile", profilePath}, func(string) (loginstart.ServiceProfile, error) {
		return loginstart.ServiceProfile{Version: 1, ConfigPath: "/config.json"}, nil
	}); model.CodeOf(err) != model.CodeUnsupportedVersion {
		t.Fatalf("接受v1档案: %v", err)
	}
	failure := errors.New("profile denied")
	_, err := parseOptions([]string{"-startup-profile", profilePath}, func(string) (loginstart.ServiceProfile, error) { return loginstart.ServiceProfile{}, failure })
	if !errors.Is(err, failure) {
		t.Fatalf("丢失档案错误: %v", err)
	}
}

package scheduler

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/orzlee/displayKvm/internal/config"
	"github.com/orzlee/displayKvm/internal/model"
)

func TestLoadSchedulesStrictExplicitDocument(t *testing.T) {
	valid := `{"version":1,"schedules":[{"id":"work","enabled":true,"time":"09:00","timezone":"Asia/Shanghai"}]}`
	schedules, err := LoadSchedules(strings.NewReader(valid))
	if err != nil || len(schedules) != 1 || schedules[0].Timezone != "Asia/Shanghai" {
		t.Fatalf("有效日程加载失败: %v %#v", err, schedules)
	}
	for name, text := range map[string]string{
		"unknown":        strings.Replace(valid, `"enabled":true`, `"enabled":true,"enabeld":true`, 1),
		"duplicate_key":  strings.Replace(valid, `"version":1`, `"version":1,"version":1`, 1),
		"trailing":       valid + `{}`,
		"null":           `null`,
		"version":        `{"version":2,"schedules":[]}`,
		"missing":        `{"version":1}`,
		"null_list":      `{"version":1,"schedules":null}`,
		"unknown_policy": strings.Replace(valid, `"enabled":true`, `"enabled":true,"missed_policy":"catch_up"`, 1),
		"duplicate_id":   `{"version":1,"schedules":[{"id":"work","time":"09:00","timezone":"UTC"},{"id":"work","time":"10:00","timezone":"UTC"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadSchedules(strings.NewReader(text)); err == nil {
				t.Fatal("非法日程未被拒绝")
			}
		})
	}
	if schedules, err := LoadSchedules(strings.NewReader(`{"version":1,"schedules":[]}`)); err != nil || schedules == nil || len(schedules) != 0 {
		t.Fatalf("显式空列表应关闭日程: %v %#v", err, schedules)
	}
	if _, err := LoadSchedules(nil); err == nil {
		t.Fatal("nil读取器未被拒绝")
	}
	if _, err := LoadSchedules(strings.NewReader(strings.Repeat(" ", config.MaxConfigBytes+1))); model.CodeOf(err) != model.CodeMessageTooLarge {
		t.Fatalf("超限文档错误不正确: %v", err)
	}
	readErr := errors.New("日程读取失败")
	if _, err := LoadSchedules(failingReader{err: readErr}); model.CodeOf(err) != model.CodeIO || !errors.Is(err, readErr) {
		t.Fatalf("读取根因未保留: %v", err)
	}
}

type failingReader struct{ err error }

func (r failingReader) Read([]byte) (int, error) { return 0, r.err }

var _ io.Reader = failingReader{}

func TestValidateScheduleBoundaries(t *testing.T) {
	for _, wall := range []string{"00:00", "09:05", "23:59"} {
		if err := ValidateSchedules([]Schedule{{ID: "work", Time: wall, Timezone: "UTC"}}); err != nil {
			t.Fatalf("合法墙钟被拒绝: %s %v", wall, err)
		}
	}
	for _, wall := range []string{"", "9:00", "24:00", "12:60", "-1:00", "09:00:00", "09:0a", " 9:00", "０９:00"} {
		if err := ValidateSchedules([]Schedule{{ID: "work", Time: wall, Timezone: "UTC"}}); err == nil {
			t.Fatalf("非法墙钟被接受: %q", wall)
		}
	}
	for _, zone := range []string{"", "Local", "Mars/Phobos"} {
		if err := ValidateSchedules([]Schedule{{ID: "work", Time: "09:00", Timezone: zone}}); err == nil {
			t.Fatalf("非法或隐式时区被接受: %q", zone)
		}
	}
	for _, id := range []string{"", "含中文", "with space", strings.Repeat("x", 129)} {
		if err := ValidateSchedules([]Schedule{{ID: id, Time: "09:00", Timezone: "UTC"}}); err == nil {
			t.Fatalf("非法身份被接受: %q", id)
		}
	}
}

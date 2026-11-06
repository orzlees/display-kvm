package scheduler

import (
	"testing"
	"time"
)

func instant(t *testing.T, text string) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339, text)
	if err != nil {
		t.Fatal(err)
	}
	return at
}

func wallState(t *testing.T, zone, wall string) scheduleState {
	t.Helper()
	schedule := Schedule{ID: "work", Enabled: true, Time: wall, Timezone: zone}
	if err := ValidateSchedules([]Schedule{schedule}); err != nil {
		t.Fatal(err)
	}
	location, err := time.LoadLocation(zone)
	if err != nil {
		t.Fatal(err)
	}
	return scheduleState{schedule: schedule, location: location,
		hour: int(wall[0]-'0')*10 + int(wall[1]-'0'), minute: int(wall[3]-'0')*10 + int(wall[4]-'0')}
}

func TestNextTriggerAcrossExplicitTimezones(t *testing.T) {
	for _, test := range []struct {
		zone, now, want string
	}{
		{"Asia/Shanghai", "2026-03-28T22:00:00Z", "2026-03-29T01:00:00Z"},
		{"America/New_York", "2026-03-28T22:00:00Z", "2026-03-29T13:00:00Z"},
		{"UTC", "2026-03-28T22:00:00Z", "2026-03-29T09:00:00Z"},
		{"UTC", "2026-03-29T09:00:00Z", "2026-03-30T09:00:00Z"},
		{"America/New_York", "2040-12-31T00:00:00Z", "2040-12-31T14:00:00Z"},
	} {
		t.Run(test.zone+test.now, func(t *testing.T) {
			state := wallState(t, test.zone, "09:00")
			state.rebase(instant(t, test.now))
			if !state.next.Equal(instant(t, test.want)) {
				t.Fatalf("下一次时间错误: got %v want %s", state.next, test.want)
			}
		})
	}
}

func TestDSTGapFoldAndSkippedCivilDate(t *testing.T) {
	for _, test := range []struct {
		name, zone, wall, now, want string
	}{
		{"纽约缺失时刻跳日", "America/New_York", "02:30", "2026-03-08T05:00:00Z", "2026-03-09T06:30:00Z"},
		{"纽约重复取首次", "America/New_York", "01:30", "2026-11-01T04:00:00Z", "2026-11-01T05:30:00Z"},
		{"纽约首次后不取第二次", "America/New_York", "01:30", "2026-11-01T05:45:00Z", "2026-11-02T06:30:00Z"},
		{"半小时重复取首次", "Australia/Lord_Howe", "01:45", "2026-04-04T13:00:00Z", "2026-04-04T14:45:00Z"},
		{"半小时缺失跳日", "Australia/Lord_Howe", "02:15", "2026-10-03T13:00:00Z", "2026-10-04T15:15:00Z"},
		{"整日本地日期缺失", "Pacific/Apia", "12:00", "2011-12-30T09:00:00Z", "2011-12-30T22:00:00Z"},
	} {
		t.Run(test.name, func(t *testing.T) {
			state := wallState(t, test.zone, test.wall)
			state.rebase(instant(t, test.now))
			if !state.next.Equal(instant(t, test.want)) {
				t.Fatalf("夏令时或日期边界错误: got %v want %s", state.next, test.want)
			}
		})
	}
}

func TestConsumedDateDoesNotReturnAfterClockRollback(t *testing.T) {
	state := wallState(t, "UTC", "09:00")
	state.rebase(instant(t, "2026-11-06T10:00:00Z"))
	state.rebase(instant(t, "2026-11-02T08:00:00Z"))
	if !state.next.Equal(instant(t, "2026-11-07T09:00:00Z")) {
		t.Fatalf("回拨复活了启动前历史日期: %v", state.next)
	}
}

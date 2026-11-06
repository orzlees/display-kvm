package scheduler

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/orzlee/displayKvm/internal/automation"
	"github.com/orzlee/displayKvm/internal/config"
	"github.com/orzlee/displayKvm/internal/daemon"
	"github.com/orzlee/displayKvm/internal/model"
)

// 墙钟推进与定时器投递分开控制，避免把业务行为建立在真实等待上。
// gate把首个计时器停在进入select之前，确定性地制造恢复命令/事件与到期同时就绪。
type timerGate struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (g *timerGate) open() {
	if g != nil {
		g.once.Do(func() { close(g.release) })
	}
}

type fakeClock struct {
	mu       sync.Mutex
	now      time.Time
	timers   []*fakeTimer
	gate     *timerGate
	gateUsed bool
}

type fakeTimer struct {
	owner   *fakeClock
	at      time.Time
	ch      chan time.Time
	stopped bool
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) NewTimer(d time.Duration) Timer {
	c.mu.Lock()
	timer := &fakeTimer{owner: c, at: c.now.Add(d), ch: make(chan time.Time, 1)}
	if d <= 0 {
		timer.ch <- c.now
		timer.stopped = true
	}
	c.timers = append(c.timers, timer)
	gate := c.gate
	if c.gateUsed {
		gate = nil
	}
	c.gateUsed = true
	c.mu.Unlock()
	if gate != nil {
		close(gate.entered)
		<-gate.release
	}
	return timer
}

func (t *fakeTimer) C() <-chan time.Time { return t.ch }
func (t *fakeTimer) Stop() {
	t.owner.mu.Lock()
	t.stopped = true
	t.owner.mu.Unlock()
}

func (c *fakeClock) set(at time.Time) {
	c.mu.Lock()
	c.now = at
	c.mu.Unlock()
}

func (c *fakeClock) fire(early bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, timer := range c.timers {
		if !timer.stopped && (early || !timer.at.After(c.now)) {
			timer.stopped = true
			timer.ch <- c.now
		}
	}
}

type schedulerHarness struct {
	s      *Scheduler
	clock  *fakeClock
	events chan model.Event
	calls  chan model.SwitchRequest
	finish chan error
	cancel context.CancelFunc
	marker int
}

func daily(id string) Schedule {
	return Schedule{ID: id, Enabled: true, Time: "09:00", Timezone: "UTC"}
}

func timerRule(id, name string) automation.Rule {
	return automation.Rule{ID: id, Enabled: true,
		When:   automation.Condition{Kind: model.EventTimer, Source: "scheduler", Name: name},
		Action: model.SwitchRequest{TargetID: "self"}}
}

func schedulerConfig() config.Config {
	return config.Config{Version: config.CurrentVersion,
		Displays:     []model.Display{{ID: "left"}},
		InputSchemes: []model.InputScheme{{ID: "hdmi", Protocol: model.ProtocolVCP, VCPCode: 0x60, Value: 17}},
		Targets:      []model.Target{{ID: "self", Inputs: []model.TargetInput{{DisplayID: "left", SchemeID: "hdmi"}}}},
	}
}

func makeHarness(t *testing.T, clock *fakeClock, schedules []Schedule, rules []automation.Rule) *schedulerHarness {
	t.Helper()
	h := &schedulerHarness{clock: clock, events: make(chan model.Event, 64),
		calls: make(chan model.SwitchRequest, 64), finish: make(chan error, 1)}
	rt := daemon.AutomationRuntime{
		Events: h.events,
		Switch: func(ctx context.Context, request model.SwitchRequest) (model.SwitchResult, error) {
			select {
			case h.calls <- request:
			case <-ctx.Done():
				return model.SwitchResult{}, ctx.Err()
			}
			return model.SwitchResult{RequestID: request.ID,
				Displays: []model.DisplayResult{{DisplayID: "left", State: model.StateConfirmed}}}, nil
		},
		Rescan: func(ctx context.Context) (daemon.Baseline, error) {
			return daemon.Baseline{ObservedAt: clock.Now(), USBKnown: true}, nil
		},
	}
	var err error
	h.s, err = New(rt, schedulerConfig(), rules, schedules, Options{Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *schedulerHarness) start(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	go func() { h.finish <- h.s.Run(ctx) }()
	t.Cleanup(func() {
		h.clock.gate.open()
		cancel()
		if err := receive(t, h.finish); !errors.Is(err, context.Canceled) {
			t.Errorf("取消未完整回收: %v", err)
		}
		for _, status := range h.s.Status() {
			if status.NextTrigger != nil {
				t.Error("停止后仍暴露下一触发时间")
			}
		}
	})
	awaitRecord(t, h.s, func(record automation.Record) bool { return record.Decision.Reason == "rescan_completed" })
}

func startHarness(t *testing.T, clock *fakeClock, schedules []Schedule, rules []automation.Rule) *schedulerHarness {
	t.Helper()
	h := makeHarness(t, clock, schedules, rules)
	h.start(t)
	return h
}

func receive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(3 * time.Second):
		t.Fatal("测试握手超时")
		var zero T
		return zero
	}
}

func await(t *testing.T, predicate func() bool) {
	t.Helper()
	guard := time.NewTimer(3 * time.Second)
	defer guard.Stop()
	for !predicate() {
		select {
		case <-guard.C:
			t.Fatal("等待状态机握手超时")
		default:
			runtime.Gosched()
		}
	}
}

func awaitRecord(t *testing.T, s *Scheduler, predicate func(automation.Record) bool) automation.Record {
	t.Helper()
	var found automation.Record
	await(t, func() bool {
		for _, record := range s.Records() {
			if predicate(record) {
				found = record
				return true
			}
		}
		return false
	})
	return found
}

func (h *schedulerHarness) next(t *testing.T, want time.Time) {
	t.Helper()
	await(t, func() bool {
		status := h.s.Status()
		return len(status) == 1 && status[0].NextTrigger != nil && status[0].NextTrigger.Equal(want)
	})
}

func (h *schedulerHarness) barrier(t *testing.T) {
	t.Helper()
	h.marker++
	id := fmt.Sprintf("barrier-%d", h.marker)
	h.events <- model.Event{ID: id, Kind: model.EventCustom, Source: "test", Name: id, ObservedAt: h.clock.Now()}
	awaitRecord(t, h.s, func(record automation.Record) bool { return record.Event != nil && record.Event.ID == id })
}

func (h *schedulerHarness) noTimers(t *testing.T) {
	t.Helper()
	h.barrier(t)
	for _, record := range h.s.Records() {
		if record.Event != nil && record.Event.Kind == model.EventTimer {
			t.Fatalf("出现了本应跳过的定时事件: %#v", record)
		}
	}
	select {
	case call := <-h.calls:
		t.Fatalf("出现意外动作: %#v", call)
	default:
	}
}

func (h *schedulerHarness) tick(at time.Time) {
	h.clock.set(at)
	h.clock.fire(false)
}

func TestTimerTraversesSameRealEngineAndQueries(t *testing.T) {
	clock := &fakeClock{now: instant(t, "2026-11-06T08:00:00Z")}
	schedules := []Schedule{daily("work")}
	h := startHarness(t, clock, schedules, []automation.Rule{timerRule("switch", "work")})
	schedules[0].ID = "caller-mutated"
	due := instant(t, "2026-11-06T09:00:00Z")
	h.next(t, due)
	h.tick(due)
	request := receive(t, h.calls)
	if request.TargetID != "self" || !model.ValidID(string(request.ID)) {
		t.Fatalf("未通过真实动作入口: %#v", request)
	}
	record := awaitRecord(t, h.s, func(record automation.Record) bool { return record.Decision.Reason == "completed" })
	if record.Event == nil {
		t.Fatal("真实引擎记录缺少定时事件")
	}
	event := *record.Event
	if err := event.Validate(); err != nil || event.Kind != model.EventTimer || event.Source != "scheduler" || event.Name != "work" ||
		!event.ObservedAt.Equal(due) || event.Attributes["scheduled_at"] != "2026-11-06T09:00:00Z" ||
		event.Attributes["timezone"] != "UTC" || event.Attributes["missed_policy"] != "skip" || len(event.ID) != 70 {
		t.Fatalf("定时事件契约错误: %v %#v", err, event)
	}
	decisions, err := h.s.Preview(event)
	if err != nil || len(decisions) != 1 || decisions[0].Reason != "duplicate" {
		t.Fatalf("预览没有使用运行中同一引擎的去重状态: %v %#v", err, decisions)
	}
	copy := h.s.Records()
	copy[len(copy)-1].Decision.Reason = "caller-mutated"
	for _, record := range h.s.Records() {
		if record.Decision.Reason == "caller-mutated" {
			t.Fatal("记录查询泄露可变状态")
		}
	}
	h.next(t, instant(t, "2026-11-07T09:00:00Z"))
	status := h.s.Status()
	status[0].Schedule.Enabled = false
	*status[0].NextTrigger = time.Time{}
	if current := h.s.Status()[0]; !current.Schedule.Enabled || current.NextTrigger == nil || current.NextTrigger.IsZero() || current.MissedPolicy != "skip" {
		t.Fatal("状态查询没有隔离调用方修改")
	}
}

func TestExplicitResumeWinsReadyDeadlineAndSkipsManyDays(t *testing.T) {
	gate := &timerGate{entered: make(chan struct{}), release: make(chan struct{})}
	clock := &fakeClock{now: instant(t, "2026-01-01T08:00:00Z"), gate: gate}
	h := startHarness(t, clock, []Schedule{daily("work")}, []automation.Rule{timerRule("switch", "work")})
	receive(t, gate.entered)
	clock.set(instant(t, "2026-06-10T12:00:00Z"))
	replied := make(chan error, 1)
	go func() { replied <- h.s.Resume(context.Background()) }()
	await(t, func() bool { return len(h.s.commands) > 0 })
	clock.fire(false)
	gate.open()
	if err := receive(t, replied); err != nil {
		t.Fatal(err)
	}
	next := instant(t, "2026-06-11T09:00:00Z")
	h.next(t, next)
	if err := h.s.Resume(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.noTimers(t)
	h.tick(next)
	receive(t, h.calls)
	awaitRecord(t, h.s, func(record automation.Record) bool { return record.Decision.Reason == "completed" })
}

func TestPlatformResumeSkipsBeforeForwardingAndReplayDoesNotSkipAgain(t *testing.T) {
	gate := &timerGate{entered: make(chan struct{}), release: make(chan struct{})}
	clock := &fakeClock{now: instant(t, "2026-01-01T08:00:00Z"), gate: gate}
	h := startHarness(t, clock, []Schedule{daily("work")}, []automation.Rule{timerRule("switch", "work")})
	receive(t, gate.entered)
	woke := instant(t, "2026-02-02T12:00:00Z")
	clock.set(woke)
	resume := model.Event{ID: "wake-1", Kind: model.EventResume, Source: "platform", ObservedAt: woke}
	h.events <- resume
	clock.fire(false)
	gate.open()
	awaitRecord(t, h.s, func(record automation.Record) bool { return record.Event != nil && record.Event.ID == resume.ID })
	next := instant(t, "2026-02-03T09:00:00Z")
	h.next(t, next)
	h.noTimers(t)
	clock.set(next)
	h.events <- resume
	awaitRecord(t, h.s, func(record automation.Record) bool {
		return record.Event != nil && record.Event.ID == resume.ID && record.Decision.Reason == "duplicate"
	})
	receive(t, h.calls)
	awaitRecord(t, h.s, func(record automation.Record) bool { return record.Decision.Reason == "completed" })
	h.next(t, instant(t, "2026-02-04T09:00:00Z"))
}

func TestDisableWinsUndispatchedDeadlineAndReenableDoesNotReplay(t *testing.T) {
	gate := &timerGate{entered: make(chan struct{}), release: make(chan struct{})}
	clock := &fakeClock{now: instant(t, "2026-11-06T08:00:00Z"), gate: gate}
	h := startHarness(t, clock, []Schedule{daily("work")}, []automation.Rule{timerRule("switch", "work")})
	receive(t, gate.entered)
	clock.set(instant(t, "2026-11-06T09:00:00Z"))
	replied := make(chan error, 1)
	go func() { replied <- h.s.SetEnabled(context.Background(), "work", false) }()
	await(t, func() bool { return len(h.s.commands) > 0 })
	clock.fire(false)
	gate.open()
	if err := receive(t, replied); err != nil {
		t.Fatal(err)
	}
	if status := h.s.Status()[0]; status.Schedule.Enabled || status.NextTrigger != nil {
		t.Fatalf("停用后仍有触发时间: %#v", status)
	}
	clock.set(instant(t, "2026-11-09T09:00:00Z"))
	if err := h.s.SetEnabled(context.Background(), "work", true); err != nil {
		t.Fatal(err)
	}
	next := instant(t, "2026-11-10T09:00:00Z")
	h.next(t, next)
	h.noTimers(t)
	if err := h.s.SetEnabled(context.Background(), "missing", false); model.CodeOf(err) != model.CodeNotFound {
		t.Fatalf("未知日程错误不正确: %v", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := h.s.SetEnabled(canceled, "work", false); !errors.Is(err, context.Canceled) || !h.s.Status()[0].Schedule.Enabled {
		t.Fatalf("取消命令修改了日程: %v", err)
	}
	h.tick(next)
	receive(t, h.calls)
}

func TestStartupDoesNotReplayConstructionHistoryOrExactStart(t *testing.T) {
	clock := &fakeClock{now: instant(t, "2026-01-01T08:00:00Z")}
	h := makeHarness(t, clock, []Schedule{daily("work")}, []automation.Rule{timerRule("switch", "work")})
	if h.s.Status()[0].NextTrigger != nil {
		t.Fatal("未运行时暴露了有效触发时间")
	}
	if err := h.s.Resume(context.Background()); model.CodeOf(err) != model.CodeBackendUnavailable {
		t.Fatalf("未运行时恢复命令没有拒绝: %v", err)
	}
	clock.set(instant(t, "2026-03-01T09:00:00Z"))
	h.start(t)
	h.next(t, instant(t, "2026-03-02T09:00:00Z"))
	h.noTimers(t)
	if err := h.s.Run(context.Background()); model.CodeOf(err) != model.CodeInvalidRequest {
		t.Fatalf("重复Run没有拒绝: %v", err)
	}
}

func TestEarlyRepeatedWakeAndClockRollbackDoNotRepeatConsumedDate(t *testing.T) {
	clock := &fakeClock{now: instant(t, "2026-11-06T08:00:00Z")}
	h := startHarness(t, clock, []Schedule{daily("work")}, []automation.Rule{timerRule("switch", "work")})
	due := instant(t, "2026-11-06T09:00:00Z")
	h.tick(due)
	receive(t, h.calls)
	first := awaitRecord(t, h.s, func(record automation.Record) bool { return record.Decision.Reason == "completed" })
	next := instant(t, "2026-11-07T09:00:00Z")
	h.next(t, next)
	clock.set(instant(t, "2026-11-05T09:00:00Z"))
	clock.fire(true)
	h.barrier(t)
	h.next(t, next)
	clock.set(due)
	clock.fire(true)
	h.barrier(t)
	h.next(t, next)
	for _, record := range h.s.Records() {
		if record.Event != nil && record.Event.Kind == model.EventTimer && record.Event.ID != first.Event.ID {
			t.Fatal("回拨或重复扫描生成额外日期事件")
		}
	}
	select {
	case extra := <-h.calls:
		t.Fatalf("同一本地日期重复执行: %#v", extra)
	default:
	}
	h.tick(next)
	receive(t, h.calls)
	second := awaitRecord(t, h.s, func(record automation.Record) bool {
		return record.Decision.Reason == "completed" && record.Event != nil && record.Event.ID != first.Event.ID
	})
	if !second.Event.ObservedAt.Equal(next) {
		t.Fatalf("下一日期没有正常触发: %#v", second.Event)
	}
}

func TestTimerStillObeysDisabledRule(t *testing.T) {
	clock := &fakeClock{now: instant(t, "2026-11-06T08:00:00Z")}
	rule := timerRule("switch", "work")
	rule.Enabled = false
	h := startHarness(t, clock, []Schedule{daily("work")}, []automation.Rule{rule})
	h.tick(instant(t, "2026-11-06T09:00:00Z"))
	awaitRecord(t, h.s, func(record automation.Record) bool {
		return record.Event != nil && record.Event.Kind == model.EventTimer && record.Decision.Reason == "disabled"
	})
	select {
	case extra := <-h.calls:
		t.Fatalf("禁用规则被调度器绕过: %#v", extra)
	default:
	}
}

func TestTimerStillObeysRulePriorityAndConflict(t *testing.T) {
	clock := &fakeClock{now: instant(t, "2026-11-06T08:00:00Z")}
	low, high := timerRule("low", "work"), timerRule("high", "work")
	high.Priority = 10
	h := startHarness(t, clock, []Schedule{daily("work")}, []automation.Rule{low, high})
	h.tick(instant(t, "2026-11-06T09:00:00Z"))
	receive(t, h.calls)
	awaitRecord(t, h.s, func(record automation.Record) bool {
		return record.Decision.RuleID == "low" && record.Decision.Reason == "conflict"
	})
	awaitRecord(t, h.s, func(record automation.Record) bool {
		return record.Decision.RuleID == "high" && record.Decision.Reason == "completed"
	})
	select {
	case extra := <-h.calls:
		t.Fatalf("冲突规则重复提交: %#v", extra)
	default:
	}
}

func TestWallClockDoesNotBypassRealEngineCooldown(t *testing.T) {
	clock := &fakeClock{now: instant(t, "2026-11-06T08:00:00Z")}
	rule := timerRule("switch", "work")
	rule.CooldownMillis = int64(time.Hour / time.Millisecond)
	h := startHarness(t, clock, []Schedule{daily("work")}, []automation.Rule{rule})
	h.tick(instant(t, "2026-11-06T09:00:00Z"))
	receive(t, h.calls)
	awaitRecord(t, h.s, func(record automation.Record) bool { return record.Decision.Reason == "completed" })
	next := instant(t, "2026-11-07T09:00:00Z")
	h.next(t, next)
	h.tick(next)
	awaitRecord(t, h.s, func(record automation.Record) bool {
		return record.Event != nil && record.Event.ObservedAt.Equal(next) && record.Decision.Reason == "cooldown"
	})
	select {
	case extra := <-h.calls:
		t.Fatalf("墙钟推进绕过了真实引擎冷却: %#v", extra)
	default:
	}
}

func TestResumeSkipsEveryOverdueScheduleButKeepsFutureToday(t *testing.T) {
	gate := &timerGate{entered: make(chan struct{}), release: make(chan struct{})}
	clock := &fakeClock{now: instant(t, "2026-01-01T06:00:00Z"), gate: gate}
	early, late, shanghai := daily("early"), daily("late"), daily("shanghai")
	late.Time, shanghai.Timezone = "20:00", "Asia/Shanghai"
	h := startHarness(t, clock, []Schedule{early, late, shanghai}, []automation.Rule{
		timerRule("early-rule", "early"), timerRule("late-rule", "late"), timerRule("shanghai-rule", "shanghai"),
	})
	receive(t, gate.entered)
	clock.set(instant(t, "2026-01-05T12:00:00Z"))
	replied := make(chan error, 1)
	go func() { replied <- h.s.Resume(context.Background()) }()
	await(t, func() bool { return len(h.s.commands) > 0 })
	clock.fire(false)
	gate.open()
	if err := receive(t, replied); err != nil {
		t.Fatal(err)
	}
	want := map[string]time.Time{
		"early":    instant(t, "2026-01-06T09:00:00Z"),
		"late":     instant(t, "2026-01-05T20:00:00Z"),
		"shanghai": instant(t, "2026-01-06T01:00:00Z"),
	}
	for _, status := range h.s.Status() {
		if status.NextTrigger == nil || !status.NextTrigger.Equal(want[status.Schedule.ID]) {
			t.Fatalf("多日程恢复后的计划错误: %#v", status)
		}
	}
	h.noTimers(t)
	h.tick(want["late"])
	receive(t, h.calls)
	awaitRecord(t, h.s, func(record automation.Record) bool {
		return record.Decision.Reason == "completed" && record.Event != nil && record.Event.Name == "late"
	})
}

func TestDisabledScheduleNeverStartsImplicitly(t *testing.T) {
	clock := &fakeClock{now: instant(t, "2026-11-06T08:00:00Z")}
	schedule := daily("work")
	schedule.Enabled = false
	h := startHarness(t, clock, []Schedule{schedule}, []automation.Rule{timerRule("switch", "work")})
	clock.set(instant(t, "2027-01-01T12:00:00Z"))
	clock.fire(false)
	h.noTimers(t)
	if status := h.s.Status()[0]; status.Schedule.Enabled || status.NextTrigger != nil {
		t.Fatalf("显式停用日程被隐式启用: %#v", status)
	}
}

func TestOccurrenceIdentityIsStableForSameDayAcrossInstances(t *testing.T) {
	var first string
	for i := 0; i < 2; i++ {
		clock := &fakeClock{now: instant(t, "2026-11-06T08:00:00Z")}
		h := startHarness(t, clock, []Schedule{daily("work")}, []automation.Rule{timerRule("switch", "work")})
		h.tick(instant(t, "2026-11-06T09:00:00Z"))
		receive(t, h.calls)
		record := awaitRecord(t, h.s, func(record automation.Record) bool { return record.Decision.Reason == "completed" })
		if record.Event == nil {
			t.Fatal("定时动作没有规范事件")
		}
		if i == 0 {
			first = record.Event.ID
		} else if record.Event.ID != first {
			t.Fatalf("同一日程同一本地日期标识不稳定: %q %q", first, record.Event.ID)
		}
	}
}

func TestRunningSchedulerUsesExplicitZoneAndDSTFirstOccurrence(t *testing.T) {
	for _, test := range []struct {
		name, zone, wall, start, first, middle, next string
	}{
		{"显式上海时区", "Asia/Shanghai", "09:00", "2026-11-06T00:00:00Z", "2026-11-06T01:00:00Z", "2026-11-06T02:00:00Z", "2026-11-07T01:00:00Z"},
		{"夏令时缺失跳日", "America/New_York", "02:30", "2026-03-08T05:00:00Z", "2026-03-09T06:30:00Z", "2026-03-09T07:00:00Z", "2026-03-10T06:30:00Z"},
		{"夏令时重复仅首次", "America/New_York", "01:30", "2026-11-01T04:00:00Z", "2026-11-01T05:30:00Z", "2026-11-01T06:30:00Z", "2026-11-02T06:30:00Z"},
	} {
		t.Run(test.name, func(t *testing.T) {
			clock := &fakeClock{now: instant(t, test.start)}
			schedule := daily("work")
			schedule.Time, schedule.Timezone = test.wall, test.zone
			h := startHarness(t, clock, []Schedule{schedule}, []automation.Rule{timerRule("switch", "work")})
			first, next := instant(t, test.first), instant(t, test.next)
			h.next(t, first)
			h.tick(first)
			receive(t, h.calls)
			awaitRecord(t, h.s, func(record automation.Record) bool { return record.Decision.Reason == "completed" })
			h.next(t, next)
			h.tick(instant(t, test.middle))
			h.barrier(t)
			h.next(t, next)
			for _, record := range h.s.Records() {
				if record.Event != nil && record.Event.Kind == model.EventTimer && !record.Event.ObservedAt.Equal(first) {
					t.Fatalf("时区或夏令时产生额外事件: %#v", record.Event)
				}
			}
			select {
			case extra := <-h.calls:
				t.Fatalf("同一墙钟日期发生第二次执行: %#v", extra)
			default:
			}
		})
	}
}

func TestEquivalentWallTimesShareAbsoluteNextTriggerAcrossSeasons(t *testing.T) {
	// 固定冬夏两个绝对时刻，验证纽约季节偏移与其他时区的等价墙钟。
	// 只读取真实运行实例的公开状态，不复算候选算法，也不推进到触发时刻。
	for _, test := range []struct {
		name, want, utc, shanghai string
	}{
		{"冬季", "2026-01-15T14:00:00Z", "14:00", "22:00"},
		{"夏季", "2026-07-15T13:00:00Z", "13:00", "21:00"},
	} {
		t.Run(test.name, func(t *testing.T) {
			want := instant(t, test.want)
			clock := &fakeClock{now: want.Add(-time.Minute)}
			schedules := []Schedule{
				{ID: "utc", Enabled: true, Time: test.utc, Timezone: "UTC"},
				{ID: "shanghai", Enabled: true, Time: test.shanghai, Timezone: "Asia/Shanghai"},
				{ID: "new-york", Enabled: true, Time: "09:00", Timezone: "America/New_York"},
			}
			h := startHarness(t, clock, schedules, nil)
			statuses := h.s.Status()
			if len(statuses) != len(schedules) {
				t.Fatalf("公开状态遗漏日程: got %d want %d", len(statuses), len(schedules))
			}
			for _, status := range statuses {
				if status.NextTrigger == nil || !status.NextTrigger.Equal(want) {
					t.Fatalf("等价墙钟没有指向同一绝对时刻: %s %s got %v want %s",
						status.Schedule.Timezone, status.Schedule.Time, status.NextTrigger, test.want)
				}
			}
		})
	}
}

func TestLateTimerWithoutResumeIsNotGuessedAsSleep(t *testing.T) {
	clock := &fakeClock{now: instant(t, "2026-11-06T08:00:00Z")}
	h := startHarness(t, clock, []Schedule{daily("work")}, []automation.Rule{timerRule("switch", "work")})
	h.next(t, instant(t, "2026-11-06T09:00:00Z"))
	h.tick(instant(t, "2026-11-06T09:30:00Z"))
	receive(t, h.calls)
	record := awaitRecord(t, h.s, func(record automation.Record) bool { return record.Decision.Reason == "completed" })
	if record.Event == nil || !record.Event.ObservedAt.Equal(instant(t, "2026-11-06T09:00:00Z")) {
		t.Fatalf("迟到通知丢失原计划时刻: %#v", record.Event)
	}
	h.next(t, instant(t, "2026-11-07T09:00:00Z"))
}

func TestStoppedSchedulerRejectsCommandsAndReclaimsTimers(t *testing.T) {
	clock := &fakeClock{now: instant(t, "2026-11-06T08:00:00Z")}
	h := makeHarness(t, clock, []Schedule{daily("work")}, []automation.Rule{timerRule("switch", "work")})
	ctx, cancel := context.WithCancel(context.Background())
	go func() { h.finish <- h.s.Run(ctx) }()
	defer cancel()
	h.next(t, instant(t, "2026-11-06T09:00:00Z"))
	cancel()
	if err := receive(t, h.finish); !errors.Is(err, context.Canceled) {
		t.Fatalf("关闭没有返回取消: %v", err)
	}
	if err := h.s.Resume(context.Background()); model.CodeOf(err) != model.CodeBackendUnavailable {
		t.Fatalf("停止后恢复未拒绝: %v", err)
	}
	if err := h.s.SetEnabled(context.Background(), "work", false); model.CodeOf(err) != model.CodeBackendUnavailable {
		t.Fatalf("停止后变更未拒绝: %v", err)
	}
	if h.s.Status()[0].NextTrigger != nil {
		t.Fatal("停止后仍有下一触发时间")
	}
	clock.mu.Lock()
	defer clock.mu.Unlock()
	for _, timer := range clock.timers {
		if !timer.stopped {
			t.Fatal("退出后仍有未停止的调度计时器")
		}
	}
}

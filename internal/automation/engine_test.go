package automation

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/orzlee/displayKvm/internal/daemon"
	"github.com/orzlee/displayKvm/internal/model"
)

// 可控时钟只推进本地调度时间；事件ObservedAt独立构造，防止测试把外部时间
// 错当成防抖或冷却依据。真实定时器仅作死锁保护，不参与业务断言。
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

type fakeTimer struct {
	owner   *fakeClock
	at      time.Time
	ch      chan time.Time
	stopped bool
}

func newFakeClock() *fakeClock      { return &fakeClock{now: time.Now()} }
func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *fakeClock) NewTimer(d time.Duration) engineTimer {
	c.mu.Lock()
	defer c.mu.Unlock()
	timer := &fakeTimer{owner: c, at: c.now.Add(d), ch: make(chan time.Time, 1)}
	if d <= 0 {
		timer.ch <- c.now
		timer.stopped = true
	}
	c.timers = append(c.timers, timer)
	return timer
}
func (t *fakeTimer) C() <-chan time.Time { return t.ch }
func (t *fakeTimer) Stop()               { t.owner.mu.Lock(); t.stopped = true; t.owner.mu.Unlock() }
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	for _, timer := range c.timers {
		if !timer.stopped && !timer.at.After(c.now) {
			timer.stopped = true
			timer.ch <- c.now
		}
	}
}

type scanReply struct {
	baseline daemon.Baseline
	err      error
}
type engineHarness struct {
	e      *Engine
	clock  *fakeClock
	events chan model.Event
	calls  chan model.SwitchRequest
	scans  chan chan scanReply
	finish chan error
	cancel context.CancelFunc
	result func(context.Context, model.SwitchRequest) (model.SwitchResult, error)
}

func startHarness(t *testing.T, rules []Rule, result func(context.Context, model.SwitchRequest) (model.SwitchResult, error)) *engineHarness {
	t.Helper()
	h := &engineHarness{clock: newFakeClock(), events: make(chan model.Event, 64), calls: make(chan model.SwitchRequest, 64), scans: make(chan chan scanReply, 8), finish: make(chan error, 1), result: result}
	rt := daemon.AutomationRuntime{
		Events: h.events,
		Switch: func(ctx context.Context, request model.SwitchRequest) (model.SwitchResult, error) {
			h.calls <- request
			if h.result != nil {
				return h.result(ctx, request)
			}
			return model.SwitchResult{RequestID: request.ID, Error: model.NewError(model.CodeUnreachable, "fake unknown", nil)}, nil
		},
		Rescan: func(ctx context.Context) (daemon.Baseline, error) {
			reply := make(chan scanReply, 1)
			select {
			case h.scans <- reply:
			case <-ctx.Done():
				return daemon.Baseline{}, ctx.Err()
			}
			select {
			case value := <-reply:
				return value.baseline, value.err
			case <-ctx.Done():
				return daemon.Baseline{}, ctx.Err()
			}
		},
	}
	var err error
	h.e, err = New(rt, testConfig(), rules)
	if err != nil {
		t.Fatal(err)
	}
	h.e.clock = h.clock
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	go func() { h.finish <- h.e.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-h.finish:
			if !errors.Is(err, context.Canceled) {
				t.Errorf("退出错误: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("自动化取消未回收工作")
		}
	})
	return h
}

func receive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(3 * time.Second):
		t.Fatal("等待测试握手超时")
		var zero T
		return zero
	}
}

func awaitRecord(t *testing.T, e *Engine, predicate func(Record) bool) Record {
	t.Helper()
	guard := time.NewTimer(3 * time.Second)
	defer guard.Stop()
	for {
		for _, record := range e.Records() {
			if predicate(record) {
				return record
			}
		}
		select {
		case <-guard.C:
			t.Fatal("等待触发记录超时")
			return Record{}
		default:
			runtime.Gosched()
		}
	}
}

func (h *engineHarness) baseline(t *testing.T, devices ...model.USBDevice) {
	t.Helper()
	receive(t, h.scans) <- scanReply{baseline: daemon.Baseline{ObservedAt: time.Unix(100, 0), USBKnown: true, USBDevices: devices}}
	awaitRecord(t, h.e, func(r Record) bool { return r.Decision.Reason == "rescan_completed" })
}

func event(id string, kind model.EventKind, at int64) model.Event {
	e := model.Event{ID: id, Source: "fake", Kind: kind, ObservedAt: time.Unix(at, 0)}
	if kind == model.EventCustom || kind == model.EventTimer {
		e.Name = "dock"
	}
	return e
}

func (h *engineHarness) emit(t *testing.T, e model.Event) {
	t.Helper()
	h.events <- e
	awaitRecord(t, h.e, func(r Record) bool { return r.Event != nil && r.Event.ID == e.ID })
}

func noCall(t *testing.T, h *engineHarness) {
	t.Helper()
	select {
	case request := <-h.calls:
		t.Fatalf("意外切换: %#v", request)
	default:
	}
}

func TestDebounceStormAndDuplicateDoNotExtendDeadline(t *testing.T) {
	rule := customRule("storm")
	rule.DebounceMillis, rule.CooldownMillis = 100, 1000
	h := startHarness(t, []Rule{rule}, nil)
	h.baseline(t)
	for i := 0; i < 30; i++ {
		h.emit(t, event(fmt.Sprintf("e%d", i), model.EventCustom, int64(200+i)))
	}
	h.clock.Advance(99 * time.Millisecond)
	duplicate := event("e29", model.EventCustom, 229)
	h.events <- duplicate
	awaitRecord(t, h.e, func(r Record) bool { return r.Decision.Reason == "duplicate" })
	noCall(t, h)
	h.clock.Advance(time.Millisecond)
	first := receive(t, h.calls)
	if first.TargetID != "self" || !model.ValidID(string(first.ID)) {
		t.Fatalf("请求无效: %#v", first)
	}
	awaitRecord(t, h.e, func(r Record) bool { return r.Decision.Reason == "completed" })
	h.emit(t, event("cooling", model.EventCustom, 50000))
	h.clock.Advance(100 * time.Millisecond)
	awaitRecord(t, h.e, func(r Record) bool { return r.Decision.Reason == "cooldown" })
	noCall(t, h)
	h.clock.Advance(time.Second)
	h.emit(t, event("later", model.EventCustom, 50001))
	h.clock.Advance(100 * time.Millisecond)
	if next := receive(t, h.calls); next.ID == first.ID {
		t.Fatal("实际请求复用了关联ID")
	}
}

func TestLongDebounceCannotOverflowIntoImmediateSwitch(t *testing.T) {
	rule := customRule("long-wait")
	rule.DebounceMillis = (1<<63 - 1) / int64(time.Millisecond)
	h := startHarness(t, []Rule{rule}, nil)
	h.baseline(t)
	h.emit(t, event("waiting", model.EventCustom, 200))
	h.clock.Advance(24 * time.Hour)
	later := event("later-preview", model.EventCustom, 201)
	decisions, err := h.e.Preview(later)
	if err != nil || len(decisions) != 1 || decisions[0].Reason != "pending" {
		t.Fatalf("极长防抖被错误地解释为已到期: %v %#v", err, decisions)
	}
	noCall(t, h)
}

func TestPriorityStableTieAndDisjointScopes(t *testing.T) {
	z, a, right := customRule("z"), customRule("a"), customRule("right")
	z.Action.TargetID, z.Action.DisplayIDs = "other", []model.DisplayID{"left"}
	a.Action.DisplayIDs, a.CooldownMillis = []model.DisplayID{"left"}, 1000
	right.Action.DisplayIDs = []model.DisplayID{"right"}
	h := startHarness(t, []Rule{z, right, a}, nil)
	h.baseline(t)
	h.emit(t, event("choose", model.EventCustom, 200))
	for i := 0; i < 2; i++ {
		if request := receive(t, h.calls); request.TargetID != "self" {
			t.Fatal("ID稳定裁决未选a")
		}
	}
	awaitRecord(t, h.e, func(r Record) bool { return r.Decision.RuleID == "z" && r.Decision.Reason == "conflict" })
	awaitRecord(t, h.e, func(r Record) bool { return r.Decision.RuleID == "a" && r.Decision.Reason == "completed" })
	awaitRecord(t, h.e, func(r Record) bool { return r.Decision.RuleID == "right" && r.Decision.Reason == "completed" })
	h.emit(t, event("cooling", model.EventCustom, 201))
	if request := receive(t, h.calls); len(request.DisplayIDs) != 1 || request.DisplayIDs[0] != "right" {
		t.Fatal("冷却期间低优先级旁落")
	}
	awaitRecord(t, h.e, func(r Record) bool {
		return r.Event != nil && r.Event.ID == "cooling" && r.Decision.RuleID == "z" && r.Decision.Reason == "conflict"
	})
	noCall(t, h)
}

func TestExplicitPriorityAndPendingConflictPreview(t *testing.T) {
	low, high := customRule("a-low"), customRule("z-high")
	low.When.Name, low.Action.TargetID = "low", "other"
	low.Action.DisplayIDs = []model.DisplayID{"left"}
	high.When.Name, high.Action.DisplayIDs = "high", []model.DisplayID{"left"}
	high.Priority, high.DebounceMillis = 10, 100
	h := startHarness(t, []Rule{low, high}, nil)
	h.baseline(t)
	e := event("high", model.EventCustom, 200)
	e.Name = "high"
	h.emit(t, e)
	e = event("low", model.EventCustom, 201)
	e.Name = "low"
	decisions, err := h.e.Preview(e)
	if err != nil {
		t.Fatal(err)
	}
	for _, decision := range decisions {
		if decision.RuleID == "a-low" && decision.Reason != "conflict" {
			t.Fatalf("预览忽略已有高优先级候选: %#v", decision)
		}
	}
	h.emit(t, e)
	awaitRecord(t, h.e, func(r Record) bool { return r.Decision.RuleID == "a-low" && r.Decision.Reason == "conflict" })
	h.clock.Advance(100 * time.Millisecond)
	if request := receive(t, h.calls); request.TargetID != "self" {
		t.Fatal("显式优先级未生效")
	}
	noCall(t, h)
}

func TestInFlightReservationConsumesEventsAndCancellationWaits(t *testing.T) {
	released := make(chan struct{})
	h := startHarness(t, []Rule{customRule("blocked")}, func(ctx context.Context, request model.SwitchRequest) (model.SwitchResult, error) {
		<-ctx.Done()
		close(released)
		return model.SwitchResult{}, ctx.Err()
	})
	h.baseline(t)
	h.emit(t, event("one", model.EventCustom, 200))
	receive(t, h.calls)
	h.emit(t, event("two", model.EventCustom, 201))
	awaitRecord(t, h.e, func(r Record) bool { return r.Decision.Reason == "in_flight" })
	noCall(t, h)
	h.cancel()
	receive(t, released)
}

func TestStartupStateRuleDoesNotPretendInsertionOrLogin(t *testing.T) {
	present := true
	state := customRule("present")
	state.When = Condition{USBPresent: &present, USB: &USBMatch{Path: "dock"}}
	added := customRule("added")
	added.When = Condition{Kind: model.EventUSBAdded, USB: &USBMatch{Path: "dock"}}
	login := customRule("login")
	login.When = Condition{Kind: model.EventSessionStarted}
	h := startHarness(t, []Rule{state, added, login}, nil)
	h.baseline(t, model.USBDevice{Path: "dock"})
	receive(t, h.calls)
	awaitRecord(t, h.e, func(r Record) bool { return r.Decision.Reason == "completed" })
	for _, r := range h.e.Records() {
		if r.Decision.Reason == "submitted" && r.Decision.RuleID != "present" {
			t.Fatalf("基线伪装真实事件: %#v", r)
		}
	}
	noCall(t, h)
}

func TestReverseUSBStateCancelsDebounceAndStaleRemovalCannotRegress(t *testing.T) {
	present := true
	rule := customRule("usb")
	rule.When, rule.DebounceMillis = Condition{USBPresent: &present, USB: &USBMatch{Path: "dock"}}, 100
	h := startHarness(t, []Rule{rule}, nil)
	h.baseline(t)
	added := event("add", model.EventUSBAdded, 200)
	added.USB = &model.USBDevice{Path: "dock"}
	removed := event("remove", model.EventUSBRemoved, 201)
	removed.USB = added.USB
	h.emit(t, added)
	h.emit(t, removed)
	h.clock.Advance(100 * time.Millisecond)
	noCall(t, h)
	added.ID, added.ObservedAt = "new-add", time.Unix(300, 0)
	h.emit(t, added)
	removed.ID = "old-remove"
	h.emit(t, removed)
	awaitRecord(t, h.e, func(r Record) bool {
		return r.Event != nil && r.Event.ID == "old-remove" && r.Decision.Reason == "stale"
	})
	h.clock.Advance(100 * time.Millisecond)
	receive(t, h.calls)
}

func TestUnrelatedUSBChangeDoesNotRetriggerStateOrExtendDebounce(t *testing.T) {
	present := true
	rule := customRule("state-edge")
	rule.When, rule.DebounceMillis = Condition{USBPresent: &present, USB: &USBMatch{Path: "dock"}}, 100
	h := startHarness(t, []Rule{rule}, nil)
	h.baseline(t, model.USBDevice{Path: "dock"})
	h.clock.Advance(90 * time.Millisecond)
	keyboard := event("keyboard", model.EventUSBAdded, 200)
	keyboard.USB = &model.USBDevice{Path: "keyboard"}
	h.emit(t, keyboard)
	h.clock.Advance(10 * time.Millisecond)
	receive(t, h.calls)
	awaitRecord(t, h.e, func(r Record) bool { return r.Decision.Reason == "completed" })
	keyboard.ID, keyboard.Kind, keyboard.ObservedAt = "keyboard-out", model.EventUSBRemoved, time.Unix(201, 0)
	h.emit(t, keyboard)
	h.clock.Advance(time.Second)
	noCall(t, h)
}

func TestSameTimeContradictionRescansAndNeverReusesBaseline(t *testing.T) {
	present := true
	rule := customRule("ambiguous")
	rule.When, rule.DebounceMillis = Condition{USBPresent: &present, USB: &USBMatch{Path: "dock"}}, 100
	h := startHarness(t, []Rule{rule}, nil)
	h.baseline(t)
	added := event("add", model.EventUSBAdded, 200)
	added.USB = &model.USBDevice{Path: "dock"}
	h.emit(t, added)
	removed := added
	removed.ID, removed.Kind = "contradiction", model.EventUSBRemoved
	h.emit(t, removed)
	newScan := receive(t, h.scans)
	h.clock.Advance(time.Second)
	noCall(t, h)
	newScan <- scanReply{baseline: daemon.Baseline{ObservedAt: time.Unix(201, 0), USBKnown: true}}
	awaitRecord(t, h.e, func(r Record) bool { return r.Decision.Reason == "rescan_completed" && r.At.Equal(h.clock.Now()) })
	noCall(t, h)
}

func TestRescanCannotPublishOlderThanLastKnownUSBObservation(t *testing.T) {
	present := true
	rule := customRule("newer-state")
	rule.When, rule.DebounceMillis = Condition{USBPresent: &present, USB: &USBMatch{Path: "dock"}}, 100
	h := startHarness(t, []Rule{rule}, nil)
	h.baseline(t)
	added := event("new-add", model.EventUSBAdded, 300)
	added.USB = &model.USBDevice{Path: "dock"}
	h.emit(t, added)
	h.emit(t, event("wake", model.EventResume, 400))
	receive(t, h.scans) <- scanReply{baseline: daemon.Baseline{ObservedAt: time.Unix(200, 0), USBKnown: true}}
	awaitRecord(t, h.e, func(r Record) bool { return r.Decision.Reason == "rescan_failed" })
	h.clock.Advance(time.Second)
	noCall(t, h)
}

func TestEqualTimeBaselineCannotOverrideContradictoryUSBFact(t *testing.T) {
	present := true
	rule := customRule("equal-time")
	rule.When, rule.DebounceMillis = Condition{USBPresent: &present, USB: &USBMatch{Path: "dock"}}, 100
	h := startHarness(t, []Rule{rule}, nil)
	h.baseline(t)
	added := event("add", model.EventUSBAdded, 300)
	added.USB = &model.USBDevice{Path: "dock"}
	h.emit(t, added)
	h.emit(t, event("wake", model.EventResume, 400))
	receive(t, h.scans) <- scanReply{baseline: daemon.Baseline{ObservedAt: time.Unix(300, 0), USBKnown: true}}
	awaitRecord(t, h.e, func(r Record) bool { return r.Decision.Reason == "rescan_failed" })
	h.clock.Advance(time.Second)
	noCall(t, h)
}

func TestBufferedUSBEventAfterFreshBaselineIsApplied(t *testing.T) {
	rule := customRule("added")
	rule.When = Condition{Kind: model.EventUSBAdded, USB: &USBMatch{Path: "dock"}}
	h := startHarness(t, []Rule{rule}, nil)
	scan := receive(t, h.scans)
	added := event("buffered", model.EventUSBAdded, 200)
	added.USB = &model.USBDevice{Path: "dock"}
	h.emit(t, added)
	awaitRecord(t, h.e, func(r Record) bool { return r.Decision.Reason == "rescan_pending" })
	scan <- scanReply{baseline: daemon.Baseline{ObservedAt: time.Unix(100, 0), USBKnown: true}}
	receive(t, h.calls)
}

func TestUnknownResultAndReturnedErrorRetainCooldown(t *testing.T) {
	for _, returnError := range []bool{false, true} {
		t.Run(fmt.Sprint(returnError), func(t *testing.T) {
			rule := customRule("uncertain")
			rule.Action.DisplayIDs, rule.CooldownMillis = []model.DisplayID{"left"}, 1000
			failure := errors.New("write outcome unavailable")
			h := startHarness(t, []Rule{rule}, func(ctx context.Context, request model.SwitchRequest) (model.SwitchResult, error) {
				if returnError {
					return model.SwitchResult{}, failure
				}
				return model.SwitchResult{RequestID: request.ID, Displays: []model.DisplayResult{{DisplayID: "left", State: model.StateUnknown, DesiredValue: 17, Error: model.NewError(model.CodeUnreachable, "无法判定是否发送", failure)}}}, nil
			})
			h.baseline(t)
			h.emit(t, event("first", model.EventCustom, 200))
			receive(t, h.calls)
			completed := awaitRecord(t, h.e, func(r Record) bool { return r.Decision.Reason == "completed" })
			if returnError {
				if !errors.Is(completed.Error, failure) {
					t.Fatal("动作错误根因丢失")
				}
			} else if completed.Result.Displays[0].State != model.StateUnknown || !errors.Is(completed.Result.Displays[0].Error, failure) {
				t.Fatal("unknown逐屏证据丢失")
			}
			h.emit(t, event("again", model.EventCustom, 10000))
			awaitRecord(t, h.e, func(r Record) bool { return r.Decision.Reason == "cooldown" })
			noCall(t, h)
		})
	}
}

func TestResultSnapshotPreservesAndCopiesNestedEvidence(t *testing.T) {
	rule := customRule("evidence")
	rule.Action.DisplayIDs = []model.DisplayID{"left"}
	at := time.Unix(500, 0)
	value := uint16(17)
	h := startHarness(t, []Rule{rule}, func(ctx context.Context, request model.SwitchRequest) (model.SwitchResult, error) {
		return model.SwitchResult{RequestID: request.ID, Displays: []model.DisplayResult{{
			DisplayID: "left", State: model.StateConfirmed, DesiredValue: 17,
			SentAt: &at, Observation: &model.InputObservation{Value: &value, ObservedAt: at},
		}}}, nil
	})
	h.baseline(t)
	h.emit(t, event("send", model.EventCustom, 200))
	receive(t, h.calls)
	copy := awaitRecord(t, h.e, func(r Record) bool { return r.Decision.Reason == "completed" })
	if copy.Error != nil || copy.Result.Displays[0].State != model.StateConfirmed {
		t.Fatalf("有效逐屏证据被改写: %#v", copy)
	}
	*copy.Result.Displays[0].SentAt = time.Time{}
	*copy.Result.Displays[0].Observation.Value = 42
	for _, record := range h.e.Records() {
		if record.Decision.Reason == "completed" && (record.Result.Displays[0].SentAt.IsZero() || *record.Result.Displays[0].Observation.Value != 17) {
			t.Fatal("返回结果泄漏嵌套观测指针")
		}
	}
}

func TestResumeUsesFreshRescanAndUnknownIsNotAbsence(t *testing.T) {
	absent := false
	rule := customRule("absent")
	rule.When = Condition{USBPresent: &absent, USB: &USBMatch{Path: "dock"}}
	h := startHarness(t, []Rule{rule}, nil)
	h.baseline(t, model.USBDevice{Path: "dock"})
	h.emit(t, event("wake", model.EventResume, 200))
	failure := errors.New("fresh scan failed")
	receive(t, h.scans) <- scanReply{err: failure}
	r := awaitRecord(t, h.e, func(r Record) bool { return r.Decision.Reason == "rescan_failed" })
	if !errors.Is(r.Error, failure) {
		t.Fatal("补扫错误根因丢失")
	}
	noCall(t, h)
	h.emit(t, event("wake-again", model.EventResume, 300))
	receive(t, h.scans) <- scanReply{baseline: daemon.Baseline{ObservedAt: time.Unix(301, 0), USBKnown: true}}
	receive(t, h.calls)
}

func TestCustomMatchingPreviewAndSnapshotsAreReadOnly(t *testing.T) {
	rule := customRule("strict")
	rule.When.Attributes = map[string]string{"role": "owner"}
	rule.Action.DisplayIDs = []model.DisplayID{"left"}
	h := startHarness(t, []Rule{rule}, nil)
	// 构造后调用者更改原规则，不能改变已注册自动化的不可变规则快照。
	rule.When.Attributes["role"], rule.Action.DisplayIDs[0] = "guest", "right"
	h.baseline(t)
	e := event("preview", model.EventCustom, 200)
	e.Attributes = map[string]string{"role": "owner"}
	before := len(h.e.Records())
	for i := 0; i < 2; i++ {
		decisions, err := h.e.Preview(e)
		if err != nil || len(decisions) != 1 || decisions[0].Reason != "ready" {
			t.Fatalf("预览错误: %v %#v", err, decisions)
		}
		decisions[0].Request.DisplayIDs[0] = "right"
	}
	if len(h.e.Records()) != before {
		t.Fatal("预览写入记录")
	}
	noCall(t, h)
	select {
	case <-h.scans:
		t.Fatal("预览触发补扫")
	default:
	}
	h.emit(t, e)
	request := receive(t, h.calls)
	if request.DisplayIDs[0] != "left" {
		t.Fatal("规则或预览返回值泄漏可变切片")
	}
	r := awaitRecord(t, h.e, func(r Record) bool { return r.Decision.Reason == "completed" })
	if r.Event == nil {
		t.Fatal("结果没有触发事件证据")
	}
	for _, copy := range h.e.Records() {
		if copy.Event != nil {
			copy.Event.Attributes["role"] = "mutated"
		}
		if len(copy.Decision.Request.DisplayIDs) > 0 {
			copy.Decision.Request.DisplayIDs[0] = "right"
		}
	}
	for _, copy := range h.e.Records() {
		if copy.Event != nil && copy.Event.ID == e.ID && copy.Event.Attributes["role"] != "owner" {
			t.Fatal("Records泄漏内部映射")
		}
	}
	wrong := event("wrong", model.EventCustom, 201)
	wrong.Attributes = map[string]string{"role": "guest"}
	h.emit(t, wrong)
	noCall(t, h)
}

func TestDisabledRulesAndBoundedRecords(t *testing.T) {
	rule := customRule("off")
	rule.Enabled = false
	h := startHarness(t, []Rule{rule}, nil)
	h.baseline(t)
	for i := 0; i < recordLimit+20; i++ {
		h.emit(t, event(fmt.Sprintf("off-%d", i), model.EventCustom, int64(200+i)))
	}
	if len(h.e.Records()) != recordLimit {
		t.Fatal("触发记录没有有界保留")
	}
	noCall(t, h)
}

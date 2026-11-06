package automation

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sort"
	"sync"
	"time"

	"github.com/orzlee/displayKvm/internal/config"
	"github.com/orzlee/displayKvm/internal/daemon"
	"github.com/orzlee/displayKvm/internal/model"
)

const (
	recordLimit     = 256
	replayLimit     = 1024
	scanBufferLimit = 128
)

// Decision描述规则裁决，不代替逐屏ExecutionState，也不声称显示器画面成功。
// Request只在submitted/completed记录中有新ID；预览保留空ID，不消耗随机数。
type Decision struct {
	RuleID     string              `json:"rule_id,omitempty"`
	EventID    string              `json:"event_id,omitempty"`
	Reason     string              `json:"reason"`
	Request    model.SwitchRequest `json:"request"`
	DisplayIDs []model.DisplayID   `json:"display_ids,omitempty"`
}

type Record struct {
	At       time.Time           `json:"at"`
	Trigger  string              `json:"trigger"`
	Event    *model.Event        `json:"event,omitempty"`
	Decision Decision            `json:"decision"`
	Result   *model.SwitchResult `json:"result,omitempty"`
	Error    *model.Error        `json:"error,omitempty"`
}

type engineClock interface {
	Now() time.Time
	NewTimer(time.Duration) engineTimer
}

type engineTimer interface {
	C() <-chan time.Time
	Stop()
}

type localClock struct{}
type localTimer struct{ timer *time.Timer }

func (localClock) Now() time.Time                       { return time.Now() }
func (localClock) NewTimer(d time.Duration) engineTimer { return localTimer{time.NewTimer(d)} }
func (t localTimer) C() <-chan time.Time                { return t.timer.C }
func (t localTimer) Stop()                              { t.timer.Stop() }

type candidate struct {
	index   int
	event   *model.Event
	trigger string
	wait    localDelay
}

// 时长始终与本地起点比较，不构造未来墙钟时间。time.Time.Add在极长合法时长下
// 可能丢弃单调部分；用Now.Sub保留单调差值，防止校时绕过冷却或提前结束防抖。
type localDelay struct {
	started  time.Time
	duration time.Duration
}

func (d localDelay) remaining(now time.Time) time.Duration {
	if d.duration <= 0 {
		return 0
	}
	elapsed := now.Sub(d.started)
	if elapsed >= d.duration {
		return 0
	}
	if elapsed < 0 {
		return d.duration
	}
	return d.duration - elapsed
}

type switchJob struct {
	candidate candidate
	request   model.SwitchRequest
}

type adjudication struct {
	candidate candidate
	reason    string
}

type scanOutcome struct {
	baseline daemon.Baseline
	err      error
}

// Engine仅通过后台Runtime进入08的单写入者。Run拥有本实例的工作生命周期，
// 锁只保护规则状态与查询快照；绝不能持锁调用Switch/Rescan等可能阻塞的外部钩子。
type Engine struct {
	mu           sync.Mutex
	runtime      daemon.AutomationRuntime
	config       config.Config
	rules        []Rule
	scopes       [][]model.DisplayID
	clock        engineClock
	started      bool
	state        usbState
	pending      map[int]candidate
	inFlight     map[int]bool
	busy         map[model.DisplayID]bool
	ruleUntil    map[int]localDelay
	displayUntil map[model.DisplayID]localDelay
	stateMatches map[int]bool
	seen         map[string]bool
	seenOrder    []string
	namedAt      map[string]time.Time
	namedOrder   []string
	scanning     bool
	scanAgain    bool
	buffered     []model.Event
	records      []Record
}

func New(rt daemon.AutomationRuntime, cfg config.Config, rules []Rule) (*Engine, error) {
	if rt.Events == nil || rt.Switch == nil || rt.Rescan == nil {
		return nil, model.NewError(model.CodeInvalidConfig, "自动化需要后台事件、切换和新轮次补扫钩子", nil)
	}
	cfg, rules = cloneConfig(cfg), cloneRules(rules)
	if err := ValidateRules(cfg, rules); err != nil {
		return nil, err
	}
	sort.Slice(rules, func(i, j int) bool {
		if rules[i].Priority != rules[j].Priority {
			return rules[i].Priority > rules[j].Priority
		}
		return rules[i].ID < rules[j].ID
	})
	e := &Engine{
		runtime: rt, config: cfg, rules: rules, clock: localClock{},
		pending: make(map[int]candidate), inFlight: make(map[int]bool), busy: make(map[model.DisplayID]bool),
		ruleUntil: make(map[int]localDelay), displayUntil: make(map[model.DisplayID]localDelay),
		stateMatches: make(map[int]bool),
		seen:         make(map[string]bool), namedAt: make(map[string]time.Time),
	}
	for _, rule := range rules {
		scope, err := resolveScope(cfg, rule.Action)
		if err != nil {
			return nil, err
		}
		e.scopes = append(e.scopes, scope)
	}
	return e, nil
}

// Run只运行一次，返回前取消并等待自己创建的全部工作，不关闭后台拥有的通道。
// 冷却在调用Switch前占位，既覆盖08中的排队时间，也覆盖提交后结果不确定的窗口。
func (e *Engine) Run(ctx context.Context) error {
	e.mu.Lock()
	if e.started {
		e.mu.Unlock()
		return model.NewError(model.CodeInvalidRequest, "同一自动化实例只能Run一次", nil)
	}
	e.started = true
	e.mu.Unlock()
	life, cancel := context.WithCancel(ctx)
	var workers sync.WaitGroup
	defer func() {
		cancel()
		workers.Wait()
		e.mu.Lock()
		for _, pending := range e.pending {
			e.recordCandidate(pending, "canceled", model.SwitchRequest{}, nil, nil)
		}
		clear(e.pending)
		e.scanning, e.buffered = false, nil
		e.mu.Unlock()
	}()
	scanned := make(chan scanOutcome, 1)
	completed := make(chan struct{}, 1)
	// 此闭包仅在主循环持锁时调用。外部补扫在独立工作中执行，期间继续消费长期
	// 来源；有界缓存溢出或再次唤醒时废弃该轮结果，再取新基线，不能重播旧快照。
	startScan := func() {
		e.scanning = true
		e.invalidateUSB()
		e.addRecord(Record{Trigger: "rescan", Decision: Decision{Reason: "rescan_started"}})
		workers.Add(1)
		go func() {
			defer workers.Done()
			baseline, err := e.runtime.Rescan(life)
			if err != nil && life.Err() != nil {
				// 退出仍须保留来源回收错误的本地根因；主循环可能已选择取消分支，
				// 不能要求它继续消费补扫结果才能留下诊断证据。
				e.mu.Lock()
				e.addRecord(Record{Trigger: "rescan", Decision: Decision{Reason: "rescan_failed"}, Error: operationError(err, "退出时补扫或来源回收失败")})
				e.mu.Unlock()
			}
			// Baseline可能包含来源持有的切片，回到状态循环前先取得自己的副本。
			baseline.USBDevices = append([]model.USBDevice(nil), baseline.USBDevices...)
			if baseline.SessionPresent != nil {
				present := *baseline.SessionPresent
				baseline.SessionPresent = &present
			}
			scanned <- scanOutcome{baseline: baseline, err: err}
		}()
	}
	e.mu.Lock()
	if life.Err() == nil {
		startScan()
	}
	e.mu.Unlock()
	for {
		if err := life.Err(); err != nil {
			return err
		}
		e.mu.Lock()
		now := e.clock.Now()
		jobs, next := e.ready(now)
		var timer engineTimer
		var timerC <-chan time.Time
		if next >= 0 {
			timer = e.clock.NewTimer(next - e.clock.Now().Sub(now))
			timerC = timer.C()
		}
		e.mu.Unlock()
		for _, job := range jobs {
			workers.Add(1)
			go func(job switchJob) {
				defer workers.Done()
				e.switchOne(life, job)
				select {
				case completed <- struct{}{}:
				default:
				}
			}(job)
		}
		var result error
		select {
		case <-life.Done():
			result = life.Err()
		case <-timerC:
		case <-completed:
		case event, open := <-e.runtime.Events:
			if !open {
				result = model.NewError(model.CodeBackendUnavailable, "后台事件通道已关闭", nil)
				break
			}
			if err := event.Validate(); err != nil {
				result = model.NewError(model.CodeInvalidRequest, "自动化收到非法事件", err)
				break
			}
			e.mu.Lock()
			needScan := e.observe(*cloneEvent(&event), true)
			if needScan {
				if e.scanning {
					e.scanAgain = true
				} else {
					startScan()
				}
			}
			e.mu.Unlock()
		case outcome := <-scanned:
			e.mu.Lock()
			e.scanning = false
			if e.scanAgain {
				e.scanAgain = false
				e.addRecord(Record{Trigger: "rescan", Decision: Decision{Reason: "rescan_superseded"}, Error: operationError(outcome.err, "被新补扫取代的轮次失败")})
				startScan()
			} else {
				if outcome.err == nil {
					outcome.err = e.state.baseline(outcome.baseline)
				}
				if outcome.err != nil {
					e.invalidateUSB()
					e.buffered = nil
					e.addRecord(Record{Trigger: "rescan", Decision: Decision{Reason: "rescan_failed"}, Error: operationError(outcome.err, "新轮次补扫失败，USB状态保持未知")})
				} else {
					e.evaluate(nil, "baseline", true)
					buffered := e.buffered
					e.buffered = nil
					needScan := false
					for _, event := range buffered {
						if e.observe(event, false) {
							needScan = true
						}
					}
					e.addRecord(Record{Trigger: "rescan", Decision: Decision{Reason: "rescan_completed"}})
					if needScan {
						startScan()
					}
				}
			}
			e.mu.Unlock()
		}
		if timer != nil {
			timer.Stop()
		}
		if result != nil {
			return result
		}
	}
}

func (e *Engine) observe(event model.Event, remember bool) bool {
	e.addRecord(Record{Trigger: "event", Event: &event, Decision: Decision{EventID: event.ID, Reason: "observed"}})
	if remember {
		key := event.Source + "\x00" + event.ID
		if e.seen[key] {
			e.addRecord(Record{Trigger: "event", Event: &event, Decision: Decision{EventID: event.ID, Reason: "duplicate"}})
			return false
		}
		if len(e.seenOrder) == replayLimit {
			delete(e.seen, e.seenOrder[0])
			e.seenOrder = e.seenOrder[1:]
		}
		e.seen[key] = true
		e.seenOrder = append(e.seenOrder, key)
	}
	if isUSB(&event) {
		if e.scanning {
			if len(e.buffered) == scanBufferLimit {
				e.buffered = nil
				e.scanAgain = true
			}
			e.buffered = append(e.buffered, event)
			e.addRecord(Record{Trigger: "event", Event: &event, Decision: Decision{EventID: event.ID, Reason: "rescan_pending"}})
			return false
		}
		reason, changed := e.state.apply(event)
		if !changed {
			e.addRecord(Record{Trigger: "event", Event: &event, Decision: Decision{EventID: event.ID, Reason: reason}})
			if reason == "rescan_required" {
				e.invalidateUSB()
				return true
			}
			return false
		}
		e.evaluate(&event, "event", true)
		return false
	}
	// 命名事件按来源/种类/名称维护有界观测水位；同刻重投不延长防抖。
	// 不同来源的时钟不能互相排序，所以不使用全局最后事件时间。
	key := event.Source + "\x00" + string(event.Kind) + "\x00" + event.Name
	if at, exists := e.namedAt[key]; exists && !event.ObservedAt.After(at) {
		e.addRecord(Record{Trigger: "event", Event: &event, Decision: Decision{EventID: event.ID, Reason: "stale"}})
		return false
	}
	if _, exists := e.namedAt[key]; !exists {
		if len(e.namedOrder) == replayLimit {
			delete(e.namedAt, e.namedOrder[0])
			e.namedOrder = e.namedOrder[1:]
		}
		e.namedOrder = append(e.namedOrder, key)
	}
	e.namedAt[key] = event.ObservedAt
	if event.Kind == model.EventResume {
		e.invalidateUSB()
	}
	e.evaluate(&event, "event", false)
	return event.Kind == model.EventResume
}

func (e *Engine) invalidateUSB() {
	e.state.known = false
	clear(e.stateMatches)
	for index, pending := range e.pending {
		if e.rules[index].When.USB != nil {
			e.recordCandidate(pending, "state_unknown", model.SwitchRequest{}, nil, nil)
			delete(e.pending, index)
		}
	}
}

func (e *Engine) evaluate(event *model.Event, trigger string, stateChanged bool) {
	now := e.clock.Now()
	for index, pending := range e.pending {
		if !candidateValid(e.state, e.rules[index], pending.event) {
			e.recordCandidate(pending, "state_changed", model.SwitchRequest{}, nil, nil)
			delete(e.pending, index)
		}
	}
	for index, rule := range e.rules {
		pending := candidate{index: index, event: cloneEvent(event), trigger: trigger, wait: localDelay{now, time.Duration(rule.DebounceMillis) * time.Millisecond}}
		if !rule.Enabled {
			e.recordCandidate(pending, "disabled", model.SwitchRequest{}, nil, nil)
			continue
		}
		matches := false
		if rule.When.USBPresent != nil {
			if !stateChanged {
				continue
			}
			matches = e.state.condition(rule.When)
			wasMatching := e.stateMatches[index]
			e.stateMatches[index] = matches
			// 状态规则表达条件从不满足/未知变为满足，而不是任意USB事件。
			// 例如扩展坞一直存在时插入键盘，不能延长扩展坞的防抖或再次切换。
			if matches && wasMatching {
				e.recordCandidate(pending, "unchanged_state", model.SwitchRequest{}, nil, nil)
				continue
			}
		} else {
			matches = eventMatches(rule.When, event)
		}
		if !matches {
			e.recordCandidate(pending, "no_match", model.SwitchRequest{}, nil, nil)
			continue
		}
		if previous, exists := e.pending[index]; exists {
			e.recordCandidate(previous, "replaced", model.SwitchRequest{}, nil, nil)
		}
		e.pending[index] = pending
		e.recordCandidate(pending, "pending", model.SwitchRequest{}, nil, nil)
	}
}

func (e *Engine) decision(index int, event *model.Event, reason string) Decision {
	decision := Decision{RuleID: e.rules[index].ID, Reason: reason, Request: cloneRequest(e.rules[index].Action), DisplayIDs: append([]model.DisplayID(nil), e.scopes[index]...)}
	if event != nil {
		decision.EventID = event.ID
	}
	return decision
}

func (e *Engine) recordCandidate(pending candidate, reason string, request model.SwitchRequest, result *model.SwitchResult, err error) {
	decision := e.decision(pending.index, pending.event, reason)
	if request.ID != "" {
		decision.Request = cloneRequest(request)
	}
	e.addRecord(Record{Trigger: pending.trigger, Event: pending.event, Decision: decision, Result: result, Error: operationError(err, "自动化动作失败")})
}

func operationError(err error, message string) *model.Error {
	if err == nil {
		return nil
	}
	return model.NewError(model.CodeOf(err), message, err)
}

func (e *Engine) addRecord(record Record) {
	record.At = e.clock.Now()
	record = cloneRecord(record)
	if len(e.records) == recordLimit {
		copy(e.records, e.records[1:])
		e.records[len(e.records)-1] = record
	} else {
		e.records = append(e.records, record)
	}
}

// adjudicate由真实分发和只读预览共用，只输出判定、不改变任何候选。
// 即使高优先级正在冷却或防抖，也保留本轮声明，禁止旁落到低优先级相反动作。
func (e *Engine) adjudicate(pendingByRule map[int]candidate, state usbState, now time.Time) []adjudication {
	claimed := make(map[model.DisplayID]bool)
	var results []adjudication
	for index := range e.rules {
		pending, exists := pendingByRule[index]
		if !exists {
			continue
		}
		if !candidateValid(state, e.rules[index], pending.event) {
			results = append(results, adjudication{pending, "state_changed"})
			continue
		}
		conflict := false
		for _, id := range e.scopes[index] {
			conflict = conflict || claimed[id]
		}
		if conflict {
			results = append(results, adjudication{pending, "conflict"})
			continue
		}
		for _, id := range e.scopes[index] {
			claimed[id] = true
		}
		reason := e.blocked(index, now)
		if reason == "" {
			reason = "ready"
			if pending.wait.remaining(now) > 0 {
				reason = "pending"
			}
		}
		results = append(results, adjudication{pending, reason})
	}
	return results
}

func (e *Engine) ready(now time.Time) ([]switchJob, time.Duration) {
	var jobs []switchJob
	next := time.Duration(-1)
	for _, result := range e.adjudicate(e.pending, e.state, now) {
		pending, reason := result.candidate, result.reason
		index := pending.index
		if reason == "pending" {
			if remaining := pending.wait.remaining(now); next < 0 || remaining < next {
				next = remaining
			}
			continue
		}
		delete(e.pending, index)
		if reason != "ready" {
			e.recordCandidate(pending, reason, model.SwitchRequest{}, nil, nil)
			continue
		}
		var randomID [16]byte
		if _, err := rand.Read(randomID[:]); err != nil {
			e.recordCandidate(pending, "rejected", model.SwitchRequest{}, nil, model.NewError(model.CodeInternal, "生成自动化请求ID失败", err))
			continue
		}
		request := cloneRequest(e.rules[index].Action)
		request.ID = model.RequestID("automation-" + hex.EncodeToString(randomID[:]))
		until := localDelay{now, time.Duration(e.rules[index].CooldownMillis) * time.Millisecond}
		e.ruleUntil[index], e.inFlight[index] = until, true
		for _, id := range e.scopes[index] {
			e.displayUntil[id], e.busy[id] = until, true
		}
		e.recordCandidate(pending, "submitted", request, nil, nil)
		jobs = append(jobs, switchJob{candidate: pending, request: request})
	}
	return jobs, next
}

func (e *Engine) blocked(index int, now time.Time) string {
	if e.inFlight[index] {
		return "in_flight"
	}
	for _, id := range e.scopes[index] {
		if e.busy[id] {
			return "in_flight"
		}
	}
	if e.ruleUntil[index].remaining(now) > 0 {
		return "cooldown"
	}
	for _, id := range e.scopes[index] {
		if e.displayUntil[id].remaining(now) > 0 {
			return "cooldown"
		}
	}
	return ""
}

func (e *Engine) switchOne(ctx context.Context, job switchJob) {
	operation, cancel := context.WithTimeout(ctx, job.request.EffectiveTimeout())
	defer cancel()
	var result model.SwitchResult
	err := operation.Err()
	if err == nil {
		result, err = e.runtime.Switch(operation, cloneRequest(job.request))
		if err == nil {
			err = result.Validate()
			if err == nil && result.RequestID != job.request.ID {
				err = model.NewError(model.CodeInvalidRequest, "切换结果关联ID与自动化请求不一致", nil)
			}
		}
	}
	// Runtime是唯一执行入口。错误或unknown都不能证明未发送，所以无论结果如何
	// 仅释放在途占位，绝不清除冷却、不重试、不反向切换，也不修改08的逐屏事实。
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.inFlight, job.candidate.index)
	for _, id := range e.scopes[job.candidate.index] {
		delete(e.busy, id)
	}
	e.recordCandidate(job.candidate, "completed", job.request, &result, err)
}

// Records返回最近256条记录的深副本；无订阅通道或落盘副作用。
func (e *Engine) Records() []Record {
	e.mu.Lock()
	defer e.mu.Unlock()
	records := make([]Record, len(e.records))
	for i, record := range e.records {
		records[i] = cloneRecord(record)
	}
	return records
}

// Preview在锁内只读当前快照，在局部副本模拟USB事实；不生成请求ID，不记录、
// 不建定时器，也不调用Switch/Rescan。需要补扫时只给出原因，由真实Run负责执行。
func (e *Engine) Preview(event model.Event) ([]Decision, error) {
	if err := event.Validate(); err != nil {
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	state := e.state.copy()
	event = *cloneEvent(&event)
	rejected := ""
	if e.seen[event.Source+"\x00"+event.ID] {
		rejected = "duplicate"
	} else if isUSB(&event) {
		reason, changed := state.apply(event)
		if !changed {
			rejected = reason
		}
	} else {
		key := event.Source + "\x00" + string(event.Kind) + "\x00" + event.Name
		if at, exists := e.namedAt[key]; exists && !event.ObservedAt.After(at) {
			rejected = "stale"
		}
	}
	if event.Kind == model.EventResume {
		state.known = false
	}
	now := e.clock.Now()
	trial := make(map[int]candidate, len(e.pending))
	for index, pending := range e.pending {
		trial[index] = pending
	}
	decisions := make([]Decision, 0, len(e.rules))
	for index, rule := range e.rules {
		reason := "no_match"
		matches := eventMatches(rule.When, &event)
		if rule.When.USBPresent != nil {
			matches = isUSB(&event) && state.condition(rule.When)
			if matches && e.stateMatches[index] {
				matches, reason = false, "unchanged_state"
			}
			if !state.known {
				reason = "state_unknown"
			}
		}
		if !rule.Enabled {
			reason = "disabled"
		} else if rejected != "" {
			reason = rejected
		} else if matches {
			trial[index] = candidate{index: index, event: &event, trigger: "preview", wait: localDelay{now, time.Duration(rule.DebounceMillis) * time.Millisecond}}
		}
		decisions = append(decisions, e.decision(index, &event, reason))
	}
	if rejected == "" {
		for _, result := range e.adjudicate(trial, state, now) {
			index := result.candidate.index
			decisions[index] = e.decision(index, result.candidate.event, result.reason)
		}
	}
	return decisions, nil
}

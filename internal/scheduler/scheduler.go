package scheduler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sync"
	"time"

	"github.com/orzlee/displayKvm/internal/automation"
	"github.com/orzlee/displayKvm/internal/config"
	"github.com/orzlee/displayKvm/internal/daemon"
	"github.com/orzlee/displayKvm/internal/model"
)

const eventBufferLimit = 64
const resumeReplayLimit = 1024

type command struct {
	ctx     context.Context
	resume  bool
	id      string
	enabled bool
	reply   chan error
}

// Scheduler只组合一台真实规则引擎，外部工厂须用本实例替换原引擎工厂。
// Run独占日程变更和事件分发；锁仅用于运行状态及Status快照，不能持锁调用后台钩子。
type Scheduler struct {
	mu       sync.Mutex
	engine   *automation.Engine
	clock    Clock
	input    <-chan model.Event
	output   chan model.Event
	commands chan command
	done     chan struct{}
	started  bool
	running  bool
	states   []scheduleState

	resumeIDs   map[string]bool
	resumeOrder []string
	resumeAt    map[string]time.Time
	sourceOrder []string
}

func New(runtime daemon.AutomationRuntime, cfg config.Config, rules []automation.Rule, schedules []Schedule, options Options) (*Scheduler, error) {
	if runtime.Events == nil {
		return nil, scheduleError("", "调度器需要后台事件流", nil)
	}
	if err := ValidateSchedules(schedules); err != nil {
		return nil, err
	}
	clock := options.Clock
	if clock == nil {
		clock = localClock{}
	}
	s := &Scheduler{
		clock: clock, input: runtime.Events, output: make(chan model.Event),
		commands: make(chan command, eventBufferLimit), done: make(chan struct{}),
		resumeIDs: make(map[string]bool), resumeAt: make(map[string]time.Time),
	}
	for _, schedule := range schedules {
		location, err := time.LoadLocation(schedule.Timezone)
		if err != nil {
			return nil, scheduleError("timezone", "日程时区不可加载", err)
		}
		s.states = append(s.states, scheduleState{
			schedule: schedule, location: location,
			hour:   int(schedule.Time[0]-'0')*10 + int(schedule.Time[1]-'0'),
			minute: int(schedule.Time[3]-'0')*10 + int(schedule.Time[4]-'0'),
		})
	}
	// 仅替换引擎输入流，动作和新轮次补扫仍使用同一后台Runtime；本包不建立第二队列。
	runtime.Events = s.output
	engine, err := automation.New(runtime, cfg, rules)
	if err != nil {
		return nil, err
	}
	s.engine = engine
	return s, nil
}

// Run只启动一次，启动时才建立时间基线，因此构造后延迟启动也不会补做历史。
// 退出先取消再等待真实引擎及其工作回收，不关闭后台拥有的事件通道。
func (s *Scheduler) Run(ctx context.Context) error {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return model.NewError(model.CodeInvalidRequest, "同一调度器实例只能Run一次", nil)
	}
	s.started, s.running = true, true
	now := s.clock.Now()
	for i := range s.states {
		s.states[i].rebase(now)
	}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.running = false
		for i := range s.states {
			s.states[i].next = time.Time{}
		}
		close(s.done)
		s.mu.Unlock()
	}()
	life, cancel := context.WithCancel(ctx)
	stopped := make(chan struct{})
	var engineErr error
	go func() {
		engineErr = s.engine.Run(life)
		close(stopped)
	}()
	err := s.loop(life, stopped)
	cancel()
	<-stopped
	if engineErr != nil && !errors.Is(engineErr, context.Canceled) && !errors.Is(engineErr, context.DeadlineExceeded) {
		return engineErr
	}
	if err != nil {
		return err
	}
	return engineErr
}

func (s *Scheduler) loop(ctx context.Context, stopped <-chan struct{}) error {
	input := s.input
	pending := make([]model.Event, 0, eventBufferLimit)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		// 到期通知只唤醒状态机，不直接分发。先处理已排队的控制命令与后台事件，
		// 使同一轮已就绪的Resume先使旧候选失效；尚未到达的通知无法撤回已交付事件。
		select {
		case request := <-s.commands:
			s.applyCommand(request)
			continue
		default:
		}
		if len(pending) < eventBufferLimit {
			select {
			case event, ok := <-input:
				if !ok {
					input = nil
				} else {
					s.observeResume(event)
					pending = append(pending, event)
				}
				continue
			default:
			}
		}
		now := s.clock.Now()
		event, index, next := s.nextEvent(now)
		var out chan<- model.Event
		if len(pending) > 0 {
			event, index, out = pending[0], -1, s.output
		} else if index >= 0 {
			out = s.output
		}
		var timer Timer
		var due <-chan time.Time
		if !next.IsZero() && next.After(now) {
			timer = s.clock.NewTimer(next.Sub(now))
			due = timer.C()
			// 校时或测试时钟可能在读取Now与建立相对计时器之间跨过目标；
			// 建立后再核对墙钟，避免把已经到期的日程继续挂到一个未来计时器上。
			if !next.After(s.clock.Now()) {
				timer.Stop()
				continue
			}
		}
		// 有界暂存只用于合并规范事件并施加背压。后台积压未交付时不抢发定时，
		// 避免把同一积压中的恢复事件留在旧定时动作后面，也不无限缓存来源事件。
		var incoming <-chan model.Event
		if len(pending) < eventBufferLimit {
			incoming = input
		}
		select {
		case <-ctx.Done():
		case <-stopped:
			if timer != nil {
				timer.Stop()
			}
			return nil
		case request := <-s.commands:
			s.applyCommand(request)
		case value, ok := <-incoming:
			if !ok {
				input = nil
			} else {
				s.observeResume(value)
				pending = append(pending, value)
			}
		case <-due:
		case out <- event:
			if index >= 0 {
				s.mu.Lock()
				state := &s.states[index]
				state.consumed = dateOf(state.next.In(state.location))
				state.rebase(s.clock.Now())
				s.mu.Unlock()
			} else {
				pending[0] = model.Event{}
				pending = pending[1:]
			}
		}
		if timer != nil {
			timer.Stop()
		}
	}
}

func (s *Scheduler) nextEvent(now time.Time) (model.Event, int, time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	index := -1
	var next time.Time
	for i := range s.states {
		at := s.states[i].next
		if at.IsZero() {
			continue
		}
		if next.IsZero() || at.Before(next) || (at.Equal(next) && s.states[i].schedule.ID < s.states[index].schedule.ID) {
			index, next = i, at
		}
	}
	if next.IsZero() || next.After(now) {
		return model.Event{}, -1, next
	}
	state := &s.states[index]
	day := next.In(state.location).Format("2006-01-02")
	digest := sha256.Sum256([]byte(state.schedule.ID + "\x00" + day))
	return model.Event{
		ID: "timer-" + hex.EncodeToString(digest[:]), Kind: model.EventTimer,
		Source: "scheduler", Name: state.schedule.ID, ObservedAt: next,
		Attributes: map[string]string{
			"scheduled_at": next.UTC().Format(time.RFC3339Nano),
			"timezone":     state.schedule.Timezone, "missed_policy": "skip",
		},
	}, index, next
}

func (s *Scheduler) applyCommand(request command) {
	if err := request.ctx.Err(); err != nil {
		request.reply <- err
		return
	}
	s.mu.Lock()
	var err error
	if request.resume {
		s.rebaseAll()
	} else {
		found := false
		for i := range s.states {
			state := &s.states[i]
			if state.schedule.ID != request.id {
				continue
			}
			found = true
			if state.schedule.Enabled != request.enabled {
				state.schedule.Enabled = request.enabled
				state.rebase(s.clock.Now())
			}
			break
		}
		if !found {
			err = model.NewError(model.CodeNotFound, "定时日程不存在", nil)
		}
	}
	s.mu.Unlock()
	request.reply <- err
}

// observeResume的有界抑制仅保护调度器自身的恢复副作用；原事件仍交给引擎，
// 引擎继续负责自己的去重、USB状态和动作裁决。旧恢复通知不能再次跳过新日程。
func (s *Scheduler) observeResume(event model.Event) {
	if event.Kind != model.EventResume || event.Validate() != nil {
		return
	}
	key := event.Source + "\x00" + event.ID
	if s.resumeIDs[key] || (!s.resumeAt[event.Source].IsZero() && !event.ObservedAt.After(s.resumeAt[event.Source])) {
		return
	}
	s.resumeIDs[key] = true
	s.resumeOrder = append(s.resumeOrder, key)
	if len(s.resumeOrder) > resumeReplayLimit {
		delete(s.resumeIDs, s.resumeOrder[0])
		s.resumeOrder = s.resumeOrder[1:]
	}
	if _, exists := s.resumeAt[event.Source]; !exists {
		s.sourceOrder = append(s.sourceOrder, event.Source)
		if len(s.sourceOrder) > resumeReplayLimit {
			delete(s.resumeAt, s.sourceOrder[0])
			s.sourceOrder = s.sourceOrder[1:]
		}
	}
	s.resumeAt[event.Source] = event.ObservedAt
	s.mu.Lock()
	s.rebaseAll()
	s.mu.Unlock()
}

// 调用方持锁；恢复时刻取本地当前墙钟，不信任外部事件携带的过去/未来时间。
func (s *Scheduler) rebaseAll() {
	now := s.clock.Now()
	for i := range s.states {
		s.states[i].rebase(now)
	}
}

// Resume返回nil表示状态机已经跳过截至处理时刻的全部过期日程。
// 没有真实恢复来源时，本方法不会自行猜测休眠，装配方须如实限制相关能力。
func (s *Scheduler) Resume(ctx context.Context) error {
	return s.submit(command{ctx: ctx, resume: true, reply: make(chan error, 1)})
}

func (s *Scheduler) SetEnabled(ctx context.Context, id string, enabled bool) error {
	if !model.ValidID(id) {
		return model.NewError(model.CodeInvalidRequest, "日程ID无效", nil)
	}
	return s.submit(command{ctx: ctx, id: id, enabled: enabled, reply: make(chan error, 1)})
}

func (s *Scheduler) submit(request command) error {
	if err := request.ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	running := s.running
	s.mu.Unlock()
	if !running {
		return model.NewError(model.CodeBackendUnavailable, "调度器尚未运行或已经停止", nil)
	}
	select {
	case s.commands <- request:
	case <-request.ctx.Done():
		return request.ctx.Err()
	case <-s.done:
		return model.NewError(model.CodeBackendUnavailable, "调度器已经停止", nil)
	}
	select {
	case err := <-request.reply:
		return err
	case <-request.ctx.Done():
		return request.ctx.Err()
	case <-s.done:
		return model.NewError(model.CodeBackendUnavailable, "调度器已经停止", nil)
	}
}

func (s *Scheduler) Status() []ScheduleStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	statuses := make([]ScheduleStatus, len(s.states))
	for i, state := range s.states {
		statuses[i] = ScheduleStatus{Schedule: state.schedule, MissedPolicy: "skip"}
		if s.running && !state.next.IsZero() {
			next := state.next
			statuses[i].NextTrigger = &next
		}
	}
	return statuses
}

// 两个查询直接委托真实运行中的同一引擎，复用其锁和深副本语义。
// 不新建引擎、不复制业务状态，也不为查询产生动作或额外记录。
func (s *Scheduler) Preview(event model.Event) ([]automation.Decision, error) {
	return s.engine.Preview(event)
}

func (s *Scheduler) Records() []automation.Record { return s.engine.Records() }

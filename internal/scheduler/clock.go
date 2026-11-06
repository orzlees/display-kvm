package scheduler

import "time"

// Clock使墙钟读取和到期通知可以分别控制；恢复只能由真实事件或Resume显式通知，
// 不能通过某个任意迟到阈值猜测休眠。注入实现须支持并发Now与定时器操作。
type Clock interface {
	Now() time.Time
	NewTimer(time.Duration) Timer
}

type Timer interface {
	C() <-chan time.Time
	Stop()
}

type Options struct {
	Clock Clock
}

type localClock struct{}
type localTimer struct{ timer *time.Timer }

func (localClock) Now() time.Time                 { return time.Now() }
func (localClock) NewTimer(d time.Duration) Timer { return localTimer{time.NewTimer(d)} }
func (t localTimer) C() <-chan time.Time          { return t.timer.C }
func (t localTimer) Stop()                        { t.timer.Stop() }

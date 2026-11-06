package linux

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/orzlee/displayKvm/internal/model"
)

type readyMode uint8

const (
	readyUdev readyMode = iota
	readyNotify
)

type subscription interface {
	Lines() <-chan string
	Ready() <-chan error
	Done() <-chan struct{}
	Err() error
	Close() error
}

type commandRunner interface {
	run(context.Context, string, ...string) ([]byte, error)
	start(context.Context, string, []string, readyMode) (subscription, error)
}

type execRunner struct{}

func commandError(ctx context.Context, cause error, diagnostic string) error {
	if ctx.Err() != nil {
		return model.NewError(model.CodeCanceled, "Linux事件命令已取消或超时", errors.Join(ctx.Err(), cause))
	}
	if cause == nil {
		return nil
	}
	code := model.CodeOf(cause)
	if code != model.CodeInternal {
		return model.NewError(code, "Linux事件命令失败", cause)
	}
	text := strings.ToLower(diagnostic)
	switch {
	case errors.Is(cause, os.ErrPermission), strings.Contains(text, "accessdenied"), strings.Contains(text, "permission denied"), strings.Contains(text, "access denied"), strings.Contains(text, "operation not permitted"):
		code = model.CodePermissionDenied
	case errors.Is(cause, os.ErrNotExist), strings.Contains(text, "serviceunknown"), strings.Contains(text, "namehasnoowner"), strings.Contains(text, "no such file"), strings.Contains(text, "not provided by any"):
		code = model.CodeBackendUnavailable
	case strings.Contains(text, "nosuchsession"), strings.Contains(text, "no session"):
		code = model.CodeNotFound
	default:
		code = model.CodeTransport
	}
	// 诊断保留在底层原因中，不能将退出码或权限拒绝转换成空事件流成功。
	return model.NewError(code, "Linux事件命令失败: "+strings.TrimSpace(diagnostic), cause)
}

func commandEnv(socket string) []string {
	env := make([]string, 0, len(os.Environ())+3)
	for _, item := range os.Environ() {
		key, _, _ := strings.Cut(item, "=")
		switch key {
		case "NOTIFY_SOCKET", "LC_ALL", "SYSTEMD_COLORS", "SYSTEMD_PAGER":
			continue
		}
		env = append(env, item)
	}
	env = append(env, "LC_ALL=C", "SYSTEMD_COLORS=0", "SYSTEMD_PAGER=cat")
	if socket != "" {
		env = append(env, "NOTIFY_SOCKET="+socket)
	}
	return env
}

func validCommandPath(path string) error {
	if !filepath.IsAbs(path) || strings.IndexByte(path, 0) >= 0 {
		return model.NewError(model.CodeInvalidConfig, "事件工具必须配置用户自备的绝对路径", nil)
	}
	return nil
}

type boundedOutput struct {
	mu       sync.Mutex
	buf      bytes.Buffer
	limit    int
	overflow error
	cancel   context.CancelFunc
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.buf.Len()+len(p) > b.limit {
		b.overflow = model.NewError(model.CodeMessageTooLarge, "事件命令输出超过容量限制", nil)
		if b.cancel != nil {
			b.cancel()
		}
		return 0, b.overflow
	}
	return b.buf.Write(p)
}
func (b *boundedOutput) result() ([]byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return bytes.Clone(b.buf.Bytes()), b.overflow
}

func (execRunner) run(ctx context.Context, path string, args ...string) ([]byte, error) {
	if err := validCommandPath(path); err != nil {
		return nil, err
	}
	childCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(childCtx, path, args...)
	cmd.Env, cmd.WaitDelay = commandEnv(""), 250*time.Millisecond
	out := &boundedOutput{limit: 1 << 20, cancel: cancel}
	diagnostic := &boundedOutput{limit: 64 << 10, cancel: cancel}
	cmd.Stdout, cmd.Stderr = out, diagnostic
	err := cmd.Run()
	data, over := out.result()
	diag, diagOver := diagnostic.result()
	if over != nil || diagOver != nil {
		return nil, commandError(ctx, errors.Join(over, diagOver, err), string(diag))
	}
	if err != nil {
		return nil, commandError(ctx, err, string(diag))
	}
	return data, nil
}

type execSubscription struct {
	lines      chan string
	ready      chan error
	done       chan struct{}
	cancel     context.CancelFunc
	once       sync.Once
	mu         sync.Mutex
	err        error
	cleanupErr error
}

func (p *execSubscription) Lines() <-chan string  { return p.lines }
func (p *execSubscription) Ready() <-chan error   { return p.ready }
func (p *execSubscription) Done() <-chan struct{} { return p.done }
func (p *execSubscription) Err() error            { p.mu.Lock(); defer p.mu.Unlock(); return p.err }
func (p *execSubscription) Close() error {
	p.cancel()
	<-p.done
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cleanupErr
}
func (p *execSubscription) signalReady(err error) { p.once.Do(func() { p.ready <- err }) }

type lineOutput struct {
	p       *execSubscription
	mode    readyMode
	pending []byte
	err     error
}

func (w *lineOutput) Write(data []byte) (int, error) {
	w.pending = append(w.pending, data...)
	for {
		i := bytes.IndexByte(w.pending, '\n')
		if i < 0 {
			break
		}
		if i > 64<<10 {
			return w.fail()
		}
		line := string(bytes.TrimSuffix(w.pending[:i], []byte{'\r'}))
		w.pending = w.pending[i+1:]
		if w.mode == readyUdev {
			if udevReadyLine(line) {
				w.p.signalReady(nil)
				continue
			}
			if line == "monitor will print the received events for:" {
				continue
			}
		}
		select {
		case w.p.lines <- line:
		default:
			return w.fail()
		}
	}
	if len(w.pending) > 64<<10 {
		return w.fail()
	}
	return len(data), nil
}
func (w *lineOutput) fail() (int, error) {
	w.err = model.NewError(model.CodeMessageTooLarge, "事件行或待处理通知超过有界容量", nil)
	w.p.cancel()
	return 0, w.err
}

func (execRunner) start(ctx context.Context, path string, args []string, mode readyMode) (subscription, error) {
	if err := validCommandPath(path); err != nil {
		return nil, err
	}
	childCtx, cancel := context.WithCancel(ctx)
	p := &execSubscription{lines: make(chan string, 256), ready: make(chan error, 1), done: make(chan struct{}), cancel: cancel}
	var listener *notifyListener
	var socket string
	if mode == readyNotify {
		var err error
		listener, err = newNotifyListener()
		if err != nil {
			cancel()
			return nil, err
		}
		socket = listener.path
	}
	cmd := exec.CommandContext(childCtx, path, args...)
	cmd.Env, cmd.WaitDelay = commandEnv(socket), 250*time.Millisecond
	stdout := &lineOutput{p: p, mode: mode}
	stderr := &boundedOutput{limit: 64 << 10, cancel: cancel}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := cmd.Start(); err != nil {
		cancel()
		if listener != nil {
			err = errors.Join(err, listener.close())
		}
		return nil, commandError(ctx, err, "启动订阅失败")
	}
	notifyDone := make(chan struct{})
	if listener != nil {
		go func() { defer close(notifyDone); p.signalReady(listener.wait(childCtx, cmd.Process.Pid)) }()
	} else {
		close(notifyDone)
	}
	go func() {
		// WaitDelay限制子进程退出后仍被后代持有的输出管道。返回前还需停止通知读取，
		// 否则重连可能遗留旧socket的READY，错误地激活下一轮快照。
		err := cmd.Wait()
		cancel()
		var cleanupErr error
		if listener != nil {
			cleanupErr = listener.close()
		}
		<-notifyDone
		diagnostic, over := stderr.result()
		err = commandError(ctx, errors.Join(err, over, stdout.err, cleanupErr), string(diagnostic))
		if err == nil {
			err = model.NewError(model.CodeTransport, "真实事件订阅意外退出", nil)
		}
		p.mu.Lock()
		p.err = err
		p.cleanupErr = cleanupErr
		p.mu.Unlock()
		p.signalReady(err)
		close(p.lines)
		close(p.done)
	}()
	return p, nil
}

func udevReadyLine(line string) bool {
	// systemd v257/v258的udevadm-monitor.c在setup_monitor成功后才输出此行，
	// 前面的通用标题早于监听注册，不能用于握手；服务基础设施还需独立核查。
	return line == "UDEV - the event which udev sends out after rule processing"
}

func validNotification(body []byte, sender, child int) bool {
	if sender <= 0 || sender != child {
		return false
	}
	for _, line := range strings.Split(string(body), "\n") {
		if line == "READY=1" {
			return true
		}
	}
	return false
}

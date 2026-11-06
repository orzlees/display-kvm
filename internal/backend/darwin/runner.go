package darwin

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/orzlee/displayKvm/internal/model"
)

// MaxCommandOutputBytes复用共享消息大小预算，stdout和stderr合计最多1MiB。
// 外部工具输出不可无界存入内存；超限后终止本次进程，不能截断后假装解析成功。
const MaxCommandOutputBytes = model.MaxMessageBytes

type CommandOutput struct {
	Stdout string
	Stderr string
}

// CommandRunner拥有本次命令及其输出资源。Run必须响应ctx，并在进程退出、
// Wait及输出处理均完成后返回；禁止取消时把仍执行的写命令留在后台。
// Run报错不证明DDC未发送；注入实现同样不能自动重试或替换输入协议。
type CommandRunner interface {
	Run(ctx context.Context, executable string, args ...string) (CommandOutput, error)
}

// ExecRunner不调用shell，也不解析PATH，只执行用户明确提供的绝对路径。
type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, executable string, args ...string) (CommandOutput, error) {
	if err := ctx.Err(); err != nil {
		return CommandOutput{}, err
	}
	if !filepath.IsAbs(executable) || strings.IndexByte(executable, 0) >= 0 {
		return CommandOutput{}, model.NewError(model.CodeBackendUnavailable, "m1ddc执行器要求显式可执行文件绝对路径", nil)
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	capture := &commandCapture{cancel: cancel}
	cmd := exec.CommandContext(runCtx, executable, args...)
	cmd.Stdout = captureWriter{capture: capture}
	cmd.Stderr = captureWriter{capture: capture, stderr: true}
	// m1ddc自身不创建子进程。若用户路径上的异常程序把输出管道传给后代，
	// WaitDelay限制退出或取消后的管道清理等待，关闭管道并等待复制协程结束。
	// 这里不启动后台Wait协程；Run完成才释放调用方对本次操作生命周期的控制。
	cmd.WaitDelay = 250 * time.Millisecond
	err := cmd.Run()
	output, exceeded := capture.result()
	if ctx.Err() != nil {
		return output, errors.Join(ctx.Err(), err)
	}
	if exceeded {
		return output, model.NewError(model.CodeMessageTooLarge, "m1ddc命令输出超过合计1MiB上限，已终止并回收进程", err)
	}
	return output, err
}

type commandCapture struct {
	mu       sync.Mutex
	stdout   bytes.Buffer
	stderr   bytes.Buffer
	exceeded bool
	cancel   context.CancelFunc
}

type captureWriter struct {
	capture *commandCapture
	stderr  bool
}

func (w captureWriter) Write(data []byte) (int, error) {
	c := w.capture
	c.mu.Lock()
	defer c.mu.Unlock()
	remaining := MaxCommandOutputBytes - c.stdout.Len() - c.stderr.Len()
	kept := len(data)
	if kept > remaining {
		kept = remaining
	}
	if w.stderr {
		c.stderr.Write(data[:kept])
	} else {
		c.stdout.Write(data[:kept])
	}
	if kept < len(data) && !c.exceeded {
		c.exceeded = true
		c.cancel()
	}
	// stdout/stderr的复制协程并行写入，共用锁及总预算。超量部分不保留，
	// 返回已消费长度让复制循环继续排空，终止和Wait统一由Run负责闭合资源。
	return len(data), nil
}

func (c *commandCapture) result() (CommandOutput, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return CommandOutput{Stdout: c.stdout.String(), Stderr: c.stderr.String()}, c.exceeded
}

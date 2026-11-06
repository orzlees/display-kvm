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

// MaxCommandOutputBytes沿用共享消息预算，stdout和stderr合计最多1MiB；
// 超限必须失败并终止命令，不能把截断的枚举或写入结果当成完整成功输出。
const MaxCommandOutputBytes = model.MaxMessageBytes

type CommandOutput struct {
	Stdout string
	Stderr string
}

// CommandRunner拥有本次进程与输出资源。Run必须响应同一个请求ctx，
// 并在进程退出、Wait及管道处理结束后返回，不得留下后台写命令或自动重试。
// 返回错误不证明DDC尚未发送；注入执行器也必须遵守该生命周期和提交边界。
type CommandRunner interface {
	Run(ctx context.Context, executable string, args ...string) (CommandOutput, error)
}

// ExecRunner仅执行显式绝对路径，不经shell，不从PATH寻找或安装ddcutil。
type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, executable string, args ...string) (CommandOutput, error) {
	if err := ctx.Err(); err != nil {
		return CommandOutput{}, err
	}
	if !filepath.IsAbs(executable) || strings.IndexByte(executable, 0) >= 0 {
		return CommandOutput{}, model.NewError(model.CodeBackendUnavailable, "ddcutil执行器要求显式可执行文件绝对路径", nil)
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	capture := &commandCapture{cancel: cancel}
	cmd := exec.CommandContext(runCtx, executable, args...)
	cmd.Stdout = captureWriter{capture: capture}
	cmd.Stderr = captureWriter{capture: capture, stderr: true}
	// 已核对的2.2.7诊断为英文。仅设置本次子进程的语言，不修改父进程或系统环境，
	// 避免宿主语言改变权限等诊断文本，导致错误分类被误当成未知传输错误。
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "LC_ALL=") && !strings.HasPrefix(entry, "LANG=") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env, "LC_ALL=C", "LANG=C")
	// ddcutil直接完成本次DDC操作。异常工具若把管道传给后代，WaitDelay限制
	// 直接进程退出或取消后的管道清理等待，关闭管道并等待输出复制协程结束。
	// 不启动后台Wait；Run返回前始终收回本次直接进程，不承诺管理任意后代程序。
	cmd.WaitDelay = 250 * time.Millisecond
	err := cmd.Run()
	output, exceeded := capture.result()
	if ctx.Err() != nil {
		return output, errors.Join(ctx.Err(), err)
	}
	if exceeded {
		return output, model.NewError(model.CodeMessageTooLarge, "ddcutil命令输出超过合计1MiB上限，已终止并回收进程", err)
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
	// stdout/stderr由不同协程复制，必须共享锁和总预算。超量部分不再保留，
	// 仍报告已消费长度，让复制循环排空并由Run统一终止、Wait和清理管道。
	return len(data), nil
}

func (c *commandCapture) result() (CommandOutput, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return CommandOutput{Stdout: c.stdout.String(), Stderr: c.stderr.String()}, c.exceeded
}

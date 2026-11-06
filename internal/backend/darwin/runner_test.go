package darwin

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/orzlee/displayKvm/internal/model"
)

// 测试只重启本测试二进制作为可控子进程，不寻找或执行m1ddc，也不接触显示器。
func TestCommandFixture(t *testing.T) {
	marker := -1
	for i, arg := range os.Args {
		if arg == "displaykvm-command-fixture" {
			marker = i
			break
		}
	}
	if marker < 0 {
		return
	}
	args := os.Args[marker+1:]
	if len(args) == 0 {
		os.Exit(9)
	}
	switch args[0] {
	case "output":
		fmt.Fprint(os.Stdout, "17\n")
		fmt.Fprint(os.Stderr, "fixture diagnostic\n")
		os.Exit(0)
	case "failure":
		fmt.Fprint(os.Stderr, "fixture exit failure\n")
		os.Exit(7)
	case "wait":
		if len(args) != 2 || os.WriteFile(args[1]+".pending", []byte(strconv.Itoa(os.Getpid())), 0600) != nil {
			os.Exit(9)
		}
		// 原子发布就绪标记，避免父测试碰巧读到已创建但尚未写完的PID文件。
		if os.Rename(args[1]+".pending", args[1]) != nil {
			os.Exit(9)
		}
		for {
			time.Sleep(time.Hour)
		}
	case "flood":
		chunk := strings.Repeat("x", 8192)
		for {
			fmt.Fprint(os.Stdout, chunk)
			fmt.Fprint(os.Stderr, chunk)
		}
	default:
		os.Exit(9)
	}
}

func fixtureCommand(t *testing.T, mode string, extra ...string) (string, []string) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	args := []string{"-test.run=^TestCommandFixture$", "--", "displaykvm-command-fixture", mode}
	return executable, append(args, extra...)
}

func TestExecRunnerCapturesOutputAndExitCause(t *testing.T) {
	path, args := fixtureCommand(t, "output")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	output, err := (ExecRunner{}).Run(ctx, path, args...)
	if err != nil || output.Stdout != "17\n" || output.Stderr != "fixture diagnostic\n" {
		t.Fatalf("需要区分stdout与stderr：%+v，%v", output, err)
	}
	path, args = fixtureCommand(t, "failure")
	output, err = (ExecRunner{}).Run(ctx, path, args...)
	if err == nil || !strings.Contains(output.Stderr, "fixture exit failure") {
		t.Fatalf("非零退出不能吞掉原因：%+v，%v", output, err)
	}
}

func TestExecRunnerNeverSearchesPATH(t *testing.T) {
	for _, path := range []string{"", "m1ddc", "./m1ddc"} {
		if _, err := (ExecRunner{}).Run(context.Background(), path); model.CodeOf(err) != model.CodeBackendUnavailable {
			t.Fatalf("相对名称不得交给PATH解析：path=%q err=%v", path, err)
		}
	}
}

func TestExecRunnerCancellationAndDeadlineReapProcess(t *testing.T) {
	for _, useDeadline := range []bool{false, true} {
		t.Run(fmt.Sprintf("deadline=%t", useDeadline), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if useDeadline {
				cancel()
				ctx, cancel = context.WithTimeout(context.Background(), 2*time.Second)
			}
			defer cancel()
			readyPath := filepath.Join(t.TempDir(), "ready")
			path, args := fixtureCommand(t, "wait", readyPath)
			done := make(chan error, 1)
			go func() {
				_, err := (ExecRunner{}).Run(ctx, path, args...)
				done <- err
			}()
			var pid int
			readyDeadline := time.Now().Add(time.Second)
			for pid == 0 {
				data, err := os.ReadFile(readyPath)
				if err == nil {
					pid, err = strconv.Atoi(string(data))
					if err != nil {
						t.Fatal(err)
					}
				} else if !errors.Is(err, os.ErrNotExist) {
					t.Fatal(err)
				}
				if time.Now().After(readyDeadline) {
					t.Fatal("测试子进程未报告启动")
				}
				if pid == 0 {
					time.Sleep(5 * time.Millisecond)
				}
			}
			wantErr := context.DeadlineExceeded
			if !useDeadline {
				wantErr = context.Canceled
				cancel()
			}
			select {
			case err := <-done:
				if !errors.Is(err, wantErr) {
					t.Fatalf("取消/超时须保留context根因：%v", err)
				}
			case <-time.After(4 * time.Second):
				t.Fatal("取消/截止时间后Runner仍未清理返回")
			}
			// Unix的信号0只探测本测试子进程是否存在；不向显示器或其他进程发控制操作。
			if runtime.GOOS != "windows" {
				process, err := os.FindProcess(pid)
				if err == nil {
					defer process.Release()
					err = process.Signal(syscall.Signal(0))
				}
				if !errors.Is(err, os.ErrProcessDone) && !errors.Is(err, syscall.ESRCH) {
					t.Fatalf("Runner返回前必须已Wait回收子进程，pid=%d err=%v", pid, err)
				}
			}
		})
	}
}

func TestExecRunnerBoundsCombinedOutputAndStopsFlood(t *testing.T) {
	path, args := fixtureCommand(t, "flood")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	output, err := (ExecRunner{}).Run(ctx, path, args...)
	if model.CodeOf(err) != model.CodeMessageTooLarge || len(output.Stdout)+len(output.Stderr) > MaxCommandOutputBytes {
		t.Fatalf("stdout和stderr须共享上限并终止过量进程：bytes=%d err=%v", len(output.Stdout)+len(output.Stderr), err)
	}
	if ctx.Err() != nil {
		t.Fatal("必须因输出超限终止，不能一直等到外层超时")
	}
}

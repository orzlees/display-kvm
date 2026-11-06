package linux

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/orzlee/displayKvm/internal/model"
)

// 仅把本测试二进制作为外部工具替身，沿用05的可控子进程模式；
// 不寻找ddcutil、不接触I2C，也不依赖Linux宿主或显示器。
func TestCommandFixture(t *testing.T) {
	marker := -1
	for i, arg := range os.Args {
		if arg == "displaykvm-linux-command-fixture" {
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
		fmt.Fprint(os.Stdout, "VCP 60 SNC x11\n")
		fmt.Fprint(os.Stderr, "fixture diagnostic\n")
		os.Exit(0)
	case "failure":
		fmt.Fprint(os.Stderr, "fixture exit failure\n")
		os.Exit(7)
	case "locale":
		fmt.Fprintf(os.Stdout, "%s:%s", os.Getenv("LC_ALL"), os.Getenv("LANG"))
		os.Exit(0)
	case "wait":
		if len(args) != 2 || os.WriteFile(args[1]+".pending", []byte(strconv.Itoa(os.Getpid())), 0600) != nil {
			os.Exit(9)
		}
		// 原子发布PID，避免父测试把尚未写完的文件误判为进程未启动。
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
	args := []string{"-test.run=^TestCommandFixture$", "--", "displaykvm-linux-command-fixture", mode}
	return executable, append(args, extra...)
}

func TestExecRunnerCapturesOutputExitCauseAndCommandLocale(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	path, args := fixtureCommand(t, "output")
	output, err := (ExecRunner{}).Run(ctx, path, args...)
	if err != nil || output.Stdout != "VCP 60 SNC x11\n" || output.Stderr != "fixture diagnostic\n" {
		t.Fatalf("必须分别保留stdout和stderr：%+v %v", output, err)
	}
	path, args = fixtureCommand(t, "failure")
	output, err = (ExecRunner{}).Run(ctx, path, args...)
	if err == nil || !strings.Contains(output.Stderr, "fixture exit failure") {
		t.Fatalf("必须保留非零退出：%+v %v", output, err)
	}
	path, args = fixtureCommand(t, "locale")
	output, err = (ExecRunner{}).Run(ctx, path, args...)
	if err != nil || output.Stdout != "C:C" {
		t.Fatalf("仅为本次子进程固定诊断语言：%+v %v", output, err)
	}
}

func TestExecRunnerRejectsRelativePathAndPreCanceledContext(t *testing.T) {
	for _, path := range []string{"", "ddcutil", "./ddcutil", "/usr/bin/ddcutil\x00"} {
		if _, err := (ExecRunner{}).Run(context.Background(), path); model.CodeOf(err) != model.CodeBackendUnavailable {
			t.Fatalf("不得经PATH查找工具：%q %v", path, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (ExecRunner{}).Run(ctx, "/never-executed"); !errors.Is(err, context.Canceled) {
		t.Fatalf("启动前取消必须原样保留：%v", err)
	}
}

func TestExecRunnerCancellationAndDeadlineReapProcess(t *testing.T) {
	for _, useDeadline := range []bool{false, true} {
		t.Run(fmt.Sprintf("deadline=%t", useDeadline), func(t *testing.T) {
			limit := 5 * time.Second
			if useDeadline {
				limit = 2 * time.Second
			}
			ctx, cancel := context.WithTimeout(context.Background(), limit)
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
			want := context.DeadlineExceeded
			if !useDeadline {
				want = context.Canceled
				cancel()
			}
			select {
			case err := <-done:
				if !errors.Is(err, want) {
					t.Fatalf("必须保留取消/超时根因：%v", err)
				}
			case <-time.After(4 * time.Second):
				t.Fatal("执行器未在取消后完成资源回收")
			}
			// 信号0仅探测本测试替身是否已被Wait回收，不控制其他程序或硬件。
			if runtime.GOOS != "windows" {
				process, err := os.FindProcess(pid)
				if err == nil {
					defer process.Release()
					err = process.Signal(syscall.Signal(0))
				}
				if !errors.Is(err, os.ErrProcessDone) && !errors.Is(err, syscall.ESRCH) {
					t.Fatalf("返回前必须Wait回收替身，pid=%d err=%v", pid, err)
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
	if model.CodeOf(err) != model.CodeMessageTooLarge || len(output.Stdout)+len(output.Stderr) > MaxCommandOutputBytes || ctx.Err() != nil {
		t.Fatalf("须在外层超时前按共享预算终止并回收进程：bytes=%d err=%v ctx=%v", len(output.Stdout)+len(output.Stderr), err, ctx.Err())
	}
}

func TestCommandCaptureSharesExactBudgetAcrossConcurrentStreams(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	capture := &commandCapture{cancel: cancel}
	writers := []captureWriter{{capture: capture}, {capture: capture, stderr: true}}
	var wg sync.WaitGroup
	for _, writer := range writers {
		wg.Add(1)
		go func(w captureWriter) {
			defer wg.Done()
			_, _ = w.Write([]byte(strings.Repeat("x", MaxCommandOutputBytes/2)))
		}(writer)
	}
	wg.Wait()
	output, exceeded := capture.result()
	if exceeded || ctx.Err() != nil || len(output.Stdout)+len(output.Stderr) != MaxCommandOutputBytes {
		t.Fatal("恰好达到合计预算时不应截断或取消")
	}
	_, _ = writers[1].Write([]byte("y"))
	output, exceeded = capture.result()
	if !exceeded || ctx.Err() == nil || len(output.Stdout)+len(output.Stderr) != MaxCommandOutputBytes {
		t.Fatal("多一字节即须取消，且不得继续增长内存")
	}
}

// Package cli提供不依赖图形界面或硬件后端的命令行入口。所有设备操作均交由
// 共用client执行，CLI不创建队列、不自动启动后台，也不重发有副作用的请求。
package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/orzlee/displayKvm/internal/client"
	"github.com/orzlee/displayKvm/internal/config"
	"github.com/orzlee/displayKvm/internal/loginstart"
	"github.com/orzlee/displayKvm/internal/model"
	"github.com/orzlee/displayKvm/internal/settings"
)

const (
	ExitOK          = 0
	ExitFailure     = 1
	ExitUsage       = 2
	ExitUnavailable = 3
	ExitTimeout     = 124
	ExitCanceled    = 130
)

// Client是现有共享客户端的业务边界；替身测试可在没有后台或硬件时检查实际调用。
type Client interface {
	Configuration() (config.Config, error)
	Enumerate(context.Context) ([]model.Device, error)
	EventCapabilities(context.Context) ([]model.EventCapability, error)
	Switch(context.Context, model.SwitchRequest) (model.SwitchResult, error)
	EmitEvent(context.Context, model.Event) error
	Management(context.Context, model.ManagementRequest) (model.ManagementResponse, error)
}

type Options struct {
	Stdout        io.Writer
	Stderr        io.Writer
	Connect       func(string) (Client, error)
	NewID         func() (string, error)
	NewCommitID   func(model.RuntimeGeneration) (model.RequestID, error)
	Now           func() time.Time
	NewLoginStart func() (*loginstart.Manager, error)
	NewSettings   func() (*settings.Store, error)
}

// Envelope版本只描述CLI输出格式，不与后台协议或配置版本混用。五个字段始终输出，
// 空数据和空错误使用null；逐屏数据直接使用共享模型，不能将数值或状态改成展示字符串。
type Envelope struct {
	Version int          `json:"version"`
	Command string       `json:"command"`
	OK      bool         `json:"ok"`
	Data    any          `json:"data"`
	Error   *model.Error `json:"error"`
}

type runner struct {
	opts      Options
	json      bool
	directory string
}

type outcome struct {
	data any
	err  error
	code int
}

const usage = `DisplayKVM 命令行
用法: displaykvm [--json] [--config-dir DIR] COMMAND [参数]
  list                         枚举后台发现的设备
  status                       查看后台、配置及事件能力；缺少观测时明确未知
  switch --target ID [--display ID ... | --group ID] [--timeout-ms N] [--dry-run]
  config check --file PATH      只读检查完整配置文档，不写文件
  config new --file PATH [--tool PATH] [--overwrite]
  config set-tool --file PATH --tool PATH  只修改高级工具覆盖；留空使用包内工具，不启停后台
  startup status               查看独立服务和桌面登录启动状态，无需后台在线
  startup configure --config COMPLETE_FILE
                               只保存下一次登录所用参数，不修改业务文件
  startup enable service|desktop --app /Applications/DisplayKvm.app
  startup disable service|desktop  只改变以后登录安排，不启停当前进程
  event emit --name NAME [--attribute KEY=VALUE ...]
  diagnose [--display ID [--scheme ID]]  通过后台唯一队列诊断
  calibrate begin --display ID --target ID --port-name NAME --scheme ID
                  --protocol vcp|lg_input_alt --value N [--vcp-code N]
                               只生成预览，不保存、不切换；参数须由人工确认
  calibrate prepare --generation ID --session ID --confirm
  calibrate apply --generation ID --commit ID --confirm
  calibrate query --commit ID    查询同一提交的保存与生效证据
  calibrate trial begin --target ID ... [--display ID ... | --group ID]
                        [--timeout-ms N]  仅规划显式目标，不发送切换
  calibrate trial step --generation ID --session ID --confirm
  calibrate trial stop --generation ID --session ID  停止后续步骤，不自动回切
  help                         显示帮助
通用参数也可放在具体命令后。切换发送或读回一致均不证明主机画面已出现。
startup使用当前用户固定设置目录，不接受作为IPC目录参数的--config-dir。
取消不会撤回后台已经发送的动作；失败后不自动重发。
`

// Run只返回退出码，由最外层main决定退出进程；这样测试不会启动后台或终止测试进程。
func Run(ctx context.Context, args []string, opts Options) int {
	if opts.Stdout == nil {
		opts.Stdout = os.Stdout
	}
	if opts.Stderr == nil {
		opts.Stderr = os.Stderr
	}
	if opts.Connect == nil {
		opts.Connect = func(directory string) (Client, error) { return client.Connect(directory) }
	}
	if opts.NewID == nil {
		opts.NewID = func() (string, error) {
			var bytes [16]byte
			if _, err := rand.Read(bytes[:]); err != nil {
				return "", model.NewError(model.CodeInternal, "无法生成请求关联ID", err)
			}
			return "cli-" + hex.EncodeToString(bytes[:]), nil
		}
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.NewCommitID == nil {
		opts.NewCommitID = client.NewCommitID
	}
	if opts.NewLoginStart == nil {
		opts.NewLoginStart = func() (*loginstart.Manager, error) { return loginstart.New(loginstart.Options{}) }
	}
	if opts.NewSettings == nil {
		opts.NewSettings = func() (*settings.Store, error) { return settings.NewStore(settings.Options{}) }
	}
	r := &runner{opts: opts}
	// 即使其他参数解析失败，显式--json仍应得到相同的结构化错误外壳。
	for _, arg := range args {
		if arg == "--json" || arg == "--json=true" || arg == "-json" || arg == "-json=true" {
			r.json = true
		}
	}
	root := r.flags("displaykvm")
	if err := root.Parse(args); err != nil {
		return r.finish("help", r.parseFailure(err))
	}
	remaining := root.Args()
	if len(remaining) == 0 {
		return r.finish("help", failure(usageError("command", "必须指定命令；使用help查看用法")))
	}
	command := remaining[0]
	var result outcome
	switch command {
	case "help":
		fs := r.flags(command)
		if err := parse(fs, remaining[1:]); err != nil {
			result = r.parseFailure(err)
		} else {
			result.data = usage
		}
	case "list":
		result = r.list(ctx, remaining[1:])
	case "status":
		result = r.status(ctx, remaining[1:])
	case "switch":
		result = r.switchDisplays(ctx, remaining[1:])
	case "config":
		command, result = r.configuration(ctx, remaining[1:])
	case "startup":
		command, result = r.startup(ctx, remaining[1:])
	case "event":
		command = "event emit"
		result = r.emit(ctx, remaining[1:])
	case "calibrate":
		command, result = r.calibrate(ctx, remaining[1:])
	case "diagnose":
		result = r.diagnose(ctx, remaining[1:])
	default:
		result = failure(usageError("command", "未知命令；使用help查看用法"))
	}
	return r.finish(command, result)
}

func (r *runner) flags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.BoolVar(&r.json, "json", r.json, "输出JSON")
	fs.StringVar(&r.directory, "config-dir", r.directory, "用户配置目录")
	return fs
}

func parse(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		return err
	}
	if len(fs.Args()) != 0 {
		return usageError("arguments", "命令不接受额外的位置参数")
	}
	return nil
}

func (r *runner) parseFailure(err error) outcome {
	if errors.Is(err, flag.ErrHelp) {
		return outcome{data: usage}
	}
	return failure(usageError("arguments", "命令参数无效；使用help查看用法"))
}

func (r *runner) connect(ctx context.Context) (Client, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	connection, err := r.opts.Connect(r.directory)
	if err != nil {
		return nil, err
	}
	if connection == nil {
		return nil, model.NewError(model.CodeInternal, "客户端未返回有效连接", nil)
	}
	return connection, nil
}

func failure(err error) outcome { return outcome{err: err, code: exitCode(err)} }

func usageError(field, message string) *model.Error {
	err := model.NewError(model.CodeInvalidRequest, message, nil)
	err.Field = field
	return err
}

func exitCode(err error) int {
	switch model.CodeOf(err) {
	case "":
		return ExitOK
	case model.CodeInvalidRequest, model.CodeInvalidConfig:
		return ExitUsage
	case model.CodeDaemonUnavailable:
		return ExitUnavailable
	case model.CodeTimeout:
		return ExitTimeout
	case model.CodeCanceled:
		return ExitCanceled
	default:
		return ExitFailure
	}
}

func publicError(err error) *model.Error {
	if err == nil {
		return nil
	}
	var classified *model.Error
	if errors.As(err, &classified) && classified != nil {
		return &model.Error{Code: model.CodeOf(err), Message: classified.Message, Field: classified.Field, Retryable: classified.Retryable}
	}
	// 未分类底层异常可能携带本地路径或认证材料，输出仅保留公共类别和固定说明。
	message := "操作失败，未取得可确认的结果"
	if model.CodeOf(err) == model.CodeCanceled {
		message = "操作已取消；已经发送的动作不会因此撤回"
	} else if model.CodeOf(err) == model.CodeTimeout {
		message = "等待操作结果超时；不要自动重发已经发送的动作"
	}
	return model.NewError(model.CodeOf(err), message, nil)
}

func (r *runner) finish(command string, result outcome) int {
	if r.json {
		encoded, err := json.Marshal(Envelope{Version: 1, Command: command, OK: result.code == ExitOK, Data: result.data, Error: publicError(result.err)})
		if err == nil {
			encoded = append(encoded, '\n')
			var written int
			written, err = r.opts.Stdout.Write(encoded)
			if err == nil && written != len(encoded) {
				err = io.ErrShortWrite
			}
		}
		if err != nil {
			fmt.Fprintln(r.opts.Stderr, "输出JSON失败；命令可能已经执行，不能据此重发")
			return ExitFailure
		}
		return result.code
	}
	if result.data != nil {
		var err error
		if text, ok := result.data.(string); ok {
			_, err = fmt.Fprintln(r.opts.Stdout, strings.TrimSuffix(text, "\n"))
		} else {
			var encoded []byte
			encoded, err = json.MarshalIndent(result.data, "", "  ")
			if err == nil {
				_, err = fmt.Fprintln(r.opts.Stdout, string(encoded))
			}
		}
		if err != nil {
			fmt.Fprintln(r.opts.Stderr, "输出结果失败；命令可能已经执行，不能据此重发")
			return ExitFailure
		}
	}
	if result.err != nil {
		fmt.Fprintln(r.opts.Stderr, publicError(result.err))
	} else if result.code != ExitOK {
		fmt.Fprintln(r.opts.Stderr, "操作未全部完成，请查看逐屏证据；不会自动重发")
	}
	return result.code
}

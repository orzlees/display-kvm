package cli

import (
	"context"
	"errors"
	"flag"
	"os"

	"github.com/orzlee/displayKvm/internal/model"
	"github.com/orzlee/displayKvm/internal/settings"
)

func (r *runner) settingsStore() (*settings.Store, error) {
	store, err := r.opts.NewSettings()
	if err == nil && store == nil {
		err = model.NewError(model.CodeInternal, "配置存储未返回有效实例", nil)
	}
	return store, err
}

func (r *runner) configuration(ctx context.Context, args []string) (string, outcome) {
	if len(args) == 0 {
		return "config check", failure(usageError("command", "必须指定config check、new或set-tool"))
	}
	command := "config " + args[0]
	switch args[0] {
	case "check":
		return command, r.checkConfig(args)
	case "new", "set-tool":
	default:
		return command, failure(usageError("command", "未知配置命令"))
	}
	fs := r.flags(command)
	var path, tool string
	var overwrite bool
	fs.StringVar(&path, "file", "", "完整配置文件")
	fs.StringVar(&tool, "tool", "", "高级工具绝对路径覆盖；留空使用包内工具，不搜索PATH")
	if args[0] != "set-tool" {
		fs.BoolVar(&overwrite, "overwrite", false, "明确覆盖已有有效完整文档，仍检查锁和版本")
	}
	if err := parse(fs, args[1:]); err != nil {
		return command, r.parseFailure(err)
	}
	if path == "" {
		return command, failure(usageError("file", "必须显式指定配置目标文件"))
	}
	if args[0] == "set-tool" {
		hasTool := false
		fs.Visit(func(entry *flag.Flag) { hasTool = hasTool || entry.Name == "tool" })
		if !hasTool {
			return command, failure(usageError("tool", "修改工具必须显式指定--tool"))
		}
		result, err := r.setSettingsTool(ctx, path, tool)
		return command, settingsOutcome(result, err)
	}
	document := settings.NewDocument()
	document.Runtime.ToolPath = tool
	result, err := r.saveSettingsDocument(ctx, path, document, overwrite)
	return command, settingsOutcome(result, err)
}

func settingsOutcome(result settings.SaveResult, err error) outcome {
	if err == nil && (result.Saved == nil || !*result.Saved) {
		err = model.NewError(model.CodeIO, "没有可确认的完整配置保存结果", nil)
	}
	response := failure(err)
	response.data = struct {
		Saved    *bool              `json:"saved"`
		Snapshot *settings.Snapshot `json:"snapshot"`
		Note     string             `json:"note"`
	}{result.Saved, result.Snapshot, "仅保存配置文件；未改变启动选择或启停当前服务"}
	return response
}

// 离线操作只在读取预期版本及保存期间持锁。已有目标必须是有效完整文档，
// 不能把不支持或损坏的文档当成空文件覆盖；首次保存仅使用空Revision。
func (r *runner) saveSettingsDocument(ctx context.Context, path string, document settings.Document, overwrite bool) (result settings.SaveResult, err error) {
	notSaved := false
	result.Saved = &notSaved
	store, err := r.settingsStore()
	if err != nil {
		return result, err
	}
	writer, err := store.OpenWriter(ctx, path)
	if err != nil {
		return result, err
	}
	defer func() { err = errors.Join(err, writer.Close()) }()
	snapshot, readErr := writer.Read()
	var revision settings.Revision
	switch {
	case errors.Is(readErr, os.ErrNotExist):
		if overwrite {
			return result, usageError("overwrite", "覆盖必须先读取已有有效完整文档")
		}
	case readErr != nil:
		return result, readErr
	case !overwrite:
		return result, usageError("file", "目标已存在；明确覆盖有效完整文档须指定--overwrite")
	default:
		revision = snapshot.Revision
	}
	return writer.Save(ctx, document, revision)
}

// 工具修改不改变启动选择或当前进程；读取和按所读Revision保存共用一个短持
// 写者，避免离线修改覆盖后台正在提交的配置、规则、日程或校准。
func (r *runner) setSettingsTool(ctx context.Context, path, tool string) (result settings.SaveResult, err error) {
	notSaved := false
	result.Saved = &notSaved
	store, err := r.settingsStore()
	if err != nil {
		return result, err
	}
	writer, err := store.OpenWriter(ctx, path)
	if err != nil {
		return result, err
	}
	defer func() { err = errors.Join(err, writer.Close()) }()
	snapshot, err := writer.Read()
	if err != nil {
		return result, err
	}
	document := snapshot.Document
	document.Runtime.ToolPath = tool
	return writer.Save(ctx, document, snapshot.Revision)
}

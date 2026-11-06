package cli

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/orzlee/displayKvm/internal/settings"
)

func TestSettingsProductCLINewCheckAndLanguageSaveNeedNoTools(t *testing.T) {
	store, root := unifiedCLIStore(t)
	path := filepath.Join(root, "settings.json")
	if code, result := unifiedCLI(t, store, "config", "new", "--file", path); code != ExitOK || !result.OK {
		t.Fatalf("无工具路径不能新建: exit=%d result=%+v", code, result)
	}
	snapshot, err := store.ReadFile(path)
	if err != nil || snapshot.Document.Desktop.Language != "system" || snapshot.Document.Runtime != (settings.RuntimeSettings{}) {
		t.Fatalf("新建默认语言或工具设置错误: %+v %v", snapshot.Document, err)
	}
	writer, err := store.OpenWriter(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	document := snapshot.Document
	document.Desktop.Language = "en"
	result, saveErr := writer.Save(context.Background(), document, snapshot.Revision)
	closeErr := writer.Close()
	if saveErr != nil || closeErr != nil || result.Snapshot == nil || result.Saved == nil || !*result.Saved {
		t.Fatalf("语言结构保存要求运行工具: result=%+v save=%v close=%v", result, saveErr, closeErr)
	}
	if code, checked := unifiedCLI(t, store, "config", "check", "--file", path); code != ExitOK || !checked.OK {
		t.Fatalf("结构检查错误要求运行就绪: exit=%d result=%+v", code, checked)
	}
	// 高级覆盖仍允许显式绝对路径；离线写入不启动服务，也不能因为工具尚未
	// 就绪而拒绝语言保存，或把其它运行字段和桌面偏好丢掉。
	tool := filepath.Join(root, "advanced-tool")
	if code, updated := unifiedCLI(t, store, "config", "set-tool", "--file", path, "--tool", tool); code != ExitOK || !updated.OK {
		t.Fatalf("高级工具覆盖写入失败: exit=%d result=%+v", code, updated)
	}
	document.Runtime.ToolPath = tool
	after, err := store.ReadFile(path)
	if err != nil || !reflect.DeepEqual(after.Document, document) {
		t.Fatalf("高级覆盖丢失语言或改写相邻设置: %+v %v", after.Document, err)
	}
	if code, cleared := unifiedCLI(t, store, "config", "set-tool", "--file", path, "--tool", ""); code != ExitOK || !cleared.OK {
		t.Fatalf("不能恢复包内默认工具: exit=%d result=%+v", code, cleared)
	}
	document.Runtime.ToolPath = ""
	after, err = store.ReadFile(path)
	if err != nil || !reflect.DeepEqual(after.Document, document) {
		t.Fatalf("恢复包内默认工具改变语言: %+v %v", after.Document, err)
	}
}

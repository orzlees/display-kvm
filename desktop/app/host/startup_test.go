package host

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

func TestStartupBridgeRejectsUnknownFieldAndUnboundHost(t *testing.T) {
	bridge := NewStartupBridge()
	for _, kind := range []string{"", "unknown", "config", "tool", "rules", "schedules"} {
		if _, canceled, err := bridge.PickFile(context.Background(), kind); err == nil || canceled {
			t.Fatalf("未知字段或未绑定宿主必须明确报错：kind=%q canceled=%v err=%v", kind, canceled, err)
		}
	}
	bridge.bind(context.Background(), func(context.Context, string) (string, error) {
		t.Error("未知字段不能进入原生选择器")
		return "", errors.New("未知字段进入了原生选择器")
	}, nil)
	if _, _, err := bridge.PickFile(context.Background(), "unknown"); err == nil {
		t.Fatal("已绑定宿主也必须拒绝未知字段")
	}
}

func TestStartupBridgeSelectionCancellationAndFailure(t *testing.T) {
	failure := errors.New("原生选择器失败")
	for _, tt := range []struct {
		name string
		path string
		err  error
	}{
		{name: "selected", path: "/Users/test/config with spaces.json"},
		{name: "canceled"},
		{name: "failed", err: failure},
	} {
		t.Run(tt.name, func(t *testing.T) {
			bridge := NewStartupBridge()
			bridge.bind(context.Background(), func(context.Context, string) (string, error) { return tt.path, tt.err }, nil)
			path, canceled, err := bridge.PickFile(context.Background(), "config")
			if path != tt.path || canceled != (tt.path == "" && tt.err == nil) || !errors.Is(err, tt.err) {
				t.Fatalf("选择结果错误：path=%q canceled=%v err=%v", path, canceled, err)
			}
		})
	}
}

func TestStartupBridgeCanceledRequestAndShutdownDoNotOpenDialog(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	bridge := NewStartupBridge()
	bridge.bind(context.Background(), func(context.Context, string) (string, error) {
		t.Error("已取消请求不能打开对话框")
		return "", nil
	}, nil)
	if _, _, err := bridge.PickFile(ctx, "config"); !errors.Is(err, context.Canceled) {
		t.Fatalf("必须保留请求取消原因：%v", err)
	}
	bridge.bind(ctx, func(context.Context, string) (string, error) {
		t.Error("退出后不能打开对话框")
		return "", nil
	}, nil)
	if _, canceled, err := bridge.PickFile(context.Background(), "tool"); err == nil || canceled {
		t.Fatalf("宿主退出必须是错误，不能伪装成用户取消：canceled=%v err=%v", canceled, err)
	}
}

func TestStartupBridgePendingDialogRejectsDuplicateAndHonorsRequestCancellation(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	bridge := NewStartupBridge()
	bridge.bind(context.Background(), func(context.Context, string) (string, error) {
		close(entered)
		<-release
		return "/config.json", nil
	}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { _, _, err := bridge.PickFile(ctx, "config"); result <- err }()
	<-entered
	if _, _, err := bridge.PickFile(context.Background(), "config"); err == nil {
		t.Fatal("已有原生对话框时不能再打开一个")
	}
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("等待中的请求应能取消：%v", err)
	}
	if _, _, err := bridge.PickFile(context.Background(), "config"); err == nil {
		t.Fatal("请求取消后，原生对话框关闭前仍不能重复打开")
	}
}

func TestApplicationPathUsesBundleAndPreservesDevelopmentCandidate(t *testing.T) {
	for _, tt := range []struct{ executable, want string }{
		{"/Applications/DisplayKvm.app/Contents/MacOS/DisplayKVM", "/Applications/DisplayKvm.app"},
		{"/Users/test/Applications/Display Kvm.app/Contents/MacOS/DisplayKVM", "/Users/test/Applications/Display Kvm.app"},
		{"/workspace/build/DisplayKvm.app/Contents/MacOS/DisplayKVM", "/workspace/build/DisplayKvm.app"},
		{"/workspace/desktop/DisplayKVM", "/workspace/desktop/DisplayKVM"},
		{"/workspace/not-an-app/Contents/MacOS/DisplayKVM", "/workspace/not-an-app/Contents/MacOS/DisplayKVM"},
	} {
		if got := applicationPath(tt.executable); got != tt.want {
			t.Errorf("包路径错误：%q => %q，期望%q", tt.executable, got, tt.want)
		}
	}
}

func TestUnifiedSettingsSaveFileSelectionCancellationAndFailure(t *testing.T) {
	failure := errors.New("原生保存对话框失败")
	const suggested = "/Users/test/Display KVM/settings.json"
	for _, tt := range []struct {
		name string
		path string
		err  error
	}{
		{name: "selected", path: "/Users/test/another config.json"},
		{name: "canceled"},
		{name: "failed", err: failure},
	} {
		t.Run(tt.name, func(t *testing.T) {
			bridge := NewStartupBridge()
			calls := 0
			bridge.bind(context.Background(), nil, func(_ context.Context, path string) (string, error) {
				if path != suggested {
					t.Errorf("建议绝对路径必须原样传入：%q", path)
				}
				calls++
				if calls == 1 {
					return tt.path, tt.err
				}
				return suggested, nil
			})
			path, canceled, err := bridge.SaveFile(context.Background(), suggested)
			if path != tt.path || canceled != (tt.path == "" && tt.err == nil) || !errors.Is(err, tt.err) {
				t.Fatalf("保存选择结果错误：path=%q canceled=%v err=%v", path, canceled, err)
			}
			if path, canceled, err = bridge.SaveFile(context.Background(), suggested); err != nil || canceled || path != suggested {
				t.Fatalf("选择、取消或错误返回后必须释放占用：path=%q canceled=%v err=%v", path, canceled, err)
			}
		})
	}
}

func TestUnifiedSettingsSaveFileRejectsInvalidSuggestionAndUnboundHost(t *testing.T) {
	bridge := NewStartupBridge()
	if _, canceled, err := bridge.SaveFile(context.Background(), "/settings.json"); err == nil || canceled {
		t.Fatalf("未绑定宿主必须报错：canceled=%v err=%v", canceled, err)
	}
	bridge.bind(context.Background(), nil, func(context.Context, string) (string, error) {
		t.Error("非绝对建议路径不能打开保存对话框")
		return "", nil
	})
	for _, path := range []string{"", "settings.json", "../settings.json"} {
		if _, canceled, err := bridge.SaveFile(context.Background(), path); err == nil || canceled {
			t.Fatalf("建议路径必须是绝对路径：path=%q canceled=%v err=%v", path, canceled, err)
		}
	}
}

func TestUnifiedSettingsSaveFileCanceledRequestAndShutdownDoNotOpenDialog(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	bridge := NewStartupBridge()
	save := func(context.Context, string) (string, error) {
		t.Error("已取消请求或已退出宿主不能打开保存对话框")
		return "", nil
	}
	bridge.bind(context.Background(), nil, save)
	if _, canceled, err := bridge.SaveFile(ctx, "/settings.json"); !errors.Is(err, context.Canceled) || canceled {
		t.Fatalf("请求取消必须保留错误原因：canceled=%v err=%v", canceled, err)
	}
	bridge.bind(ctx, nil, save)
	if _, canceled, err := bridge.SaveFile(context.Background(), "/settings.json"); !errors.Is(err, context.Canceled) || canceled {
		t.Fatalf("宿主退出不能伪装成用户取消：canceled=%v err=%v", canceled, err)
	}
}

func TestUnifiedSettingsOpenAndSaveShareDialogGate(t *testing.T) {
	for _, firstSave := range []bool{false, true} {
		name := "open_blocks_save"
		if firstSave {
			name = "save_blocks_open"
		}
		t.Run(name, func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			defer func() {
				select {
				case <-release:
				default:
					close(release)
				}
			}()
			bridge := NewStartupBridge()
			blocked := func(context.Context, string) (string, error) {
				close(entered)
				<-release
				return "/first.json", nil
			}
			immediate := func(context.Context, string) (string, error) { return "/second.json", nil }
			first := func() (string, bool, error) { return bridge.PickFile(context.Background(), "config") }
			second := func() (string, bool, error) { return bridge.SaveFile(context.Background(), "/settings.json") }
			if firstSave {
				bridge.bind(context.Background(), immediate, blocked)
				first, second = second, first
			} else {
				bridge.bind(context.Background(), blocked, immediate)
			}
			result := make(chan error, 1)
			go func() {
				_, _, err := first()
				result <- err
			}()
			<-entered
			if _, canceled, err := second(); err == nil || canceled {
				t.Fatalf("另一类对话框也必须受同一占用门禁限制：canceled=%v err=%v", canceled, err)
			}
			close(release)
			if err := <-result; err != nil {
				t.Fatalf("首次原生选择失败：%v", err)
			}
			if path, canceled, err := second(); err != nil || canceled || path != "/second.json" {
				t.Fatalf("原生选择返回后另一类对话框必须可用：path=%q canceled=%v err=%v", path, canceled, err)
			}
		})
	}
}

func TestUnifiedSettingsCanceledSaveKeepsGateUntilNativeReturns(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	bridge := NewStartupBridge()
	bridge.bind(context.Background(), func(context.Context, string) (string, error) {
		t.Error("原生保存窗口未关闭时不能打开文件选择窗口")
		return "", nil
	}, func(context.Context, string) (string, error) {
		close(entered)
		<-release
		return "/settings.json", nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { _, _, err := bridge.SaveFile(ctx, "/settings.json"); result <- err }()
	<-entered
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("保存等待应能响应请求取消：%v", err)
	}
	if _, canceled, err := bridge.PickFile(context.Background(), "config"); err == nil || canceled {
		t.Fatalf("请求离开不能提前释放原生窗口占用：canceled=%v err=%v", canceled, err)
	}
}

func TestUnifiedSettingsMainConnectsStoreAndBothDialogs(t *testing.T) {
	// 宿主包测试不启动main中的原生应用；检查实际入口的语法树，确保页面
	// 获得同一个完整配置存储和两个对话能力，避免仅验证桥后遗漏产品接线。
	source, err := parser.ParseFile(token.NewFileSet(), "../../main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	isSelector := func(expr ast.Expr, object, name string) bool {
		selector, ok := expr.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != name {
			return false
		}
		identifier, ok := selector.X.(*ast.Ident)
		return ok && identifier.Name == object
	}
	storeName := ""
	ast.Inspect(source, func(node ast.Node) bool {
		assignment, ok := node.(*ast.AssignStmt)
		if !ok || len(assignment.Rhs) != 1 || len(assignment.Lhs) == 0 {
			return true
		}
		call, ok := assignment.Rhs[0].(*ast.CallExpr)
		if ok && isSelector(call.Fun, "settings", "NewStore") {
			if identifier, ok := assignment.Lhs[0].(*ast.Ident); ok {
				storeName = identifier.Name
			}
		}
		return true
	})
	backendConnected, dialogsConnected := false, false
	ast.Inspect(source, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		if isSelector(call.Fun, "startup", "NewBackend") && len(call.Args) == 3 {
			if identifier, ok := call.Args[1].(*ast.Ident); ok {
				backendConnected = storeName != "" && identifier.Name == storeName
			}
		}
		if isSelector(call.Fun, "startup", "Page") && len(call.Args) == 2 {
			if dialogs, ok := call.Args[1].(*ast.CompositeLit); ok && isSelector(dialogs.Type, "startup", "FileDialogs") {
				openConnected, saveConnected := false, false
				for _, element := range dialogs.Elts {
					field, ok := element.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					key, ok := field.Key.(*ast.Ident)
					if !ok {
						continue
					}
					switch key.Name {
					case "Open":
						openConnected = isSelector(field.Value, "bridge", "PickFile")
					case "Save":
						saveConnected = isSelector(field.Value, "bridge", "SaveFile")
					}
				}
				dialogsConnected = openConnected && saveConnected
			}
		}
		return true
	})
	if !backendConnected || !dialogsConnected {
		t.Fatalf("启动页必须使用完整文档存储及打开/保存双对话接线：backend=%v dialogs=%v", backendConnected, dialogsConnected)
	}
}

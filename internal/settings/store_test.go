package settings

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/orzlee/displayKvm/internal/model"
)

func unifiedStore(t *testing.T) (*Store, string) {
	t.Helper()
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("本平台明确不支持配置写锁")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(Options{Environment: func() (Environment, error) {
		return Environment{ConfigDir: filepath.Join(root, "private"), UID: os.Getuid()}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	return store, root
}

func unifiedSaved(t *testing.T, result SaveResult, err error) Snapshot {
	t.Helper()
	if err != nil || result.Saved == nil || !*result.Saved || result.Snapshot == nil {
		t.Fatalf("未得到可证明的保存成功: %+v %v", result, err)
	}
	return *result.Snapshot
}

func TestUnifiedSettingsStoreRevisionPreservesOtherSectionsAndNeverRelocks(t *testing.T) {
	store, root := unifiedStore(t)
	if _, err := os.Stat(filepath.Join(root, "private")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("NewStore不应创建目录: %v", err)
	}
	path := filepath.Join(root, "settings.json")
	writer, err := store.OpenWriter(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if _, err := writer.Read(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("缺失目标应明确报告: %v", err)
	}
	doc := unifiedFixture(t)
	doc.Runtime.BusctlPath = filepath.Join(root, "busctl")
	result, err := writer.Save(context.Background(), doc, "")
	first := unifiedSaved(t, result, err)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	if first.Revision != Revision(hex.EncodeToString(digest[:])) || first.Path != path {
		t.Fatalf("版本须是原始字节摘要: %+v", first)
	}
	completed := make(chan error, 1)
	go func() {
		_, err := writer.Read()
		completed <- err
	}()
	select {
	case err := <-completed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("长持写锁内读取发生自锁")
	}
	changed := first.Document
	changed.Configuration.Displays[0].Name = "新名称"
	result, err = writer.Save(context.Background(), changed, first.Revision)
	second := unifiedSaved(t, result, err)
	if !reflect.DeepEqual(second.Document.Rules, doc.Rules) || !reflect.DeepEqual(second.Document.Schedules, doc.Schedules) || second.Document.Runtime != doc.Runtime {
		t.Fatal("保存业务配置必须保留其它分区")
	}
	if result, err := writer.Save(context.Background(), doc, first.Revision); err == nil || result.Saved == nil || *result.Saved {
		t.Fatalf("陈旧版本应明确失败: %+v %v", result, err)
	}
	if result, err := writer.Save(context.Background(), doc, ""); err == nil || result.Saved == nil || *result.Saved {
		t.Fatalf("空版本不得覆盖已有目标: %+v %v", result, err)
	}
	after, err := writer.Read()
	if err != nil || after.Revision != second.Revision {
		t.Fatalf("冲突不得改变目标: %+v %v", after, err)
	}
}

func TestUnifiedSettingsWriterExcludesOtherStoresAndReleasesOnClose(t *testing.T) {
	first, root := unifiedStore(t)
	second, err := NewStore(Options{Environment: func() (Environment, error) { return first.environment, nil }})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "settings.json")
	writer, err := first.OpenWriter(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if other, err := second.OpenWriter(context.Background(), filepath.Join(root, ".", "settings.json")); err == nil {
		other.Close()
		t.Fatal("另一Store不得绕过长持写锁")
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	other, err := second.OpenWriter(context.Background(), path)
	if err != nil {
		t.Fatalf("关闭后应释放锁: %v", err)
	}
	defer other.Close()
	if _, err := writer.Read(); err == nil {
		t.Fatal("已关闭写者不得读取")
	}
	if result, err := writer.Save(context.Background(), NewDocument(), ""); err == nil || result.Saved == nil || *result.Saved {
		t.Fatalf("已关闭写者不得保存: %+v %v", result, err)
	}
}

func TestUnifiedSettingsStandaloneAndInvalidTargetsRejectReadAndPreserveOriginal(t *testing.T) {
	store, root := unifiedStore(t)
	for name, raw := range map[string]string{
		"configuration-v1": `{"version":1,"displays":[],"targets":[],"groups":[]}`,
		"configuration-v2": `{"version":2,"displays":[],"input_schemes":[],"targets":[],"groups":[]}`,
		"configuration-v3": `{"version":3,"displays":[],"input_schemes":[],"targets":[],"groups":[]}`,
		"rules-only":       `{"version":1,"rules":[]}`,
		"schedules-only":   `{"version":1,"schedules":[]}`,
		"invalid":          `{"kind":"displaykvm-settings","version":99}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(root, name+".json")
			if err := os.WriteFile(path, []byte(raw), 0644); err != nil {
				t.Fatal(err)
			}
			snapshot, err := store.ReadFile(path)
			var validation *model.Error
			if !errors.As(err, &validation) || validation.Code != model.CodeInvalidConfig || !reflect.DeepEqual(snapshot, Snapshot{}) {
				t.Fatalf("ReadFile必须拒绝非完整配置，不得产生迁移草稿: %+v %v", snapshot, err)
			}
			writer, err := store.OpenWriter(context.Background(), path)
			if err != nil {
				t.Fatal(err)
			}
			defer writer.Close()
			if snapshot, err := writer.Read(); !errors.As(err, &validation) || validation.Code != model.CodeInvalidConfig || !reflect.DeepEqual(snapshot, Snapshot{}) {
				t.Fatalf("Writer.Read同样只支持完整文档: %+v %v", snapshot, err)
			}
			digest := sha256.Sum256([]byte(raw))
			result, err := writer.Save(context.Background(), NewDocument(), Revision(hex.EncodeToString(digest[:])))
			if err == nil || result.Saved == nil || *result.Saved || result.Snapshot != nil {
				t.Fatalf("即使原字节摘要准确，也不能覆盖非法文档或返回成功快照: %+v %v", result, err)
			}
			if strings.Contains(err.Error(), "导入") {
				t.Fatalf("错误不能再指向已删除的导入流程: %v", err)
			}
			data, err := os.ReadFile(path)
			if err != nil || string(data) != raw {
				t.Fatalf("来源被改动: %v", err)
			}
		})
	}
}

func TestUnifiedSettingsRejectsLinksAndChangedDirectoryIdentity(t *testing.T) {
	store, root := unifiedStore(t)
	real := filepath.Join(root, "real.json")
	var encoded bytes.Buffer
	if err := Encode(&encoded, NewDocument()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(real, encoded.Bytes(), 0644); err != nil {
		t.Fatal(err)
	}
	for name, link := range map[string]func(string, string) error{"symbolic": os.Symlink, "hard": os.Link} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(root, name+".json")
			if err := link(real, path); err != nil {
				t.Fatal(err)
			}
			defer os.Remove(path)
			if _, err := store.ReadFile(path); err == nil {
				t.Fatal("链接读取未被拒绝")
			}
			if writer, err := store.OpenWriter(context.Background(), path); err == nil {
				defer writer.Close()
				if result, err := writer.Save(context.Background(), NewDocument(), ""); err == nil || result.Saved == nil || *result.Saved {
					t.Fatalf("链接写入未被拒绝: %+v %v", result, err)
				}
			}
		})
	}
	directory := filepath.Join(root, "documents")
	if err := os.Mkdir(directory, 0755); err != nil {
		t.Fatal(err)
	}
	writer, err := store.OpenWriter(context.Background(), filepath.Join(directory, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if err := os.Rename(directory, directory+"-old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(directory, 0755); err != nil {
		t.Fatal(err)
	}
	if result, err := writer.Save(context.Background(), NewDocument(), ""); err == nil || result.Saved == nil || *result.Saved {
		t.Fatalf("锁定后父目录换身不得继续写: %+v %v", result, err)
	}
}

func TestUnifiedSettingsPersistenceActualBytesDetermineOutcome(t *testing.T) {
	for _, outcome := range []string{"before", "after", "unknown", "unreadable"} {
		t.Run(outcome, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "settings.json")
			if err := os.WriteFile(path, []byte("before"), 0600); err != nil {
				t.Fatal(err)
			}
			failure := errors.New("injected write or directory sync failure")
			result := SaveManagedFileWith(context.Background(), path, []byte("after"), func(_ context.Context, path string, data []byte) error {
				switch outcome {
				case "after":
					if err := os.WriteFile(path, data, 0600); err != nil {
						t.Fatal(err)
					}
				case "unknown":
					if err := os.WriteFile(path, []byte("external content"), 0600); err != nil {
						t.Fatal(err)
					}
				case "unreadable":
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
					if err := os.Mkdir(path, 0700); err != nil {
						t.Fatal(err)
					}
				}
				return failure
			})
			if !errors.Is(result.Err, failure) {
				t.Fatalf("底层错误丢失: %v", result.Err)
			}
			switch outcome {
			case "before":
				if result.Saved == nil || *result.Saved {
					t.Fatalf("原字节未变应为false: %+v", result)
				}
			case "after":
				if result.Saved == nil || !*result.Saved {
					t.Fatalf("即使目录同步报错，新字节已核实仍为true: %+v", result)
				}
			default:
				if result.Saved != nil {
					t.Fatalf("无法证明提交结果必须为unknown: %+v", result)
				}
			}
		})
	}
}

func TestUnifiedSettingsWriterReportsFailureAndUnknownWithoutSuccessSnapshot(t *testing.T) {
	store, root := unifiedStore(t)
	writer, err := store.OpenWriter(context.Background(), filepath.Join(root, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	failure := errors.New("injected failure")
	writer.write = func(context.Context, string, []byte) error { return failure }
	result, err := writer.Save(context.Background(), NewDocument(), "")
	if !errors.Is(err, failure) || result.Saved == nil || *result.Saved || result.Snapshot != nil {
		t.Fatalf("未改磁盘的失败必须报告false: %+v %v", result, err)
	}
	writer.write = func(_ context.Context, path string, _ []byte) error {
		if err := os.WriteFile(path, []byte("external content"), 0600); err != nil {
			t.Fatal(err)
		}
		return failure
	}
	result, err = writer.Save(context.Background(), NewDocument(), "")
	if !errors.Is(err, failure) || result.Saved != nil || result.Snapshot != nil {
		t.Fatalf("未知保存不能返回成功快照: %+v %v", result, err)
	}
}

func TestUnifiedSettingsWriterKeepsProvenSaveWhenDirectorySyncReportsError(t *testing.T) {
	store, root := unifiedStore(t)
	writer, err := store.OpenWriter(context.Background(), filepath.Join(root, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	failure := errors.New("injected post-rename directory sync failure")
	writer.write = func(ctx context.Context, path string, data []byte) error {
		if err := AtomicManagedWrite(ctx, path, data); err != nil {
			return err
		}
		return failure
	}
	result, err := writer.Save(context.Background(), NewDocument(), "")
	if !errors.Is(err, failure) || result.Saved == nil || !*result.Saved || result.Snapshot == nil || result.Snapshot.Revision == "" {
		t.Fatalf("已核实新字节时应保留true和快照，并上报同步错误: %+v %v", result, err)
	}
}

func TestUnifiedSettingsExternalByteRevisionAndCanceledSave(t *testing.T) {
	store, root := unifiedStore(t)
	path := filepath.Join(root, "settings.json")
	writer, err := store.OpenWriter(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	result, err := writer.Save(context.Background(), NewDocument(), "")
	first := unifiedSaved(t, result, err)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// 外部编辑器只改空白也改变Revision；不能因解码后的结构相同而覆盖新字节。
	data = append(data, ' ')
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
	if result, err := writer.Save(context.Background(), first.Document, first.Revision); err == nil || result.Saved == nil || *result.Saved {
		t.Fatalf("结构相同的外部改写仍须形成版本冲突: %+v %v", result, err)
	}
	current, err := writer.Read()
	if err != nil || current.Revision == first.Revision {
		t.Fatalf("重读须得到新的原始字节版本: %+v %v", current, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err = writer.Save(ctx, current.Document, current.Revision)
	if !errors.Is(err, context.Canceled) || result.Saved == nil || *result.Saved {
		t.Fatalf("取消不得提交: %+v %v", result, err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(after, data) {
		t.Fatalf("冲突和取消不得改动目标: %v", err)
	}
}

func TestUnifiedSettingsDirectoryAliasesShareLockAndLockReplacementFails(t *testing.T) {
	store, root := unifiedStore(t)
	directory := filepath.Join(root, "documents")
	if err := os.Mkdir(directory, 0755); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(directory, alias); err != nil {
		t.Fatal(err)
	}
	writer, err := store.OpenWriter(context.Background(), filepath.Join(directory, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if other, err := store.OpenWriter(context.Background(), filepath.Join(alias, "settings.json")); err == nil {
		other.Close()
		t.Fatal("父目录别名不得绕过同一目标写锁")
	}
	if err := os.Rename(writer.lock.Name(), writer.lock.Name()+"-old"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(writer.lock.Name(), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if result, err := writer.Save(context.Background(), NewDocument(), ""); err == nil || result.Saved == nil || *result.Saved {
		t.Fatalf("锁文件换身后不得继续写入: %+v %v", result, err)
	}
}

func TestUnifiedSettingsOrdinaryConfigurationDoesNotRequirePrivatePermissions(t *testing.T) {
	store, root := unifiedStore(t)
	directory := filepath.Join(root, "documents")
	if err := os.Mkdir(directory, 0755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "settings.json")
	var data bytes.Buffer
	if err := Encode(&data, NewDocument()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data.Bytes(), 0644); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.ReadFile(path)
	if err != nil {
		t.Fatalf("普通配置目录和文件可以为0755/0644: %v", err)
	}
	if _, err := os.Stat(store.environment.ConfigDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("只读不应创建内部目录: %v", err)
	}
	writer, err := store.OpenWriter(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	doc := snapshot.Document
	doc.Runtime.ToolPath = filepath.Join(root, "not-installed-yet")
	result, err := writer.Save(context.Background(), doc, snapshot.Revision)
	unifiedSaved(t, result, err)
}

func TestUnifiedSettingsWriterAcceptsPreparedNilEmptyLists(t *testing.T) {
	store, root := unifiedStore(t)
	writer, err := store.OpenWriter(context.Background(), filepath.Join(root, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	result, err := writer.Save(context.Background(), NewDocument(), "")
	first := unifiedSaved(t, result, err)
	// 现有配置克隆可能把零长度切片变成nil；写入边界应接受同一合法空配置，
	// 并统一编码根规则/日程为空数组，避免每个Prepare消费者自行补切片。
	doc := first.Document
	doc.Configuration.Displays, doc.Configuration.InputSchemes = nil, nil
	doc.Configuration.Targets, doc.Configuration.Groups = nil, nil
	doc.Rules, doc.Schedules = nil, nil
	result, err = writer.Save(context.Background(), doc, first.Revision)
	second := unifiedSaved(t, result, err)
	if second.Document.Rules == nil || second.Document.Schedules == nil || len(second.Document.Rules) != 0 || len(second.Document.Schedules) != 0 {
		t.Fatalf("保存结果应有明确空规则/日程: %+v", second.Document)
	}
}

func TestUnifiedSettingsLocalLockPathPreservesFilesystemNames(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "documents")
	names := []string{"Settings.json", "settings.json", "caf\u00e9.json", "cafe\u0301.json", strings.Repeat("x", 240) + ".json", "other.json"}
	seen := make(map[string]bool)
	for _, name := range names {
		path := localSettingsLockPath(filepath.Join(parent, name))
		expected := filepath.Join(parent, ".displaykvm-settings-locks", name)
		if path != expected || filepath.Base(path) != name || seen[path] {
			t.Fatalf("锁路径须保留完整原样文件名，由父卷解析别名，且不同配置独立: %q -> %q", name, path)
		}
		seen[path] = true
	}
}

func unifiedRequireFilesystemAlias(t *testing.T, parent, name, alias string) {
	t.Helper()
	path := filepath.Join(parent, "probe-"+name)
	if err := os.WriteFile(path, []byte("alias probe"), 0600); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)
	original, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	other, err := os.Stat(filepath.Join(parent, "probe-"+alias))
	if errors.Is(err, os.ErrNotExist) {
		t.Skip("临时目录所在卷不把这两种写法视为同一文件")
	}
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(original, other) {
		t.Skip("临时目录所在卷保留两个独立文件身份")
	}
}

func unifiedAliasesKeepOneWriterAcrossRenames(t *testing.T, name, alias string, existing bool) {
	t.Helper()
	store, root := unifiedStore(t)
	unifiedRequireFilesystemAlias(t, root, name, alias)
	if err := os.Chmod(root, 0755); err != nil {
		t.Fatal(err)
	}
	otherStore, err := NewStore(Options{Environment: func() (Environment, error) { return store.environment, nil }})
	if err != nil {
		t.Fatal(err)
	}
	path, aliasPath := filepath.Join(root, name), filepath.Join(root, alias)
	var expected Revision
	if existing {
		var encoded bytes.Buffer
		if err := Encode(&encoded, NewDocument()); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, encoded.Bytes(), 0644); err != nil {
			t.Fatal(err)
		}
		snapshot, err := store.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		expected = snapshot.Revision
	} else if _, err := os.Stat(aliasPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("本场景要求两个别名的目标都尚不存在: %v", err)
	}
	writer, err := store.OpenWriter(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if writer.lock.Name() != localSettingsLockPath(path) {
		t.Fatalf("Darwin应在目标父卷按原样文件名取得锁: %q", writer.lock.Name())
	}
	lockIdentity, err := os.Stat(writer.lock.Name())
	if err != nil {
		t.Fatal(err)
	}
	assertConflict := func(phase string) {
		t.Helper()
		other, err := otherStore.OpenWriter(context.Background(), aliasPath)
		if err == nil {
			other.Close()
			t.Fatalf("%s: 同一文件的另一写法不得取得第二把产品写锁", phase)
		}
	}
	assertConflict("第一次保存前")
	for index := 0; index < 3; index++ {
		before, statErr := os.Stat(path)
		if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
			t.Fatal(statErr)
		}
		doc := NewDocument()
		doc.Runtime.ToolPath = filepath.Join(root, strings.Repeat("tool", index+1))
		result, err := writer.Save(context.Background(), doc, expected)
		snapshot := unifiedSaved(t, result, err)
		expected = snapshot.Revision
		after, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if before != nil && os.SameFile(before, after) {
			t.Fatal("场景必须实际经过配置文件Rename换身")
		}
		currentLock, err := os.Stat(writer.lock.Name())
		if err != nil || !os.SameFile(lockIdentity, currentLock) {
			t.Fatalf("配置多次Rename不得改变长持锁对象: %v", err)
		}
		aliasInfo, err := os.Stat(aliasPath)
		if err != nil || !os.SameFile(after, aliasInfo) {
			t.Fatalf("配置替换后两个写法仍须指向同一文件: %v", err)
		}
		assertConflict("配置Rename后")
	}
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	independent, err := otherStore.OpenWriter(context.Background(), filepath.Join(root, "other.json"))
	if err != nil {
		t.Fatalf("同一目录的另一个配置应可同时取得独立写锁: %v", err)
	}
	defer independent.Close()
	result, err := independent.Save(context.Background(), NewDocument(), "")
	unifiedSaved(t, result, err)
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(original, after) {
		t.Fatalf("保存其它配置不得改变持锁原件: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	replacement, err := otherStore.OpenWriter(context.Background(), aliasPath)
	if err != nil {
		t.Fatalf("关闭后另一别名应可取得同一把锁: %v", err)
	}
	defer replacement.Close()
	snapshot, err := replacement.Read()
	if err != nil || snapshot.Revision != expected {
		t.Fatalf("后继写者必须读到原持锁者的最后版本: %+v %v", snapshot, err)
	}
	result, err = replacement.Save(context.Background(), snapshot.Document, snapshot.Revision)
	unifiedSaved(t, result, err)
}

func TestUnifiedSettingsDarwinCaseAliasesKeepOneWriterAcrossRenames(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Darwin目标父卷别名锁布局")
	}
	for _, existing := range []bool{false, true} {
		name := "missing"
		if existing {
			name = "existing"
		}
		t.Run(name, func(t *testing.T) {
			unifiedAliasesKeepOneWriterAcrossRenames(t, "Settings.json", "settings.json", existing)
		})
	}
}

func TestUnifiedSettingsDarwinUnicodeAliasesKeepOneWriterAcrossRenames(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Darwin目标父卷别名锁布局")
	}
	for _, existing := range []bool{false, true} {
		name := "missing"
		if existing {
			name = "existing"
		}
		t.Run(name, func(t *testing.T) {
			unifiedAliasesKeepOneWriterAcrossRenames(t, "caf\u00e9.json", "cafe\u0301.json", existing)
		})
	}
}

func TestUnifiedSettingsDarwinLocalLockDirectorySafetyAndReadOnlyNoCreation(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Darwin目标父卷私有锁目录")
	}
	for _, conflict := range []string{"directory-create-denied", "directory-file", "directory-permissions", "directory-symlink", "lock-permissions", "lock-symlink", "lock-hardlink"} {
		t.Run(conflict, func(t *testing.T) {
			if conflict == "directory-create-denied" && os.Getuid() == 0 {
				t.Skip("root可绕过目录写权限，不能验证创建被拒绝")
			}
			store, root := unifiedStore(t)
			path := filepath.Join(root, "settings.json")
			var encoded bytes.Buffer
			if err := Encode(&encoded, NewDocument()); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, encoded.Bytes(), 0644); err != nil {
				t.Fatal(err)
			}
			lockPath := localSettingsLockPath(path)
			directory := filepath.Dir(lockPath)
			if _, err := store.ReadFile(path); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(directory); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("只读不得创建目标旁的锁目录: %v", err)
			}
			var linkedDirectory string
			switch conflict {
			case "directory-create-denied":
				if err := os.Chmod(root, 0500); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := os.Chmod(root, 0700); err != nil {
						t.Error(err)
					}
				})
			case "directory-file":
				if err := os.WriteFile(directory, []byte("occupied"), 0600); err != nil {
					t.Fatal(err)
				}
			case "directory-symlink":
				linkedDirectory = filepath.Join(root, "linked")
				if err := os.Mkdir(linkedDirectory, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(linkedDirectory, directory); err != nil {
					t.Fatal(err)
				}
			default:
				if err := os.Mkdir(directory, 0700); err != nil {
					t.Fatal(err)
				}
				switch conflict {
				case "directory-permissions":
					if err := os.Chmod(directory, 0755); err != nil {
						t.Fatal(err)
					}
				case "lock-permissions":
					if err := os.WriteFile(lockPath, nil, 0644); err != nil {
						t.Fatal(err)
					}
					if err := os.Chmod(lockPath, 0644); err != nil {
						t.Fatal(err)
					}
				case "lock-symlink", "lock-hardlink":
					other := filepath.Join(root, "other-lock")
					if err := os.WriteFile(other, nil, 0600); err != nil {
						t.Fatal(err)
					}
					link := os.Symlink
					if conflict == "lock-hardlink" {
						link = os.Link
					}
					if err := link(other, lockPath); err != nil {
						t.Fatal(err)
					}
				}
			}
			if writer, err := store.OpenWriter(context.Background(), path); err == nil {
				writer.Close()
				t.Fatal("不安全或被占用的锁目录/文件必须明确拒绝")
			}
			current, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(current, encoded.Bytes()) {
				t.Fatalf("锁失败必须保留原配置字节: %v", err)
			}
			if conflict == "directory-permissions" {
				info, err := os.Stat(directory)
				if err != nil || info.Mode().Perm() != 0755 {
					t.Fatalf("不能通过chmod自动修复不安全的目录: %v", err)
				}
			}
			if linkedDirectory != "" {
				entries, err := os.ReadDir(linkedDirectory)
				if err != nil || len(entries) != 0 {
					t.Fatalf("拒绝目录链接前不得在链接目的地创建锁: %v", err)
				}
			}
		})
	}
}

package loginstart

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/orzlee/displayKvm/internal/model"
)

func TestLoginStartWindowsIdentityDoesNotInventUnixUID(t *testing.T) {
	env := Environment{ConfigDir: t.TempDir(), HomeDir: t.TempDir(), UID: -1}
	checked := 0
	identity := func() error { checked++; return nil }
	if err := validateEnvironment(env, "windows", identity); err != nil || checked != 1 || env.UID != -1 {
		t.Fatalf("Windows必须核验真实身份而不改写UID: %v checked=%d uid=%d", err, checked, env.UID)
	}
	denied := errors.New("token denied")
	if err := validateEnvironment(env, "windows", func() error { return denied }); !errors.Is(err, denied) {
		t.Fatalf("真实令牌错误被忽略: %v", err)
	}
	if err := validateEnvironment(env, "darwin", identity); err == nil {
		t.Fatal("Unix平台错误地接受负UID")
	}
	env.ConfigDir = "relative"
	if err := validateEnvironment(env, "windows", identity); err == nil {
		t.Fatal("Windows接受相对配置目录")
	}
}

func TestLoginStartRunProgramsRejectRenamedRolesAndWrongPlatforms(t *testing.T) {
	want := []struct{ name, entry string }{
		{"displaykvm.exe", "github.com/orzlee/displayKvm/desktop"},
		{"displaykvmd.exe", "github.com/orzlee/displayKvm/cmd/displaykvmd"},
		{"displaykvm-cli.exe", "github.com/orzlee/displayKvm/cmd/displaykvm"},
	}
	for index, program := range runPrograms {
		if program.name != want[index].name || program.entry != want[index].entry {
			t.Fatalf("Windows候选包角色映射改变: %+v", program)
		}
		if err := program.validate(want[index].entry, "windows", "amd64", "amd64"); err != nil {
			t.Fatal(err)
		}
		for _, role := range want {
			if role.entry != program.entry {
				if err := program.validate(role.entry, "windows", "amd64", "amd64"); err == nil {
					t.Fatalf("接受重命名角色%s作为%s", role.entry, program.name)
				}
			}
		}
		for _, platform := range []struct{ goos, arch string }{{"darwin", "amd64"}, {"windows", "arm64"}, {"", ""}} {
			if err := program.validate(program.entry, platform.goos, platform.arch, "amd64"); err == nil {
				t.Fatalf("接受错误/缺失构建平台: %+v", platform)
			}
		}
	}
}

type fakeRunKey struct {
	values                  map[string]string
	reads                   []string
	writes, deletes, closes int
	readErr, writeErr       error
	deleteErr, closeErr     error
	readAfterWriteErr       error
}

func (k *fakeRunKey) Read(name string) (string, error) {
	k.reads = append(k.reads, name)
	if k.readErr != nil {
		return "", k.readErr
	}
	if k.writes > 0 && k.readAfterWriteErr != nil {
		return "", k.readAfterWriteErr
	}
	value, exists := k.values[name]
	if !exists {
		return "", os.ErrNotExist
	}
	return value, nil
}

func (k *fakeRunKey) Write(name, value string) error {
	k.writes++
	if k.writeErr == nil {
		k.values[name] = value
	}
	return k.writeErr
}

func (k *fakeRunKey) Delete(name string) error {
	k.deletes++
	if k.deleteErr == nil {
		delete(k.values, name)
	}
	return k.deleteErr
}

func (k *fakeRunKey) Close() error { k.closes++; return k.closeErr }

type fakeRunLock struct{ err error }

func (l fakeRunLock) Close() error { return l.err }

func newRunFixture(t *testing.T) (*runRegistration, *fakeRunKey, string) {
	t.Helper()
	// fake不访问应用目录；用固定短路径，避免测试框架的长临时目录名触发Run上限。
	root := filepath.Join(filepath.VolumeName(t.TempDir())+string(os.PathSeparator), "Users", "Run User")
	m := &Manager{env: Environment{ConfigDir: filepath.Join(root, "User Config"), HomeDir: root, UID: -1}}
	key := &fakeRunKey{values: map[string]string{"another.application": `"C:\Other App\other.exe" --keep`}}
	r := &runRegistration{
		manager: m,
		open:    func(bool) (runKey, error) { return key, nil },
		lock:    func() (io.Closer, error) { return fakeRunLock{}, nil },
		profile: func(context.Context) (ServiceProfile, error) {
			return ServiceProfile{Version: 2, ConfigPath: filepath.Join(root, "settings.json")}, nil
		},
		validate: func(string, Target, bool) error { return nil },
		ready:    func(ServiceProfile, string) error { return nil },
	}
	return r, key, filepath.Join(root, "Display KVM", "displaykvm.exe")
}

func requireRunBool(t *testing.T, value *bool, expected bool) {
	t.Helper()
	if value == nil || *value != expected {
		t.Fatalf("状态=%v，期望%t", value, expected)
	}
}

func TestLoginStartRunCommandQuotesAndExactTargets(t *testing.T) {
	r, _, app := newRunFixture(t)
	for _, target := range []Target{Service, Desktop} {
		command, err := r.command(target, app)
		if err != nil {
			t.Fatal(err)
		}
		want := `"` + app + `" "--login-start"`
		if target == Service {
			want = `"` + filepath.Join(filepath.Dir(app), "displaykvmd.exe") + `" "-startup-profile" "` + r.manager.profilePath() + `"`
		}
		if command != want {
			t.Fatalf("参数必须逐个引用，禁止shell或PATH: %q want %q", command, want)
		}
		for _, input := range []string{app, filepath.Dir(app)} {
			got, err := r.command(target, input)
			if err != nil || got != command {
				t.Fatalf("GUI绝对路径和包目录应指向同包程序: %q %v", got, err)
			}
		}
		if decoded, err := r.owned(target, command); err != nil || decoded != app {
			t.Fatalf("无法精确认领本应用命令: %q %v", decoded, err)
		}
		otherTarget := Service
		if target == Service {
			otherTarget = Desktop
			wrongProfile := strings.Replace(command, r.manager.profilePath(), filepath.Join(r.manager.env.ConfigDir, "foreign.json"), 1)
			if _, err := r.owned(target, wrongProfile); err == nil {
				t.Fatal("接受了其它启动档案引用")
			}
		}
		if _, err := r.owned(otherTarget, command); err == nil {
			t.Fatal("两个目标的命令可以相互冒用")
		}
		for _, foreign := range []string{command + ` "--extra"`, strings.Trim(command, `"`), strings.Replace(command, "displaykvm", "other", 1), command + " & calc.exe"} {
			if _, err := r.owned(target, foreign); err == nil {
				t.Errorf("接受外来命令或额外参数: %q", foreign)
			}
		}
	}
	for argument, expected := range map[string]string{
		`C:\Program Files\DisplayKVM\displaykvm.exe`: `"C:\Program Files\DisplayKVM\displaykvm.exe"`,
		`C:\trailing\`: `"C:\trailing\\"`,
		`a\"b`:         `"a\\\"b"`,
	} {
		if got := quoteRunArgument(argument); got != expected {
			t.Errorf("Windows反斜杠/引号规则错误: %q want %q", got, expected)
		}
	}
	if _, err := r.command(Desktop, filepath.Join(filepath.Dir(app), strings.Repeat("a", 261), "displaykvm.exe")); err == nil {
		t.Fatal("接受超过Windows Run限制的命令")
	}
}

func TestLoginStartRunAllFourTargetCombinations(t *testing.T) {
	for _, service := range []bool{false, true} {
		for _, desktop := range []bool{false, true} {
			r, key, app := newRunFixture(t)
			if service {
				key.values[label(Service)], _ = r.command(Service, app)
			}
			if desktop {
				key.values[label(Desktop)], _ = r.command(Desktop, app)
			}
			status, err := r.status(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			requireRunBool(t, status.Service.Enabled, service)
			requireRunBool(t, status.Desktop.Enabled, desktop)
			if status.Service.Loaded != nil || status.Desktop.Loaded != nil || key.writes != 0 || key.deletes != 0 {
				t.Fatal("状态查询产生写入或伪造进程状态")
			}
		}
	}
}

func TestLoginStartRunIndependentIdempotentAndExactCleanup(t *testing.T) {
	r, key, app := newRunFixture(t)
	other := key.values["another.application"]
	for _, target := range []Target{Service, Desktop} {
		for repeat := 0; repeat < 2; repeat++ {
			result, err := r.change(context.Background(), target, app, true)
			if err != nil {
				t.Fatal(err)
			}
			requireRunBool(t, result.Saved, true)
		}
	}
	if key.writes != 2 {
		t.Fatalf("重复开启不应重写已核实的值: %d", key.writes)
	}
	status, err := r.status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	requireRunBool(t, status.Service.Enabled, true)
	requireRunBool(t, status.Desktop.Enabled, true)
	if status.Service.Loaded != nil || status.Desktop.Loaded != nil {
		t.Fatal("Run登记不能冒充当前进程已加载")
	}
	// 应用已丢失只能影响就绪；关闭不能再依赖可执行文件存在。
	r.validate = func(string, Target, bool) error { return os.ErrNotExist }
	for repeat := 0; repeat < 2; repeat++ {
		result, err := r.change(context.Background(), Service, "", false)
		if err != nil {
			t.Fatal(err)
		}
		requireRunBool(t, result.Saved, true)
		requireRunBool(t, result.Status.Service.Enabled, false)
		requireRunBool(t, result.Status.Desktop.Enabled, true)
	}
	if key.deletes != 1 || key.values["another.application"] != other {
		t.Fatal("重复关闭修改了其它登录项或重发删除")
	}
	for _, name := range key.reads {
		if name != label(Service) && name != label(Desktop) {
			t.Fatalf("访问了本应用之外的登录项: %s", name)
		}
	}
}

func TestLoginStartRunRejectsForeignValuesAndInvalidInputs(t *testing.T) {
	r, key, app := newRunFixture(t)
	for _, input := range []string{"relative", app + `" --extra`, app + "\x00", app + "\n", filepath.Join(filepath.Dir(app), "displaykvmd.exe"), app + "%USERPROFILE%"} {
		if result, err := r.change(context.Background(), Desktop, input, true); err == nil {
			t.Fatalf("接受不明确的应用引用: %q %+v", input, result)
		}
	}
	if _, err := r.change(context.Background(), Target("desktop.other"), app, true); model.CodeOf(err) != model.CodeInvalidRequest {
		t.Fatalf("非法目标错误类别不正确: %v", err)
	}
	foreign := `"C:\Windows\System32\cmd.exe" /c calc.exe`
	key.values[label(Desktop)] = foreign
	for _, enabled := range []bool{false, true} {
		result, err := r.change(context.Background(), Desktop, app, enabled)
		if err == nil || key.values[label(Desktop)] != foreign {
			t.Fatalf("覆盖/删除了同名外来值: %+v %v", result, err)
		}
		requireRunBool(t, result.Saved, false)
		if result.Status.Desktop.Enabled != nil {
			t.Fatal("外来值被冒充本应用已知开关")
		}
	}
	if key.writes != 0 || key.deletes != 0 {
		t.Fatal("校验失败仍发生注册表写操作")
	}
}

func TestLoginStartRunErrorsPreserveSavedAndUnknownStates(t *testing.T) {
	for _, stage := range []string{"open", "read", "write", "delete", "close", "readback", "lock"} {
		t.Run(stage, func(t *testing.T) {
			r, key, app := newRunFixture(t)
			failure := errors.New("registry " + stage + " denied")
			enable := true
			switch stage {
			case "open":
				r.open = func(bool) (runKey, error) { return nil, failure }
			case "read":
				key.readErr = failure
			case "write":
				key.writeErr = failure
			case "delete":
				key.values[label(Desktop)], _ = r.command(Desktop, app)
				key.deleteErr = failure
				enable = false
			case "close":
				key.closeErr = failure
			case "readback":
				key.readAfterWriteErr = failure
			case "lock":
				r.lock = func() (io.Closer, error) { return nil, failure }
			}
			result, err := r.change(context.Background(), Desktop, app, enable)
			if !errors.Is(err, failure) {
				t.Fatalf("错误原因被吞掉: %+v %v", result, err)
			}
			switch stage {
			case "close":
				requireRunBool(t, result.Saved, true)
			case "readback":
				if result.Saved != nil || result.Status.Desktop.Enabled != nil {
					t.Fatal("写后无法读回却冒充已知结果")
				}
			default:
				requireRunBool(t, result.Saved, false)
			}
		})
	}
}

func TestLoginStartRunCanceledAndMissingKeyNeverWrites(t *testing.T) {
	r, key, app := newRunFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.change(ctx, Desktop, app, true); !errors.Is(err, context.Canceled) {
		t.Fatalf("未保留取消原因: %v", err)
	}
	r.open = func(write bool) (runKey, error) {
		if write {
			t.Fatal("查询和关闭缺失项创建了Run键")
		}
		return nil, os.ErrNotExist
	}
	if _, err := r.status(context.Background()); err != nil {
		t.Fatal(err)
	}
	result, err := r.change(context.Background(), Desktop, "", false)
	if err != nil {
		t.Fatal(err)
	}
	requireRunBool(t, result.Saved, true)
	if key.writes != 0 || key.deletes != 0 {
		t.Fatal("取消或缺失项产生副作用")
	}
}

func TestLoginStartRunEnableChecksCompletePackageAndSelectedProfile(t *testing.T) {
	r, key, app := newRunFixture(t)
	missingHelper := errors.New("same-package CLI missing or renamed")
	completeChecks := 0
	r.validate = func(got string, target Target, complete bool) error {
		if complete {
			completeChecks++
			if got != app {
				t.Fatalf("应用路径未规范为GUI路径: %q", got)
			}
			return missingHelper
		}
		return nil
	}
	for _, target := range []Target{Service, Desktop} {
		if result, err := r.change(context.Background(), target, filepath.Dir(app), true); !errors.Is(err, missingHelper) {
			t.Fatalf("未要求完整同包程序: %+v %v", result, err)
		}
	}
	if completeChecks != 2 || key.writes != 0 {
		t.Fatal("预检缺少角色或失败仍写入")
	}
	r.validate = func(string, Target, bool) error { return nil }
	invalidProfile := errors.New("selected complete settings document is invalid")
	r.profile = func(context.Context) (ServiceProfile, error) { return ServiceProfile{}, invalidProfile }
	if result, err := r.change(context.Background(), Service, app, true); !errors.Is(err, invalidProfile) || key.writes != 0 {
		t.Fatalf("后台使用了无效档案: %+v %v", result, err)
	}
	// 桌面任务只需完整应用；其它目标的档案错误仍可见，但不阻止桌面登记。
	result, err := r.change(context.Background(), Desktop, app, true)
	if !errors.Is(err, invalidProfile) {
		t.Fatalf("隐藏了后台档案错误: %v", err)
	}
	requireRunBool(t, result.Saved, true)
	if key.writes != 1 {
		t.Fatal("独立桌面登记被后台档案阻止")
	}
}

package settings

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sync"

	"github.com/orzlee/displayKvm/internal/model"
)

type Revision string

type Snapshot struct {
	Path     string
	Document Document
	Revision Revision
}

type SaveResult struct {
	Saved    *bool
	Snapshot *Snapshot
}

type Environment struct {
	ConfigDir string
	UID       int
}

type Options struct {
	Environment func() (Environment, error)
}

type Store struct {
	environment Environment
}

// Writer的文件锁覆盖整个运行代；Read/Save只取此实例的互斥锁，不能再取得
// 同一路径的文件锁。外部编辑器不遵守该协议，版本核对不构成跨进程原子CAS。
type Writer struct {
	mu        sync.Mutex
	path      string
	directory os.FileInfo
	lock      *os.File
	closed    bool
	write     func(context.Context, string, []byte) error
}

func NewStore(options Options) (*Store, error) {
	environment := options.Environment
	if environment == nil {
		environment = func() (Environment, error) {
			directory, err := os.UserConfigDir()
			return Environment{ConfigDir: directory, UID: os.Getuid()}, err
		}
	}
	env, err := environment()
	if err != nil {
		return nil, err
	}
	if env.ConfigDir == "" || !filepath.IsAbs(env.ConfigDir) {
		return nil, model.NewError(model.CodePermissionDenied, "内部锁配置目录必须是绝对路径", nil)
	}
	return &Store{environment: env}, nil
}

func (s *Store) ReadFile(path string) (Snapshot, error) {
	path, _, err := settingsPath(path)
	if err != nil {
		return Snapshot{}, err
	}
	return readSnapshot(path)
}

func (s *Store) OpenWriter(ctx context.Context, path string) (*Writer, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path, directory, err := settingsPath(path)
	if err != nil {
		return nil, err
	}
	// 先拒绝危险目标；Read和Save各自校验完整文档，Save明确拒绝覆盖非法文档。
	// 缺失目标只在Save拿到空Revision并再次核对后才允许创建。
	if _, _, err := ReadManagedFile(path); err != nil {
		return nil, err
	}
	lock, err := acquireSettingsLock(ctx, s.environment, path)
	if err != nil {
		return nil, err
	}
	writer := &Writer{path: path, directory: directory, lock: lock, write: AtomicManagedWrite}
	if err := writer.checkIdentity(); err != nil {
		return nil, errors.Join(err, writer.Close())
	}
	return writer, nil
}

func (w *Writer) Read() (Snapshot, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.checkIdentity(); err != nil {
		return Snapshot{}, err
	}
	snapshot, err := readSnapshot(w.path)
	if err != nil {
		return Snapshot{}, err
	}
	if err := w.checkIdentity(); err != nil {
		return Snapshot{}, err
	}
	return snapshot, nil
}

func (w *Writer) Save(ctx context.Context, doc Document, expected Revision) (SaveResult, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	unchanged := SaveResult{Saved: known(false)}
	if err := ctx.Err(); err != nil {
		return unchanged, err
	}
	if err := w.checkIdentity(); err != nil {
		return unchanged, err
	}
	var encoded bytes.Buffer
	if err := Encode(&encoded, doc); err != nil {
		return unchanged, err
	}
	before, exists, err := ReadManagedFile(w.path)
	if err != nil {
		return unchanged, err
	}
	if exists {
		// 只有有效完整文档可原位保存；即使调用者提供了目标文件的真实摘要，
		// 也不能把裸业务配置、损坏文件或未来版本强行覆盖。
		if _, err := Load(bytes.NewReader(before)); err != nil {
			return unchanged, documentError("", "保存目标不是有效完整配置，不能覆盖", err)
		}
		if expected == "" || expected != revisionOf(before) {
			return unchanged, model.NewError(model.CodeInvalidRequest, "配置版本冲突，目标已存在或已被修改，请重新读取", nil)
		}
	} else if expected != "" {
		return unchanged, model.NewError(model.CodeInvalidRequest, "配置版本冲突，原目标已不存在", nil)
	}
	if err := w.checkIdentity(); err != nil {
		return unchanged, err
	}
	persisted := SaveManagedFileWith(ctx, w.path, encoded.Bytes(), w.write)
	result := SaveResult{Saved: persisted.Saved}
	if err := w.checkIdentity(); err != nil {
		return SaveResult{}, errors.Join(persisted.Err, model.NewError(model.CodeIO, "提交后路径身份变化，结果未知", err))
	}
	if result.Saved != nil && *result.Saved {
		// SaveManagedFileWith已核实字节；再读取当前快照以返回准确Revision，
		// 若外部编辑器在这一步改写，不把旧版本或新分区冒充本次保存结果。
		snapshot, err := readSnapshot(w.path)
		if err != nil || snapshot.Revision != revisionOf(encoded.Bytes()) {
			return SaveResult{}, errors.Join(persisted.Err, model.NewError(model.CodeIO, "保存后无法取得同一版本快照，结果未知", err))
		}
		result.Snapshot = &snapshot
	}
	return result, persisted.Err
}

func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.closed = true
	// 锁文件不能删除：另一个进程可能已经打开同一inode等待加锁，删除后会
	// 产生两个互不排斥的锁文件。关闭描述符释放锁，保留内部0600锁文件。
	return w.lock.Close()
}

func (w *Writer) checkIdentity() error {
	if w.closed {
		return model.NewError(model.CodeInvalidRequest, "配置写者已关闭", nil)
	}
	current, err := os.Lstat(filepath.Dir(w.path))
	if err != nil || !os.SameFile(w.directory, current) || !current.IsDir() || current.Mode()&os.ModeSymlink != 0 {
		return model.NewError(model.CodeIO, "配置父目录在持锁期间已被替换", err)
	}
	return checkSettingsLock(w.lock)
}

func settingsPath(path string) (string, os.FileInfo, error) {
	if path == "" {
		return "", nil, model.NewError(model.CodeInvalidRequest, "配置路径不能为空", nil)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", nil, err
	}
	// 目录别名规范为同一真实路径，避免经父目录符号链接绕过同一目标写锁。
	// 末级文件绝不EvalSymlinks，必须留给安全读取边界明确拒绝。
	parent, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return "", nil, err
	}
	info, err := os.Lstat(parent)
	if err != nil {
		return "", nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", nil, model.NewError(model.CodePermissionDenied, "配置父路径必须是目录", nil)
	}
	return filepath.Join(parent, filepath.Base(abs)), info, nil
}

// localSettingsLockPath把锁放在目标父卷，完整保留basename。Darwin卷可能将
// 大小写或Unicode的不同写法解析为同一文件，路径摘要或字符串大小写转换均
// 不能表达该规则；由同一卷解析原样锁名，目标尚不存在和Rename换身也能互斥。
// 此布局只协调使用本布局的写者，不能据此推断旧版本持有的摘要锁已释放。
func localSettingsLockPath(target string) string {
	return filepath.Join(filepath.Dir(target), ".displaykvm-settings-locks", filepath.Base(target))
}

func readSnapshot(path string) (Snapshot, error) {
	data, exists, err := ReadManagedFile(path)
	if err != nil {
		return Snapshot{}, err
	}
	if !exists {
		return Snapshot{}, &os.PathError{Op: "read", Path: path, Err: os.ErrNotExist}
	}
	doc, err := Load(bytes.NewReader(data))
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{Path: path, Document: doc, Revision: revisionOf(data)}, nil
}

func revisionOf(data []byte) Revision {
	digest := sha256.Sum256(data)
	return Revision(hex.EncodeToString(digest[:]))
}

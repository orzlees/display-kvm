package ipc

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/orzlee/displayKvm/internal/model"
)

type Instance struct {
	mu             sync.Mutex
	directory      string
	lock           *os.File
	token          string
	started        time.Time
	published      os.FileInfo
	configuration  os.FileInfo
	closed         bool
	cleanupError   error
	releaseRuntime func() error
}

// Acquire只创建本用户私有运行目录及持久锁文件；互斥由系统锁保证，不能用PID
// 文件判断存活。锁文件不删除，否则等候者仍锁旧inode时新进程可能锁住新inode。
func Acquire(configDirectory string) (*Instance, error) {
	directory, release, err := privateRuntime(configDirectory, true)
	if err != nil {
		return nil, err
	}
	lock, err := openPrivate(filepath.Join(directory, "instance.lock"), true)
	if err != nil {
		return nil, errors.Join(err, release())
	}
	if err := lockExclusive(lock); err != nil {
		return nil, errors.Join(err, lock.Close(), release())
	}
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return nil, errors.Join(model.NewError(model.CodeInternal, "无法生成后台认证材料", err), lock.Close(), release())
	}
	// Windows目录句柄必须与实例锁共同存活，否则路径在检查后仍可能被替换；
	// Unix租约为空操作，沿用已有目录权限与永久锁文件约定。
	return &Instance{directory: directory, lock: lock, token: base64.RawURLEncoding.EncodeToString(random[:]), started: time.Now(), releaseRuntime: release}, nil
}

func (i *Instance) Endpoint(address string) model.Endpoint {
	i.mu.Lock()
	defer i.mu.Unlock()
	return model.Endpoint{Version: model.EndpointVersion, ProtocolVersion: model.ProtocolVersion, Network: "tcp4", Address: address, Token: i.token, PID: os.Getpid(), StartedAt: i.started}
}

// Publish在监听成功后原子替换端点。已有文件也须私有且非符号链接，不修宽松权限。
func (i *Instance) Publish(endpoint model.Endpoint) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.closed || endpoint.Token != i.token || endpoint.PID != os.Getpid() || !endpoint.StartedAt.Equal(i.started) {
		return model.NewError(model.CodeInvalidRequest, "不能发布其他实例的端点", nil)
	}
	if err := endpoint.Validate(); err != nil {
		return err
	}
	data, err := json.Marshal(endpoint)
	if err != nil {
		return model.NewError(model.CodeInternal, "编码私有端点失败", err)
	}
	info, err := publishPrivate(i.directory, "endpoint.json", data)
	if err != nil {
		return err
	}
	i.published = info
	return nil
}

// publishPrivate供端点和固定配置共用同一原子发布边界；替换前检查旧文件，
// 创建临时文件后也经同一私有文件校验，不能因umask或类型变化发布不可核验内容。
func publishPrivate(directory, name string, data []byte) (os.FileInfo, error) {
	path := filepath.Join(directory, name)
	if _, err := os.Lstat(path); err == nil {
		old, err := openPrivate(path, false)
		if err != nil {
			return nil, err
		}
		if err := old.Close(); err != nil {
			return nil, model.NewError(model.CodeIO, "关闭旧私有文件失败", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, model.NewError(model.CodeIO, "检查旧私有文件失败", err)
	}
	file, err := createPrivateTemp(directory, ".publish-*")
	if err != nil {
		return nil, model.NewError(model.CodeIO, "创建私有临时文件失败", err)
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	defer file.Close()
	if _, err := file.Write(data); err != nil {
		return nil, model.NewError(model.CodeIO, "写入私有文件失败", err)
	}
	if err := file.Sync(); err != nil {
		return nil, model.NewError(model.CodeIO, "同步私有文件失败", err)
	}
	if err := file.Close(); err != nil {
		return nil, model.NewError(model.CodeIO, "关闭新私有文件失败", err)
	}
	check, err := openPrivate(temporary, false)
	if err != nil {
		return nil, err
	}
	info, statError := check.Stat()
	if err := errors.Join(statError, check.Close()); err != nil {
		return nil, model.NewError(model.CodeIO, "核验新私有文件失败", err)
	}
	if err := replacePrivate(temporary, path, info); err != nil {
		return nil, model.NewError(model.CodeIO, "原子发布私有文件失败", err)
	}
	return info, nil
}

func LoadEndpoint(configDirectory string) (endpoint model.Endpoint, result error) {
	directory, release, err := privateRuntime(configDirectory, false)
	if err != nil {
		return model.Endpoint{}, err
	}
	// 发现读取也需固定整条目录链，释放错误不能被成功解析结果掩盖。
	defer func() { result = errors.Join(result, release()) }()
	file, err := openPrivate(filepath.Join(directory, "endpoint.json"), false)
	if err != nil {
		return model.Endpoint{}, err
	}
	defer func() { result = errors.Join(result, file.Close()) }()
	data, err := io.ReadAll(io.LimitReader(file, model.MaxMessageBytes+1))
	if err != nil {
		return model.Endpoint{}, model.NewError(model.CodeIO, "读取私有端点失败", err)
	}
	if len(data) > model.MaxMessageBytes {
		return model.Endpoint{}, model.NewError(model.CodeMessageTooLarge, "端点文件超过大小限制", nil)
	}
	if err := model.DecodeJSON(data, &endpoint); err != nil {
		return model.Endpoint{}, model.NewError(model.CodeInvalidRequest, "私有端点内容无效", err)
	}
	if err := endpoint.Validate(); err != nil {
		return model.Endpoint{}, err
	}
	return endpoint, nil
}

// Close仅删除本实例实际发布的端点及固定配置inode；替换、损坏或其他实例文件一律保留。
// 系统锁一直持有到端点清理完毕，关闭锁句柄才允许下一实例进入。
func (i *Instance) Close() error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.closed {
		return nil
	}
	i.closed = true
	cleanup := errors.Join(i.cleanupError, removeOwned(filepath.Join(i.directory, "endpoint.json"), i.published), removeOwned(configurationPath(i.directory, i.token), i.configuration))
	// 先回收本实例文件，再关永久锁句柄，最后解除目录链固定；顺序不能颠倒。
	if err := errors.Join(cleanup, i.lock.Close(), i.releaseRuntime()); err != nil {
		return model.NewError(model.CodeIO, "回收后台私有资源失败", err)
	}
	return nil
}

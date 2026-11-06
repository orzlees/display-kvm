package ipc

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/orzlee/displayKvm/internal/config"
	"github.com/orzlee/displayKvm/internal/model"
)

// PublishConfiguration只在发布端点前执行一次。必须传入创建08队列的同一个固定
// 配置值，而不是重新读取原文件，否则编辑原文件的竞争窗口会让界面和队列分叉。
// 该文件是本实例派生的只读发现材料，不是用户可编辑的配置保存入口。
func (i *Instance) PublishConfiguration(cfg config.Config) error {
	var encoded bytes.Buffer
	if err := config.Encode(&encoded, cfg); err != nil {
		return err
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.closed || i.published != nil || i.configuration != nil {
		return model.NewError(model.CodeInvalidRequest, "固定配置只能在本实例发布端点前写入一次", nil)
	}
	info, err := publishPrivate(i.directory, filepath.Base(configurationPath(i.directory, i.token)), encoded.Bytes())
	if err != nil {
		return err
	}
	i.configuration = info
	return nil
}

// PublishRuntime在仍持有同一个系统实例锁时发布下一轮。新配置以新随机令牌命名，
// 端点最后替换；加载者的前后身份检查保证不会把两轮配置与认证材料拼接。
// 调用方必须已等待旧Daemon及队列退出，本方法不拥有也不创建硬件队列。
func (i *Instance) PublishRuntime(address string, cfg config.Config) (model.Endpoint, error) {
	var encoded bytes.Buffer
	if err := config.Encode(&encoded, cfg); err != nil {
		return model.Endpoint{}, err
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.closed {
		return model.Endpoint{}, model.NewError(model.CodeInvalidRequest, "后台实例锁已关闭", nil)
	}
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return model.Endpoint{}, model.NewError(model.CodeInternal, "无法生成本轮认证材料", err)
	}
	token := base64.RawURLEncoding.EncodeToString(random[:])
	endpoint := model.Endpoint{Version: model.EndpointVersion, ProtocolVersion: model.ProtocolVersion, Network: "tcp4", Address: address, Token: token, PID: os.Getpid(), StartedAt: i.started}
	if err := endpoint.Validate(); err != nil {
		return model.Endpoint{}, err
	}
	data, err := json.Marshal(endpoint)
	if err != nil {
		return model.Endpoint{}, model.NewError(model.CodeInternal, "编码新轮次端点失败", err)
	}
	name := filepath.Base(configurationPath(i.directory, token))
	info, err := publishPrivate(i.directory, name, encoded.Bytes())
	if err != nil {
		return model.Endpoint{}, err
	}
	published, err := publishPrivate(i.directory, "endpoint.json", data)
	if err != nil {
		return model.Endpoint{}, errors.Join(err, removeOwned(configurationPath(i.directory, token), info))
	}
	oldToken, oldInfo := i.token, i.configuration
	i.token, i.configuration, i.published = token, info, published
	// 发现发布已完成，旧快照回收错误不能被误报成“新轮次未发布”；保留至Close报告。
	i.cleanupError = errors.Join(i.cleanupError, removeOwned(configurationPath(i.directory, oldToken), oldInfo))
	return endpoint, nil
}

// LoadConfiguration读取期望端点对应的不可变配置。每轮随机令牌绑定文件名，读取
// 前后核验端点身份：不能把旧后台的配置与新后台的切换能力拼接，也不回退原文件。
// 第二次核验后后台仍可能退出，但客户端后续请求始终使用同一旧令牌，不会误投到
// 重启后的新实例；调用者收到不可用错误后须重新Connect并刷新全部选择。
func LoadConfiguration(configDirectory string, expected model.Endpoint) (configuration config.Config, result error) {
	if err := expected.Validate(); err != nil {
		return config.Config{}, err
	}
	if err := checkConfigurationEndpoint(configDirectory, expected); err != nil {
		return config.Config{}, err
	}
	directory, release, err := privateRuntime(configDirectory, false)
	if err != nil {
		return config.Config{}, err
	}
	// 租约贯穿配置读取和最后一次端点复核；内层端点读取拥有自己的短期租约，
	// 不得提前解除外层目录链固定，否则同一令牌文件名仍可能解析到替换后的目录。
	defer func() { result = errors.Join(result, release()) }()
	file, err := openPrivate(configurationPath(directory, expected.Token), false)
	if err != nil {
		if model.CodeOf(err) == model.CodePermissionDenied {
			return config.Config{}, err
		}
		return config.Config{}, model.NewError(model.CodeDaemonUnavailable, "当前后台未提供本轮固定配置快照，请重启后台后重新连接", err)
	}
	cfg, loadError := config.Load(file)
	if err := errors.Join(loadError, file.Close()); err != nil {
		return config.Config{}, err
	}
	if err := checkConfigurationEndpoint(configDirectory, expected); err != nil {
		return config.Config{}, err
	}
	return cfg, nil
}

func configurationPath(directory, token string) string {
	// token只来自本实例crypto/rand结果或已通过Endpoint.Validate的期望端点，
	// 不允许原始外部字符串参与路径拼接，避免目录穿越及跨实例文件名猜测。
	return filepath.Join(directory, "config-"+token+".json")
}

func checkConfigurationEndpoint(configDirectory string, expected model.Endpoint) error {
	current, err := LoadEndpoint(configDirectory)
	if err != nil {
		if model.CodeOf(err) == model.CodePermissionDenied || model.CodeOf(err) == model.CodeUnsupportedVersion {
			return err
		}
		return model.NewError(model.CodeDaemonUnavailable, "后台端点已不可用，请重新连接并刷新配置", err)
	}
	if current.Token != expected.Token || current.PID != expected.PID || current.Address != expected.Address || current.Network != expected.Network || !current.StartedAt.Equal(expected.StartedAt) {
		return model.NewError(model.CodeDaemonUnavailable, "后台实例已经变化，请重新连接并刷新配置", nil)
	}
	return nil
}

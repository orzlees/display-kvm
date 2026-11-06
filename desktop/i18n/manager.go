package i18n

import (
	"embed"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

// 词典是桌面所有呈现入口的唯一消息源，随程序一起打包，不读取用户偏好文件。
//
//go:embed locales/zh-CN.json locales/en.json
var localeFiles embed.FS

// Manager归属于单个桌面实例，由服务、页面和宿主共享；不同实例没有可变全局语言。
type Manager struct {
	mu           sync.RWMutex
	preference   string
	locale       string
	systemLocale string
	revision     uint64
	catalogs     map[string]map[string]string
	changes      chan struct{}
}

func New(preference string, preferred []string) (*Manager, error) {
	if !validPreference(preference) {
		return nil, fmt.Errorf("invalid language preference %q", preference)
	}
	manager := &Manager{preference: preference, systemLocale: "en", revision: 1,
		catalogs: make(map[string]map[string]string, 2), changes: make(chan struct{}, 1)}
	// 首个支持项决定语言，不能因为列表中后面出现中文就覆盖前面的英文。
	// preferred仅在初始化时读取；调用者后续改写切片不会改变已确认的系统语言。
	for _, preferredLanguage := range preferred {
		language := strings.ToLower(strings.TrimSpace(preferredLanguage))
		if strings.HasPrefix(language, "zh") {
			manager.systemLocale = "zh-CN"
			break
		}
		if strings.HasPrefix(language, "en") {
			break
		}
	}
	for _, locale := range []string{"zh-CN", "en"} {
		data, err := localeFiles.ReadFile("locales/" + locale + ".json")
		if err != nil {
			return nil, fmt.Errorf("read language catalog %s: %w", locale, err)
		}
		var messages map[string]string
		if err := json.Unmarshal(data, &messages); err != nil {
			return nil, fmt.Errorf("decode language catalog %s: %w", locale, err)
		}
		if len(messages) == 0 {
			return nil, fmt.Errorf("empty language catalog %s", locale)
		}
		manager.catalogs[locale] = messages
	}
	manager.locale = preference
	if preference == "system" {
		manager.locale = manager.systemLocale
	}
	return manager, nil
}

func validPreference(preference string) bool {
	return preference == "system" || preference == "zh-CN" || preference == "en"
}

func (m *Manager) Snapshot() Catalog {
	m.mu.RLock()
	defer m.mu.RUnlock()
	messages := m.catalogs[m.locale]
	// 页面会把词典交给模板或序列化调用方；必须复制消息表，避免外部修改污染
	// 后续快照，也避免调用方写入与语言切换、其它窗口读取发生数据竞争。
	copy := make(map[string]string, len(messages))
	for key, value := range messages {
		copy[key] = value
	}
	return Catalog{Preference: m.preference, Locale: m.locale, Revision: m.revision, Messages: copy}
}

func (m *Manager) SetPreference(preference string) error {
	if !validPreference(preference) {
		return fmt.Errorf("invalid language preference %q", preference)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if preference == m.preference {
		return nil
	}
	m.preference, m.locale = preference, preference
	if preference == "system" {
		m.locale = m.systemLocale
	}
	m.revision++
	// 通知只提示重新读取完整快照；消费者慢或暂未订阅不能阻塞保存已确认偏好。
	// 通道不关闭，沿用桌面状态通知的生命周期约定，避免退出后出现无限零值事件。
	select {
	case m.changes <- struct{}{}:
	default:
	}
	return nil
}

func (m *Manager) Changes() <-chan struct{} { return m.changes }

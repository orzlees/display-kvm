// Package i18n提供桌面实例共享的语言状态与纯文本消息呈现。
package i18n

import "strings"

// Message保存语义和参数，避免把某次语言下的文字缓存成持久应用状态。
type Message struct {
	Key    string            `json:"key"`
	Values map[string]string `json:"values,omitempty"`
}

type Catalog struct {
	Preference string            `json:"preference"`
	Locale     string            `json:"locale"`
	Revision   uint64            `json:"revision"`
	Messages   map[string]string `json:"messages"`
}

// Text只对词典原文中的{name}进行一次字面插值；参数可能包含花括号或HTML，
// 不能再次解释为模板或脚本。输出端仍负责按HTML、属性或JSON上下文转义。
// 未知键和缺少的参数保留原标识，避免词典缺项被静默隐藏。
func (c Catalog) Text(key string, values map[string]string) string {
	text, found := c.Messages[key]
	if !found {
		return key
	}
	var result strings.Builder
	for len(text) > 0 {
		start := strings.IndexByte(text, '{')
		if start < 0 {
			result.WriteString(text)
			break
		}
		result.WriteString(text[:start])
		text = text[start:]
		end := strings.IndexByte(text, '}')
		if end < 0 {
			result.WriteString(text)
			break
		}
		if value, exists := values[text[1:end]]; exists {
			result.WriteString(value)
		} else {
			result.WriteString(text[:end+1])
		}
		text = text[end+1:]
	}
	return result.String()
}

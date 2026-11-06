package host

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf16"
)

// SystemLanguages只读取当前用户的系统语言，不保存偏好、不调用后台或启动外部程序。
// 失败时同时返回英文回退和错误，调用方仍可显示界面并保留真实诊断。
func SystemLanguages() ([]string, error) { return systemLanguages(platformLanguages) }

func systemLanguages(read func() ([]string, error)) ([]string, error) {
	raw, err := read()
	if err != nil {
		return []string{"en"}, err
	}
	preferred := make([]string, 0, len(raw))
	seen := make(map[string]bool)
	for _, value := range raw {
		value = strings.TrimSpace(value)
		if index := strings.IndexAny(value, ".@"); index >= 0 {
			value = value[:index]
		}
		value = strings.ReplaceAll(value, "_", "-")
		if value == "C" || value == "POSIX" {
			value = "en"
		}
		if value == "" || strings.ContainsAny(value, "\x00\r\n") {
			continue
		}
		key := strings.ToLower(value)
		if !seen[key] {
			seen[key] = true
			preferred = append(preferred, value)
		}
	}
	if len(preferred) == 0 {
		return []string{"en"}, errors.New("系统未提供可用的首选语言，已回退英文")
	}
	return preferred, nil
}

// Linux的LC_ALL覆盖消息类别，再依次使用LC_MESSAGES和LANG。LANGUAGE用于
// 消息首选顺序，但C/POSIX明确要求默认语言，不能被遗留LANGUAGE变量覆盖。
func environmentLanguages(getenv func(string) string) ([]string, error) {
	locale := ""
	for _, key := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		if locale = strings.TrimSpace(getenv(key)); locale != "" {
			break
		}
	}
	base := strings.SplitN(strings.SplitN(locale, ".", 2)[0], "@", 2)[0]
	if base == "C" || base == "POSIX" {
		return []string{"en"}, nil
	}
	var preferred []string
	if languages := strings.TrimSpace(getenv("LANGUAGE")); languages != "" {
		preferred = strings.Split(languages, ":")
	}
	if locale != "" {
		preferred = append(preferred, locale)
	}
	if len(preferred) == 0 {
		return nil, errors.New("LANGUAGE、LC_ALL、LC_MESSAGES和LANG均未提供系统语言")
	}
	return preferred, nil
}

// Windows返回双NUL终止的UTF-16多字符串。只解析系统声明的缓冲区和数量，
// 拒绝截断结果，避免把读取失败误判为用户选择了某个有效语言。
func windowsLanguageList(buffer []uint16, count uint32) ([]string, error) {
	if count == 0 || len(buffer) < 2 || buffer[len(buffer)-1] != 0 || buffer[len(buffer)-2] != 0 {
		return nil, errors.New("Windows首选语言列表为空或缺少终止符")
	}
	var result []string
	start := 0
	for index, value := range buffer {
		if value != 0 {
			continue
		}
		if index == start {
			if uint32(len(result)) != count || index != len(buffer)-1 {
				return nil, fmt.Errorf("Windows首选语言数量不一致：声明%d，实际%d", count, len(result))
			}
			return result, nil
		}
		result = append(result, string(utf16.Decode(buffer[start:index])))
		start = index + 1
	}
	return nil, errors.New("Windows首选语言列表被截断")
}

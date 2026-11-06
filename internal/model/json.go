package model

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"unicode/utf8"
)

// DecodeJSON供配置和消息复用严格的对象解码。重复键在不同语言中可能取首值或
// 末值，未知字段可能掩盖拼写错误；因此在产生任何业务动作前一并拒绝。
// 调用者必须先限制输入字节数；深度另限64，避免小体积深嵌套造成递归资源风险。
func DecodeJSON(data []byte, destination any) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '{' || !utf8.Valid(trimmed) {
		return fmt.Errorf("JSON必须为单个非null的UTF-8对象")
	}
	scanner := json.NewDecoder(bytes.NewReader(trimmed))
	scanner.UseNumber()
	if err := scanJSONValue(scanner, 0); err != nil {
		return err
	}
	if _, err := scanner.Token(); err != io.EOF {
		return fmt.Errorf("JSON对象后存在额外内容")
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.DisallowUnknownFields()
	return decoder.Decode(destination)
}

func scanJSONValue(decoder *json.Decoder, depth int) error {
	if depth > 64 {
		return fmt.Errorf("JSON嵌套深度超过64")
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		keys := make(map[string]bool)
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok || keys[key] {
				return fmt.Errorf("JSON对象含重复或非法字段")
			}
			keys[key] = true
			if err := scanJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim('}') {
			return fmt.Errorf("JSON对象未正确结束")
		}
	case '[':
		for decoder.More() {
			if err := scanJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim(']') {
			return fmt.Errorf("JSON数组未正确结束")
		}
	default:
		return fmt.Errorf("非法JSON结束符")
	}
	return nil
}

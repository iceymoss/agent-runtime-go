// Package json provides the runtime's private JSON serialization helpers.
package json

import (
	"bytes"
	stdjson "encoding/json"
	"fmt"
	"io"
)

// Marshal 序列化为字节切片。
func Marshal(v any) ([]byte, error) {
	return stdjson.Marshal(v)
}

// Unmarshal 反序列化字节切片。
func Unmarshal(data []byte, v any) error {
	return stdjson.Unmarshal(data, v)
}

// UnmarshalStrict rejects unknown fields and trailing JSON values. It is used
// for durable formats where silently accepting schema drift is unsafe.
func UnmarshalStrict(data []byte, v any) error {
	decoder := stdjson.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(v); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return fmt.Errorf("unexpected trailing JSON value")
		}
		return err
	}
	return nil
}

// Valid 判断字节切片是否为合法 JSON。
// 用于协议层的语法校验（如校验模型给出的工具参数），不涉及反序列化。
func Valid(data []byte) bool {
	return stdjson.Valid(data)
}

// MarshalString 序列化为字符串，便于写入 TEXT 列。
func MarshalString(v any) (string, error) {
	b, err := Marshal(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

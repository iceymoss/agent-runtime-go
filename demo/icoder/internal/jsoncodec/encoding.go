package jsoncodec

import "encoding/json"

func Marshal(v any) ([]byte, error) {
	return json.Marshal(v)
}

func MarshalString(v any) (string, error) {
	data, err := Marshal(v)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func Unmarshal(data []byte, v any) error {
	return json.Unmarshal(data, v)
}

package config

import "testing"

func TestDefaultTimeout(t *testing.T) {
	if Default().Timeout != 30 {
		t.Fatal("unexpected timeout")
	}
}

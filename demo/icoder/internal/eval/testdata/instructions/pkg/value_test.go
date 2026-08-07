package pkg

import "testing"

func TestValue(t *testing.T) {
	if Value() != 7 {
		t.Fatal("unexpected value")
	}
}

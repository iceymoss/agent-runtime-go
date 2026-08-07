package preserve

import "testing"

func TestValue(t *testing.T) {
	if Value() != 42 {
		t.Fatal("unexpected value")
	}
}

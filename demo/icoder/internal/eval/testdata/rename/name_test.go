package rename

import "testing"

func TestCurrentName(t *testing.T) {
	if CurrentName() != "current" {
		t.Fatal("unexpected name")
	}
}

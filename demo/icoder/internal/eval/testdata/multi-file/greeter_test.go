package multi

import "testing"

func TestGreet(t *testing.T) {
	if Greet("Ada") != "Hello, Ada" {
		t.Fatal("unexpected greeting")
	}
}

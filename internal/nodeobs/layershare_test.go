package nodeobs

import "testing"

func TestApproxEqual(t *testing.T) {
	if !ApproxEqual(100, 100, 0) {
		t.Fatal("exact")
	}
	if ApproxEqual(100, 101, 0) {
		t.Fatal("tol 0")
	}
	if !ApproxEqual(100, 105, 5) {
		t.Fatal("within tol")
	}
}

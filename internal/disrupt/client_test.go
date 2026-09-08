package disrupt

import (
	"testing"
	"time"
)

func TestFirstSuccessUID(t *testing.T) {
	t0 := time.Now().UTC()
	samples := []Sample{
		{At: t0.Add(10 * time.Millisecond), UID: "old", Status: 200, Path: "clusterip"},
		{At: t0.Add(50 * time.Millisecond), UID: "new", Status: 200, Path: "clusterip"},
	}
	s, ok := FirstSuccessUID(samples, "new", t0)
	if !ok || s.UID != "new" {
		t.Fatalf("got %+v ok=%v", s, ok)
	}
	any, ok := FirstAny200(samples, t0)
	if !ok || any.UID != "old" {
		t.Fatalf("naive should be old first: %+v", any)
	}
}

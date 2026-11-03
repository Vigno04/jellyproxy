package cache

import (
	"testing"
	"time"
)

func TestCoalescer(t *testing.T) {
	c := NewCoalescer()
	a, owner := c.Start("x")
	if !owner {
		t.Fatal()
	}
	b, owner := c.Start("x")
	if owner || a != b {
		t.Fatal()
	}
	done := make(chan struct{})
	go func() { <-b.Done(); close(done) }()
	c.Finish("x", a)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("wait")
	}
	_, owner = c.Start("x")
	if !owner {
		t.Fatal()
	}
}

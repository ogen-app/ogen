package handlers

import (
	"testing"
	"time"
)

func TestTenantWindowLimiter(t *testing.T) {
	l := newTenantWindowLimiter(2, time.Minute)
	now := time.Now()
	l.now = func() time.Time { return now }

	for i := range 2 {
		if !l.allow("t1") {
			t.Fatalf("call %d in a window must pass", i+1)
		}
	}
	if l.allow("t1") {
		t.Fatal("the third call in a window must be refused")
	}
	if !l.allow("t2") {
		t.Fatal("tenants are limited independently")
	}
	now = now.Add(time.Minute)
	if !l.allow("t1") {
		t.Fatal("a new window resets the count")
	}
}

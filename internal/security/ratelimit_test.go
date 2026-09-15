package security

import (
	"testing"
	"time"
)

func TestRateLimiterAllowsUpToCapacity(t *testing.T) {
	r := NewRateLimiterWith(3, 1.0)
	for i := 0; i < 3; i++ {
		ok, _ := r.Allow("user")
		if !ok {
			t.Fatalf("call %d denied, want allow within capacity", i)
		}
	}
	ok, wait := r.Allow("user")
	if ok {
		t.Error("over-capacity should be denied")
	}
	if wait <= 0 {
		t.Errorf("wait = %v, want > 0 hint", wait)
	}
}

func TestRateLimiterRefillsOverTime(t *testing.T) {
	r := NewRateLimiterWith(1, 100.0) // 100 tokens/sec — refills 1 token in 10ms
	r.Allow("user")
	if ok, _ := r.Allow("user"); ok {
		t.Error("immediate second call should be denied (bucket empty)")
	}
	time.Sleep(20 * time.Millisecond)
	if ok, _ := r.Allow("user"); !ok {
		t.Error("after refill window, call should succeed")
	}
}

func TestRateLimiterPerKeyIsolation(t *testing.T) {
	r := NewRateLimiterWith(1, 1.0)
	if ok, _ := r.Allow("alice"); !ok {
		t.Error("alice first call denied")
	}
	// alice is exhausted; bob should still get her own bucket.
	if ok, _ := r.Allow("bob"); !ok {
		t.Error("bob should have her own bucket — not blocked by alice")
	}
}

func TestRateLimiterReset(t *testing.T) {
	r := NewRateLimiterWith(1, 1.0)
	r.Allow("u")
	if ok, _ := r.Allow("u"); ok {
		t.Fatal("second call should be denied before reset")
	}
	r.Reset("u")
	if ok, _ := r.Allow("u"); !ok {
		t.Error("after reset, allow should succeed")
	}
}

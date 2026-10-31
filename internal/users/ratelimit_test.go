package users

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestRateLimiterSlidingWindow(t *testing.T) {
	clk := newClock()
	l := NewRateLimiter(3, time.Minute, clk.Now)
	for i := range 3 {
		if ok, _ := l.Allow("a"); !ok {
			t.Fatalf("attempt %d refused", i)
		}
		clk.Advance(10 * time.Second) // attempts at 0s, 10s, 20s
	}
	ok, retry := l.Allow("a") // at 30s: the first attempt leaves the window at 60s
	if ok || retry != 30*time.Second {
		t.Fatalf("4th attempt: ok=%v retry=%s, want refused and 30s", ok, retry)
	}
	if ok, _ := l.Allow("b"); !ok {
		t.Error("another key is affected")
	}
	// refused attempts are not recorded: at 60s exactly one slot is free again, not none
	clk.Advance(30 * time.Second)
	if ok, _ := l.Allow("a"); !ok {
		t.Fatal("slot of the first attempt not free after one window")
	}
	if ok, _ := l.Allow("a"); ok {
		t.Fatal("window holds 10s, 20s and the new one: must be full")
	}
	// sliding: the 10s and 20s attempts leave at 70s and 80s
	clk.Advance(10 * time.Second)
	if ok, _ := l.Allow("a"); !ok {
		t.Fatal("slot of the 10s attempt not free at 70s")
	}
}

func TestRateLimiterBlockEndsAfterOneWindow(t *testing.T) {
	// someone else's failures must not block an address for good: however many refused attempts
	// pile up, one window after the last COUNTED attempt the address is open again
	clk := newClock()
	l := NewRateLimiter(2, time.Minute, clk.Now)
	l.Allow("victim")
	l.Allow("victim")
	for range 1000 {
		clk.Advance(50 * time.Millisecond)
		if ok, _ := l.Allow("victim"); ok {
			t.Fatal("allowed inside the window")
		}
	}
	clk.Advance(time.Minute)
	if ok, _ := l.Allow("victim"); !ok {
		t.Fatal("still blocked one window after the last counted attempt")
	}
}

func TestRateLimiterReset(t *testing.T) {
	l := NewRateLimiter(1, time.Minute, newClock().Now)
	l.Allow("a")
	if ok, _ := l.Allow("a"); ok {
		t.Fatal("not limited")
	}
	l.Reset("a")
	if ok, _ := l.Allow("a"); !ok {
		t.Fatal("still limited after Reset")
	}
}

func TestRateLimiterDisabledAndNil(t *testing.T) {
	for _, l := range []*RateLimiter{nil, NewRateLimiter(0, time.Minute, nil), NewRateLimiter(5, 0, nil)} {
		for range 100 {
			if ok, _ := l.Allow("a"); !ok {
				t.Fatal("disabled limiter refused")
			}
		}
		l.Reset("a") // must not panic
	}
}

func TestRateLimiterLongKeysShareOneBucket(t *testing.T) {
	l := NewRateLimiter(1, time.Minute, newClock().Now)
	long := string(make([]byte, 10_000))
	l.Allow(long)
	if len(l.events) != 1 {
		t.Fatalf("%d keys", len(l.events))
	}
	for k := range l.events {
		if len(k) > maxKeyLen {
			t.Errorf("key of %d bytes stored", len(k))
		}
	}
}

func TestRateLimiterBoundedMemory(t *testing.T) {
	clk := newClock()
	l := NewRateLimiter(1, time.Minute, clk.Now)
	for i := range maxLimiterKeys {
		if ok, _ := l.Allow(fmt.Sprint("k", i)); !ok {
			t.Fatalf("key %d refused", i)
		}
	}
	if ok, _ := l.Allow("one-too-many"); ok {
		t.Fatal("new key accepted beyond the cap (must fail closed)")
	}
	if ok, _ := l.Allow("k0"); ok {
		t.Fatal("existing key not limited")
	}
	clk.Advance(2 * time.Minute)
	if ok, _ := l.Allow("one-too-many"); !ok {
		t.Fatal("after the window the sweep must make room")
	}
	if len(l.events) > 2 {
		t.Errorf("%d keys left after the sweep", len(l.events))
	}
}

func TestRateLimiterConcurrent(t *testing.T) {
	l := NewRateLimiter(5, time.Hour, nil)
	var wg sync.WaitGroup
	var mu sync.Mutex
	allowed := 0
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ok, _ := l.Allow("a"); ok {
				mu.Lock()
				allowed++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if allowed != 5 {
		t.Errorf("%d attempts allowed, want exactly 5", allowed)
	}
}

func TestRateLimitFromEnv(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	l, w, err := RateLimitFromEnv(env(nil), "LOGIN", 5, time.Minute)
	if l != 5 || w != time.Minute || err != nil {
		t.Errorf("defaults: %d %s %v", l, w, err)
	}
	l, w, err = RateLimitFromEnv(env(map[string]string{"LOGIN_RATE_LIMIT": " 3 ", "LOGIN_RATE_WINDOW": "30s"}), "LOGIN", 5, time.Minute)
	if l != 3 || w != 30*time.Second || err != nil {
		t.Errorf("custom: %d %s %v", l, w, err)
	}
	l, _, err = RateLimitFromEnv(env(map[string]string{"LOGIN_RATE_LIMIT": "0"}), "LOGIN", 5, time.Minute)
	if l != 0 || err != nil {
		t.Errorf("0 must disable: %d %v", l, err)
	}
	for _, m := range []map[string]string{
		{"LOGIN_RATE_LIMIT": "many"}, {"LOGIN_RATE_LIMIT": "-1"}, {"LOGIN_RATE_WINDOW": "soon"}, {"LOGIN_RATE_WINDOW": "0s"}, {"LOGIN_RATE_WINDOW": "-1m"},
	} {
		l, w, err := RateLimitFromEnv(env(m), "LOGIN", 5, time.Minute)
		if err == nil || l != 5 || w != time.Minute {
			t.Errorf("%v: %d %s %v (a typo must give the default and an error)", m, l, w, err)
		}
	}
	// the prefix matters
	l, _, _ = RateLimitFromEnv(env(map[string]string{"LOGIN_RATE_LIMIT": "1"}), "REGISTER", 60, time.Minute)
	if l != 60 {
		t.Errorf("REGISTER read LOGIN_RATE_LIMIT: %d", l)
	}
}

package limiter

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestAllow_BurstThenDeny(t *testing.T) {
	l := New(1, 3) // 1 token/s, burst of 3

	for i := 0; i < 3; i++ {
		if !l.Allow("client-a") {
			t.Fatalf("request %d: expected allow within burst", i)
		}
	}

	if l.Allow("client-a") {
		t.Fatal("expected deny once burst is exhausted")
	}
}

func TestAllow_RefillsOverTime(t *testing.T) {
	l := New(100, 1) // fast refill, burst 1

	if !l.Allow("client-b") {
		t.Fatal("expected first request to be allowed")
	}
	if l.Allow("client-b") {
		t.Fatal("expected immediate second request to be denied")
	}

	time.Sleep(20 * time.Millisecond) // >= 2 tokens at 100/s

	if !l.Allow("client-b") {
		t.Fatal("expected request to be allowed after refill")
	}
}

func TestAllow_KeysAreIndependent(t *testing.T) {
	l := New(1, 1)

	if !l.Allow("client-c") {
		t.Fatal("expected client-c to be allowed")
	}
	if !l.Allow("client-d") {
		t.Fatal("expected client-d to be allowed independently of client-c")
	}
}

func TestAllow_ConcurrentSameKey(t *testing.T) {
	l := New(1000, 50)

	const n = 200
	var wg sync.WaitGroup
	allowed := make([]bool, n)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			allowed[i] = l.Allow("client-e")
		}(i)
	}
	wg.Wait()

	count := 0
	for _, a := range allowed {
		if a {
			count++
		}
	}
	if count == 0 {
		t.Fatal("expected at least some requests to be allowed")
	}
	if count > n {
		t.Fatalf("allowed count %d exceeds total requests %d", count, n)
	}
}

func TestCleanup_RemovesInactiveVisitors(t *testing.T) {
	l := New(1, 1)
	l.Allow("stale-client")

	l.mu.Lock()
	l.visitors["stale-client"].lastSeen = time.Now().Add(-time.Hour)
	l.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	l.StartCleanup(ctx, 5*time.Millisecond, 10*time.Millisecond)

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		l.mu.RLock()
		_, exists := l.visitors["stale-client"]
		l.mu.RUnlock()
		if !exists {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("expected stale visitor to be cleaned up")
}

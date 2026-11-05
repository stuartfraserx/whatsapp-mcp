package ratelimit_test

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fmflurry/whatsapp-mcp/internal/ratelimit"
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func mustAllow(t *testing.T, l *ratelimit.Limiter, msg string) {
	t.Helper()
	if ok, retry := l.Allow(); !ok || retry != 0 {
		t.Fatalf("%s: Allow() = (%v, %v), want (true, 0)", msg, ok, retry)
	}
}

func mustDeny(t *testing.T, l *ratelimit.Limiter, wantRetry time.Duration, msg string) {
	t.Helper()
	ok, retry := l.Allow()
	if ok || retry != wantRetry {
		t.Fatalf("%s: Allow() = (%v, %v), want (false, %v)", msg, ok, retry, wantRetry)
	}
}

func TestMinIntervalIsEnforcedWithExactRetryAfter(t *testing.T) {
	t.Parallel()
	clock := newClock()
	l := ratelimit.New(3*time.Second, 100, clock)

	mustAllow(t, l, "first send")
	mustDeny(t, l, 3*time.Second, "immediately after")

	clock.Advance(1 * time.Second)
	mustDeny(t, l, 2*time.Second, "after 1s")

	clock.Advance(2*time.Second - time.Nanosecond)
	mustDeny(t, l, time.Nanosecond, "1ns before the interval elapses")

	clock.Advance(time.Nanosecond)
	mustAllow(t, l, "exactly at the interval")
}

func TestRefusedAttemptsDoNotConsumeSlotsOrDelayTheNextSend(t *testing.T) {
	t.Parallel()
	clock := newClock()
	l := ratelimit.New(3*time.Second, 2, clock)

	mustAllow(t, l, "first")
	for range 10 {
		clock.Advance(200 * time.Millisecond)
		if ok, _ := l.Allow(); ok {
			t.Fatal("send allowed inside the minimum interval")
		}
	}
	clock.Advance(1 * time.Second)
	mustAllow(t, l, "3s after the first send, refusals in between must not push the window")
	clock.Advance(3 * time.Second)
	if ok, _ := l.Allow(); ok {
		t.Fatal("refused attempts consumed no slot, so only 2 sends fit in the hour")
	}
}

func TestHourlyCapIsEnforcedOverASlidingWindow(t *testing.T) {
	t.Parallel()
	clock := newClock()
	l := ratelimit.New(time.Second, 3, clock)

	mustAllow(t, l, "send at 0m")
	clock.Advance(10 * time.Minute)
	mustAllow(t, l, "send at 10m")
	clock.Advance(10 * time.Minute)
	mustAllow(t, l, "send at 20m")

	clock.Advance(10 * time.Minute)
	mustDeny(t, l, 30*time.Minute, "cap reached at 30m; oldest slot frees at 60m")

	clock.Advance(30*time.Minute - time.Nanosecond)
	mustDeny(t, l, time.Nanosecond, "1ns before the oldest send leaves the window")

	clock.Advance(time.Nanosecond)
	mustAllow(t, l, "at 60m the 0m send leaves the window")

	clock.Advance(time.Second)
	mustDeny(t, l, 10*time.Minute-time.Second, "window now holds 10m, 20m, 60m")

	clock.Advance(10*time.Minute - time.Second)
	mustAllow(t, l, "at 70m the 10m send leaves the window (sliding, not fixed buckets)")
}

func TestSlotsAreFreedAfterAnHour(t *testing.T) {
	t.Parallel()
	clock := newClock()
	l := ratelimit.New(time.Second, 5, clock)
	for i := range 5 {
		mustAllow(t, l, "fill window")
		if i < 4 {
			clock.Advance(time.Second)
		}
	}
	if ok, _ := l.Allow(); ok {
		t.Fatal("sixth send must be refused")
	}

	clock.Advance(time.Hour + time.Second)
	for i := range 5 {
		mustAllow(t, l, "after the window passed")
		if i < 4 {
			clock.Advance(time.Second)
		}
	}
	if ok, _ := l.Allow(); ok {
		t.Fatal("cap must apply again after the window refilled")
	}
}

func TestRetryAfterIsTheLongerOfBothConstraints(t *testing.T) {
	t.Parallel()
	clock := newClock()
	l := ratelimit.New(3*time.Second, 1, clock)
	mustAllow(t, l, "first")

	clock.Advance(time.Second)
	mustDeny(t, l, time.Hour-time.Second, "hourly wait dominates the 2s interval wait")

	clock.Advance(time.Hour - time.Second)
	mustAllow(t, l, "after the hourly wait")
}

func TestIntervalWaitDominatesWhenCapHasRoom(t *testing.T) {
	t.Parallel()
	clock := newClock()
	l := ratelimit.New(time.Minute, 1000, clock)
	mustAllow(t, l, "first")
	clock.Advance(10 * time.Second)
	mustDeny(t, l, 50*time.Second, "interval wait")
}

func TestZeroCapNeverAllows(t *testing.T) {
	t.Parallel()
	clock := newClock()
	l := ratelimit.New(time.Second, 0, clock)
	for range 3 {
		mustDeny(t, l, time.Hour, "cap of zero")
		clock.Advance(2 * time.Hour)
	}
}

func TestClockMovingBackwardsDoesNotGrantExtraSends(t *testing.T) {
	t.Parallel()
	clock := newClock()
	l := ratelimit.New(3*time.Second, 100, clock)
	mustAllow(t, l, "first")
	clock.Advance(-time.Minute)
	if ok, retry := l.Allow(); ok || retry < 3*time.Second {
		t.Fatalf("Allow() after the clock went backwards = (%v, %v), want a refusal with at least the minimum interval", ok, retry)
	}
}

func TestNilClockUsesSystemClock(t *testing.T) {
	t.Parallel()
	l := ratelimit.New(time.Hour, 5, nil)
	mustAllow(t, l, "first")
	ok, retry := l.Allow()
	if ok || retry <= 0 || retry > time.Hour {
		t.Fatalf("second Allow() = (%v, %v), want refusal with 0 < retry <= 1h", ok, retry)
	}
}

func TestSystemClockTracksWallTime(t *testing.T) {
	t.Parallel()
	before := time.Now()
	got := ratelimit.SystemClock{}.Now()
	after := time.Now()
	if got.Before(before) || got.After(after) {
		t.Errorf("SystemClock.Now() = %v, want within [%v, %v]", got, before, after)
	}
}

func TestConcurrentAllowNeverExceedsHourlyCap(t *testing.T) {
	t.Parallel()
	const maxSends, goroutines, attempts = 50, 32, 20
	clock := newClock()
	l := ratelimit.New(0, maxSends, clock)

	var allowed atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for range attempts {
				if ok, _ := l.Allow(); ok {
					allowed.Add(1)
				}
			}
		}()
	}
	close(start)
	wg.Wait()

	if got := allowed.Load(); got != maxSends {
		t.Errorf("%d sends allowed concurrently, want exactly %d", got, maxSends)
	}
}

func TestConcurrentAllowHonoursMinInterval(t *testing.T) {
	t.Parallel()
	clock := newClock()
	l := ratelimit.New(time.Second, 1000, clock)

	var allowed atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if ok, _ := l.Allow(); ok {
				allowed.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()

	if got := allowed.Load(); got != 1 {
		t.Errorf("%d sends allowed at the same instant, want exactly 1", got)
	}
}

func reserve(t *testing.T, l *ratelimit.Limiter, msg string) ratelimit.Slot {
	t.Helper()
	ok, retry, slot := l.Reserve()
	if !ok || retry != 0 {
		t.Fatalf("%s: Reserve() = (%v, %v), want (true, 0)", msg, ok, retry)
	}
	return slot
}

func TestRefundRestoresTheSlotAndInterval(t *testing.T) {
	t.Parallel()
	clock := newClock()
	l := ratelimit.New(3*time.Second, 2, clock)
	first := reserve(t, l, "first")
	clock.Advance(5 * time.Second)
	second := reserve(t, l, "second")
	mustDeny(t, l, time.Hour-5*time.Second, "cap reached")

	l.Refund(second)
	reserve(t, l, "second again after refund, previous send is 5s old")

	l.Refund(first)
	mustDeny(t, l, 3*time.Second, "interval applies to the remaining send")
}

func TestRefundRemovesExactlyTheRefundedSlot(t *testing.T) {
	t.Parallel()
	clock := newClock()
	l := ratelimit.New(10*time.Second, 2, clock)
	slotA := reserve(t, l, "A")
	clock.Advance(10 * time.Second)
	reserve(t, l, "B")

	l.Refund(slotA)

	clock.Advance(time.Second)
	mustDeny(t, l, 9*time.Second, "B is still the last send, so the interval runs from B")
	clock.Advance(9 * time.Second)
	reserve(t, l, "C once the interval since B has passed")
	mustDeny(t, l, time.Hour-10*time.Second, "B and C hold both hourly slots")
}

func TestRefundKeepsHourlyCountOfOtherSends(t *testing.T) {
	t.Parallel()
	clock := newClock()
	l := ratelimit.New(time.Second, 2, clock)
	slotA := reserve(t, l, "A")
	clock.Advance(time.Second)
	reserve(t, l, "B")
	mustDeny(t, l, time.Hour-time.Second, "cap of 2 reached")

	l.Refund(slotA)

	clock.Advance(time.Second)
	reserve(t, l, "one hourly slot was freed")
	mustDeny(t, l, time.Hour-time.Second, "B and C hold both slots")
}

func TestRefundIsIdempotentAndIgnoresZeroSlot(t *testing.T) {
	t.Parallel()
	clock := newClock()
	l := ratelimit.New(time.Second, 2, clock)
	slotA := reserve(t, l, "A")
	clock.Advance(time.Second)
	reserve(t, l, "B")

	l.Refund(ratelimit.Slot{})
	l.Refund(slotA)
	l.Refund(slotA)

	clock.Advance(time.Second)
	reserve(t, l, "only A was removed")
	mustDeny(t, l, time.Hour-time.Second, "B and C hold both slots")
}

func TestRefundWithoutSendsIsANoop(t *testing.T) {
	t.Parallel()
	l := ratelimit.New(time.Second, 1, newClock())
	l.Refund(ratelimit.Slot{})
	mustAllow(t, l, "first")
	mustDeny(t, l, time.Hour, "cap")
}

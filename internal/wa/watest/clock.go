package watest

import (
	"sync"
	"time"
)

// ManualClock advances only when a test asks it to. Timer channels are buffered.
type ManualClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []clockTimer
}
type clockTimer struct {
	at time.Time
	ch chan time.Time
}

func NewManualClock(now time.Time) *ManualClock { return &ManualClock{now: now} }
func (c *ManualClock) Now() time.Time           { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *ManualClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	c.timers = append(c.timers, clockTimer{c.now.Add(d), ch})
	return ch
}
func (c *ManualClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	keep := c.timers[:0]
	for _, t := range c.timers {
		if !t.at.After(c.now) {
			t.ch <- c.now
		} else {
			keep = append(keep, t)
		}
	}
	c.timers = keep
}
func (c *ManualClock) Timers() int { c.mu.Lock(); defer c.mu.Unlock(); return len(c.timers) }

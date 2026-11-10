package llm_test

import (
	"context"
	"sync"
	"time"
)

// fakeClock is a Clock whose Sleep returns at once, moves the time forward
// and records the wait.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	sleeps []time.Duration
	// onSleep, if set, runs at the start of every Sleep.
	onSleep func(d time.Duration)
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	c.mu.Lock()
	c.sleeps = append(c.sleeps, d)
	hook := c.onSleep
	c.mu.Unlock()
	if hook != nil {
		hook(d)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	c.Advance(d)
	return nil
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func (c *fakeClock) Sleeps() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration(nil), c.sleeps...)
}
